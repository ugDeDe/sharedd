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
	if err := os.WriteFile(path, []byte("[registry]\nurl = \"http://127.0.0.1:8080\"\n"), 0600); err != nil {
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

func TestLoadOrGenerateRandomIDPersists(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "nested", "node_id")

	first, err := loadOrGenerateRandomID(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "node-") || len(first) != len("node-")+16 {
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
