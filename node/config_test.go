package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadNodeConfigMinimal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.toml")
	content := `
[registry]
url = "https://registry.example.com/"
token = "test-node-token"

[telemt]
config_path = "/etc/telemt/telemt.toml"

[sync]
apply_to_telemt = true
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CONFIG_PATH", path)
	// сбросить os.Args, т.к. flag-парсер читает их
	old := os.Args
	os.Args = []string{"node"}
	defer func() { os.Args = old }()

	cfg, err := loadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Registry.URL != "https://registry.example.com" {
		t.Fatalf("trailing slash must be trimmed, got %q", cfg.Registry.URL)
	}
	if cfg.Telemt.ConfigPath != "/etc/telemt/telemt.toml" {
		t.Fatalf("unexpected config path %q", cfg.Telemt.ConfigPath)
	}
	if !cfg.Sync.ApplyToTelemt {
		t.Fatal("apply_to_telemt lost")
	}
}

func TestLoadNodeConfigDefaultsApplied(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.toml")
	if err := os.WriteFile(path, []byte("[registry]\nurl = \"http://127.0.0.1:8080\"\ntoken = \"test-node-token\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CONFIG_PATH", path)
	old := os.Args
	os.Args = []string{"node"}
	defer func() { os.Args = old }()

	cfg, err := loadNodeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Telemt.ConfigPath != defaultTelemtConfigPath {
		t.Fatalf("default telemt path expected, got %q", cfg.Telemt.ConfigPath)
	}
}

func TestLoadNodeConfigRequiresRegistryURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "node.toml")
	if err := os.WriteFile(path, []byte("[telemt]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CONFIG_PATH", path)
	old := os.Args
	os.Args = []string{"node"}
	defer func() { os.Args = old }()

	if _, err := loadNodeConfig(); err == nil {
		t.Fatal("config without registry.url must fail")
	}
}

func TestLoadNodeConfigRequiresRegistryToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node.toml")
	if err := os.WriteFile(path, []byte("[registry]\nurl = \"https://registry.example.com\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_CONFIG_PATH", path)
	old := os.Args
	os.Args = []string{"node"}
	defer func() { os.Args = old }()

	if _, err := loadNodeConfig(); err == nil || !strings.Contains(err.Error(), "registry.token") {
		t.Fatalf("config without registry.token must fail, got %v", err)
	}
}

func TestLoadNodeConfigRejectsInvalidURLAndWatchdog(t *testing.T) {
	for name, body := range map[string]string{
		"url":      "[registry]\nurl = \"file:///tmp/registry\"\ntoken = \"x\"\n",
		"watchdog": "[registry]\nurl = \"https://registry.example\"\ntoken = \"x\"\n[watchdog]\ndead_kill_ms = -2\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node.toml")
			if err := os.WriteFile(path, []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NODE_CONFIG_PATH", path)
			old := os.Args
			os.Args = []string{"node"}
			defer func() { os.Args = old }()
			if _, err := loadNodeConfig(); err == nil {
				t.Fatal("invalid node config accepted")
			}
		})
	}
}

func TestLoadOrGenerateRandomIDPersists(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "nested", "node_id")

	first, err := loadOrGenerateRandomID(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !nodeIDPattern.MatchString(first) || !strings.HasPrefix(first, "node-") {
		t.Fatalf("bad random id format: %q", first)
	}

	second, err := loadOrGenerateRandomID(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("id must persist across restarts: %q != %q", first, second)
	}

	info, err := os.Stat(stateFile)
	if err != nil {
		t.Fatal("state file must exist")
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state file must be 0600, got %o", info.Mode().Perm())
	}
}

func TestLoadOrGenerateRandomIDMigratesLegacyID(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "node_id")
	if err := os.WriteFile(stateFile, []byte("helsinki-long-name-0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := loadOrGenerateRandomID(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !nodeIDPattern.MatchString(id) || !strings.HasPrefix(id, "helsinki-l-") {
		t.Fatalf("legacy ID was not migrated with a preserved 10-char name: %q", id)
	}
	data, err := os.ReadFile(stateFile)
	if err != nil || string(data) != id {
		t.Fatalf("migrated ID was not persisted: %q err=%v", data, err)
	}
}
