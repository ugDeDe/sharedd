package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sharedd/node/internal/config"
	"sync/atomic"
	"testing"
	"time"
)

func TestGPPollFinite(t *testing.T) {
	for _, stalled := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "hung API"}[stalled], func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stalled {
					<-r.Context().Done()
					return
				}
				_, _ = w.Write([]byte(`{"status":"in-progress"}`))
			}))
			defer srv.Close()
			gp := NewGlobalpingChecker(srv.URL)
			start := time.Now()
			if _, err := gp.pollUntilDone("pending", 20*time.Millisecond); err == nil {
				t.Fatal("pending measurement counted as complete")
			}
			if time.Since(start) > time.Second {
				t.Fatal("poll exceeded finite budget")
			}
		})
	}
}

func TestGPFailedOrUnavailableNeverRegisters(t *testing.T) {
	isolateRecovery(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	old := runGlobalpingCheck
	defer func() { runGlobalpingCheck = old }()
	for _, rep := range []HealthReport{
		{Error: "GP API outage", GlobalpingMeasurementID: "created-only"},
		{GlobalpingOK: false, GlobalpingMeasurementID: "blocked"},
		{GlobalpingOK: true},
	} {
		runGlobalpingCheck = func(*config.NodeConfig, string, string) HealthReport { return rep }
		globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
	}
	if requests.Load() != 0 {
		t.Fatalf("unsuccessful GP registered/reported: %d", requests.Load())
	}
	if rep := RunGlobalpingCheck(nil, nodeID, ""); rep.Error == "" {
		t.Fatal("missing config must be a finite error")
	}
}

func TestRegistrationNetworkBackoff(t *testing.T) {
	isolateRecovery(t)
	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, context.DeadlineExceeded })}
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = "http://registry.invalid"
	for i := 0; i < 5; i++ {
		tryRegistration(client, cfg, "8.8.8.8", true)
	}
	if calls.Load() != 1 || time.Until(recovery.next) < 4*time.Second {
		t.Fatal("network error retried without backoff")
	}
	if gate.banActive() {
		t.Fatal("network backoff became a prune ban")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestConcurrentTerminateDuringRegistration(t *testing.T) {
	isolateRecovery(t)
	entered, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	done := make(chan struct{})
	go func() { tryRegistration(srv.Client(), cfg, "8.8.8.8", true); close(done) }()
	<-entered
	// No second request and no blocking on the in-flight network call.
	if ok, _ := tryRegistration(srv.Client(), cfg, "8.8.8.8", true); ok {
		t.Error("duplicate register succeeded")
	}
	selfTerminate(nil, reasonIPBan, "", "8.8.8.8")
	close(release)
	<-done
	if !recovery.needed() {
		t.Fatal("late registration cleared a newer health episode")
	}
}

func TestInitial403DoesNotBlockGP(t *testing.T) {
	isolateRecovery(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"terminate":true,"reason":"ip_ban"}`))
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	tryRegistration(srv.Client(), cfg, "8.8.8.8", false)
	old := runGlobalpingCheck
	defer func() { runGlobalpingCheck = old }()
	var checks atomic.Int32
	runGlobalpingCheck = func(*config.NodeConfig, string, string) HealthReport {
		checks.Add(1)
		return HealthReport{Error: "API unavailable"}
	}
	oldGate := gate
	gate = &netGate{mMuted: true, banUntil: time.Now().Add(time.Hour)}
	defer func() { gate = oldGate }()
	for i := 0; i < 3; i++ {
		globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
	}
	if checks.Load() != 3 {
		t.Fatal("blocked health state stopped GP")
	}
}

func TestCompletedGPRegistersBeforeReportWithoutKick(t *testing.T) {
	isolateRecovery(t)
	drainGpKick()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	old := runGlobalpingCheck
	defer func() { runGlobalpingCheck = old }()
	runGlobalpingCheck = func(*config.NodeConfig, string, string) HealthReport {
		return HealthReport{NodeID: nodeID, IP: "8.8.8.8", GlobalpingOK: true, GlobalpingMeasurementID: "completed", CheckedAt: time.Now()}
	}
	selfTerminate(nil, reasonIPBan, "", "8.8.8.8")
	globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
	if len(paths) != 2 || paths[0] != "/register" || paths[1] != "/report" {
		t.Fatalf("paths=%v", paths)
	}
	if len(gpKick) != 0 {
		t.Fatal("register->GP storm")
	}
	if recovery.registeredIP != "8.8.8.8" {
		t.Fatal("heartbeat would re-register the GP-recovered IP and kick again")
	}
	// A repeated delivery of the same completed measurement cannot open a
	// second recovery registration after another terminate signal.
	selfTerminate(nil, reasonIPBan, "", "8.8.8.8")
	globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
	if len(paths) != 2 {
		t.Fatalf("measurement reused for registration: %v", paths)
	}
}
