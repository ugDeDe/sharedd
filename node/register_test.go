package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// V7.9.7: регистратор в карантине после prune отвечает 429 + Retry-After —
// агент обязан считать retryAfter и молчать до дедлайна (гейт в heartbeatLoop).

func TestRegister429CarriesRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":"node was pruned as inactive, re-registration deferred","retry_after_sec":900}`))
	}))
	defer srv.Close()

	nodeID = "n-test-reg"
	cfg := &NodeConfig{}
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
	cfg := &NodeConfig{}
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
