package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadStateIgnoresNullCandidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte(`{"candidates":{"broken":null},"assignments":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &resolvedRegistryConfig{}
	cfg.State.File = path
	r := &Registry{cfg: cfg}
	if err := r.loadState(); err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if _, ok := r.state.Candidates["broken"]; !ok {
		t.Fatal("state candidate entry should be preserved for inspection")
	}
	if r.state.Candidates["broken"] != nil {
		t.Fatal("expected null candidate to remain nil without crashing")
	}
}

func TestLoadStateMissingFileIsFirstStart(t *testing.T) {
	cfg := &resolvedRegistryConfig{}
	cfg.State.File = filepath.Join(t.TempDir(), "missing.json")
	r := &Registry{cfg: cfg}
	if err := r.loadState(); err != nil {
		t.Fatalf("missing state file must be a valid first start, got %v", err)
	}
}

// A corrupt state file must abort startup instead of being silently replaced
// by an empty state on the next persist. This test pins the error contract;
// Run() turns it into a fatal startup error.
func TestLoadStateRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	const corrupt = "{\"candidates\":{\"a\":"
	if err := os.WriteFile(path, []byte(corrupt), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &resolvedRegistryConfig{}
	cfg.State.File = path
	r := &Registry{cfg: cfg}
	if err := r.loadState(); err == nil {
		t.Fatal("corrupt state file must return an error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != corrupt {
		t.Fatalf("loadState must not rewrite the corrupt file, got %q", got)
	}
}
