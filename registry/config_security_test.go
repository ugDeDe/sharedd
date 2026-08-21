package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRegistryConfigRequiresNodeToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.toml")
	config := `
[cloudflare]
api_token = "dummy"
zone_id = "dummy"
domains = ["proxy.example.com"]

[shared_proxy]
tls_domain = "front.example.com"
[shared_proxy.users]
user = "0123456789abcdef0123456789abcdef"
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGISTRY_CONFIG_PATH", path)
	old := os.Args
	os.Args = []string{"registry"}
	defer func() { os.Args = old }()

	if _, err := loadRegistryConfig(); err == nil || !strings.Contains(err.Error(), "security.node_token") {
		t.Fatalf("config without security.node_token must fail, got %v", err)
	}
}

func validRegistryConfigForTest() RegistryConfig {
	var cfg RegistryConfig
	cfg.Security.NodeToken = "node-token"
	cfg.Panel.Token = "panel-token-for-tests"
	cfg.Cloudflare.APIToken = "cf-token"
	cfg.Cloudflare.ZoneID = "zone"
	cfg.Cloudflare.Domains = []string{"proxy.example.com"}
	cfg.SharedProxy.TLSDomain = "front.example.com"
	cfg.SharedProxy.Users = map[string]string{"user": "0123456789abcdef0123456789abcdef"}
	applyRegistryDefaults(&cfg)
	return cfg
}

func TestValidateRegistryConfigRejectsUnsafeStartupValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*RegistryConfig)
	}{
		{"negative ticker duration", func(c *RegistryConfig) { c.Healthcheck.ProbeIntervalMs = -1 }},
		{"zero threshold after defaults", func(c *RegistryConfig) { c.Healthcheck.FailThreshold = -1 }},
		{"invalid HTTP address", func(c *RegistryConfig) { c.HTTP.Addr = "bad address" }},
		{"invalid domain", func(c *RegistryConfig) { c.Cloudflare.Domains = []string{"bad_domain"} }},
		{"invalid URL", func(c *RegistryConfig) { c.Globalping.APIBase = "file:///tmp/api" }},
		{"invalid DNS TTL", func(c *RegistryConfig) { c.Cloudflare.DNSTTL = 30 }},
		{"invalid secret", func(c *RegistryConfig) { c.SharedProxy.Users["user"] = "short" }},
		{"empty panel token", func(c *RegistryConfig) { c.Panel.Token = "" }},
		{"public URL from untrusted shape", func(c *RegistryConfig) { c.Panel.PublicURL = "https://registry.example.com/panel?next=evil" }},
		{"negative disabled TTL", func(c *RegistryConfig) { v := -1; c.Healthcheck.PruneUnhealthyMin = &v }},
		{"GP validity not greater than interval", func(c *RegistryConfig) {
			c.Healthcheck.GlobalpingValidityMin = 5
			c.NodeDefaults.GlobalpingMs = 5 * 60 * 1000
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validRegistryConfigForTest()
			tt.edit(&cfg)
			if err := validateRegistryConfig(&cfg); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
	cfg := validRegistryConfigForTest()
	zero := 0
	cfg.Healthcheck.PruneUnhealthyMin = &zero
	cfg.Healthcheck.TerminateDeadMin = &zero
	cfg.Rotation.MasterTTLMinutes = &zero
	if err := validateRegistryConfig(&cfg); err != nil {
		t.Fatalf("documented explicit zero disable semantics rejected: %v", err)
	}
}
