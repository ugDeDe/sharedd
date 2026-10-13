package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// reportHostMetrics шлёт metrics-отчёт (без measurement_id) со снапшотом host
// метрик и накопительных счётчиков трафика.
func reportHostMetrics(t *testing.T, r *Registry, id string, snap map[string]float64, at time.Time) {
	t.Helper()
	payload := HealthReportPayload{
		NodeID: id, Port: 443, CheckedAt: at, MetricsOK: true, MetricsSnapshot: snap,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("host-metrics report for %s: expected 200, got %d (%s)", id, rec.Code, rec.Body.String())
	}
}

// TestNodeDetailHostingSeries — host-метрики отчёта попадают в
// hosting.cpu_ram_series, а net_series — это положительные дельты traffic,
// уже записанные в history (без повторного дифа).
func TestNodeDetailHostingSeries(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	r.db = openHistoryDB(t.TempDir() + "/hist.db")
	if r.db == nil {
		t.Fatal("history db must open")
	}
	defer r.db.Close()

	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})
	au, err := r.control.User(aliceID)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := r.control.AuthorizeNode("node-a1b2c", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	base := time.Now().Add(-time.Hour)
	// baseline: счётчики зафиксированы (дельта 0), host-сэмпл #1.
	reportHostMetrics(t, r, "node-a1b2c", map[string]float64{
		trafficUsersMetric:   1,
		trafficIngressMetric: 1000,
		trafficEgressMetric:  2000,
		hostCPUMetric:        40,
		hostRAMMetric:        70,
	}, base)
	// дельта: счётчики выросли, те же users, host-сэмпл #2.
	reportHostMetrics(t, r, "node-a1b2c", map[string]float64{
		trafficUsersMetric:   1,
		trafficIngressMetric: 1500,
		trafficEgressMetric:  2600,
		hostCPUMetric:        55,
		hostRAMMetric:        80,
	}, base.Add(time.Minute))

	rec := cabinetCall(t, r, http.MethodGet, "/api/me/nodes/node-a1b2c/detail", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Hosting struct {
			CPUPercent   *float64          `json:"cpu_percent"`
			RAMPercent   *float64          `json:"ram_percent"`
			CPURAMSeries []nodeCPURAMPoint `json:"cpu_ram_series"`
			NetSeries    []nodeNetPoint    `json:"net_series"`
		} `json:"hosting"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if resp.Hosting.CPUPercent == nil || *resp.Hosting.CPUPercent != 55 {
		t.Fatalf("hosting.cpu_percent must reflect last snapshot: %v", resp.Hosting.CPUPercent)
	}
	if resp.Hosting.RAMPercent == nil || *resp.Hosting.RAMPercent != 80 {
		t.Fatalf("hosting.ram_percent must reflect last snapshot: %v", resp.Hosting.RAMPercent)
	}
	if len(resp.Hosting.CPURAMSeries) != 2 {
		t.Fatalf("cpu_ram_series must hold both samples, got %+v", resp.Hosting.CPURAMSeries)
	}
	s := resp.Hosting.CPURAMSeries
	if s[0].At.After(s[1].At) {
		t.Fatalf("cpu_ram_series must be ordered by time: %+v", s)
	}
	if s[0].CPUPercent == nil || *s[0].CPUPercent != 40 || s[1].CPUPercent == nil || *s[1].CPUPercent != 55 {
		t.Fatalf("cpu series values wrong: %+v", s)
	}
	if s[0].RAMPercent == nil || *s[0].RAMPercent != 70 || s[1].RAMPercent == nil || *s[1].RAMPercent != 80 {
		t.Fatalf("ram series values wrong: %+v", s)
	}
	if len(resp.Hosting.NetSeries) != 1 {
		t.Fatalf("net_series must derive one delta row from traffic, got %+v", resp.Hosting.NetSeries)
	}
	n := resp.Hosting.NetSeries[0]
	if n.IngressBytes != 500 || n.EgressBytes != 600 {
		t.Fatalf("net_series must carry traffic deltas 500/600, got %+v", n)
	}
}

// TestNodeDetailHostingSeriesEmptyNotNil — без данных серии — пустые массивы.
func TestNodeDetailHostingSeriesEmptyNotNil(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	aliceID := cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")

	r.db = openHistoryDB(t.TempDir() + "/hist.db")
	if r.db == nil {
		t.Fatal("history db must open")
	}
	defer r.db.Close()

	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})
	au, _ := r.control.User(aliceID)
	if err := r.control.AuthorizeNode("node-a1b2c", au.User); err != nil {
		t.Fatalf("authorize: %v", err)
	}

	rec := cabinetCall(t, r, http.MethodGet, "/api/me/nodes/node-a1b2c/detail", cookieMap(sess, csrf), csrf, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	// Оба поля должны присутствовать как массивы (не null).
	if !strings.Contains(rec.Body.String(), `"cpu_ram_series":[]`) ||
		!strings.Contains(rec.Body.String(), `"net_series":[]`) {
		t.Fatalf("empty series must be [] not null: %s", rec.Body.String())
	}
}
