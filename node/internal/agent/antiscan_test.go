package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"sharedd/node/internal/config"
)

func TestParseAntiscanEntries(t *testing.T) {
	got := parseAntiscanEntries([]byte("# revision abc\n1.2.3.4\ninvalid\n2001:db8::1\n10.0.0.7/24\n1.2.3.4\n2001:db8::/32\n"))
	if strings.Join(got, ",") != "1.2.3.4,10.0.0.0/24" {
		t.Fatalf("unexpected list: %v", got)
	}
}

// antiscanHarness — изолированное состояние антискана: фиктивные iptables/
// ipset (команды пишутся в calls), telemt.toml с портом, state-файл во
// временном каталоге и регистратор с заданным ответом /antiscan.
type antiscanHarness struct {
	cfg      *config.NodeConfig
	calls    []string
	fetches  int
	list     string
	rev      string
	fetchErr error
}

func newAntiscanHarness(t *testing.T, localPort int) *antiscanHarness {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = "+strconv.Itoa(localPort)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &antiscanHarness{cfg: &config.NodeConfig{}}
	h.cfg.Telemt.ConfigPath = path
	oldRun, oldFetch, oldShared := antiscanRun, antiscanFetch, sharedConfigCache.Get()
	oldFile, oldApplied, oldSeen := antiscanStateFile, antiscanApplied, antiscanNoListSeen
	t.Cleanup(func() {
		antiscanRun, antiscanFetch = oldRun, oldFetch
		sharedConfigCache.Set(oldShared)
		antiscanStateFile, antiscanApplied, antiscanNoListSeen = oldFile, oldApplied, oldSeen
	})
	antiscanStateFile = filepath.Join(dir, "antiscan.list")
	antiscanApplied, antiscanNoListSeen = "", false
	antiscanRun = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		h.calls = append(h.calls, line)
		if line == "iptables -S INPUT" {
			return []byte("-A INPUT -p tcp -m tcp --dport 443 -j ANTISCAN_MTPROTO\n"), nil
		}
		if strings.Contains(line, " -C ") {
			return nil, os.ErrNotExist
		}
		return nil, nil
	}
	antiscanFetch = func(context.Context, *config.NodeConfig) ([]byte, string, error) {
		h.fetches++
		return []byte(h.list), h.rev, h.fetchErr
	}
	return h
}

func (h *antiscanHarness) joined() string { return strings.Join(h.calls, "\n") }

func TestApplyAntiscanLoadsRegistryListAsHashNet(t *testing.T) {
	h := newAntiscanHarness(t, 8443)
	h.list, h.rev = "5.0.1.1\n5.0.2.0/24\n", "rev1"
	sharedConfigCache.Set(SharedConfig{ProxyPort: 8443, AntiscanRevision: "rev1"})
	if err := applyAntiscan(h.cfg); err != nil {
		t.Fatal(err)
	}
	joined := h.joined()
	for _, want := range []string{
		"ipset create sharedd_scanners_net hash:net family inet",
		"ipset swap sharedd_scan_tmp sharedd_scanners_net",
		"iptables -I INPUT 1 -p tcp --dport 8443 -j ANTISCAN_MTPROTO",
		"iptables -D ANTISCAN_MTPROTO -m set --match-set sharedd_scanners src -j DROP",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if rev, entries, ok := loadAntiscanState(); !ok || rev != "rev1" || strings.Join(entries, ",") != "5.0.1.1,5.0.2.0/24" {
		t.Fatalf("applied list must be saved: %v %v %v", rev, entries, ok)
	}

	// Та же ревизия — без повторной загрузки.
	h.calls = nil
	if err := applyAntiscan(h.cfg); err != nil {
		t.Fatal(err)
	}
	if h.fetches != 1 || strings.Contains(h.joined(), "ipset swap") {
		t.Fatalf("unchanged revision must not refetch (fetches=%d):\n%s", h.fetches, h.joined())
	}
}

func TestApplyAntiscanWithoutRegistryListKeepsLast(t *testing.T) {
	h := newAntiscanHarness(t, 443)
	sharedConfigCache.Set(SharedConfig{ProxyPort: 443})
	if err := applyAntiscan(h.cfg); err != nil {
		t.Fatal(err)
	}
	if h.fetches != 0 || strings.Contains(h.joined(), "ipset") || strings.Contains(h.joined(), "-D INPUT") {
		t.Fatalf("without a revision nothing may be touched:\n%s", h.joined())
	}

	// После рестарта агента сохранённый список поднимается без загрузки.
	if err := saveAntiscanState("saved", []string{"5.0.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if err := applyAntiscan(h.cfg); err != nil {
		t.Fatal(err)
	}
	if antiscanApplied != "saved" || h.fetches != 0 || !strings.Contains(h.joined(), "ipset swap sharedd_scan_tmp sharedd_scanners_net") {
		t.Fatalf("saved list must be restored (applied=%q):\n%s", antiscanApplied, h.joined())
	}

	// Регистратор недоступен при новой ревизии — прежний set не трогаем.
	h.calls, h.fetchErr = nil, errors.New("down")
	sharedConfigCache.Set(SharedConfig{ProxyPort: 443, AntiscanRevision: "rev2"})
	if err := applyAntiscan(h.cfg); err == nil || antiscanApplied != "saved" || strings.Contains(h.joined(), "ipset swap") {
		t.Fatalf("fetch failure must keep the last set (applied=%q err=%v):\n%s", antiscanApplied, err, h.joined())
	}
}

func TestApplyAntiscanDisabledRemovesEverything(t *testing.T) {
	h := newAntiscanHarness(t, 443)
	if err := saveAntiscanState("old", []string{"5.0.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	sharedConfigCache.Set(SharedConfig{ProxyPort: 443, AntiscanRevision: antiscanDisabled})
	if err := applyAntiscan(h.cfg); err != nil {
		t.Fatal(err)
	}
	joined := h.joined()
	for _, want := range []string{"iptables -D INPUT", "iptables -X ANTISCAN_MTPROTO", "ipset destroy sharedd_scanners_net", "ipset destroy sharedd_scanners"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(antiscanStateFile); !os.IsNotExist(err) {
		t.Fatal("saved list must be removed when disabled")
	}
}

func TestApplyAntiscanDisablesHookOnPortMismatch(t *testing.T) {
	h := newAntiscanHarness(t, 443)
	sharedConfigCache.Set(SharedConfig{ProxyPort: 8443, AntiscanRevision: "rev1"})
	err := applyAntiscan(h.cfg)
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	if !strings.Contains(h.joined(), "iptables -D INPUT") || h.fetches != 0 {
		t.Fatalf("old hook must be removed and nothing fetched:\n%s", h.joined())
	}
}
