package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

// withManagerDirs подменяет каталоги детекта менеджеров на tmp-фикстуры.
func withManagerDirs(t *testing.T) (meko, mtproxyl string) {
	t.Helper()
	dir := t.TempDir()
	meko = filepath.Join(dir, "mtpr-simple")
	mtproxyl = filepath.Join(dir, "mtproxyl")
	oldM, oldP, oldD := mekoInstallDir, mtproxylInstallDir, mtddConfigPath
	mekoInstallDir, mtproxylInstallDir = meko, mtproxyl
	mtddConfigPath = filepath.Join(dir, "mtdd", "config.json")
	t.Cleanup(func() { mekoInstallDir, mtproxylInstallDir, mtddConfigPath = oldM, oldP, oldD })
	return meko, mtproxyl
}

// Детект типа ноды по каталогу менеджера; без каталогов — classic.
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
	_ = os.Remove(meko)
	_ = os.Remove(mtproxyl)
	_ = os.WriteFile(mtproxyl, []byte("not a dir"), 0644)
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

// Register шлёт node_type и пиннит lastNodeType только при HTTP 200 —
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

	cfg := &config.NodeConfig{}
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
	cfg := &config.NodeConfig{}
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

// MTProxyL помечается движком: конфиг /opt/mtproxyl/mtdd/config.json —
// mtproxyl_mtdd, иначе mtproxyl. Регистратор, не знающий mtproxyl_mtdd
// (400 про node_type), получает mtproxyl — нода не выпадает из пула.
func TestRegisterReportsMtproxylEngine(t *testing.T) {
	_, mtproxyl := withManagerDirs(t)
	lastNodeType = ""
	nodeTypeLegacyOnly.Store(false)
	t.Cleanup(func() { lastNodeType = ""; nodeTypeLegacyOnly.Store(false) })
	if err := os.Mkdir(mtproxyl, 0755); err != nil {
		t.Fatal(err)
	}
	legacy := false
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p registerPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		sent = append(sent, p.NodeType)
		if legacy && p.NodeType == NodeTypeMTProxyLMtdd {
			http.Error(w, "node_type must be classic, mtproxyl, meko, or mtdd", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	nodeID = "node-t-3"
	client := &http.Client{Timeout: 3 * time.Second}

	cfg.Telemt.ConfigPath = filepath.Join(mtproxyl, "mtproxy", "config.toml")
	if reportedNodeType(cfg) != NodeTypeMTProxyL {
		t.Fatalf("MTProxyL on telemt must report mtproxyl, got %q", reportedNodeType(cfg))
	}
	cfg.Telemt.ConfigPath = filepath.Join(mtproxyl, "mtdd", "config.json")
	if ok, _ := register(client, cfg, "1.2.3.4"); !ok || lastNodeType != NodeTypeMTProxyLMtdd {
		t.Fatalf("MTProxyL on mtdd must register as mtproxyl_mtdd, sent %v, pinned %q", sent, lastNodeType)
	}

	legacy, sent = true, nil
	if ok, _ := register(client, cfg, "1.2.3.4"); !ok {
		t.Fatalf("old registry: fallback registration failed, sent %v", sent)
	}
	if len(sent) != 2 || sent[0] != NodeTypeMTProxyLMtdd || sent[1] != NodeTypeMTProxyL {
		t.Fatalf("expected mtproxyl_mtdd then mtproxyl, sent %v", sent)
	}
	// Дальше — сразу mtproxyl, и тип совпадает с зарегистрированным (без
	// перерегистрации на каждом heartbeat).
	if reportedNodeType(cfg) != NodeTypeMTProxyL || registeredNodeType() != NodeTypeMTProxyL {
		t.Fatalf("fallback must stick: reported %q, registered %q", reportedNodeType(cfg), registeredNodeType())
	}
}
