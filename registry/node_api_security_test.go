package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func securedTestRegistry(t *testing.T) *Registry {
	r := newTestRegistry(t)
	r.cfg.Security.NodeToken = "shared-secret"
	return r
}

func authenticatedRequest(method, target, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer shared-secret")
	return req
}

func TestNodeAPIRoutesRequireBearerToken(t *testing.T) {
	mux := securedTestRegistry(t).buildMux()
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/register"},
		{http.MethodPost, "/heartbeat"},
		{http.MethodPost, "/report"},
		{http.MethodPost, "/retire"},
		{http.MethodGet, "/config"},
	} {
		for _, auth := range []string{"", "Bearer wrong", "Basic shared-secret"} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			if auth != "" {
				req.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s auth %q: got %d, want 401", tc.method, tc.path, auth, rec.Code)
			}
		}
	}
}

func TestNodeAPIValidation(t *testing.T) {
	validID := "node-a1b2c"
	for _, tc := range []struct {
		name string
		body registerRequest
	}{
		{"valid", registerRequest{NodeID: validID, IP: "8.8.8.8", NodeType: "classic"}},
		{"bad id", registerRequest{NodeID: "name-is-too-long-a1b2c", IP: "8.8.8.8", NodeType: "classic"}},
		{"private ip", registerRequest{NodeID: validID, IP: "10.0.0.1", NodeType: "classic"}},
		{"documentation ip", registerRequest{NodeID: validID, IP: "203.0.113.1", NodeType: "classic"}},
		{"ipv6", registerRequest{NodeID: validID, IP: "2001:4860:4860::8888", NodeType: "classic"}},
		{"bad type", registerRequest{NodeID: validID, IP: "8.8.8.8", NodeType: "root"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateRegisterRequest(tc.body)
			if tc.name == "valid" && err != nil {
				t.Fatalf("valid registration rejected: %v", err)
			}
			if tc.name != "valid" && err == nil {
				t.Fatal("invalid registration accepted")
			}
		})
	}

	r := securedTestRegistry(t)
	mux := r.buildMux()
	req := authenticatedRequest(http.MethodPost, "/register", `{"node_id":"name-is-too-long-a1b2c","ip":"8.8.8.8","node_type":"classic"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("route accepted invalid node_id: status=%d", rec.Code)
	}
	req = authenticatedRequest(http.MethodPost, "/register", `{"node_id":"node-a1b2c","ip":"8.8.8.8","node_type":"classic"}`)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("route rejected valid registration: status=%d body=%s", rec.Code, rec.Body.String())
	}

	now := time.Now()
	valid := HealthReportPayload{NodeID: validID, IP: "8.8.4.4", Port: 443, CheckedAt: now}
	if err := validateHealthReport(valid, now); err != nil {
		t.Fatalf("valid report rejected: %v", err)
	}
	for name, mutate := range map[string]func(*HealthReportPayload){
		"zero port": func(p *HealthReportPayload) { p.Port = 0 },
		"high port": func(p *HealthReportPayload) { p.Port = 65536 },
		"stale":     func(p *HealthReportPayload) { p.CheckedAt = now.Add(-maxReportAge - time.Second) },
		"future":    func(p *HealthReportPayload) { p.CheckedAt = now.Add(maxReportFutureSkew + time.Second) },
		"large snapshot": func(p *HealthReportPayload) {
			p.MetricsSnapshot = make(map[string]float64, maxMetricsSnapshotSize+1)
			for i := 0; i <= maxMetricsSnapshotSize; i++ {
				p.MetricsSnapshot[string(rune(i))] = float64(i)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			payload := valid
			mutate(&payload)
			if err := validateHealthReport(payload, now); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
}

func TestNodeIDFormat(t *testing.T) {
	for _, id := range []string{"a-abc12", "helsinki-z9x8w", "node_01-00000", "A.B-cdef0"} {
		if err := validateNodeID(id); err != nil {
			t.Errorf("valid node ID %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{"-abc12", "node-ab12", "abcdefghijk-abc12", "node-ABC12", "node-abc!2", "node-0123456789abcdef"} {
		if err := validateNodeID(id); err == nil {
			t.Errorf("invalid node ID %q accepted", id)
		}
	}
}

func TestNodeAPIMaxBodyAndConfigNoStore(t *testing.T) {
	mux := securedTestRegistry(t).buildMux()
	req := authenticatedRequest(http.MethodPost, "/register", strings.Repeat(" ", maxNodeJSONBytes+1))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", rec.Code)
	}

	req = authenticatedRequest(http.MethodGet, "/config", "")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("config response: status=%d cache-control=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}
