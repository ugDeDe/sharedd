package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/machineapi"
)

// dc_stats с metrics-отчёта должен осесть в r.dcStats и всплыть в
// telemt.control_api детальной карточки; три "absent"-имени при этом уходят.
func TestDCStatsSurfacesInNodeDetail(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})
	au, err := r.control.User(aliceID)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := r.control.AuthorizeNode("node-a1b2c", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	reportDCStats(t, r, "node-a1b2c", machineapi.DCStats{
		MiddleProxyEnabled:   true,
		GeneratedAtEpochSecs: 1791921120,
		DCs: []machineapi.DCStat{
			{DC: 1, AvailableEndpoints: 3, AliveWriters: 5, FreshAliveWriters: 5, RTTMs: 11.5},
			{DC: 2, AvailableEndpoints: 5, AliveWriters: 12, FreshAliveWriters: 10, RTTMs: 42.5},
		},
	})

	if got := r.dcStats["node-a1b2c"]; len(got.DCs) != 2 || got.DCs[1].RTTMs != 42.5 {
		t.Fatalf("dc_stats must be stored in-memory, got %+v", got)
	}

	rec := cabinetCall(t, r, http.MethodGet, "/api/me/nodes/node-a1b2c/detail", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Telemt struct {
			ControlAPI *struct {
				OK                   bool                `json:"ok"`
				GeneratedAtEpochSecs int64               `json:"generated_at_epoch_secs"`
				MiddleProxyEnabled   bool                `json:"middle_proxy_enabled"`
				DCs                  []machineapi.DCStat `json:"dcs"`
			} `json:"control_api"`
		} `json:"telemt"`
		Absent []string `json:"absent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	ca := resp.Telemt.ControlAPI
	if ca == nil {
		t.Fatalf("control_api must be present, body=%s", rec.Body.String())
	}
	if !ca.OK || !ca.MiddleProxyEnabled || ca.GeneratedAtEpochSecs != 1791921120 {
		t.Fatalf("control_api header wrong: %+v", ca)
	}
	if len(ca.DCs) != 2 || ca.DCs[1].DC != 2 || ca.DCs[1].FreshAliveWriters != 10 {
		t.Fatalf("control_api dcs wrong: %+v", ca.DCs)
	}
	for _, name := range []string{"telemt_control_api", "dc_endpoints", "quality_health"} {
		if containsStr(resp.Absent, name) {
			t.Fatalf("%q must be removed from absent when snapshot present: %v", name, resp.Absent)
		}
	}
}

// Без dc_stats control_api == null, а три имени остаются в absent[].
func TestNodeDetailAbsentControlAPIWithoutSnapshot(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})
	au, _ := r.control.User(aliceID)
	if err := r.control.AuthorizeNode("node-a1b2c", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	rec := cabinetCall(t, r, http.MethodGet, "/api/me/nodes/node-a1b2c/detail", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Telemt struct {
			ControlAPI any `json:"control_api"`
		} `json:"telemt"`
		Absent []string `json:"absent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if resp.Telemt.ControlAPI != nil {
		t.Fatalf("control_api must be omitted, got %v", resp.Telemt.ControlAPI)
	}
	for _, name := range []string{"telemt_control_api", "dc_endpoints", "quality_health"} {
		if !containsStr(resp.Absent, name) {
			t.Fatalf("%q must be absent without snapshot: %v", name, resp.Absent)
		}
	}
}

// reportDCStats шлёт metrics-отчёт с dc_stats (dc2-агент, без measurement_id).
func reportDCStats(t *testing.T, r *Registry, id string, stats machineapi.DCStats) {
	t.Helper()
	payload := HealthReportPayload{
		NodeID: id, Port: 443, CheckedAt: time.Now(),
		MetricsOK: true, DC2OK: true, DCStats: &stats,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dc_stats report for %s: expected 200, got %d (%s)", id, rec.Code, rec.Body.String())
	}
}

func containsStr(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
