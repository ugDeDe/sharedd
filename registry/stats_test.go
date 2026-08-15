package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Публичная страница статистики ноды — та же детальная статистика,
// но без чувствительных данных: IP замаскирован (первые два октета),
// measurement id Globalping (по нему публичное API Globalping открыло бы
// target = полный IP ноды) не покидает сервер ни в одном поле, тексты
// событий санитизируются.

func TestMaskPublicIP(t *testing.T) {
	cases := map[string]string{
		"203.0.113.7":         "203.0.x.x",
		"1.2.3.4":             "1.2.x.x",
		"2001:db8:85a3::8a2e": "2001:db8:…",
		"::1":                 "…", // первая группа пустая — ничего не светим
		"garbage":             "x.x.x.x",
		"":                    "x.x.x.x",
	}
	for in, want := range cases {
		if got := maskPublicIP(in); got != want {
			t.Errorf("maskPublicIP(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizePublicDetail(t *testing.T) {
	in := "re-registered, ip changed 203.0.113.7 -> 198.51.100.9"
	got := sanitizePublicDetail(in)
	if got != "re-registered, ip changed 203.0.x.x -> 198.51.x.x" {
		t.Errorf("ipv4 must be partially masked, got %q", got)
	}
	in = "verified ratio 0.25 (measurement m-SECRET1); node self-reported ok"
	got = sanitizePublicDetail(in)
	if strings.Contains(got, "m-SECRET1") {
		t.Errorf("measurement id must be stripped, got %q", got)
	}
	if !strings.Contains(got, "verified ratio 0.25") {
		t.Errorf("human-readable part must survive, got %q", got)
	}
	if got := sanitizePublicDetail(""); got != "" {
		t.Error("empty stays empty")
	}
}

// statsFixtureRegistry — нода с полным букетом «секретов» в полях.
func statsFixtureRegistry(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	now := time.Now()
	c := &Candidate{
		NodeID: "node-abcdef1234567890", IP: "203.0.113.7", Port: 443,
		RegisteredAt: now.Add(-time.Hour), LastHeartbeat: now,
		Healthy: true, GlobalpingOK: true, MetricsOK: true, MetricsHealthy: true,
		LastReportAt:            now,
		GlobalpingMeasurementID: "m-SECRET1",
		MetricsSnapshot:         map[string]float64{uniqueIPsMetric: 41, writersMetric: 12},
		TCPHist:                 []TCPPoint{{At: now, OK: true}},
		GPHist:                  []GPPoint{{At: now, OK: false, Ratio: 0.25, ProbesOK: 1, ProbesTotal: 4}},
		ReportHist:              []ReportPoint{{At: now, MetricsOK: true, Clients: 41, Writers: 12}},
		GPLast: &GPDetail{
			At: now, MeasurementID: "m-SECRET1", OK: false, Ratio: 0.25,
			ProbesOK: 1, ProbesTotal: 4,
			Probes: []GPProbeLine{{Country: "RU", City: "Moscow", Network: "MTS", ASN: 8359, OK: false, Status: "failed"}},
		},
	}
	r.state.Candidates[c.NodeID] = c
	r.state.Assignments = map[string]string{"d1.example.com": c.NodeID}
	r.mu.Lock()
	r.addEventLocked(Event{Type: EventNodeRegistered, NodeID: c.NodeID, IP: c.IP,
		Detail: "re-registered, ip changed 203.0.113.7 -> 198.51.100.9"})
	r.addEventLocked(Event{Type: EventGlobalpingBlocked, NodeID: c.NodeID, IP: c.IP,
		Detail: "verified ratio 0.25 (measurement m-SECRET1)"})
	r.mu.Unlock()
	return r
}

func TestStatsNodeSanitization(t *testing.T) {
	r := statsFixtureRegistry(t)

	req := httptest.NewRequest(http.MethodGet, "/statistics/api/node?id=node-abcdef1234567890", nil)
	rec := httptest.NewRecorder()
	r.handleStatsNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	// ── полный IP не утёк НИГДЕ (карточка, gp_last, события, истории) ──
	if strings.Contains(body, "203.0.113.7") {
		t.Fatal("full node IP must not appear anywhere in the public payload")
	}
	if strings.Contains(body, "198.51.100.9") {
		t.Fatal("old IP from event details must not appear either")
	}
	if !strings.Contains(body, "203.0.x.x") {
		t.Fatal("masked IP (first two octets) must be present")
	}
	// ── measurement id не утёк ни в каком виде ──
	if strings.Contains(body, "m-SECRET1") {
		t.Fatal("measurement id must not appear anywhere (it exposes the target IP)")
	}
	if strings.Contains(body, "measurement_id") {
		t.Fatal("even the measurement_id FIELD must be absent from public JSON")
	}

	// структурные проверки
	var resp struct {
		Node   map[string]any   `json:"node"`
		GPLast *map[string]any  `json:"gp_last"`
		Events []map[string]any `json:"events"`
		HC     map[string]int   `json:"hc"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Node["ip"] != "203.0.x.x" {
		t.Errorf("node.ip must be masked, got %v", resp.Node["ip"])
	}
	if resp.Node["is_master"] != true {
		t.Errorf("node must show as master, got %v", resp.Node["is_master"])
	}
	if resp.GPLast == nil {
		t.Fatal("gp_last must be present (sanitized)")
	}
	if _, leak := (*resp.GPLast)["measurement_id"]; leak {
		t.Error("gp_last.measurement_id must be stripped")
	}
	if len(resp.Events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(resp.Events))
	}
	for _, ev := range resp.Events {
		if _, leak := ev["ip"]; leak {
			t.Errorf("public event must not carry ip field: %v", ev)
		}
	}
	if resp.HC["fail_threshold"] != 3 || resp.HC["recover_threshold"] != 2 || resp.HC["freshness_min"] != 15 {
		t.Errorf("hc thresholds must be exposed for UI hints, got %v", resp.HC)
	}
}

func TestStatsNodeResolveSuffixAnd404(t *testing.T) {
	r := statsFixtureRegistry(t)

	// hex-суффикс без «node-» тоже резолвится
	req := httptest.NewRequest(http.MethodGet, "/statistics/api/node?id=abcdef1234567890", nil)
	rec := httptest.NewRecorder()
	r.handleStatsNode(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hex-suffix must resolve, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/statistics/api/node?id=ghost", nil)
	rec = httptest.NewRecorder()
	r.handleStatsNode(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node must yield 404, got %d", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/statistics/api/node", nil)
	rec = httptest.NewRecorder()
	r.handleStatsNode(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id must yield 400, got %d", rec.Code)
	}
}

func TestStatsListSanitization(t *testing.T) {
	r := statsFixtureRegistry(t)

	req := httptest.NewRequest(http.MethodGet, "/statistics/api/list", nil)
	rec := httptest.NewRecorder()
	r.handleStatsList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "203.0.113.7") || strings.Contains(body, "m-SECRET1") {
		t.Fatal("list payload must be sanitized as well")
	}
	if !strings.Contains(body, "node-abcdef1234567890") || !strings.Contains(body, "203.0.x.x") {
		t.Fatalf("list must contain node id and masked ip, got %s", body)
	}
}

// #1: публичный список несёт обратный отсчёт принудительной
// ротации мастера (master_ttl_remaining_sec), и только для мастеров.
func TestStatsListMasterTTLCountdown(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true // /statistics монтируется только при включённой панели
	now := time.Now()
	r.cfg.Rotation.MasterTTLMinutes = new(int)
	*r.cfg.Rotation.MasterTTLMinutes = 30
	r.mu.Lock()
	r.state.Candidates["node-m"] = &Candidate{
		NodeID: "node-m", IP: "10.0.0.9", Port: 443, RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now, Healthy: true, MetricsHealthy: true, GlobalpingOK: true,
	}
	r.state.Candidates["node-n"] = &Candidate{
		NodeID: "node-n", IP: "10.0.0.10", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now,
	}
	r.state.Assignments = map[string]string{"d1.example.com": "node-m"}
	r.state.AssignmentsSince = map[string]time.Time{"d1.example.com": now.Add(-12 * time.Minute)}
	r.mu.Unlock()

	mux := r.buildMux()
	req := httptest.NewRequest(http.MethodGet, "/statistics/api/list", nil)
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("list: %d", resp.Code)
	}
	var out struct {
		Nodes []struct {
			NodeID       string `json:"node_id"`
			IsMaster     bool   `json:"is_master"`
			TTLRemaining *int64 `json:"master_ttl_remaining_sec"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var m, n *struct {
		NodeID       string `json:"node_id"`
		IsMaster     bool   `json:"is_master"`
		TTLRemaining *int64 `json:"master_ttl_remaining_sec"`
	}
	for i := range out.Nodes {
		switch out.Nodes[i].NodeID {
		case "node-m":
			m = &out.Nodes[i]
		case "node-n":
			n = &out.Nodes[i]
		}
	}
	if m == nil || !m.IsMaster {
		t.Fatalf("node-m must be the master: %+v", out.Nodes)
	}
	if m.TTLRemaining == nil {
		t.Fatal("master must carry master_ttl_remaining_sec")
	}
	// 30 мин лимита минус 12 мин стинта = 1080 c (±10 c на прогон)
	if *m.TTLRemaining < 1070 || *m.TTLRemaining > 1085 {
		t.Fatalf("remaining mismatch: %d", *m.TTLRemaining)
	}
	if n == nil || n.TTLRemaining != nil {
		t.Fatalf("non-master must not carry ttl remaining: %+v", n)
	}
}
