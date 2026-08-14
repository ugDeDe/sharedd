package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// withManagerDirs подменяет каталоги детекта менеджеров на tmp-фикстуры.
func withManagerDirs(t *testing.T) (meko, mtproxyl string) {
	t.Helper()
	dir := t.TempDir()
	meko = filepath.Join(dir, "mtpr-simple")
	mtproxyl = filepath.Join(dir, "mtproxyl")
	oldM, oldP := mekoInstallDir, mtproxylInstallDir
	mekoInstallDir, mtproxylInstallDir = meko, mtproxyl
	t.Cleanup(func() { mekoInstallDir, mtproxylInstallDir = oldM, oldP })
	return meko, mtproxyl
}

// V7.9: детект типа ноды по каталогу менеджера; без каталогов — classic.
func TestDetectNodeType(t *testing.T) {
	meko, mtproxyl := withManagerDirs(t)

	if got := detectNodeType(); got != NodeTypeClassic {
		t.Fatalf("no managers installed -> classic, got %q", got)
	}
	if err := os.Mkdir(mtproxyl, 0755); err != nil {
		t.Fatal(err)
	}
	if got := detectNodeType(); got != NodeTypeMTProxyL {
		t.Fatalf("mtproxyl dir -> mtproxyl, got %q", got)
	}
	if err := os.Mkdir(meko, 0755); err != nil {
		t.Fatal(err)
	}
	// оба стоят (переустановка не дочистила) — детерминированный приоритет MEKO
	if got := detectNodeType(); got != NodeTypeMEKO {
		t.Fatalf("both dirs -> meko (deterministic priority), got %q", got)
	}
	// файл, а не каталог — не детект
	os.Remove(meko)
	os.Remove(mtproxyl)
	os.WriteFile(mtproxyl, []byte("not a dir"), 0644)
	if got := detectNodeType(); got != NodeTypeClassic {
		t.Fatalf("plain file is not an install, got %q", got)
	}
}

func TestNodeTypeLabel(t *testing.T) {
	if nodeTypeLabel(NodeTypeMTProxyL) != "MTProxyL" ||
		nodeTypeLabel(NodeTypeMEKO) != "MEKO" ||
		nodeTypeLabel(NodeTypeClassic) != "Classic" {
		t.Fatal("labels mismatch")
	}
}

// V7.9: register шлёт node_type и пиннит lastNodeType только при HTTP 200 —
// при отказе сервера heartbeat заметит расхождение и повторит.
func TestRegisterSendsNodeType(t *testing.T) {
	_, mtproxyl := withManagerDirs(t)
	lastNodeType = ""
	t.Cleanup(func() { lastNodeType = "" })
	if err := os.Mkdir(mtproxyl, 0755); err != nil {
		t.Fatal(err)
	}

	var got registerPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := &NodeConfig{}
	cfg.Registry.URL = srv.URL
	nodeID = "node-t-1"
	if ok, _ := register(&http.Client{Timeout: 3 * time.Second}, cfg, "1.2.3.4"); !ok {
		t.Fatal("register must succeed against 200 server")
	}
	if got.NodeType != NodeTypeMTProxyL || got.NodeID != "node-t-1" || got.IP != "1.2.3.4" {
		t.Fatalf("payload must carry node_type, got %+v", got)
	}
	if lastNodeType != NodeTypeMTProxyL {
		t.Fatalf("lastNodeType must be pinned after 200, got %q", lastNodeType)
	}
}

func TestRegisterFailureKeepsNodeType(t *testing.T) {
	withManagerDirs(t) // ничего не стоит → classic
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg := &NodeConfig{}
	cfg.Registry.URL = srv.URL
	lastNodeType = NodeTypeMTProxyL // ранее регистрировались как MTProxyL
	nodeID = "node-t-2"

	if ok, _ := register(&http.Client{Timeout: 3 * time.Second}, cfg, "1.2.3.4"); ok {
		t.Fatal("register against 500 must report failure")
	}
	if lastNodeType != NodeTypeMTProxyL {
		t.Fatalf("failed register must NOT pin new type (retry needed), got %q", lastNodeType)
	}
}
