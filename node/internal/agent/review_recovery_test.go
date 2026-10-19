package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

func isolateReviewRecovery(t *testing.T) {
	t.Helper()
	isolateRecovery(t)
	isolateSecret(t)
	oldGate, oldCache := gate, sharedConfigCache
	gate, sharedConfigCache = &netGate{}, &SharedConfigCache{}
	t.Cleanup(func() { gate, sharedConfigCache = oldGate, oldCache })
}

func TestMissingCandidateReportRecoversWhileMetricsMuted(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			isolateReviewRecovery(t)
			nodeID, nodeSecret = "muted-abc12", "personal"
			var paths []string
			candidate := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer personal" {
					t.Error("secret changed")
				}
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/register" {
					var p registerPayload
					if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.NodeID != nodeID {
						t.Error("identity changed")
					}
					candidate = true
				} else if !candidate {
					w.WriteHeader(status)
				}
			}))
			defer srv.Close()
			cfg := &config.NodeConfig{}
			cfg.Registry.URL = srv.URL
			if ok, _ := tryRegistration(srv.Client(), cfg, "8.8.8.8", false); !ok {
				t.Fatal("initial register")
			}
			drainGpKick()
			candidate = false
			gate.mMuted = true
			oldCheck := runGlobalpingCheck
			t.Cleanup(func() { runGlobalpingCheck = oldCheck })
			checks := 0
			runGlobalpingCheck = func(*config.NodeConfig, string, string) HealthReport {
				checks++
				return HealthReport{NodeID: nodeID, IP: "8.8.8.8", GlobalpingOK: true, GlobalpingMeasurementID: fmt.Sprintf("fresh-%d", checks), CheckedAt: time.Now()}
			}
			// No metricsLoop/dead notification is running: only GP can rescue it.
			globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
			if !recovery.needed() {
				t.Fatal("missing candidate did not arm recovery")
			}
			globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
			if got := fmt.Sprint(paths); got != "[/register /report /register /report]" {
				t.Fatal(got)
			}
			if recovery.needed() || len(gpKick) != 0 || nodeSecret != "personal" {
				t.Fatal("recovery incomplete or kicked GP")
			}
		})
	}
}

func TestPendingBootstrapRetriesWithoutSharedConfig(t *testing.T) {
	isolateReviewRecovery(t)
	nodeSecret = "pending"
	status, calls := http.StatusForbidden, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer pending" {
			t.Error("bootstrap discarded pending secret")
		}
		w.WriteHeader(status)
		if status == http.StatusForbidden {
			_, _ = w.Write([]byte(`{"terminate":true,"reason":"ip_ban"}`))
		}
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	ipr := &ipResolver{fixed: "8.8.8.8"}
	retryBootstrapRegistration(srv.Client(), cfg, ipr)
	if !recovery.active || calls != 1 {
		t.Fatal("initial refusal not observed")
	}
	if rep := RunGlobalpingCheck(cfg, nodeID, "8.8.8.8"); rep.Error == "" {
		t.Fatal("empty shared cache must not invent GP evidence")
	}
	for i := 0; i < 5; i++ {
		retryBootstrapRegistration(srv.Client(), cfg, ipr)
	}
	if calls != 1 {
		t.Fatal("bootstrap ignored backoff")
	}
	// The same secret must also survive an authentication refusal during retry.
	status = http.StatusUnauthorized
	recovery.next = time.Time{}
	retryBootstrapRegistration(srv.Client(), cfg, ipr)
	if calls != 2 || nodeSecret != "pending" {
		t.Fatal("401 lost bootstrap credentials")
	}
	status = http.StatusOK
	recovery.next = time.Time{}
	gate.mMuted = true // missing metrics must not prevent the pending claim
	retryBootstrapRegistration(srv.Client(), cfg, ipr)
	if calls != 3 || recovery.needed() {
		t.Fatal("bootstrap never retried after cooldown")
	}
	selfTerminate(nil, reasonIPBan, "", "8.8.8.8")
	retryBootstrapRegistration(srv.Client(), cfg, ipr)
	if calls != 3 {
		t.Fatal("ordinary recovery reused bootstrap exception")
	}
	if ok, _ := tryRegistration(srv.Client(), cfg, "8.8.8.8", false); ok {
		t.Fatal("ordinary recovery skipped completed GP")
	}
}

func TestRegistration429SilencesAllCallers(t *testing.T) {
	for _, fromGP := range []bool{false, true} {
		t.Run(fmt.Sprint(fromGP), func(t *testing.T) {
			isolateReviewRecovery(t)
			requests, checks := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				w.Header().Set("Retry-After", "900")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer srv.Close()
			cfg := &config.NodeConfig{}
			cfg.Registry.URL = srv.URL
			oldCheck := runGlobalpingCheck
			t.Cleanup(func() { runGlobalpingCheck = oldCheck })
			runGlobalpingCheck = func(*config.NodeConfig, string, string) HealthReport {
				checks++
				return HealthReport{GlobalpingOK: true, GlobalpingMeasurementID: fmt.Sprint(checks)}
			}
			if fromGP {
				globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
			} else {
				tryRegistration(srv.Client(), cfg, "8.8.8.8", false)
			}
			if !gate.silent() || !gate.banActive() || time.Until(recovery.next) < 899*time.Second {
				t.Fatal("common 429 failed to silence registry traffic")
			}
			until := gate.banUntil
			gate.noteBan(time.Now().Add(time.Second))
			if gate.banUntil != until {
				t.Fatal("shorter wait reduced ban")
			}
			for i := 0; i < 3; i++ {
				globalpingIteration(cfg, &ipResolver{fixed: "8.8.8.8"})
				tryRegistration(srv.Client(), cfg, "8.8.8.8", false)
				retryBootstrapRegistration(srv.Client(), cfg, &ipResolver{fixed: "8.8.8.8"})
			}
			if checks < 3 || requests != 1 {
				t.Fatalf("GP stopped or registry traffic leaked: checks=%d requests=%d", checks, requests)
			}
		})
	}
}

func TestSuccessfulConfigEndsBootstrapException(t *testing.T) {
	isolateReviewRecovery(t)
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/config" {
			t.Error("active recovery attempted ordinary register")
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"tls_domain":"front.example.com"}`))
		}
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	if _, err := fetchSharedConfig(cfg); err == nil || !recovery.bootstrapNeeded() {
		t.Fatal("failed config ended bootstrap")
	}
	status = http.StatusOK
	if _, err := fetchSharedConfig(cfg); err != nil || recovery.bootstrapNeeded() {
		t.Fatal("successful config did not end bootstrap")
	}
	selfTerminate(nil, reasonIPBan, "", "8.8.8.8")
	retryBootstrapRegistration(srv.Client(), cfg, &ipResolver{fixed: "8.8.8.8"})
	if ok, _ := tryRegistration(srv.Client(), cfg, "8.8.8.8", false); ok {
		t.Fatal("configured health recovery bypassed GP")
	}
}

func TestRegistration429KeepsLongerPruneDeadline(t *testing.T) {
	isolateReviewRecovery(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	cfg := &config.NodeConfig{}
	cfg.Registry.URL = srv.URL
	until := time.Now().Add(time.Hour)
	gate.noteBan(until)
	// A response from an already in-flight call must not shorten a prior ban.
	register(srv.Client(), cfg, "8.8.8.8")
	if gate.banUntil != until {
		t.Fatal("shorter server Retry-After reduced silence")
	}
	tryRegistration(srv.Client(), cfg, "8.8.8.8", true)
	if recovery.next.Before(until) {
		t.Fatal("registration gate lost longer prune deadline")
	}
}

func TestReportAuthRefusalDoesNotBecomeMissingCandidate(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			isolateReviewRecovery(t)
			nodeSecret = "personal"
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()
			cfg := &config.NodeConfig{}
			cfg.Registry.URL = srv.URL
			err := SendReport(cfg, HealthReport{NodeID: nodeID})
			var te *TerminatedError
			if err == nil || errors.As(err, &te) || nodeSecret != "personal" {
				t.Fatal("auth refusal changed recovery authority or credentials")
			}
		})
	}
}
