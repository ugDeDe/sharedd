package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sectionHeaderRe — заголовок TOML-таблицы. Толерантен к пробелам внутри скобок
// и хвостовым комментариям: "[ access.users ] # users".
var sectionHeaderRe = regexp.MustCompile(`^\[\s*([A-Za-z0-9_.\-]+)\s*\]\s*(#.*)?$`)

// arrayTableHeaderRe — начало array-table ("[[server.listeners]]"). НЕ цель поиска
// секций, но обязана ЗАВЕРШАТЬ текущую секцию при поиске границ: без этого
// findSection протягивал бы секцию сквозь array-table, и вставляемый ключ
// приземлялся бы внутрь чужого элемента массива (битый TOML/семантика).
var arrayTableHeaderRe = regexp.MustCompile(`^\[\[`)

// keyRe — ключ в начале строки ТОМЛа. Обязан включать `.` и `"`: quoted-ключи
// с точками — норма таблиц вида [censorship.exclusive_mask] ("m.beboo.ru" =...).
var keyRe = regexp.MustCompile(`^([A-Za-z0-9_.\-"]+)\s*=`)

func isAnyTableHeader(line string) bool {
	t := strings.TrimSpace(line)
	return sectionHeaderRe.MatchString(t) || arrayTableHeaderRe.MatchString(t)
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return strings.Split(string(data), "\n"), nil
}

// writeLines — атомарная запись через temp + rename, но с переносом
// владельца и прав исходного файла: rename подменяет inode целиком, поэтому
// без явного переноса telemt.toml получил бы владельца=агент (root) и 0600,
// и telemt под своим пользователем больше не смог бы прочитать конфиг.
func writeLines(path string, lines []string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return err
	}
	if err := preserveFileAttrs(tmp, info); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// preserveFileAttrs переносит uid/gid и permission mode исходного файла на
// новый. Порядок важен: сначала chown (он может сбросить биты режима), потом chmod.
func preserveFileAttrs(path string, orig os.FileInfo) error {
	if st, ok := orig.Sys().(*syscall.Stat_t); ok {
		if err := os.Chown(path, int(st.Uid), int(st.Gid)); err != nil {
			return fmt.Errorf("preserve owner of %s (uid=%d gid=%d): %w", path, st.Uid, st.Gid, err)
		}
	}
	if err := os.Chmod(path, orig.Mode().Perm()); err != nil {
		return fmt.Errorf("preserve mode of %s (%o): %w", path, orig.Mode().Perm(), err)
	}
	return nil
}

func findSection(lines []string, name string) (headerIdx, endIdx int, found bool) {
	for i, l := range lines {
		m := sectionHeaderRe.FindStringSubmatch(strings.TrimSpace(l))
		if m != nil && m[1] == name {
			headerIdx = i
			found = true
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if isAnyTableHeader(lines[j]) {
					end = j
					break
				}
			}
			endIdx = end
			return
		}
	}
	return 0, 0, false
}

func hasKey(lines []string, start, end int, key string) (int, bool) {
	for i := start; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if m := keyRe.FindStringSubmatch(trimmed); m != nil {
			if strings.Trim(m[1], `"`) == key {
				return i, true
			}
		}
	}
	return -1, false
}

func insertIntoSection(lines []string, endIdx int, newLine string) []string {
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:endIdx]...)
	out = append(out, newLine)
	out = append(out, lines[endIdx:]...)
	return out
}

func appendNewSection(lines []string, header string, body []string) []string {
	out := append([]string{}, lines...)
	if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
		out = append(out, "")
	}
	out = append(out, "["+header+"]")
	out = append(out, body...)
	return out
}

func ensureUser(lines []string, username, secret string) ([]string, bool) {
	headerIdx, endIdx, found := findSection(lines, "access.users")
	entry := fmt.Sprintf(`%s = "%s"`, username, secret)
	if !found {
		// Секции нет (например, конфиг без [access.users] вообще) — создаём её
		// СРАЗУ ПОСЛЕ [access], а не в хвосте файла: исторический баг — secret
		// уезжал в конец файла, где выглядел «брошенным» и путал оператора.
		if _, aEnd, aFound := findSection(lines, "access"); aFound {
			out := make([]string, 0, len(lines)+3)
			out = append(out, lines[:aEnd]...)
			if aEnd > 0 && strings.TrimSpace(lines[aEnd-1]) != "" {
				out = append(out, "")
			}
			out = append(out, "[access.users]", entry)
			out = append(out, lines[aEnd:]...)
			return out, true
		}
		return appendNewSection(lines, "access.users", []string{entry}), true
	}
	if idx, exists := hasKey(lines, headerIdx+1, endIdx, username); exists {
		_, current, ok := parseKeyValueLine(lines[idx])
		if ok && current == secret {
			return lines, false
		}
		out := append([]string{}, lines...)
		out[idx] = entry
		return out, true
	}
	return insertIntoSection(lines, endIdx, entry), true
}

// parseKeyValueLine — разбор строки `key = value` (учитывает quoted-ключи с
// точками и хвостовые комментарии). Value возвращается без кавычек/комментария.
func parseKeyValueLine(line string) (key, val string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	eq := strings.Index(t, "=")
	if eq < 0 {
		return "", "", false
	}
	key = strings.Trim(strings.TrimSpace(t[:eq]), `"`)
	val = strings.TrimSpace(t[eq+1:])
	if strings.HasPrefix(val, `"`) {
		end := strings.Index(val[1:], `"`)
		if end < 0 {
			return key, "", false
		}
		val = val[1 : 1+end]
	} else if i := strings.Index(val, "#"); i >= 0 {
		val = strings.TrimSpace(val[:i])
	}
	return key, val, true
}

// censorshipMaskEnabled — стоит ли в [censorship] явный `mask = true`.
// Ключа нет = false (хотя telemt по умолчанию маскирует): exclusive_mask мы
// ведём только по явному запросу оператора, лишних правок не делаем.
func censorshipMaskEnabled(lines []string) bool {
	h, end, found := findSection(lines, "censorship")
	if !found {
		return false
	}
	if idx, exists := hasKey(lines, h+1, end, "mask"); exists {
		_, v, ok := parseKeyValueLine(lines[idx])
		return ok && v == "true"
	}
	return false
}

// ensureExclusiveMask — при mask=true держит в [censorship.exclusive_mask]
// маппинг текущего SNI на самого себя: `"sni" = "sni:443"`. Тогда ответ на
// активное прощупывание этим SNI проксируется на НАСТОЯЩИЙ сайт (реальный
// контент и сертификат m.beboo.ru), а не на mask_host/дефолтный апстрим.
// Старые наши записи (подпись: значение == "<ключ>:443") под протухшими SNI
// вычищаем (SNI ротируется из панели); чужие записи не трогаем.
func ensureExclusiveMask(lines []string, sni string) ([]string, bool) {
	entry := fmt.Sprintf(`"%s" = "%s:443"`, sni, sni)
	h, end, found := findSection(lines, "censorship.exclusive_mask")
	if !found {
		// таблицы нет — вставляем СРАЗУ ПОСЛЕ блока [censorship], не в хвост файла
		if _, cEnd, cFound := findSection(lines, "censorship"); cFound {
			out := make([]string, 0, len(lines)+3)
			out = append(out, lines[:cEnd]...)
			if cEnd > 0 && strings.TrimSpace(lines[cEnd-1]) != "" {
				out = append(out, "")
			}
			out = append(out, "[censorship.exclusive_mask]", entry)
			out = append(out, lines[cEnd:]...)
			return out, true
		}
		return appendNewSection(lines, "censorship.exclusive_mask", []string{entry}), true
	}

	out := append([]string{}, lines...)
	changed := false
	// чистим наши старые записи (SNI мог смениться из панели)
	for i := h + 1; i < end; i++ {
		k, v, ok := parseKeyValueLine(out[i])
		if !ok || k == sni {
			continue
		}
		if v == k+":443" {
			out = append(out[:i], out[i+1:]...)
			end--
			i--
			changed = true
		}
	}
	// текущий SNI: добавить или выровнять значение
	if idx, exists := hasKey(out, h+1, end, sni); exists {
		if _, v, ok := parseKeyValueLine(out[idx]); !ok || v != sni+":443" {
			out[idx] = entry
			changed = true
		}
	} else {
		out = insertIntoSection(out, end, entry)
		changed = true
	}
	return out, changed
}

// ensurePrimaryTLSDomain — только bootstrap: выставляется при полном отсутствии
// tls_domain и дальше НИКОГДА не перезаписывается агентом.
func ensurePrimaryTLSDomain(lines []string, domain string) ([]string, bool) {
	headerIdx, endIdx, found := findSection(lines, "censorship")
	entry := fmt.Sprintf(`tls_domain = "%s"`, domain)
	if !found {
		return appendNewSection(lines, "censorship", []string{entry}), true
	}
	if _, exists := hasKey(lines, headerIdx+1, endIdx, "tls_domain"); exists {
		return lines, false
	}
	return insertIntoSection(lines, endIdx, entry), true
}

var tlsDomainsLineRe = regexp.MustCompile(`^tls_domains\s*=\s*\[(.*)\]\s*$`)

// setExtraTLSDomains — в tls_domains должен быть РОВНО актуальный SNI
// регистратора: при смене SNI старое значение заменяется, а не дописывается.
func setExtraTLSDomains(lines []string, domain string) ([]string, bool) {
	entry := fmt.Sprintf(`tls_domains = ["%s"]`, domain)
	headerIdx, endIdx, found := findSection(lines, "censorship")
	if !found {
		return appendNewSection(lines, "censorship", []string{entry}), true
	}
	idx, exists := hasKey(lines, headerIdx+1, endIdx, "tls_domains")
	if !exists {
		return insertIntoSection(lines, endIdx, entry), true
	}
	m := tlsDomainsLineRe.FindStringSubmatch(strings.TrimSpace(lines[idx]))
	if m == nil {
		// нестандартная запись (мультистрока и т.п.) — не трогаем, лог снаружи
		return lines, false
	}
	items := splitQuotedList(m[1])
	if len(items) == 1 && items[0] == domain {
		return lines, false
	}
	newLines := append([]string{}, lines...)
	newLines[idx] = entry
	return newLines, true
}

// ensureMetricsListen — включает метрики телемт только если их нет совсем.
// Пишем ТОЛЬКО metrics_listen (полный listen-адрес): telemt считает его
// приоритетным, писать metrics_port рядом бессмысленно.
// Если уже есть metrics_port ИЛИ metrics_listen — ничего не добавляем.
func ensureMetricsListen(lines []string) ([]string, bool) {
	headerIdx, endIdx, found := findSection(lines, "server")
	entry := fmt.Sprintf(`%s = "%s"`, metricsListenKey, metricsListenValue)
	if !found {
		return appendNewSection(lines, "server", []string{entry}), true
	}
	if _, exists := hasKey(lines, headerIdx+1, endIdx, metricsListenKey); exists {
		return lines, false
	}
	if _, exists := hasKey(lines, headerIdx+1, endIdx, metricsPortKey); exists {
		return lines, false
	}
	return insertIntoSection(lines, endIdx, entry), true
}

func splitQuotedList(inner string) []string {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil
	}
	parts := strings.Split(inner, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.Trim(strings.TrimSpace(p), `"`)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// ---- shared config + intervals из регистратора ----

type NodeIntervals struct {
	HeartbeatMs  int `json:"heartbeat_ms"`
	GlobalpingMs int `json:"globalping_ms"`
	MetricsMs    int `json:"metrics_ms"`
	SyncMs       int `json:"sync_ms"`
}

type SharedConfig struct {
	TLSDomain       string            `json:"tls_domain"`
	ProxyPort       int               `json:"proxy_port"`
	Users           map[string]string `json:"users"`
	Intervals       NodeIntervals     `json:"intervals"`
	ForceGlobalping bool              `json:"force_globalping"`
}

type SharedConfigCache struct {
	mu   sync.RWMutex
	data SharedConfig
}

var sharedConfigCache = &SharedConfigCache{}

func (c *SharedConfigCache) Get() SharedConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.data
}

func (c *SharedConfigCache) Set(v SharedConfig) {
	c.mu.Lock()
	c.data = v
	c.mu.Unlock()
}

// agentIntervals — текущие интервалы циклов. Дефолты действуют до первого /config,
// дальше значения приезжают с регистратора и применяются на лету.
type agentIntervals struct {
	mu         sync.RWMutex
	heartbeat  time.Duration
	globalping time.Duration
	metrics    time.Duration
	syncI      time.Duration
}

var intervals = &agentIntervals{
	heartbeat:  defaultIntervalsHeartbeat * time.Millisecond,
	globalping: defaultIntervalsGlobal * time.Millisecond,
	metrics:    defaultIntervalsMetrics * time.Millisecond,
	syncI:      defaultIntervalsSync * time.Millisecond,
}

func (a *agentIntervals) apply(iv NodeIntervals) {
	if iv.HeartbeatMs <= 0 && iv.GlobalpingMs <= 0 && iv.MetricsMs <= 0 && iv.SyncMs <= 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	set := func(dst *time.Duration, ms int, name string) {
		if ms <= 0 {
			return
		}
		if d := time.Duration(ms) * time.Millisecond; *dst != d {
			log.Printf("interval %s updated by registry: %v -> %v", name, *dst, d)
			*dst = d
		}
	}
	set(&a.heartbeat, iv.HeartbeatMs, "heartbeat")
	set(&a.globalping, iv.GlobalpingMs, "globalping")
	set(&a.metrics, iv.MetricsMs, "metrics")
	set(&a.syncI, iv.SyncMs, "sync")
}

func (a *agentIntervals) Heartbeat() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.heartbeat
}
func (a *agentIntervals) Globalping() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.globalping
}
func (a *agentIntervals) Metrics() time.Duration {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.metrics
}
func (a *agentIntervals) Sync() time.Duration { a.mu.RLock(); defer a.mu.RUnlock(); return a.syncI }

func fetchSharedConfig(cfg *NodeConfig) (SharedConfig, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := registryRequest(client, cfg, http.MethodGet, "/config", nil)
	if err != nil {
		netw.noteFail() // сетевой вотчдог: сброс keep-alive / детект смены IP
		return SharedConfig{}, fmt.Errorf("fetch /config error: %w", err)
	}
	netw.noteOK()
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return SharedConfig{}, fmt.Errorf("fetch /config failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var shared SharedConfig
	if err := json.Unmarshal(body, &shared); err != nil {
		return SharedConfig{}, fmt.Errorf("parse /config error: %w", err)
	}
	return shared, nil
}

// applySharedConfig — единая точка применения /config: кэш + интервалы + telemt.toml.
// Интеграция ТОЛЬКО через файл (построчный патч telemt.toml) — telemt REST API
// выпилен (не работал в реальных раскладках). Работает одинаково и с ванильным
// telemt, и с MTProxyL (она владеет тем же файлом).
// Запись файла идёт через applySharedConfigManaged — с остановкой и
// рестартом прокси (конфиг на работающем прокси не применяется сам).
func applySharedConfig(cfg *NodeConfig, shared SharedConfig) {
	previousPort := sharedConfigCache.Get().ProxyPort
	sharedConfigCache.Set(shared)
	if previousPort != shared.ProxyPort {
		kickAntiscan()
	}
	intervals.apply(shared.Intervals)
	if shared.ForceGlobalping {
		log.Printf("globalping check requested by registry (stale or missing result)")
		kickGlobalping()
	}

	if !cfg.Sync.ApplyToTelemt {
		return
	}
	if err := applySharedConfigManaged(cfg, shared); err != nil {
		log.Printf("failed to apply shared config to telemt.toml: %v", err)
	}
}

func syncLoop(cfg *NodeConfig) {
	for {
		shared, err := fetchSharedConfig(cfg)
		if err != nil {
			log.Printf("failed to fetch shared config: %v", err)
		} else {
			applySharedConfig(cfg, shared)
		}
		time.Sleep(intervals.Sync())
	}
}

// computeSharedConfigPatch — чистый расчёт патча telemt.toml: читает текущий
// файл и возвращает НОВЫЕ строки, ничего не записывая. changed=false →
// файл уже соответствует, трогать (и рестартовать прокси) нечего.
func computeSharedConfigPatch(cfg *NodeConfig, shared SharedConfig) (newLines []string, changed bool, err error) {
	path := cfg.Telemt.ConfigPath
	lines, err := readLines(path)
	if err != nil {
		return nil, false, fmt.Errorf("read telemt config: %w", err)
	}

	anyChange := false

	// mask_host НЕ вычищаем (, смена поведения): раз конфиг его содержит —
	// это выбор оператора (например, MTProxyL QuickSettings). Активное
	// прощупывание нашего SNI вместо этого накрываем exclusive_mask на
	// настоящий сайт — см. ниже.
	maskOn := censorshipMaskEnabled(lines)

	for username, secret := range shared.Users {
		var ch bool
		lines, ch = ensureUser(lines, username, secret)
		if ch {
			log.Printf("synchronized secret for user %q in %s", username, path)
			anyChange = true
		}
	}

	if shared.TLSDomain != "" {
		var ch bool
		lines, ch = ensurePrimaryTLSDomain(lines, shared.TLSDomain)
		if ch {
			log.Printf("bootstrap: set primary tls_domain=%s in %s (was empty)", shared.TLSDomain, path)
			anyChange = true
		}

		var ch2 bool
		lines, ch2 = setExtraTLSDomains(lines, shared.TLSDomain)
		if ch2 {
			log.Printf("tls_domains in %s set to current SNI: %s (old extra domains replaced)", path, shared.TLSDomain)
			anyChange = true
		}

		// mask=true → exclusive_mask: наш SNI маскируется под настоящий сайт
		// (telemt отдаёт реальный контент/сертификат домена при пробах)
		if maskOn {
			var ch4 bool
			lines, ch4 = ensureExclusiveMask(lines, shared.TLSDomain)
			if ch4 {
				log.Printf(`exclusive_mask in %s: "%s" -> "%s:443" (real-site masking on probe)`, path, shared.TLSDomain, shared.TLSDomain)
				anyChange = true
			}
		}
	}

	var ch3 bool
	lines, ch3 = ensureMetricsListen(lines)
	if ch3 {
		log.Printf("enabled metrics in %s via %s (применится с рестартом прокси)", path, metricsListenValue)
		anyChange = true
	}

	if !anyChange {
		return lines, false, nil
	}
	return lines, true, nil
}

// applySvcMu — сериализация применения конфига: два подряд «стоп→патч→старт»
// не нужны никому.
var applySvcMu sync.Mutex

// Indirections keep service failure paths deterministic in unit tests.
var (
	applySystemdAvailable = systemdAvailable
	applyDetectProxyUnit  = detectProxyUnit
	applyPreferMtproxyl   = preferMtproxylCLI
	applyDetectNodeType   = detectNodeType
	applyMtproxylRestart  = tryMtproxylRestart
	applyBlindRestart     = blindRestartTelemt
	applyProxyCtl         = proxyCtl
	applyWaitMetrics      = waitMetricsReady
)

func rollbackConfig(path string, orig []string, restart func() error, cause error) error {
	if orig == nil {
		return cause
	}
	if err := writeLines(path, orig); err != nil {
		return fmt.Errorf("%w; rollback write failed: %v", cause, err)
	}
	if err := restart(); err != nil {
		return fmt.Errorf("%w; rollback restart failed: %v", cause, err)
	}
	return fmt.Errorf("%w; config rolled back", cause)
}

// applySharedConfigManaged — боевой конвейер применения /config к telemt.toml:
//
// 1. стоп прокси (systemd-юнит telemt.service и т.п.);
// 2. патч и сохранение конфига (атомарно, с сохранением владельца/прав);
// 3. старт прокси;
// 4. ожидание подъёма — поллим /metrics до HTTP 200 (это же и проверка,
// что конфиг принят: с кривым конфигом прокси не поднимется);
// 5. прокси не встал за таймаут → откат файла на исходную версию + рестарт.
//
// Без изменений (changed=false) прокси вообще не трогаем — никаких лишних
// рестартов на каждом sync-тике.
func applySharedConfigManaged(cfg *NodeConfig, shared SharedConfig) error {
	applySvcMu.Lock()
	defer applySvcMu.Unlock()

	path := cfg.Telemt.ConfigPath
	newLines, changed, err := computeSharedConfigPatch(cfg, shared)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}

	// исходный файл — для отката, если прокси с новым конфигом не встанет
	orig, oerr := readLines(path)
	if oerr != nil {
		orig = nil
	}

	unit := ""
	if applySystemdAvailable() {
		unit = applyDetectProxyUnit()
	}
	if unit != "" && applyPreferMtproxyl() {
		// На MTProxyL systemctl-рестарт поднял бы прокси со СТАРЫМ
		// сгенерированным config.toml — наш патч лежит в superexpert.toml и до
		// рабочего конфига его доносит только `mtproxyl restart`.
		log.Printf("node type MTProxyL: managing proxy via mtproxyl CLI (unit %s found but CLI rebuilds config)", unit)
		unit = ""
	}

	if unit == "" {
		// systemd-юнита нет (или он вторичен, см. выше). MTProxyL: CLI сам
		// пересобирает config.toml из superexpert.toml и поднимает прокси.
		// Classic/MEKO без найденного юнита: пишем файл и ВСЛЕПУЮ пробуем
		// `systemctl restart telemt.service` — на этих установках юнит зовётся
		// именно так, и рестарт по имени срабатывает, даже если детект (show/
		// cat/list-unit-files/диск) вслепую его не увидел (битый dbus и т.п.).
		if err := writeLines(path, newLines); err != nil {
			return err
		}
		if applyPreferMtproxyl() || applyDetectNodeType() == NodeTypeMTProxyL {
			log.Printf("%s updated; restarting proxy via mtproxyl CLI (no systemd unit found)...", path)
			if rerr := applyMtproxylRestart(); rerr != nil {
				return rollbackConfig(path, orig, applyMtproxylRestart, fmt.Errorf("mtproxyl restart after config write: %w", rerr))
			}
			if werr := applyWaitMetrics(cfg, proxyUpTimeout); werr != nil {
				return rollbackConfig(path, orig, applyMtproxylRestart, fmt.Errorf("proxy not ready after mtproxyl restart: %w", werr))
			}
			return nil
		}
		log.Printf("%s updated; systemd unit not detected — last resort: blind `systemctl restart telemt.service` ...", path)
		if rerr := applyBlindRestart(cfg); rerr != nil {
			return rollbackConfig(path, orig, func() error { return applyBlindRestart(cfg) }, fmt.Errorf("blind restart telemt.service after config write: %w", rerr))
		}
		return nil
	}

	log.Printf("applying %s changes with proxy %s: stop -> patch -> start -> wait-ready", path, unit)

	// 1. СТОП прокси (если стоп падает — файл не трогаем: писать конфиг в
	// работающий прокси бессмысленно, он его не перечитает).
	if err := applyProxyCtl("stop", unit); err != nil {
		return fmt.Errorf("stop %s before config write: %w", unit, err)
	}
	// 2. патч и сохранение конфига
	if err := writeLines(path, newLines); err != nil {
		_ = applyProxyCtl("start", unit)
		return fmt.Errorf("write %s: %w (proxy started back with old config)", path, err)
	}
	// 3. СТАРТ прокси
	if err := applyProxyCtl("start", unit); err != nil {
		return rollbackConfig(path, orig, func() error { return applyProxyCtl("start", unit) }, fmt.Errorf("start %s after config write: %w", unit, err))
	}
	// 4. ждём подъёма по метрикам
	if err := applyWaitMetrics(cfg, proxyUpTimeout); err != nil {
		log.Printf("proxy %s did not come up after config apply: %v — rolling back %s", unit, err, path)
		if orig != nil {
			_ = applyProxyCtl("stop", unit)
			if werr := writeLines(path, orig); werr != nil {
				log.Printf("rollback: write original %s failed: %v", path, werr)
			}
		}
		rerr := applyProxyCtl("restart", unit)
		if rerr != nil {
			return fmt.Errorf("proxy not ready: %v; rollback restart failed: %v", err, rerr)
		}
		if werr := applyWaitMetrics(cfg, proxyRollbackTimeout); werr != nil {
			return fmt.Errorf("proxy not ready: %v; rolled back but proxy STILL not ready: %v", err, werr)
		}
		return fmt.Errorf("config apply rolled back (proxy runs previous config): %w", err)
	}
	log.Printf("proxy %s is up with new config (%s metrics answering)", unit, path)
	return nil
}
