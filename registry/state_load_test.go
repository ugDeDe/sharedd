package main

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
	r.loadState()
	if _, ok := r.state.Candidates["broken"]; !ok {
		t.Fatal("state candidate entry should be preserved for inspection")
	}
	if r.state.Candidates["broken"] != nil {
		t.Fatal("expected null candidate to remain nil without crashing")
	}
}
