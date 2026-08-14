package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// pushRing — кольцевой буфер: длина ограничена, порядок хвоста сохраняется.
func TestPushRing(t *testing.T) {
	var s []int
	for i := 0; i < tcpHistCap+40; i++ {
		s = pushRing(s, i, tcpHistCap)
	}
	if len(s) != tcpHistCap {
		t.Fatalf("ring must be capped at %d, got %d", tcpHistCap, len(s))
	}
	if s[0] != 40 || s[len(s)-1] != tcpHistCap+39 {
		t.Fatalf("ring must keep the newest tail, got %d..%d", s[0], s[len(s)-1])
	}
}

// Отчёт с measurement_id: независимая верификация через мок Globalping API,
// записывает GP-историю и деталь по площадкам; metrics-отчёт пишет ReportHist.
func TestHealthReportRecordsHistory(t *testing.T) {
	measurementJSON := `{"id":"m-1","status":"finished","results":[
		{"probe":{"continent":"EU","country":"DE","city":"Berlin","network":"AS123 Net","asn":123},
		 "result":{"status":"finished","statusCode":200}},
		{"probe":{"continent":"EU","country":"PL","city":"Warsaw","network":"AS456 ISP","asn":456},
		 "result":{"status":"finished","statusCode":0}}]}`
	gpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/measurements/m-1") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(measurementJSON))
	}))
	defer gpSrv.Close()

	r := newTestRegistry(t)
	r.cfg.Globalping.APIBase = gpSrv.URL
	r.register(registerRequest{NodeID: "node-gp", IP: "1.2.3.4"})
	mux := r.buildMux()

	postReport := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(body))
		mux.ServeHTTP(rec, req)
		return rec
	}

	// отчёт с measurement_id → верификация, GPLast/GPHist/ReportHist
	rec := postReport(`{"node_id":"node-gp","ip":"1.2.3.4","port":443,"globalping_ok":true,` +
		`"globalping_measurement_id":"m-1","metrics_ok":true,` +
		`"metrics_snapshot":{"telemt_user_unique_ips_current":42,"telemt_me_writers_active_current":3}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("report: %v", rec.Code)
	}
	c := r.state.Candidates["node-gp"]
	if c.GPLast == nil {
		t.Fatal("GPLast must be recorded after successful verification fetch")
	}
	if c.GPLast.MeasurementID != "m-1" || c.GPLast.ProbesTotal != 2 || c.GPLast.ProbesOK != 1 {
		t.Fatalf("GPLast counters: %+v", c.GPLast)
	}
	if len(c.GPLast.Probes) != 2 {
		t.Fatalf("GPLast probes: %+v", c.GPLast.Probes)
	}
	p0 := c.GPLast.Probes[0]
	if p0.Country != "DE" || p0.City != "Berlin" || p0.Network != "AS123 Net" || p0.ASN != 123 || !p0.OK || p0.HTTPCode != 200 {
		t.Fatalf("probe[0]: %+v", p0)
	}
	if c.GPLast.Probes[1].OK {
		t.Fatal("probe[1] must be failed (statusCode 0)")
	}
	if len(c.GPHist) != 1 || !c.GPHist[0].OK || c.GPHist[0].ProbesTotal != 2 {
		t.Fatalf("GPHist: %+v", c.GPHist)
	}
	if len(c.ReportHist) != 1 || c.ReportHist[0].Clients != 42 || c.ReportHist[0].Writers != 3 || !c.ReportHist[0].MetricsOK {
		t.Fatalf("ReportHist: %+v", c.ReportHist)
	}

	// metrics-отчёт без measurement_id: GP не трогаем, ReportHist растёт
	rec = postReport(`{"node_id":"node-gp","ip":"1.2.3.4","port":443,"metrics_ok":true,` +
		`"metrics_snapshot":{"telemt_user_unique_ips_current":51}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("report2: %v", rec.Code)
	}
	if len(c.GPHist) != 1 || !c.GlobalpingOK {
		t.Fatal("metrics-only report must not touch GP state")
	}
	if len(c.ReportHist) != 2 || c.ReportHist[1].Clients != 51 {
		t.Fatalf("ReportHist after 2nd report: %+v", c.ReportHist)
	}
}

// GET /panel/api/node: агрегаты + истории + деталь GP + события ноды;
// 404 на неизвестную ноду, 401 без токена.
func TestNodeDetailEndpoint(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true // иначе mountPanel не монтирует API
	c := &Candidate{
		NodeID: "node-det", IP: "9.9.9.9", Port: 443,
		RegisteredAt: time.Now().Add(-time.Hour), LastHeartbeat: time.Now(),
		Healthy: true, GlobalpingOK: true, MetricsOK: true, MetricsHealthy: true,
		LastReportAt: time.Now(),
		TCPHist:      []TCPPoint{{At: time.Now(), OK: true}},
		GPHist:       []GPPoint{{At: time.Now(), OK: true, Ratio: 1, ProbesOK: 2, ProbesTotal: 2}},
		ReportHist:   []ReportPoint{{At: time.Now(), MetricsOK: true, Clients: 42, Writers: 3}},
		GPLast: &GPDetail{
			At: time.Now(), MeasurementID: "m-9", OK: true, Ratio: 1,
			ProbesOK: 2, ProbesTotal: 2,
			Probes: []GPProbeLine{{Country: "DE", City: "Berlin", OK: true, Status: "finished", HTTPCode: 200}},
		},
	}
	r.state.Candidates["node-det"] = c
	r.state.Assignments = map[string]string{"d1.example.com": "node-det"}
	r.mu.Lock()
	r.addEventLocked(Event{Type: EventMasterElected, NodeID: "node-det", Domain: "d1.example.com"})
	r.addEventLocked(Event{Type: EventNodeRegistered, NodeID: "other-node"})
	r.mu.Unlock()

	mux := r.buildMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/node?id=node-det", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("node detail: %v", rec.Code)
	}
	var resp struct {
		Node struct {
			NodeID        string   `json:"node_id"`
			IsMaster      bool     `json:"is_master"`
			MasterDomains []string `json:"master_domains"`
			QueuePosition int      `json:"queue_position"`
		} `json:"node"`
		TCPHist    []TCPPoint `json:"tcp_hist"`
		GPHist     []GPPoint  `json:"gp_hist"`
		ReportHist []struct {
			Clients int `json:"clients"`
		} `json:"report_hist"`
		GPLast *GPDetail `json:"gp_last"`
		Events []Event   `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Node.NodeID != "node-det" || !resp.Node.IsMaster {
		t.Fatalf("node: %+v", resp.Node)
	}
	if len(resp.Node.MasterDomains) != 1 || resp.Node.MasterDomains[0] != "d1.example.com" {
		t.Fatalf("master domains: %+v", resp.Node.MasterDomains)
	}
	if resp.Node.QueuePosition != 1 {
		t.Fatalf("queue position: %d", resp.Node.QueuePosition)
	}
	if len(resp.TCPHist) != 1 || len(resp.GPHist) != 1 || len(resp.ReportHist) != 1 {
		t.Fatal("histories must be present")
	}
	if resp.ReportHist[0].Clients != 42 {
		t.Fatalf("report_hist clients: %+v", resp.ReportHist[0])
	}
	if resp.GPLast == nil || resp.GPLast.Probes[0].City != "Berlin" {
		t.Fatalf("gp_last: %+v", resp.GPLast)
	}
	// события отфильтрованы по ноде
	if len(resp.Events) != 1 || resp.Events[0].NodeID != "node-det" {
		t.Fatalf("events: %+v", resp.Events)
	}

	// неизвестная нода → 404
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/node?id=ghost", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown node: %v", rec.Code)
	}

	// с токеном — без Bearer 401
	r.cfgMu.Lock()
	r.cfg.Panel.Token = "sekret"
	r.cfgMu.Unlock()
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/node?id=node-det", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %v", rec.Code)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/panel/api/node?id=node-det", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with token: %v", rec.Code)
	}
}
