package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

// Регистратор в карантине после prune отвечает 429 + Retry-After —
// агент обязан считать retryAfter и молчать до дедлайна (гейт в heartbeatLoop).

func TestRegister429CarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"node was pruned as inactive, re-registration deferred","retry_after_sec":900}`))
	}))
	defer srv.Close()

	nodeID = "n-test-reg"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL

	ok, retryAfter := register(srv.Client(), cfg, "203.0.113.9")
	if ok {
		t.Fatal("429 register must not report success")
	}
	if retryAfter != 15*time.Minute {
		t.Fatalf("Retry-After: 900 must parse to 15m, got %v", retryAfter)
	}
}

func TestRegister200HasNoRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	nodeID = "n-test-ok"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL

	ok, retryAfter := register(srv.Client(), cfg, "203.0.113.9")
	if !ok || retryAfter != 0 {
		t.Fatalf("200 register must be (true, 0), got (%v, %v)", ok, retryAfter)
	}
}

func TestParseRetryAfterSeconds(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"45": 45 * time.Second, " 900 ": 15 * time.Minute,
		"": 0, "garbage": 0, "-5": 0,
	} {
		if got := parseRetryAfterSeconds(in); got != want {
			t.Fatalf("parseRetryAfter(%q): want %v, got %v", in, want, got)
		}
	}
}

// ── node_secret (invitations-migration-contract.md §1) ────────────

// isolateSecret — изолировать глобальное состояние секретов теста.
func isolateSecret(t *testing.T) {
	t.Helper()
	oldFile, oldSecret := nodeSecretFile, nodeSecret
	nodeSecretFile = t.TempDir() + "/node_secret"
	nodeSecret = ""
	t.Cleanup(func() { nodeSecretFile, nodeSecret = oldFile, oldSecret })
}

// Выданный секрет сохраняется (0600) и сразу используется как Bearer;
// старый реестр без секрета в ответе не ломает агента.
func TestRegisterStoresIssuedSecret(t *testing.T) {
	isolateSecret(t)
	var gotBearer []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		gotBearer = append(gotBearer, strings.TrimPrefix(auth, "Bearer "))
		if auth == "Bearer tok" {
			_, _ = w.Write([]byte(`{"node_secret":"issued-secret-1"}`))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	nodeID = "n-secret-1"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	cfg.Registry.Token = "tok"

	if ok, _ := register(srv.Client(), cfg, "203.0.113.9"); !ok {
		t.Fatal("register must succeed")
	}
	if nodeSecret != "issued-secret-1" {
		t.Fatalf("secret not adopted: %q", nodeSecret)
	}
	b, err := os.ReadFile(nodeSecretFile)
	if err != nil || strings.TrimSpace(string(b)) != "issued-secret-1" {
		t.Fatalf("secret not persisted: %q err=%v", b, err)
	}
	if fi, err := os.Stat(nodeSecretFile); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("secret file mode: %v err=%v", fi, err)
	}
	// Вторая регистрация уже с секретом; реестр без секрета в ответе
	// (старая версия) — хранимое значение остаётся.
	if ok, _ := register(srv.Client(), cfg, "203.0.113.9"); !ok {
		t.Fatal("re-register must succeed")
	}
	if len(gotBearer) != 2 || gotBearer[0] != "tok" || gotBearer[1] != "issued-secret-1" {
		t.Fatalf("bearers=%v", gotBearer)
	}
	if nodeSecret != "issued-secret-1" {
		t.Fatalf("secret must survive old-registry response, got %q", nodeSecret)
	}
}

// 401 с просроченным секретом: секрет сбрасывается, следующая попытка
// идёт с общим токеном и перевыпускает секрет (сценарий OFF-миграции).
func TestRegister401ClearsStaleSecret(t *testing.T) {
	isolateSecret(t)
	var gotBearer []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		gotBearer = append(gotBearer, strings.TrimPrefix(auth, "Bearer "))
		if auth != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"node_secret":"fresh-secret"}`))
	}))
	defer srv.Close()

	nodeID = "n-secret-2"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	cfg.Registry.Token = "tok"
	nodeSecret = "stale-secret"
	if err := os.WriteFile(nodeSecretFile, []byte("stale-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}

	ok, _ := register(srv.Client(), cfg, "203.0.113.9") // 401 → сброс
	if ok {
		t.Fatal("401 register must not report success")
	}
	if nodeSecret != "" {
		t.Fatalf("stale secret must be cleared, got %q", nodeSecret)
	}
	if _, err := os.Stat(nodeSecretFile); !os.IsNotExist(err) {
		t.Fatalf("stale secret file must be removed, err=%v", err)
	}
	if ok, _ := register(srv.Client(), cfg, "203.0.113.9"); !ok { // перевыпуск
		t.Fatal("re-register with shared token must succeed")
	}
	if nodeSecret != "fresh-secret" {
		t.Fatalf("fresh secret not stored, got %q", nodeSecret)
	}
}

// 403 node_id_mismatch: секрет от другой ноды (node_id сменился при
// переустановке) — сбрасывается, перевыпуск идёт с общим токеном.
func TestRegisterNodeIDMismatchClearsForeignSecret(t *testing.T) {
	isolateSecret(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"node_id_mismatch","message":"bearer node secret belongs to another node"}`))
			return
		}
		_, _ = w.Write([]byte(`{"node_secret":"own-secret"}`))
	}))
	defer srv.Close()

	nodeID = "n-secret-3"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	cfg.Registry.Token = "tok"
	nodeSecret = "foreign-secret"
	if err := os.WriteFile(nodeSecretFile, []byte("foreign-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := register(srv.Client(), cfg, "203.0.113.9"); ok {
		t.Fatal("403 register must not report success")
	}
	if nodeSecret != "" {
		t.Fatalf("foreign secret must be cleared, got %q", nodeSecret)
	}
	if _, err := os.Stat(nodeSecretFile); !os.IsNotExist(err) {
		t.Fatalf("foreign secret file must be removed, err=%v", err)
	}
	if ok, _ := register(srv.Client(), cfg, "203.0.113.9"); !ok || nodeSecret != "own-secret" {
		t.Fatalf("re-register must issue an own secret: ok=%v secret=%q", ok, nodeSecret)
	}
}

// Прочие 403 (терминальный kill и т.п.) секрет не трогают.
func TestRegisterOther403KeepsSecret(t *testing.T) {
	isolateSecret(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"pending_secret"}`))
	}))
	defer srv.Close()
	nodeID = "n-secret-4"
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	cfg.Registry.Token = "tok"
	nodeSecret = "my-secret"
	register(srv.Client(), cfg, "203.0.113.9")
	if nodeSecret != "my-secret" {
		t.Fatal("only node_id_mismatch may clear the secret")
	}
}
