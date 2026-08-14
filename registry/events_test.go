package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func eventTypes(r *Registry) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, len(r.state.Events))
	for i, ev := range r.state.Events {
		out[i] = ev.Type
	}
	return out
}

func hasEvent(types []string, t string) bool {
	for _, x := range types {
		if x == t {
			return true
		}
	}
	return false
}

// Кольцевой буфер журнала: переполнение отбрасывает самые старые события.
func TestEventRingTrim(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Panel.EventsMax = 5

	r.mu.Lock()
	for i := 0; i < 8; i++ {
		r.addEventLocked(Event{Type: EventNodeRegistered, Detail: fmt.Sprintf("ev-%d", i)})
	}
	r.mu.Unlock()

	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.state.Events) != 5 {
		t.Fatalf("journal must be trimmed to 5, got %d", len(r.state.Events))
	}
	if r.state.Events[0].Detail != "ev-3" || r.state.Events[4].Detail != "ev-7" {
		t.Fatalf("oldest events must be dropped first, got %+v", r.state.Events)
	}
	if !r.eventsDirty {
		t.Fatal("journal must be marked dirty after addEventLocked")
	}
}

// История смен мастера: избрание, потеря, перевыборы; учёт времени мастерства.
// V7: события master_elected/master_lost помечены доменом.
func TestMasterEventsAndStintAccounting(t *testing.T) {
	r := newTestRegistry(t)
	makeHealthy := func(id string) {
		c := r.state.Candidates[id]
		c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
		c.LastReportAt = time.Now()
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeHealthy("A")
	r.evaluateAssignments(time.Now())

	types := eventTypes(r)
	if !hasEvent(types, EventMasterElected) || !hasEvent(types, EventQueueJoined) {
		t.Fatalf("expected master_elected+queue_joined, got %v", types)
	}
	if r.state.Counters.MasterSwitches != 1 {
		t.Fatalf("master switches must be 1, got %d", r.state.Counters.MasterSwitches)
	}
	a := r.state.Candidates["A"]
	if a.MasterStints != 1 || a.MasterSince.IsZero() {
		t.Fatalf("A must hold open stint #1, got stints=%d since=%v", a.MasterStints, a.MasterSince)
	}

	// A умерла -> домен уходит B; stint A закрывается.
	// Симулируем час мастерства: отматываем MasterSince назад, чтобы
	// накопленные секунды были значимыми (реальный stint длится минуты).
	r.mu.Lock()
	r.state.Candidates["A"].MasterSince = time.Now().Add(-time.Hour)
	r.mu.Unlock()

	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeHealthy("B")
	r.evaluateAssignments(time.Now()) // B join; у A один домен — fill-empty его не отнимает
	if r.state.Assignments["d1.example.com"] != "A" {
		t.Fatal("B must not steal A's only domain")
	}
	r.state.Candidates["A"].MetricsOK = false
	r.state.Candidates["A"].MetricsHealthy = false // V7.9.4: роняем защёлку
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")

	types = eventTypes(r)
	if !hasEvent(types, EventMasterLost) || !hasEvent(types, EventQueueLeft) {
		t.Fatalf("expected master_lost+queue_left, got %v", types)
	}
	a = r.state.Candidates["A"]
	if !a.MasterSince.IsZero() {
		t.Fatal("A stint must be closed on master loss")
	}
	if a.MasterSeconds < 3500 {
		t.Fatalf("A must accumulate ~1h of master seconds, got %d", a.MasterSeconds)
	}
	if a.MasterTimeSec(time.Now()) != a.MasterSeconds {
		t.Fatal("closed stint must equal accumulated seconds")
	}
	if r.state.Counters.MasterSwitches != 2 {
		t.Fatalf("master switches must be 2, got %d", r.state.Counters.MasterSwitches)
	}

	// мастер исчезает из пула целиком (не прислал heartbeat) -> домен-«сирота»
	// уходит следующему здоровому; master_lost с доменом и причиной
	r.register(registerRequest{NodeID: "C", IP: "3.3.3.3"})
	makeHealthy("C")
	r.evaluateAssignments(time.Now())
	r.mu.Lock()
	delete(r.state.Candidates, "B")
	r.mu.Unlock()
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "B", "C")
	found := false
	r.mu.RLock()
	for _, ev := range r.state.Events {
		if ev.Type == EventMasterLost && ev.NodeID == "B" && ev.Domain == "d1.example.com" &&
			strings.Contains(ev.Detail, "removed from pool") {
			found = true
		}
	}
	r.mu.RUnlock()
	if !found {
		t.Fatalf("master_lost for removed assignee expected, events=%v", eventTypes(r))
	}
	if r.state.Counters.MasterSwitches != 3 {
		t.Fatalf("master switches must be 3, got %d", r.state.Counters.MasterSwitches)
	}
}

// Событие блокировки по результату НЕЗАВИСИМОЙ проверки Globalping (+recovery).
func TestBlockedEventOnHealthReport(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "m-x", "status": "finished",
			"results": []map[string]any{
				{"result": map[string]any{"status": "failed", "statusCode": 0}},
				{"result": map[string]any{"status": "failed", "statusCode": 0}},
			},
		})
	}))
	defer mock.Close()

	r := newTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	r.register(registerRequest{NodeID: "n1", IP: "1.2.3.4"})

	// начальное "здоровое" состояние, чтобы переход был true -> false
	r.mu.Lock()
	r.state.Candidates["n1"].GlobalpingOK = true
	r.mu.Unlock()

	payload := HealthReportPayload{
		NodeID: "n1", IP: "1.2.3.4", Port: 443,
		GlobalpingOK: true, GlobalpingMeasurementID: "m-x", MetricsOK: true,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if !hasEvent(eventTypes(r), EventGlobalpingBlocked) {
		t.Fatalf("globalping_blocked event expected, got %v", eventTypes(r))
	}
	if r.state.Counters.GPBlocked != 1 {
		t.Fatalf("gp_blocked counter must be 1, got %d", r.state.Counters.GPBlocked)
	}
	c := r.state.Candidates["n1"]
	if c.GPChecksTotal != 1 || c.GPChecksOK != 0 {
		t.Fatalf("gp check counters wrong: %+v", c)
	}
	if c.ReportsTotal != 1 || c.ReportsOK != 0 {
		t.Fatalf("report stats wrong: %+v", c)
	}
}

// Панель: авторизация Bearer-токеном, /panel отдаёт HTML, overview/events JSON.
func TestPanelEndpoints(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	r.cfg.Panel.Token = "test-token"
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	r.mu.Lock()
	c := r.state.Candidates["A"]
	c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
	c.LastReportAt = time.Now()
	r.mu.Unlock()
	r.evaluateAssignments(time.Now())

	mux := http.NewServeMux()
	r.mountPanel(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// HTML отдаётся без токена (данные ходят отдельно, через API)
	resp, err := http.Get(srv.URL + "/panel")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /panel: %v status=%v", err, resp.StatusCode)
	}
	htmlBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(htmlBody), "sharedd") {
		t.Fatal("panel HTML must contain title")
	}

	// API без токена -> 401
	resp, err = http.Get(srv.URL + "/panel/api/overview")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("overview without token must be 401, got %v (%v)", resp.StatusCode, err)
	}
	resp.Body.Close()

	// API с токеном -> 200 и корректные поля
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/overview", nil)
	req2.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req2)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("overview with token must be 200, got %v (%v)", resp.StatusCode, err)
	}
	var ov panelOverview
	json.NewDecoder(resp.Body).Decode(&ov)
	resp.Body.Close()

	if len(ov.Masters) != 1 || ov.Masters[0].Domain != "d1.example.com" ||
		ov.Masters[0].NodeID != "A" || ov.Masters[0].IP != "1.1.1.1" || ov.Masters[0].Dead {
		t.Fatalf("overview must show A as master of d1, got %+v", ov.Masters)
	}
	if ov.NodesTotal != 1 || ov.NodesFullyHealthy != 1 || ov.QueueSize != 1 {
		t.Fatalf("wrong pool aggregates: %+v", ov)
	}
	if len(ov.Nodes) != 1 || ov.Nodes[0].QueuePosition != 1 || !ov.Nodes[0].IsMaster {
		t.Fatalf("node entry wrong: %+v", ov.Nodes)
	}
	if len(ov.Nodes[0].MasterDomains) != 1 || ov.Nodes[0].MasterDomains[0] != "d1.example.com" {
		t.Fatalf("master_domains wrong: %+v", ov.Nodes[0].MasterDomains)
	}

	// events endpoint
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/events?limit=10", nil)
	req3.Header.Set("Authorization", "Bearer test-token")
	resp, err = http.DefaultClient.Do(req3)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("events must be 200, got %v", resp)
	}
	var evResp struct {
		Events []Event `json:"events"`
	}
	json.NewDecoder(resp.Body).Decode(&evResp)
	resp.Body.Close()
	if len(evResp.Events) == 0 {
		t.Fatal("events must be non-empty after registration+election")
	}
	// новые сверху: первое — master_elected или queue_joined (порядок по убыванию времени)
	first := evResp.Events[0]
	if first.Type != EventMasterElected && first.Type != EventQueueJoined {
		t.Fatalf("unexpected newest event: %+v", first)
	}
}

// Регресс: сборка полного mux (все API + панель) не должна паниковать на
// конфликте паттернов Go-1.22+ mux ("GET /" vs "/register" и т.п.).
func TestBuildMuxNoConflicts(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	mux := r.buildMux() // паника здесь = конфликт паттернов

	srv := httptest.NewServer(mux)
	defer srv.Close()

	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := noRedirect.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/panel" {
		t.Fatalf("GET / must redirect to /panel, got %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("GET /healthz must be 200, got %v (%v)", resp.StatusCode, err)
	}
	resp.Body.Close()
}

// Журнал и счётчики переживают персист/загрузку state-файла.
func TestEventsPersisted(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "node-x", IP: "5.6.7.8"})

	r2 := newTestRegistry(t)
	r2.cfg.State.File = r.cfg.State.File
	r2.loadState()
	if len(r2.state.Events) == 0 {
		t.Fatal("events must survive state reload")
	}
	if !hasEvent([]string{r2.state.Events[len(r2.state.Events)-1].Type}, EventNodeRegistered) {
		t.Fatalf("last event must be node_registered, got %+v", r2.state.Events)
	}
	if r2.state.Counters.Registrations != 1 {
		t.Fatalf("registrations counter must survive, got %d", r2.state.Counters.Registrations)
	}
}
