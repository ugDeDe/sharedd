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
	"strings"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

func managedMtddFixture(t *testing.T) (*config.NodeConfig, map[string]mtproxylMtddUser, *[][]string) {
	t.Helper()
	oldRoot, oldUsers, oldCmd, oldWait := mtproxylInstallDir, readMtproxylMtddUsers, applyMtproxylCmd, applyWaitMetrics
	t.Cleanup(func() {
		mtproxylInstallDir, readMtproxylMtddUsers, applyMtproxylCmd, applyWaitMetrics = oldRoot, oldUsers, oldCmd, oldWait
	})
	mtproxylInstallDir = filepath.Join(t.TempDir(), "mtproxyl")
	dir := filepath.Join(mtproxylInstallDir, "mtdd")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"proxy_port":443,"control_host":"127.0.0.1","control_port":9091,"tls_domain":"local.example.org"}`), 0640); err != nil {
		t.Fatal(err)
	}
	cfg := &config.NodeConfig{}
	cfg.Telemt.ConfigPath = path
	users := map[string]mtproxylMtddUser{
		"local": {Label: "local", Secret: strings.Repeat("b", 32), Enabled: true},
	}
	readMtproxylMtddUsers = func() ([]mtproxylMtddUser, error) {
		var rows []mtproxylMtddUser
		for _, u := range users {
			rows = append(rows, u)
		}
		return rows, nil
	}
	var calls [][]string
	applyMtproxylCmd = func(args ...string) error {
		calls = append(calls, append([]string(nil), args...))
		name := args[2]
		u := users[name]
		switch args[1] {
		case "add":
			u = mtproxylMtddUser{Label: name, Secret: args[3], Enabled: true}
		case "rename":
			delete(users, name)
			name, u.Label = args[3], args[3]
		case "remove":
			delete(users, name)
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
		users[name] = u
		return nil
	}
	applyWaitMetrics = func(*config.NodeConfig, time.Duration) error { return nil }
	return cfg, users, &calls
}

func TestManagedMtddUsersPersistThroughCLI(t *testing.T) {
	cfg, users, calls := managedMtddFixture(t)
	before, _ := os.ReadFile(cfg.Telemt.ConfigPath)
	shared := SharedConfig{Users: map[string]string{"pool": strings.Repeat("a", 32)}, TLSDomain: "pool.example.org"}
	if err := applySharedConfigManaged(cfg, shared); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"secret", "add", "pool", strings.Repeat("a", 32)}, {"secret", "domain", "pool", "pool.example.org"}}
	if !reflect.DeepEqual(*calls, want) || users["local"].Secret != strings.Repeat("b", 32) {
		t.Fatalf("wrong CLI operations or local user changed: %v", *calls)
	}
	after, _ := os.ReadFile(cfg.Telemt.ConfigPath)
	if string(after) != string(before) {
		t.Fatal("generated config was edited directly")
	}
	*calls = nil
	if err := applySharedConfigManaged(cfg, shared); err != nil || len(*calls) != 0 {
		t.Fatalf("unchanged config was reapplied: %v, %v", err, *calls)
	}
	u := users["pool"]
	u.Enabled = false
	users["pool"] = u
	shared.Users["pool"], shared.TLSDomain = strings.Repeat("c", 32), "local.example.org"
	if err := applySharedConfigManaged(cfg, shared); err != nil {
		t.Fatal(err)
	}
	want = [][]string{{"secret", "rotate", "pool", strings.Repeat("c", 32)}, {"secret", "enable", "pool"}, {"secret", "domain", "pool", "remove"}}
	if !reflect.DeepEqual(*calls, want) {
		t.Fatalf("rotation/enable/domain reset: %v", *calls)
	}
}

func TestManagedMtddAliasAndErrors(t *testing.T) {
	cfg, users, calls := managedMtddFixture(t)
	users["old-pool"] = mtproxylMtddUser{Label: "old-pool", Secret: strings.Repeat("a", 32), Enabled: true}
	shared := SharedConfig{Users: map[string]string{"pool": strings.Repeat("a", 32)}}
	if err := applySharedConfigManaged(cfg, shared); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || (*calls)[0][1] != "rename" || users["old-pool"].Label != "" {
		t.Fatalf("alias was not renamed: %v", *calls)
	}
	shared.Users["pool"] = strings.Repeat("d", 32)
	applyMtproxylCmd = func(...string) error { return fmt.Errorf("private CLI output %s", shared.Users["pool"]) }
	err := applySharedConfigManaged(cfg, shared)
	if err == nil || strings.Contains(err.Error(), shared.Users["pool"]) {
		t.Fatalf("CLI failure hidden or credentials leaked: %v", err)
	}
}

func TestManagedMtddMetricsUseOwnCredentials(t *testing.T) {
	cfg, _, _ := managedMtddFixture(t)
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
	if err := os.WriteFile(cfg.Telemt.ConfigPath, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(filepath.Dir(cfg.Telemt.ConfigPath), "credentials.env")
	if err := os.WriteFile(creds, []byte("READ_METRICS='"+token+"'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	resp, err := metricsGet(srv.Client(), cfg, srv.URL+"/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics returned %d", resp.StatusCode)
	}
	c, err := loadMtddConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "configPath") {
		t.Fatal("internal config path exposed in JSON")
	}
}

func TestManagedMtddRecoveryUsesCLI(t *testing.T) {
	cfg, _, _ := managedMtddFixture(t)
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
	if err := os.WriteFile(cfg.Telemt.ConfigPath, []byte(data), 0640); err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(filepath.Dir(cfg.Telemt.ConfigPath), "credentials.env")
	if err := os.WriteFile(creds, []byte("MTDD_API_TOKEN="+strings.Repeat("e", 64)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	oldTimeout := ensureMetricsTimeout
	ensureMetricsTimeout = -time.Second
	t.Cleanup(func() { ensureMetricsTimeout = oldTimeout })
	if err := ensureProxyUp(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("MTProxyL restart was not called")
	}
}

func TestManagedMtddAdPausePreservesLocalUsers(t *testing.T) {
	cfg, users, _ := managedMtddFixture(t)
	users["pool"] = mtproxylMtddUser{Label: "pool", Secret: strings.Repeat("a", 32), Enabled: true, AdTag: strings.Repeat("d", 32)}
	u := users["local"]
	u.AdTag = strings.Repeat("b", 32)
	users["local"] = u
	oldUsers, oldPrev := adPauseSharedUsers, adPausePrevFile
	adPauseSharedUsers = func() []string { return []string{"pool"} }
	adPausePrevFile = filepath.Join(t.TempDir(), "ad-prev.json")
	t.Cleanup(func() { adPauseSharedUsers, adPausePrevFile = oldUsers, oldPrev })
	before, _ := os.ReadFile(cfg.Telemt.ConfigPath)
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: strings.Repeat("e", 32)}); got != adPausePool {
		t.Fatalf("pool tag failed: %s", got)
	}
	if users["local"] != u || users["pool"].AdTag != strings.Repeat("e", 32) {
		t.Fatal("pool tag affected local user or was not applied")
	}
	if got := applyAdPause(cfg, adPauseDirective{}); got != adPauseOwn {
		t.Fatalf("tag restore failed: %s", got)
	}
	after, _ := os.ReadFile(cfg.Telemt.ConfigPath)
	if users["pool"].AdTag != strings.Repeat("d", 32) || string(before) != string(after) {
		t.Fatal("previous user tag/global config not preserved")
	}
}
