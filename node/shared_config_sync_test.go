package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

func splitLines(s string) []string { return strings.Split(s, "\n") }

// Конфиг, где [access.users] нет вообще (типичный случай из репорта: секрет
// уезжал в конец файла). Теперь секция создаётся сразу после [access].
func TestEnsureUserPlacesSectionAfterAccess(t *testing.T) {
	lines := []string{
		"[general]",
		"use_middle_proxy = true",
		"",
		"[access]",
		"replay_check_len = 65536",
		"",
		"[censorship]",
		"tls_domain = \"a.com\"",
		"",
		"[server]",
		"port = 443",
	}
	out, changed := ensureUser(lines, "ivan", "11111111111111111111111111111111")
	if !changed {
		t.Fatal("expected change")
	}
	// индекс нового [access.users] — сразу за блоком [access], до [censorship]
	idxUsers, idxCensorship := -1, -1
	for i, l := range out {
		switch strings.TrimSpace(l) {
		case "[access.users]":
			idxUsers = i
		case "[censorship]":
			idxCensorship = i
		}
	}
	if idxUsers < 0 || idxCensorship < 0 || idxUsers > idxCensorship {
		t.Fatalf("[access.users] must be created right after [access], got:\n%s", strings.Join(out, "\n"))
	}
	// следующая строка за заголовком — сам секрет
	if !strings.Contains(out[idxUsers+1], `ivan = "11111111111111111111111111111111"`) {
		t.Fatalf("user must sit inside new [access.users]:\n%s", strings.Join(out, "\n"))
	}
}

// Граница секции ОБЯЗАНА резаться и на array-table [[...]] — иначе ключи
// вставляются внутрь чужого элемента массива (битая семантика TOML).
func TestArrayTableTerminatesSection(t *testing.T) {
	lines := []string{
		"[access.users]",
		"hello = \"00000000000000000000000000000000\"",
		"",
		"[[server.listeners]]",
		"ip = \"0.0.0.0\"",
		"",
		"[censorship]",
		"tls_domain = \"a.com\"",
	}
	out, changed := ensureUser(lines, "ivan", "11111111111111111111111111111111")
	if !changed {
		t.Fatal("expected change")
	}
	// ivan должен стоять ДО [[server.listeners]], внутри [access.users]
	ivanIdx, listenersIdx := -1, -1
	for i, l := range out {
		if strings.HasPrefix(l, `ivan =`) {
			ivanIdx = i
		}
		if strings.HasPrefix(l, "[[server.listeners]]") {
			listenersIdx = i
		}
	}
	if ivanIdx < 0 || listenersIdx < 0 || ivanIdx > listenersIdx {
		t.Fatalf("user must land inside [access.users], before [[server.listeners]]:\n%s", strings.Join(out, "\n"))
	}
}

// Заголовки секций с хвостовым комментарием/пробелами тоже должны находиться —
// иначе ensureUser создавал ДУБЛЬ [access.users] в конце файла.
func TestSectionHeaderWithTrailingComment(t *testing.T) {
	lines := []string{
		"[access]",
		"replay_check_len = 65536",
		"[ access.users ]  # пользователи прокси",
		"hello = \"00000000000000000000000000000000\"",
		"",
		"[censorship]",
		"tls_domain = \"a.com\"",
	}
	out, changed := ensureUser(lines, "ivan", "11111111111111111111111111111111")
	if !changed {
		t.Fatal("expected change")
	}
	count := 0
	ivanIdx, lastUsersHeader := -1, -1
	for i, l := range out {
		if sectionHeaderRe.MatchString(strings.TrimSpace(l)) {
			if m := sectionHeaderRe.FindStringSubmatch(strings.TrimSpace(l)); m[1] == "access.users" {
				count++
				lastUsersHeader = i
			}
		}
		if strings.HasPrefix(l, `ivan =`) {
			ivanIdx = i
		}
	}
	if count != 1 {
		t.Fatalf("must NOT duplicate [access.users] header (count=%d):\n%s", count, strings.Join(out, "\n"))
	}
	if ivanIdx < lastUsersHeader+1 {
		t.Fatalf("ivan must follow the [access.users] header:\n%s", strings.Join(out, "\n"))
	}
}

// parseKeyValueLine: quoted-ключи с точками, хвостовые комментарии, quoted/unquoted значения.
func TestParseKeyValueLine(t *testing.T) {
	cases := []struct {
		line string
		key  string
		val  string
		ok   bool
	}{
		{`"m.beboo.ru" = "m.beboo.ru:443"`, "m.beboo.ru", "m.beboo.ru:443", true},
		{`mask = true   # comment`, "mask", "true", true},
		{`mask = false`, "mask", "false", true},
		{`tls_domain = "a.com" # tail`, "tls_domain", "a.com", true},
		{`# mask = true`, "", "", false},
		{``, "", "", false},
		{`[censorship]`, "", "", false},
		{`bad = "unclosed`, "bad", "", false},
	}
	for _, c := range cases {
		k, v, ok := parseKeyValueLine(c.line)
		if k != c.key || v != c.val || ok != c.ok {
			t.Fatalf("parseKeyValueLine(%q) = %q,%q,%v; want %q,%q,%v", c.line, k, v, ok, c.key, c.val, c.ok)
		}
	}
}

// mask=true детектируется только в [censorship] и только явный true.
func TestCensorshipMaskEnabled(t *testing.T) {
	if !censorshipMaskEnabled(splitLines("[censorship]\ntls_domain = \"a.com\"\nmask = true")) {
		t.Fatal("mask = true must be detected")
	}
	if !censorshipMaskEnabled(splitLines("[censorship]\nmask = true  # включено")) {
		t.Fatal("trailing comment must not break detection")
	}
	if censorshipMaskEnabled(splitLines("[censorship]\ntls_domain = \"a.com\"")) {
		t.Fatal("absent mask key must be false")
	}
	if censorshipMaskEnabled(splitLines("[censorship]\nmask = false")) {
		t.Fatal("mask = false must be false")
	}
	if censorshipMaskEnabled(splitLines("[censorship]\n# mask = true")) {
		t.Fatal("commented mask must be false")
	}
	// mask в другой секции не считается
	if censorshipMaskEnabled(splitLines("[censorship]\ntls_domain = \"a.com\"\n\n[access]\nmask = true")) {
		t.Fatal("mask outside [censorship] must not count")
	}
}

// mask_host БОЛЬШЕ не вычищается (V7.3); без mask=true exclusive_mask не создаётся.
func TestApplySharedConfigPreservesMaskHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	original := `[server]
port = 443

[censorship]
tls_domain = "original.example"
mask_host = "some-upstream.example"

[access.users]
hello = "00000000000000000000000000000000"
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	cfg.Sync.ApplyToTelemt = true
	shared := SharedConfig{
		TLSDomain: "mask.example",
		Users:     map[string]string{"ivan": "11111111111111111111111111111111"},
	}
	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), `mask_host = "some-upstream.example"`) {
		t.Fatalf("mask_host must be preserved (V7.3):\n%s", content)
	}
	if strings.Contains(string(content), "exclusive_mask") {
		t.Fatalf("mask key absent -> exclusive_mask must NOT be created:\n%s", content)
	}
	// идемпотентность
	before := append([]byte(nil), content...)
	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("must be idempotent")
	}
}

// ensureExclusiveMask: создание таблицы сразу после [censorship], идемпотентность,
// замена кривого значения, вычитка протухших SNI, чужие записи целы.
func TestEnsureExclusiveMask(t *testing.T) {
	lines := splitLines(`[censorship]
tls_domain = "a.com"
mask = true

[access.users]
hello = "00000000000000000000000000000000"`)

	out, changed := ensureExclusiveMask(lines, "m.beboo.ru")
	if !changed {
		t.Fatal("table must be created")
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "[censorship.exclusive_mask]") ||
		!strings.Contains(joined, `"m.beboo.ru" = "m.beboo.ru:443"`) {
		t.Fatalf("exclusive_mask entry expected:\n%s", joined)
	}
	// таблица ДО [access.users]
	if strings.Index(joined, "[censorship.exclusive_mask]") > strings.Index(joined, "[access.users]") {
		t.Fatalf("exclusive_mask table must sit right after [censorship]:\n%s", joined)
	}

	// идемпотентно
	if _, ch := ensureExclusiveMask(out, "m.beboo.ru"); ch {
		t.Fatalf("second run must be no-op, got:\n%s", joined)
	}

	// ротация SNI: старый маппинг с нашей подписью вычищается
	out2, ch2 := ensureExclusiveMask(out, "m.new.ru")
	joined2 := strings.Join(out2, "\n")
	if !ch2 || !strings.Contains(joined2, `"m.new.ru" = "m.new.ru:443"`) {
		t.Fatalf("new SNI mapping expected:\n%s", joined2)
	}
	if strings.Contains(joined2, "m.beboo.ru") {
		t.Fatalf("stale SNI mapping must be dropped:\n%s", joined2)
	}

	// чужая запись (значение НЕ "<ключ>:443") не трогается
	foreign := splitLines(`[censorship]
tls_domain = "a.com"

[censorship.exclusive_mask]
"old-sni.example" = "old-sni.example:443"
"special.example" = "10.0.0.9:8443"`)
	out3, _ := ensureExclusiveMask(foreign, "cur.example")
	joined3 := strings.Join(out3, "\n")
	if !strings.Contains(joined3, `"special.example" = "10.0.0.9:8443"`) {
		t.Fatalf("foreign entry must be preserved:\n%s", joined3)
	}
	if strings.Contains(joined3, "old-sni.example") {
		t.Fatalf("stale self-mapping must be dropped:\n%s", joined3)
	}

	// наш ключ с чужим значением → выравниваем на "<sni>:443"
	wrong := splitLines(`[censorship]
tls_domain = "a.com"

[censorship.exclusive_mask]
"cur.example" = "0.0.0.0:1"`)
	out4, ch4 := ensureExclusiveMask(wrong, "cur.example")
	if !ch4 || !strings.Contains(strings.Join(out4, "\n"), `"cur.example" = "cur.example:443"`) {
		t.Fatalf("wrong value for current SNI must be fixed:\n%s", strings.Join(out4, "\n"))
	}
}

// E2E: mask=true + mask_host → exclusive_mask на текущий SNI, mask_host цел,
// ротация SNI переносит маппинг.
func TestApplySharedConfigExclusiveMask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	original := `[server]
port = 443

[censorship]
tls_domain = "original.example"
mask = true
mask_host = "some-upstream.example"

[access.users]
hello = "00000000000000000000000000000000"
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	cfg.Sync.ApplyToTelemt = true
	shared := SharedConfig{TLSDomain: "m.beboo.ru"}

	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	s := string(content)
	for _, want := range []string{
		`mask = true`,
		`mask_host = "some-upstream.example"`,
		`tls_domains = ["m.beboo.ru"]`,
		"[censorship.exclusive_mask]",
		`"m.beboo.ru" = "m.beboo.ru:443"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("expected %q in:\n%s", want, s)
		}
	}

	// идемпотентность
	before := append([]byte(nil), content...)
	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("must be idempotent:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// ротация SNI из панели → маппинг переезжает на новый домен
	if _, err := applySharedConfigAdditively(cfg, SharedConfig{TLSDomain: "m.other.ru"}); err != nil {
		t.Fatal(err)
	}
	s = func() string { b, _ := os.ReadFile(path); return string(b) }()
	if !strings.Contains(s, `"m.other.ru" = "m.other.ru:443"`) || strings.Contains(s, `"m.beboo.ru"`) {
		t.Fatalf("exclusive_mask must follow the current SNI:\n%s", s)
	}

	// результат обязан оставаться ВАЛИДНЫМ TOML с правильной вложенностью таблицы
	data, _ := os.ReadFile(path)
	var parsed struct {
		Censorship struct {
			Mask          bool              `toml:"mask"`
			MaskHost      string            `toml:"mask_host"`
			TLSDomains    []string          `toml:"tls_domains"`
			ExclusiveMask map[string]string `toml:"exclusive_mask"`
		} `toml:"censorship"`
	}
	if err := toml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("patched config must stay valid TOML: %v\n%s", err, data)
	}
	if !parsed.Censorship.Mask || parsed.Censorship.ExclusiveMask["m.other.ru"] != "m.other.ru:443" {
		t.Fatalf("parsed exclusive_mask wrong: %+v", parsed.Censorship)
	}
	if parsed.Censorship.MaskHost != "some-upstream.example" {
		t.Fatalf("mask_host must survive in parsed config: %+v", parsed.Censorship)
	}
}

// E2E: mask=false → exclusive_mask не появляется вовсе.
func TestApplySharedConfigMaskFalse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	original := "[server]\nport = 443\n\n[censorship]\ntls_domain = \"a.com\"\nmask = false\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	cfg.Sync.ApplyToTelemt = true
	if _, err := applySharedConfigAdditively(cfg, SharedConfig{TLSDomain: "m.beboo.ru"}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if strings.Contains(string(content), "exclusive_mask") {
		t.Fatalf("mask=false -> no exclusive_mask expected:\n%s", content)
	}
}

func TestEnsureUserAddsOnlyWhenMissing(t *testing.T) {
	lines := splitLines("[access.users]\nhello = \"aaaa\"\n\n[server]\nport = 443")
	out, changed := ensureUser(lines, "bob", "bbbb")
	if !changed {
		t.Fatal("expected change for new user")
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, `bob = "bbbb"`) || !strings.Contains(joined, `hello = "aaaa"`) {
		t.Fatalf("both users must be present, got:\n%s", joined)
	}
	if strings.Index(joined, "bob") > strings.Index(joined, "[server]") {
		t.Fatalf("bob must be inserted inside [access.users], got:\n%s", joined)
	}

	out2, changed2 := ensureUser(out, "bob", "cccc")
	if changed2 {
		t.Fatalf("existing username must never be replaced, got changed output:\n%s", strings.Join(out2, "\n"))
	}
	if !strings.Contains(strings.Join(out2, "\n"), `bob = "bbbb"`) {
		t.Fatal("existing secret must be preserved")
	}
}

func TestEnsureUserCreatesSection(t *testing.T) {
	lines := splitLines("[server]\nport = 443")
	out, changed := ensureUser(lines, "x", "y")
	if !changed {
		t.Fatal("expected change")
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, "[access.users]") || !strings.Contains(joined, `x = "y"`) {
		t.Fatalf("section must be created, got:\n%s", joined)
	}
}

func TestEnsurePrimaryTLSDomainBootstrapOnly(t *testing.T) {
	lines := splitLines("[censorship]\nmask = true")
	out, changed := ensurePrimaryTLSDomain(lines, "a.com")
	if !changed || !strings.Contains(strings.Join(out, "\n"), `tls_domain = "a.com"`) {
		t.Fatalf("bootstrap must set tls_domain, got changed=%v:\n%s", changed, strings.Join(out, "\n"))
	}
	lines2 := splitLines("[censorship]\ntls_domain = \"keep.me\"")
	out2, changed2 := ensurePrimaryTLSDomain(lines2, "a.com")
	if changed2 || !strings.Contains(strings.Join(out2, "\n"), `tls_domain = "keep.me"`) {
		t.Fatalf("existing tls_domain must not be overwritten, got changed=%v:\n%s", changed2, strings.Join(out2, "\n"))
	}
}

// Ключевое изменение V2: tls_domains ЗАМЕНЯЕТСЯ актуальным SNI, не дописывается.
func TestSetExtraTLSDomainsReplacesStale(t *testing.T) {
	lines := splitLines(`[censorship]
tls_domain = "primary.com"
tls_domains = ["old1.com", "old2.com"]`)
	out, changed := setExtraTLSDomains(lines, "new.com")
	if !changed {
		t.Fatal("expected change when SNI rotated")
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, `tls_domains = ["new.com"]`) {
		t.Fatalf("tls_domains must be replaced by current SNI only, got:\n%s", joined)
	}
	if strings.Contains(joined, "old1.com") || strings.Contains(joined, "old2.com") {
		t.Fatalf("stale domains must be removed, got:\n%s", joined)
	}
	// primary не тронут
	if !strings.Contains(joined, `tls_domain = "primary.com"`) {
		t.Fatalf("primary tls_domain must be untouched, got:\n%s", joined)
	}

	// тот же SNI — ничего не делаем
	if _, changed2 := setExtraTLSDomains(out, "new.com"); changed2 {
		t.Fatal("idempotent: same SNI must not rewrite the file")
	}

	// нет ключа — добавляем
	lines3 := splitLines(`[censorship]
tls_domain = "primary.com"`)
	out3, changed3 := setExtraTLSDomains(lines3, "d.com")
	if !changed3 || !strings.Contains(strings.Join(out3, "\n"), `tls_domains = ["d.com"]`) {
		t.Fatalf("expected tls_domains created, got changed=%v:\n%s", changed3, strings.Join(out3, "\n"))
	}
}

// V2: пишем ТОЛЬКО metrics_listen, и только если нет ни listen, ни port.
func TestEnsureMetricsListenOnly(t *testing.T) {
	lines := splitLines("[server]\nport = 443")
	out, changed := ensureMetricsListen(lines)
	if !changed {
		t.Fatal("expected change when metrics absent")
	}
	joined := strings.Join(out, "\n")
	if !strings.Contains(joined, `metrics_listen = "127.0.0.1:9090"`) {
		t.Fatalf("metrics_listen must be added, got:\n%s", joined)
	}
	if strings.Contains(joined, "metrics_port") {
		t.Fatalf("metrics_port must NOT be written alongside metrics_listen, got:\n%s", joined)
	}

	if _, c2 := ensureMetricsListen(out); c2 {
		t.Fatal("idempotent: second run must not change anything")
	}

	withPort := splitLines("[server]\nport = 443\nmetrics_port = 8888")
	if _, c3 := ensureMetricsListen(withPort); c3 {
		t.Fatal("must not add metrics_listen when metrics_port already exists")
	}

	withListen := splitLines("[server]\nport = 443\nmetrics_listen = \"0.0.0.0:7777\"")
	if _, c4 := ensureMetricsListen(withListen); c4 {
		t.Fatal("must not touch existing metrics_listen")
	}
}

func TestApplySharedConfigPreservesCommentsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	original := `# my telemt config

[general]
# enable middle proxy
use_middle_proxy = true

[server]
port = 443

[censorship]
# never change this by hand
tls_domain = "original.example"
tls_domains = ["stale-mask.example"]

[access.users]
# existing user
hello = "00000000000000000000000000000000"
`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	cfg.Sync.ApplyToTelemt = true

	shared := SharedConfig{
		TLSDomain: "new-mask.example",
		Users:     map[string]string{"ivan": "11111111111111111111111111111111"},
	}

	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	data, _ := os.ReadFile(path)
	content := string(data)

	for _, want := range []string{
		"# never change this by hand",
		"# existing user",
		`tls_domain = "original.example"`,    // primary untouched
		`tls_domains = ["new-mask.example"]`, // stale domain REPLACED
		`ivan = "11111111111111111111111111111111"`,
		`metrics_listen = "127.0.0.1:9090"`, // metrics enabled via listen only
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("expected %q in output:\n%s", want, content)
		}
	}
	if strings.Contains(content, "stale-mask.example") {
		t.Fatalf("stale tls_domains entry must be gone:\n%s", content)
	}
	if strings.Contains(content, "metrics_port") {
		t.Fatalf("metrics_port must not be written:\n%s", content)
	}

	before, _ := os.ReadFile(path)
	if _, err := applySharedConfigAdditively(cfg, shared); err != nil {
		t.Fatalf("second apply failed: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("apply must be idempotent, diff:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}
}

func TestIntervalsApplyFromRegistry(t *testing.T) {
	a := &agentIntervals{
		heartbeat:  15 * time.Second,
		globalping: 5 * time.Minute,
		metrics:    time.Minute,
		syncI:      time.Minute,
	}
	a.apply(NodeIntervals{HeartbeatMs: 30000, GlobalpingMs: 600000, MetricsMs: 45000, SyncMs: 120000})
	if a.Heartbeat() != 30*time.Second || a.Globalping() != 10*time.Minute ||
		a.Metrics() != 45*time.Second || a.Sync() != 2*time.Minute {
		t.Fatalf("intervals not applied: %+v", a)
	}
	// нули/отрицательные игнорируются
	a.apply(NodeIntervals{HeartbeatMs: 0, MetricsMs: -5})
	if a.Heartbeat() != 30*time.Second || a.Metrics() != 45*time.Second {
		t.Fatal("zero/negative values must be ignored")
	}
}

// Регрессия: перезапись telemt.toml не должна сбрасывать владельца и права
// (раньше temp+rename давал root:0600 — telemt под своим пользователем терял доступ).
func TestWriteLinesPreservesOwnerAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "telemt.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = 443\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil { // umask-proof
		t.Fatal(err)
	}
	st0, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	uid0 := st0.Sys().(*syscall.Stat_t).Uid
	gid0 := st0.Sys().(*syscall.Stat_t).Gid

	err = writeLines(path, []string{"[server]", "port = 443", `metrics_listen = "127.0.0.1:9090"`})
	if err != nil {
		t.Fatal(err)
	}
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st1.Mode().Perm(); got != 0640 {
		t.Fatalf("mode must be preserved: got %o, want 640", got)
	}
	st1s := st1.Sys().(*syscall.Stat_t)
	if st1s.Uid != uid0 || st1s.Gid != gid0 {
		t.Fatalf("owner must be preserved: got %d:%d, want %d:%d", st1s.Uid, st1s.Gid, uid0, gid0)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "metrics_listen") {
		t.Fatal("content must be the new one")
	}
	// temp-файл не должен остаться
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file must be renamed away")
	}
}

// applySharedConfigAdditively — посчитать патч и записать файл без управления
// сервисом. Боевой путь — applySharedConfigManaged (стоп→патч→старт→ожидание);
// этот ярлык существует только для тестов: они не трогают systemd.
func applySharedConfigAdditively(cfg *NodeConfig, shared SharedConfig) (bool, error) {
	newLines, changed, err := computeSharedConfigPatch(cfg, shared)
	if err != nil || !changed {
		return changed, err
	}
	if err := writeLines(cfg.Telemt.ConfigPath, newLines); err != nil {
		return false, err
	}
	return true, nil
}
