package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGlobalpingFreshnessQuarantineDoesNotCountFailure(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.GlobalpingValidityTTL = 15 * time.Minute
	now := time.Now()
	r.state.Candidates["stale"] = &Candidate{
		NodeID: "stale", IP: "1.2.3.4", GlobalpingOK: true,
		LastGlobalpingAt: now.Add(-16 * time.Minute),
	}
	r.state.Candidates["fresh"] = &Candidate{
		NodeID: "fresh", IP: "1.2.3.5", GlobalpingOK: true,
		LastGlobalpingAt: now.Add(-14 * time.Minute),
	}

	r.sweepGlobalpingFreshness(now)
	stale := r.state.Candidates["stale"]
	if stale.Quarantine == nil || !stale.Quarantine.Stale || stale.Quarantine.Attempts != 0 || stale.GlobalpingOK {
		t.Fatalf("stale GP must enter zero-attempt quarantine: %+v", stale)
	}
	if r.state.Candidates["fresh"].Quarantine != nil {
		t.Fatal("fresh GP must not enter quarantine")
	}
}

func TestConfigRequestsGlobalpingForStaleNode(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.GlobalpingValidityTTL = 15 * time.Minute
	r.state.Candidates["node-1"] = &Candidate{NodeID: "node-1", IP: "1.2.3.4"}
	req := httptest.NewRequest(http.MethodGet, "/config", nil)
	req.Header.Set("X-ShareDD-Node-ID", "node-1")
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /config: %d %s", rec.Code, rec.Body.String())
	}
	var got sharedConfigResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.ForceGlobalping {
		t.Fatalf("expected force_globalping, got %+v err=%v", got, err)
	}
}

func TestStaleOnlyQuarantineExpiryDoesNotCreateIPBan(t *testing.T) {
	r := newTestRegistry(t)
	now := time.Now()
	r.state.Candidates["node-1"] = &Candidate{
		NodeID: "node-1", IP: "1.2.3.4", LastHeartbeat: now.Add(-2 * r.cfg.HeartbeatTTL),
		Quarantine: &QuarantineState{EnteredAt: now.Add(-time.Minute), Attempts: 0, Stale: true},
	}
	r.sweepExpired(now)
	if r.state.Candidates["node-1"] != nil {
		t.Fatal("expired node must leave the candidate pool")
	}
	if r.state.Terminated["node-1"] != nil {
		t.Fatal("GP expiry without a verified failure must not create an IP ban")
	}
}
