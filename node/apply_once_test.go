package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withApplyOnceEnv — изоляция внешнего мира для one-shot тестов: никакого
// реального systemd/mtproxyl/менеджер-diр.
func withApplyOnceEnv(t *testing.T) {
	t.Helper()
	oldAttempts, oldDelay := applyOnceFetchAttempts, applyOnceFetchDelay
	oldUp, oldEnsure := proxyUpTimeout, ensureMetricsTimeout
	oldMeko, oldMtproxyl := mekoInstallDir, mtproxylInstallDir
	oldDirs, oldCands := systemdUnitDirs, proxyUnitCandidates
	applyOnceFetchAttempts = 2
	applyOnceFetchDelay = 5 * time.Millisecond
	proxyUpTimeout = 200 * time.Millisecond
	ensureMetricsTimeout = 50 * time.Millisecond
	mekoInstallDir = filepath.Join(t.TempDir(), "no-meko")
	mtproxylInstallDir = filepath.Join(t.TempDir(), "no-mtproxyl")
	t.Cleanup(func() {
		applyOnceFetchAttempts, applyOnceFetchDelay = oldAttempts, oldDelay
		proxyUpTimeout, ensureMetricsTimeout = oldUp, oldEnsure
		mekoInstallDir, mtproxylInstallDir = oldMeko, oldMtproxyl
		systemdUnitDirs, proxyUnitCandidates = oldDirs, oldCands
	})
}

func mkApplyOnceCfg(registryURL, telemtPath string) *NodeConfig {
	cfg := &NodeConfig{}
	cfg.Registry.URL = registryURL
	cfg.Registry.Token = "test-node-token"
	cfg.Telemt.ConfigPath = telemtPath
	cfg.Sync.ApplyToTelemt = true
	return cfg
}

func TestApplyOnceFlagScan(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()
	os.Args = []string{"node", "-config", "/etc/sharedd/node.toml", "-apply-once"}
	if !applyOnceFlag() {
		t.Fatal("-apply-once must be detected")
	}
	os.Args = []string{"node", "--apply-once"}
	if !applyOnceFlag() {
		t.Fatal("--apply-once must be detected")
	}
	os.Args = []string{"node", "-config", "/x"}
	if applyOnceFlag() {
		t.Fatal("no flag expected")
	}
}

func TestUnitFileOnDiskFallback(t *testing.T) {
	dir := t.TempDir()
	unit := "telemt.service"
	if err := os.WriteFile(filepath.Join(dir, unit), []byte("[Unit]\n"), 0644); err != nil {
		t.Fatal(err)
	}
	systemdUnitDirs = []string{dir}
	proxyUnitCandidates = []string{unit, "mtproxy.service"}
	if !unitFileOnDisk(unit) {
		t.Fatal("unitFileOnDisk must see the file in a unit dir")
	}
	if unitFileOnDisk("mtproxy.service") {
		t.Fatal("unitFileOnDisk must not invent units")
	}
	// detectProxyUnit: на машине без systemd (или без такого юнита) все
	// systemctl-уровни проваливаются, файловый рубеж обязан вытащить юнит.
	if got := detectProxyUnit(); got != unit {
		t.Fatalf("detectProxyUnit via disk fallback = %q, want %q", got, unit)
	}
}

func TestRunApplyOnceRegistryUnreachable(t *testing.T) {
	withApplyOnceEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	telemtPath := filepath.Join(t.TempDir(), "telemt.toml")
	orig := "[server]\nport = 443\n"
	if err := os.WriteFile(telemtPath, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}

	rc := runApplyOnce(mkApplyOnceCfg(srv.URL, telemtPath))
	if rc != applyOnceNoRegistry {
		t.Fatalf("rc = %d, want %d (registry unreachable)", rc, applyOnceNoRegistry)
	}
	// Контракт с установщиком: без регистратора конфиг НЕ трогаем.
	data, _ := os.ReadFile(telemtPath)
	if string(data) != orig {
		t.Fatalf("telemt.toml must stay untouched without registry, got:\n%s", data)
	}
}

func TestRunApplyOncePatchesConfig(t *testing.T) {
	withApplyOnceEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"tls_domain":"front.example.com","users":{"ivan":"sec-1"},"intervals":{"sync_ms":60000}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	telemtPath := filepath.Join(t.TempDir(), "telemt.toml")
	// metrics_listen на мёртвый порт — waitMetricsReady гарантированно не
	// дождётся ответа какое бы окружение ни было (в CI мог висеть чужой
	// процесс на 9090, и тест превращался бы в лотерею).
	original := "[server]\nport = 443\nmetrics_listen = \"127.0.0.1:19999\"\n"
	if err := os.WriteFile(telemtPath, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	// systemd юнита прокси нет (и вслепую telemt.service не рестартанётся):
	// поднять прокси не выйдет → код 1, а файл откатится.
	rc := runApplyOnce(mkApplyOnceCfg(srv.URL, telemtPath))
	if rc != applyOnceFailed {
		t.Fatalf("rc = %d, want %d (no systemd to start proxy)", rc, applyOnceFailed)
	}
	data, _ := os.ReadFile(telemtPath)
	if string(data) != original {
		t.Fatalf("failed restart must roll telemt.toml back:\n%s", data)
	}
}

func TestRunApplyOnceDisabled(t *testing.T) {
	cfg := mkApplyOnceCfg("http://127.0.0.1:1", filepath.Join(t.TempDir(), "telemt.toml"))
	cfg.Sync.ApplyToTelemt = false
	if rc := runApplyOnce(cfg); rc != applyOnceOK {
		t.Fatalf("rc = %d, want %d (apply disabled)", rc, applyOnceOK)
	}
}
