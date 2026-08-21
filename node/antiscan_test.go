package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAntiscanIPs(t *testing.T) {
	got := parseAntiscanIPs([]byte("# comment\n1.2.3.4 3\ninvalid\n2001:db8::1\n1.2.3.4 4\n5.6.7.8\n"))
	if strings.Join(got, ",") != "1.2.3.4,5.6.7.8" {
		t.Fatalf("unexpected list: %v", got)
	}
}

func TestApplyAntiscanUsesRegistryPortWithoutTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemt.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = 8443\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	oldRun, oldFetch := antiscanRun, antiscanFetch
	oldShared := sharedConfigCache.Get()
	defer func() {
		antiscanRun, antiscanFetch = oldRun, oldFetch
		sharedConfigCache.Set(oldShared)
	}()
	sharedConfigCache.Set(SharedConfig{ProxyPort: 8443})
	antiscanFetch = func(context.Context) ([]byte, error) { return []byte("1.2.3.4\n5.6.7.8\n"), nil }
	calls := []string{}
	antiscanRun = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		calls = append(calls, line)
		if line == "iptables -S INPUT" {
			return nil, nil
		}
		if strings.Contains(line, " -C ") || strings.HasSuffix(line, " -N ANTISCAN_MTPROTO") {
			return nil, os.ErrNotExist
		}
		return nil, nil
	}
	if err := applyAntiscan(cfg); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--dport 8443") || strings.Contains(strings.ToLower(joined), "ttl") {
		t.Fatalf("commands must use registry port and contain no TTL rule:\n%s", joined)
	}
}

func TestApplyAntiscanDisablesHookOnPortMismatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemt.toml")
	if err := os.WriteFile(path, []byte("[server]\nport = 443\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = path
	oldRun, oldShared := antiscanRun, sharedConfigCache.Get()
	defer func() { antiscanRun = oldRun; sharedConfigCache.Set(oldShared) }()
	sharedConfigCache.Set(SharedConfig{ProxyPort: 8443})
	calls := []string{}
	antiscanRun = func(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
		line := name + " " + strings.Join(args, " ")
		calls = append(calls, line)
		if line == "iptables -S INPUT" {
			return []byte("-A INPUT -p tcp -m tcp --dport 443 -j ANTISCAN_MTPROTO\n"), nil
		}
		return nil, nil
	}
	err := applyAntiscan(cfg)
	if err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("expected mismatch error, got %v", err)
	}
	if !strings.Contains(strings.Join(calls, "\n"), "iptables -D INPUT") {
		t.Fatalf("old hook was not removed: %v", calls)
	}
}
