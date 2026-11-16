package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

// managedMtdd — MTProxyL Manager с движком mtdd: users — состояние менеджера;
// после каждой команды CLI оно пишется в secrets.conf / devices.conf (их читает
// агент) и в users.json движка (по нему подтверждается рекламная пауза).
type managedMtdd struct {
	cfg   *config.NodeConfig
	users map[string]mtproxylMtddUser
	calls [][]string
	// engineAdTags=false — сборка mtdd без полного API: метку пользователя
	// MTProxyL хранит, а движок не принимает.
	engineAdTags bool
	fail         func(args []string) error
	t            *testing.T
}

func (m *managedMtdd) persist() {
	m.t.Helper()
	names := make([]string, 0, len(m.users))
	for n := range m.users {
		names = append(names, n)
	}
	sort.Strings(names)
	var secrets, devices strings.Builder
	secrets.WriteString("# MTProxyL — база секретов\n# Формат: LABEL|SECRET|CREATED_TS|ENABLED|MAX_CONNS|MAX_IPS|QUOTA_BYTES|EXPIRES|NOTES|AD_TAG|RATE_UP:RATE_DOWN\n")
	type rec struct {
		Name      string `json:"name"`
		Secret    string `json:"secret"`
		Enabled   bool   `json:"enabled"`
		TLSDomain string `json:"tls_domain,omitempty"`
		AdTag     string `json:"ad_tag,omitempty"`
	}
	var engine struct {
		Users []rec `json:"users"`
	}
	for _, n := range names {
		u := m.users[n]
		fmt.Fprintf(&secrets, "%s|%s|1700000000|%t|0|0|0|0|note|%s|\n", u.Label, u.Secret, u.Enabled, u.AdTag)
		if u.TLSDomain != "" {
			fmt.Fprintf(&devices, "%s||%s|\n", u.Label, u.TLSDomain)
		}
		r := rec{Name: u.Label, Secret: u.Secret, Enabled: u.Enabled, TLSDomain: u.TLSDomain}
		if m.engineAdTags {
			r.AdTag = u.AdTag
		}
		engine.Users = append(engine.Users, r)
	}
	raw, _ := json.Marshal(engine)
	for path, data := range map[string][]byte{
		filepath.Join(mtproxylInstallDir, "secrets.conf"):             []byte(secrets.String()),
		filepath.Join(mtproxylInstallDir, "devices.conf"):             []byte(devices.String()),
		filepath.Join(mtproxylInstallDir, "mtdd-state", "users.json"): raw,
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			m.t.Fatal(err)
		}
	}
}

func managedMtddFixture(t *testing.T) *managedMtdd {
	t.Helper()
	oldRoot, oldUsers, oldCmd, oldWait := mtproxylInstallDir, readMtproxylMtddUsers, applyMtproxylCmd, applyWaitMetrics
	t.Cleanup(func() {
		mtproxylInstallDir, readMtproxylMtddUsers, applyMtproxylCmd, applyWaitMetrics = oldRoot, oldUsers, oldCmd, oldWait
	})
	mtproxylInstallDir = filepath.Join(t.TempDir(), "mtproxyl")
	dir := filepath.Join(mtproxylInstallDir, "mtdd")
	for _, d := range []string{dir, filepath.Join(mtproxylInstallDir, "mtdd-state")} {
		if err := os.MkdirAll(d, 0750); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.json")
	conf := fmt.Sprintf(`{"proxy_port":443,"control_host":"127.0.0.1","control_port":9091,"tls_domain":"local.example.org","users_path":%q}`,
		filepath.Join(mtproxylInstallDir, "mtdd-state", "users.json"))
	if err := os.WriteFile(path, []byte(conf), 0640); err != nil {
		t.Fatal(err)
	}
	m := &managedMtdd{
		cfg:          &config.NodeConfig{},
		users:        map[string]mtproxylMtddUser{"local": {Label: "local", Secret: strings.Repeat("b", 32), Enabled: true}},
		engineAdTags: true,
		t:            t,
	}
	m.cfg.Telemt.ConfigPath = path
	// Настоящий разбор файлов; перед чтением — то, что тест поменял в users.
	realRead := readMtproxylMtddUsers
	readMtproxylMtddUsers = func() ([]mtproxylMtddUser, error) {
		m.persist()
		return realRead()
	}
	applyMtproxylCmd = func(args ...string) error {
		m.calls = append(m.calls, append([]string(nil), args...))
		if m.fail != nil {
			if err := m.fail(args); err != nil {
				return err
			}
		}
		name := args[2]
		u := m.users[name]
		switch args[1] {
		case "add":
			u = mtproxylMtddUser{Label: name, Secret: args[3], Enabled: true}
		case "rename":
			delete(m.users, name)
			name, u.Label = args[3], args[3]
		case "remove":
			delete(m.users, name)
			m.persist()
			return nil
		case "rotate":
			u.Secret = args[3]
		case "enable":
			u.Enabled = true
		case "domain":
			if args[3] == "" {
				t.Fatal("empty domain only reads the current setting; use remove")
			}
			u.TLSDomain = args[3]
			if u.TLSDomain == "remove" {
				u.TLSDomain = ""
			}
		case "adtag":
			u.AdTag = args[3]
			if u.AdTag == "remove" {
				u.AdTag = ""
			}
		default:
			t.Fatalf("unexpected command: %v", args[:2])
		}
		m.users[name] = u
		m.persist()
		return nil
	}
	applyWaitMetrics = func(*config.NodeConfig, time.Duration) error { return nil }
	m.persist()
	return m
}

func TestManagedMtddUsersPersistThroughCLI(t *testing.T) {
	m := managedMtddFixture(t)
	before, _ := os.ReadFile(m.cfg.Telemt.ConfigPath)
	shared := SharedConfig{Users: map[string]string{"pool": strings.Repeat("a", 32)}, TLSDomain: "pool.example.org"}
	if err := applySharedConfigManaged(m.cfg, shared); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"secret", "add", "pool", strings.Repeat("a", 32)}, {"secret", "domain", "pool", "pool.example.org"}}
	if !reflect.DeepEqual(m.calls, want) || m.users["local"].Secret != strings.Repeat("b", 32) {
		t.Fatalf("wrong CLI operations or local user changed: %v", m.calls)
	}
	after, _ := os.ReadFile(m.cfg.Telemt.ConfigPath)
	if string(after) != string(before) {
		t.Fatal("generated config was edited directly")
	}
	m.calls = nil
	if err := applySharedConfigManaged(m.cfg, shared); err != nil || len(m.calls) != 0 {
		t.Fatalf("unchanged config was reapplied: %v, %v", err, m.calls)
	}
	u := m.users["pool"]
	u.Enabled = false
	m.users["pool"] = u
	shared.Users["pool"], shared.TLSDomain = strings.Repeat("c", 32), "local.example.org"
	if err := applySharedConfigManaged(m.cfg, shared); err != nil {
		t.Fatal(err)
	}
	want = [][]string{{"secret", "rotate", "pool", strings.Repeat("c", 32)}, {"secret", "enable", "pool"}, {"secret", "domain", "pool", "remove"}}
	if !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("rotation/enable/domain reset: %v", m.calls)
	}
}

// Разбор баз менеджера — по правилам load_secrets: комментарии, битые строки
// и метки, которые MTProxyL отбрасывает, пропускаются.
func TestReadMtproxylMtddUsersParsesManagerFiles(t *testing.T) {
	old := mtproxylInstallDir
	t.Cleanup(func() { mtproxylInstallDir = old })
	mtproxylInstallDir = t.TempDir()
	secrets := strings.Join([]string{
		"# MTProxyL — база секретов",
		"",
		"pool|" + strings.Repeat("A", 32) + "|1700000000|false|0|0|0|0|a note|" + strings.Repeat("E", 32) + "|10:20",
		"old|" + strings.Repeat("b", 32),
		"bad.label|" + strings.Repeat("c", 32) + "|1|true",
		"short|abc|1|true",
		"tag|" + strings.Repeat("d", 32) + "|1|true|0|0|0|0||nothex|",
	}, "\n") + "\r\n"
	if err := os.WriteFile(filepath.Join(mtproxylInstallDir, "secrets.conf"), []byte(secrets), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mtproxylInstallDir, "devices.conf"), []byte("pool|3|Pool.Example.org|\nold|2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readMtproxylMtddUsers()
	if err != nil {
		t.Fatal(err)
	}
	want := []mtproxylMtddUser{
		{Label: "pool", Secret: strings.Repeat("a", 32), Enabled: false, TLSDomain: "pool.example.org", AdTag: strings.Repeat("e", 32)},
		{Label: "old", Secret: strings.Repeat("b", 32), Enabled: true},
		{Label: "tag", Secret: strings.Repeat("d", 32), Enabled: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("users = %+v, want %+v", got, want)
	}
	// Без devices.conf (старый MTProxyL) — просто без персональных доменов.
	_ = os.Remove(filepath.Join(mtproxylInstallDir, "devices.conf"))
	if got, err := readMtproxylMtddUsers(); err != nil || len(got) != 3 || got[0].TLSDomain != "" {
		t.Fatalf("without devices.conf: %+v, %v", got, err)
	}
}

func TestManagedMtddAliasAndErrors(t *testing.T) {
	m := managedMtddFixture(t)
	m.users["old-pool"] = mtproxylMtddUser{Label: "old-pool", Secret: strings.Repeat("a", 32), Enabled: true}
	shared := SharedConfig{Users: map[string]string{"pool": strings.Repeat("a", 32)}}
	if err := applySharedConfigManaged(m.cfg, shared); err != nil {
		t.Fatal(err)
	}
	if len(m.calls) != 1 || m.calls[0][1] != "rename" || m.users["old-pool"].Label != "" || m.users["pool"].Label != "pool" {
		t.Fatalf("alias was not renamed: %v", m.calls)
	}
	shared.Users["pool"] = strings.Repeat("d", 32)
	m.fail = func([]string) error {
		return fmt.Errorf("mtproxyl secret rotate: exit status 1: private CLI output %s", shared.Users["pool"])
	}
	err := applySharedConfigManaged(m.cfg, shared)
	if err == nil || strings.Contains(err.Error(), shared.Users["pool"]) || !strings.Contains(err.Error(), "private CLI output") {
		t.Fatalf("CLI failure hidden, reason lost or credentials leaked: %v", err)
	}
}

// Один плохой пользователь не блокирует остальных: имя с точкой (регистратор
// его допускает, MTProxyL — нет) и упавшая команда только пропускаются.
func TestManagedMtddOneBadUserDoesNotBlockOthers(t *testing.T) {
	m := managedMtddFixture(t)
	m.fail = func(args []string) error {
		if args[2] == "broken" {
			return fmt.Errorf("exit status 1")
		}
		return nil
	}
	shared := SharedConfig{Users: map[string]string{
		"a.dot":  strings.Repeat("1", 32),
		"broken": strings.Repeat("2", 32),
		"good":   strings.Repeat("3", 32),
	}}
	err := applySharedConfigManaged(m.cfg, shared)
	if err == nil {
		t.Fatal("errors must still be reported")
	}
	if m.users["good"].Secret != strings.Repeat("3", 32) {
		t.Fatalf("good user was not applied: %v", m.calls)
	}
	for _, c := range m.calls {
		if c[2] == "a.dot" {
			t.Fatalf("invalid label reached the CLI: %v", c)
		}
	}
}

func TestManagedMtddMetricsUseOwnCredentials(t *testing.T) {
	m := managedMtddFixture(t)
	token := strings.Repeat("e", 64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("wrong metrics credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "mtdd_me_pool_ready{dc=\"2\"} 3\n")
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	data := fmt.Sprintf(`{"proxy_port":443,"control_port":9091,"metrics_host":%q,"metrics_port":%s,"metrics_token_env":"READ_METRICS"}`, host, port)
	if err := os.WriteFile(m.cfg.Telemt.ConfigPath, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(filepath.Dir(m.cfg.Telemt.ConfigPath), "credentials.env")
	if err := os.WriteFile(creds, []byte("READ_METRICS='"+token+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, err := metricsGet(srv.Client(), m.cfg, srv.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics returned %d", resp.StatusCode)
	}
	c, err := loadMtddConfig(m.cfg.Telemt.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "configPath") {
		t.Fatal("internal config path exposed in JSON")
	}
}

// Конфиг, как его пишет MTProxyL: metrics_port без metrics_token_env. Метрики —
// со своего слушателя без токена (mtdd пускает по metrics_allow), а не
// устаревший /metrics на порту API под admin-токеном.
func TestManagedMtddMetricsListenerWithoutToken(t *testing.T) {
	m := managedMtddFixture(t)
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "mtdd_me_pool_ready{dc=\"2\"} 3\n")
	}))
	defer srv.Close()
	_, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	data := fmt.Sprintf(`{"proxy_port":443,"control_port":1,"api_token_env":"MTDD_API_TOKEN","metrics_host":"0.0.0.0","metrics_port":%s}`, port)
	if err := os.WriteFile(m.cfg.Telemt.ConfigPath, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(filepath.Dir(m.cfg.Telemt.ConfigPath), "credentials.env")
	if err := os.WriteFile(creds, []byte("MTDD_API_TOKEN="+strings.Repeat("f", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	u, err := metricsURLForConfig(m.cfg)
	if err != nil || u != "http://127.0.0.1:"+port+"/metrics" {
		t.Fatalf("metrics url = %q err=%v", u, err)
	}
	if err := waitMetricsReady(m.cfg, 0); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("tokenless metrics listener must not get the admin token, got %q", gotAuth)
	}
}

func TestManagedMtddRecoveryUsesCLI(t *testing.T) {
	m := managedMtddFixture(t)
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	t.Setenv("MTPROXYL_RESTART_MARKER", marker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cli := "#!/bin/sh\n[ \"$1\" = restart ] || exit 19\nprintf restarted > \"$MTPROXYL_RESTART_MARKER\"\n"
	if err := os.WriteFile(filepath.Join(dir, "mtproxyl"), []byte(cli), 0755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := os.Stat(marker); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	data := fmt.Sprintf(`{"proxy_port":443,"control_host":%q,"control_port":%s}`, host, port)
	if err := os.WriteFile(m.cfg.Telemt.ConfigPath, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(filepath.Dir(m.cfg.Telemt.ConfigPath), "credentials.env")
	if err := os.WriteFile(creds, []byte("MTDD_API_TOKEN="+strings.Repeat("e", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTimeout := ensureMetricsTimeout
	ensureMetricsTimeout = -time.Second
	t.Cleanup(func() { ensureMetricsTimeout = oldTimeout })
	if err := ensureProxyUp(m.cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("MTProxyL restart was not called")
	}
}

func withAdPauseUsers(t *testing.T, users ...string) {
	t.Helper()
	oldUsers, oldPrev := adPauseSharedUsers, adPausePrevFile
	adPauseSharedUsers = func() []string { return users }
	adPausePrevFile = filepath.Join(t.TempDir(), "ad-prev.json")
	t.Cleanup(func() { adPauseSharedUsers, adPausePrevFile = oldUsers, oldPrev })
}

func TestManagedMtddAdPausePreservesLocalUsers(t *testing.T) {
	m := managedMtddFixture(t)
	m.users["pool"] = mtproxylMtddUser{Label: "pool", Secret: strings.Repeat("a", 32), Enabled: true, AdTag: strings.Repeat("d", 32)}
	u := m.users["local"]
	u.AdTag = strings.Repeat("b", 32)
	m.users["local"] = u
	m.persist()
	withAdPauseUsers(t, "pool")
	before, _ := os.ReadFile(m.cfg.Telemt.ConfigPath)
	if got := applyAdPause(m.cfg, adPauseDirective{Active: true, Tag: strings.Repeat("e", 32)}); got != adPausePool {
		t.Fatalf("pool tag failed: %s", got)
	}
	if m.users["local"] != u || m.users["pool"].AdTag != strings.Repeat("e", 32) {
		t.Fatal("pool tag affected local user or was not applied")
	}
	if got := applyAdPause(m.cfg, adPauseDirective{}); got != adPauseOwn {
		t.Fatalf("tag restore failed: %s", got)
	}
	after, _ := os.ReadFile(m.cfg.Telemt.ConfigPath)
	if m.users["pool"].AdTag != strings.Repeat("d", 32) || string(before) != string(after) {
		t.Fatal("previous user tag/global config not preserved")
	}
}

// mtdd без полного API: MTProxyL метку записал, движок её не принял — окно не
// подтверждается как pool (иначе время рекламы засчитывалось бы впустую).
func TestManagedMtddAdPauseConfirmsEngineState(t *testing.T) {
	m := managedMtddFixture(t)
	m.engineAdTags = false
	m.users["pool"] = mtproxylMtddUser{Label: "pool", Secret: strings.Repeat("a", 32), Enabled: true}
	m.persist()
	withAdPauseUsers(t, "pool")
	if got := applyAdPause(m.cfg, adPauseDirective{Active: true, Tag: strings.Repeat("e", 32)}); got != adPauseFailed {
		t.Fatalf("tag not accepted by mtdd must not be confirmed, got %s", got)
	}
	if m.users["pool"].AdTag != strings.Repeat("e", 32) {
		t.Fatal("tag was not requested through the CLI")
	}
}
