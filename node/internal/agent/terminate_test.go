package agent

import (
	"net/http"
	"net/http/httptest"
	"sharedd/node/internal/config"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func isolateRecovery(t *testing.T) {
	t.Helper()
	old := recovery
	oldGate := gate
	recovery = &recoveryState{}
	gate = &netGate{}
	t.Cleanup(func() { recovery = old; gate = oldGate; drainGpKick() })
}

func TestParseTerminateBody(t *testing.T) {
	te, ok := parseTerminateBody([]byte(`{"terminate":true,"reason":"ip_ban","message":"blocked"}`))
	if !ok || te.Reason != reasonIPBan {
		t.Fatal("valid terminate rejected")
	}
	for _, body := range []string{`{"terminate":false,"reason":"dead"}`, `{"terminate":true}`, `not json`, `{}`} {
		if _, ok := parseTerminateBody([]byte(body)); ok {
			t.Fatalf("accepted %s", body)
		}
	}
}

// Replaces awaitIPChange: the SAME address must recover without a supervisor
// exit, on every episode rather than only sync.Once's first.
func TestSameIPRecoveryRepeatedCycles(t *testing.T) {
	isolateRecovery(t)
	isolateSecret(t)
	nodeSecret = "personal-or-pending"
	nodeID = "same-abc12"
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer personal-or-pending" {
			t.Error("lost personal bearer")
		}
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	for i := 0; i < 10; i++ {
		selfTerminate(cfg, reasonIPBan, "", "8.8.8.8")
		if !recovery.needed() {
			t.Fatal("recovery not armed")
		}
		if ok, _ := tryRegistration(srv.Client(), cfg, "8.8.8.8", true); !ok {
			t.Fatal("same IP recovery failed")
		}
		if recovery.needed() {
			t.Fatal("recovery not cleared")
		}
		if len(gpKick) != 0 {
			t.Fatal("recovery registration kicked GP: storm")
		}
	}
	if calls.Load() != 10 {
		t.Fatalf("requests=%d", calls.Load())
	}
}

// Replaces dead's blocking local wait: concurrent signals return even while
// the registry is unavailable, coalescing into one repeatable recovery episode.
func TestTerminateNonblockingConcurrent(t *testing.T) {
	isolateRecovery(t)
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); selfTerminate(nil, reasonDead, "", "8.8.8.8") }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("terminate blocked")
	}
	if recovery.epoch != 1 || !recovery.needed() {
		t.Fatal("terminate signals not coalesced")
	}
}

func TestRegistrationRetryGate(t *testing.T) {
	isolateRecovery(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	for i := 0; i < 10; i++ {
		tryRegistration(srv.Client(), cfg, "8.8.8.8", true)
	}
	if calls.Load() != 1 {
		t.Fatalf("cooldown bypassed: %d", calls.Load())
	}
	if time.Until(recovery.next) < 899*time.Second {
		t.Fatal("Retry-After lost")
	}
}

func TestRecovery401PreservesSecret(t *testing.T) {
	isolateRecovery(t)
	isolateSecret(t)
	nodeSecret = "pending"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	tryRegistration(srv.Client(), cfg, "8.8.8.8", true)
	if nodeSecret != "pending" {
		t.Fatal("health recovery erased credentials")
	}
}

func TestSelfTerminateNotifiesRetire(t *testing.T) {
	isolateRecovery(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retire" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		calls.Add(1)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	for i := 0; i < 3; i++ {
		selfTerminate(cfg, reasonDead, "", "8.8.8.8")
	}
	if calls.Load() != 0 {
		t.Fatal("terminate performed blocking I/O")
	}
	sendPendingRetire(cfg)
	sendPendingRetire(cfg)
	if calls.Load() != 1 {
		t.Fatalf("retire requests=%d", calls.Load())
	}
}
