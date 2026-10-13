package cryptoutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// M4: существующий master.key с групповыми/прочими битами доступа отвергается;
// корректный 0600 читается; новый создаётся 0600.
func TestLoadMasterKeyPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")
	key, err := LoadMasterKey(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("key length = %d, want 32", len(key))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("created key mode = %o, want 0600", info.Mode().Perm())
	}
	// корректные права — читается тем же ключом
	again, err := LoadMasterKey(path)
	if err != nil || !bytes.Equal(again, key) {
		t.Fatalf("reload: key=%x err=%v", again, err)
	}
	// небезопасные права — отказ
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if _, err := LoadMasterKey(path); err == nil {
		t.Fatal("world-readable master.key must be rejected")
	}
}
