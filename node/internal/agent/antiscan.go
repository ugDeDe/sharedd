package agent

// Антискан на ноде: регистратор собирает список сканеров (abuse-листы ∩
// российские диапазоны) и объявляет его ревизию в /config; агент скачивает
// список с GET /antiscan только при смене ревизии, атомарно загружает его в
// ipset (hash:net — список содержит подсети) и дропает источники на общем
// порту прокси. Агент сам abuse-листы больше не качает.
//
// Нет списка от регистратора (старый регистратор, ещё не собран, источники
// недоступны) — действует последний применённый: он живёт в ядре и в
// antiscanStateFile, откуда восстанавливается после рестарта/перезагрузки.
// Ревизия "disabled" — оператор выключил антискан: хук, цепочка и set снимаются.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sharedd/node/internal/config"
)

const (
	antiscanSet     = "sharedd_scanners_net"
	antiscanTempSet = "sharedd_scan_tmp"
	// antiscanLegacySet — hash:ip прежних версий (ipsum целиком, без гео).
	// Тип ipset на лету не меняется, поэтому новый set назван иначе, а
	// legacy снимается при первом применении списка от регистратора.
	antiscanLegacySet = "sharedd_scanners"
	antiscanChain     = "ANTISCAN_MTPROTO"
	antiscanInterval  = 30 * time.Minute
	// antiscanDisabled — ревизия в /config при выключенном антискане
	// (machineapi.AntiscanDisabled регистратора).
	antiscanDisabled       = "disabled"
	antiscanRevisionHeader = "X-ShareDD-Antiscan-Revision"
	antiscanMaxBody        = 32 << 20
)

var antiscanKick = make(chan struct{}, 1)

// antiscanStateFile — последний применённый список (первая строка —
// "# revision <rev>"); var — для тестов.
var antiscanStateFile = "/var/lib/sharedd/antiscan.list"

// antiscanApplied — ревизия, загруженная в ядро этим процессом ("" — ещё
// ничего). Трогает только antiscanLoop.
var (
	antiscanApplied    string
	antiscanNoListSeen bool
)

var antiscanRun = func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	return cmd.CombinedOutput()
}

// antiscanFetch — GET /antiscan. rev == "" — регистратору нечего отдать (204).
var antiscanFetch = func(_ context.Context, cfg *config.NodeConfig) (data []byte, rev string, err error) {
	resp, err := registryRequest(&http.Client{Timeout: 60 * time.Second}, cfg, http.MethodGet, "/antiscan", nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, "", nil
	case http.StatusOK:
	default:
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	rev = resp.Header.Get(antiscanRevisionHeader)
	if rev == "" {
		return nil, "", fmt.Errorf("response without %s", antiscanRevisionHeader)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, antiscanMaxBody+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) > antiscanMaxBody {
		return nil, "", fmt.Errorf("list exceeds %d bytes", antiscanMaxBody)
	}
	return data, rev, nil
}

// parseAntiscanEntries — IPv4-адрес или CIDR на строку; прочее пропускается.
func parseAntiscanEntries(data []byte) []string {
	seen := map[string]bool{}
	out := []string{}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		var entry string
		if strings.Contains(fields[0], "/") {
			p, err := netip.ParsePrefix(fields[0])
			if err != nil || !p.Addr().Is4() {
				continue
			}
			entry = p.Masked().String()
		} else {
			a, err := netip.ParseAddr(fields[0])
			if err != nil || !a.Is4() {
				continue
			}
			entry = a.String()
		}
		if !seen[entry] {
			seen[entry] = true
			out = append(out, entry)
		}
	}
	return out
}

func runAntiscan(ctx context.Context, stdin []byte, name string, args ...string) error {
	out, err := antiscanRun(ctx, stdin, name, args...)
	if err != nil {
		return fmt.Errorf("%s %s: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func removeAntiscanHooks(ctx context.Context) {
	out, err := antiscanRun(ctx, nil, "iptables", "-S", "INPUT")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "-A" || fields[1] != "INPUT" || !strings.Contains(line, "-j "+antiscanChain) {
			continue
		}
		args := append([]string{"-D", "INPUT"}, fields[2:]...)
		_, _ = antiscanRun(ctx, nil, "iptables", args...)
	}
}

// removeAntiscanAll — полное снятие (антискан выключен оператором).
func removeAntiscanAll(ctx context.Context) {
	removeAntiscanHooks(ctx)
	_, _ = antiscanRun(ctx, nil, "iptables", "-F", antiscanChain)
	_, _ = antiscanRun(ctx, nil, "iptables", "-X", antiscanChain)
	for _, set := range []string{antiscanSet, antiscanLegacySet, antiscanTempSet} {
		_, _ = antiscanRun(ctx, nil, "ipset", "destroy", set)
	}
}

// loadAntiscanSet — атомарная замена содержимого set через временный + swap.
func loadAntiscanSet(ctx context.Context, entries []string) error {
	_ = runAntiscan(ctx, nil, "ipset", "destroy", antiscanTempSet)
	var restore strings.Builder
	fmt.Fprintf(&restore, "create %s hash:net family inet hashsize 4096 maxelem 1048576\n", antiscanTempSet)
	for _, e := range entries {
		fmt.Fprintf(&restore, "add %s %s\n", antiscanTempSet, e)
	}
	if err := runAntiscan(ctx, []byte(restore.String()), "ipset", "restore", "-exist"); err != nil {
		return err
	}
	if err := runAntiscan(ctx, nil, "ipset", "create", antiscanSet, "hash:net", "family", "inet", "hashsize", "4096", "maxelem", "1048576", "-exist"); err != nil {
		return err
	}
	if err := runAntiscan(ctx, nil, "ipset", "swap", antiscanTempSet, antiscanSet); err != nil {
		return err
	}
	_ = runAntiscan(ctx, nil, "ipset", "destroy", antiscanTempSet)
	return nil
}

// hookAntiscan — цепочка дропает источники из set, INPUT-хук стоит только на
// общем порту. Правило legacy-set'а и сам legacy-set снимаются.
func hookAntiscan(ctx context.Context, port int) error {
	// Существующая цепочка — норма, ошибка создания игнорируется.
	_, _ = antiscanRun(ctx, nil, "iptables", "-N", antiscanChain)
	if _, err := antiscanRun(ctx, nil, "iptables", "-C", antiscanChain, "-m", "set", "--match-set", antiscanSet, "src", "-j", "DROP"); err != nil {
		if err := runAntiscan(ctx, nil, "iptables", "-A", antiscanChain, "-m", "set", "--match-set", antiscanSet, "src", "-j", "DROP"); err != nil {
			return err
		}
	}
	if _, err := antiscanRun(ctx, nil, "iptables", "-D", antiscanChain, "-m", "set", "--match-set", antiscanLegacySet, "src", "-j", "DROP"); err == nil {
		_, _ = antiscanRun(ctx, nil, "ipset", "destroy", antiscanLegacySet)
		log.Printf("antiscan: legacy %s set (unfiltered ipsum) removed", antiscanLegacySet)
	}
	removeAntiscanHooks(ctx)
	return runAntiscan(ctx, nil, "iptables", "-I", "INPUT", "1", "-p", "tcp", "--dport", fmt.Sprint(port), "-j", antiscanChain)
}

func saveAntiscanState(rev string, entries []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# revision %s\n", rev)
	for _, e := range entries {
		b.WriteString(e)
		b.WriteByte('\n')
	}
	dir := filepath.Dir(antiscanStateFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".antiscan-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), antiscanStateFile)
}

func loadAntiscanState() (rev string, entries []string, ok bool) {
	data, err := os.ReadFile(antiscanStateFile)
	if err != nil {
		return "", nil, false
	}
	first, _, _ := strings.Cut(string(data), "\n")
	rev, found := strings.CutPrefix(first, "# revision ")
	if !found || strings.TrimSpace(rev) == "" {
		return "", nil, false
	}
	return strings.TrimSpace(rev), parseAntiscanEntries(data), true
}

func applyAntiscan(cfg *config.NodeConfig) error {
	shared := sharedConfigCache.Get()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	rev := shared.AntiscanRevision

	if rev == antiscanDisabled {
		if antiscanApplied != antiscanDisabled {
			removeAntiscanAll(ctx)
			_ = os.Remove(antiscanStateFile)
			antiscanApplied = antiscanDisabled
			log.Printf("antiscan disabled by registry: hook, chain and scanner set removed")
		}
		return nil
	}
	if shared.ProxyPort < 1 || shared.ProxyPort > 65535 {
		return fmt.Errorf("registry returned invalid proxy_port %d", shared.ProxyPort)
	}
	telemtCfg, err := loadTelemtConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		return err
	}
	if localPort := telemtProxyPort(telemtCfg); localPort != shared.ProxyPort {
		removeAntiscanHooks(ctx)
		return fmt.Errorf("local proxy port %d differs from registry shared port %d; antiscan disabled", localPort, shared.ProxyPort)
	}

	// Первый прогон процесса: поднять сохранённый список, если регистратор
	// ещё ничего не объявил или объявил ту же ревизию (без повторной загрузки).
	if antiscanApplied == "" || antiscanApplied == antiscanDisabled {
		if savedRev, entries, ok := loadAntiscanState(); ok && (rev == "" || rev == savedRev) {
			if err := loadAntiscanSet(ctx, entries); err != nil {
				return err
			}
			antiscanApplied = savedRev
			log.Printf("antiscan: restored saved list (%d entries, revision %s)", len(entries), savedRev)
		}
	}

	var fetchErr error
	if rev != "" && rev != antiscanApplied {
		data, served, err := antiscanFetch(ctx, cfg)
		switch {
		case err != nil:
			fetchErr = fmt.Errorf("download scanner list from registry: %w", err)
		case served == "":
			// Регистратор успел потерять список (рестарт) — держим последний.
		default:
			entries := parseAntiscanEntries(data)
			if err := loadAntiscanSet(ctx, entries); err != nil {
				return err
			}
			antiscanApplied = served
			if err := saveAntiscanState(served, entries); err != nil {
				log.Printf("antiscan: save %s: %v", antiscanStateFile, err)
			}
			log.Printf("antiscan updated: %d RU scanner entries (revision %s)", len(entries), served)
		}
	}
	if antiscanApplied == "" || antiscanApplied == antiscanDisabled {
		if !antiscanNoListSeen {
			antiscanNoListSeen = true
			log.Printf("antiscan: registry has no scanner list yet; nothing to apply")
		}
		return fetchErr
	}
	if err := hookAntiscan(ctx, shared.ProxyPort); err != nil {
		return err
	}
	return fetchErr
}

func kickAntiscan() {
	select {
	case antiscanKick <- struct{}{}:
	default:
	}
}

func antiscanLoop(cfg *config.NodeConfig) {
	run := func() {
		if err := applyAntiscan(cfg); err != nil {
			log.Printf("antiscan: %v", err)
		}
	}
	// Initial /config is normally applied before this goroutine starts and may
	// have queued a kick. Consume it so startup performs exactly one update.
	select {
	case <-antiscanKick:
	default:
	}
	run()
	ticker := time.NewTicker(antiscanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			run()
		case <-antiscanKick:
			run()
		}
	}
}
