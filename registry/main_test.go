package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	cfg := &resolvedRegistryConfig{}
	cfg.State.File = filepath.Join(t.TempDir(), "state.json")
	cfg.Cloudflare.APIToken = "dummy" // not used in these code paths
	// один домен по умолчанию: поведение совпадает с «один мастер»
	cfg.Cloudflare.Domains = []string{"d1.example.com"}
	cfg.Healthcheck.FailThreshold = 3
	cfg.Healthcheck.RecoverThreshold = 2
	cfg.ReportFreshnessTTL = 15 * time.Minute
	cfg.HeartbeatTTL = time.Minute
	cfg.PruneUnhealthyTTL = time.Hour
	cfg.Panel.EventsMax = 500
	cfg.QuarantineAttempts = 3 // как ставит applyRegistryDefaults в проде
	cfg.SharedProxy.TLSDomain = "front.example.com"
	return &Registry{
		cfg: cfg,
		state: State{
			Candidates: make(map[string]*Candidate),
			Terminated: make(map[string]*TerminatedRecord),
		},
		startedAt: time.Now(),
	}
}

func TestRegisterDedupByIP(t *testing.T) {
	r := newTestRegistry(t)

	r.register(registerRequest{NodeID: "node-a", IP: "1.2.3.4"})
	if _, ok := r.state.Candidates["node-a"]; !ok {
		t.Fatal("node-a must be registered")
	}

	// same host re-registers with a new id -> old entry replaced, queue position resets
	r.register(registerRequest{NodeID: "node-b", IP: "1.2.3.4"})
	if _, ok := r.state.Candidates["node-a"]; ok {
		t.Fatal("node-a must have been replaced by node-b (same IP)")
	}
	c := r.state.Candidates["node-b"]
	if c == nil || c.IP != "1.2.3.4" {
		t.Fatal("node-b must hold the registration")
	}

	// re-register with same id just refreshes heartbeat, keeps RegisteredAt
	first := c.RegisteredAt
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "node-b", IP: "9.9.9.9"})
	if !r.state.Candidates["node-b"].RegisteredAt.Equal(first) {
		t.Fatal("RegisteredAt must not change on re-registration with same id")
	}
	if r.state.Candidates["node-b"].IP != "9.9.9.9" {
		t.Fatal("IP must be updated on re-registration")
	}
}

func TestCandidateHealthGating(t *testing.T) {
	c := &Candidate{Healthy: true, GlobalpingOK: true, MetricsOK: true, MetricsHealthy: true, LastReportAt: time.Now()}
	if !c.IsFullyHealthy(15 * time.Minute) {
		t.Fatal("fully healthy candidate must pass")
	}
	c.GlobalpingOK = false
	if c.IsFullyHealthy(15 * time.Minute) {
		t.Fatal("globalping fail must disqualify")
	}
	c.GlobalpingOK = true
	c.MetricsHealthy = false // судьбу решает защёлка, не сырой MetricsOK
	if c.IsFullyHealthy(15 * time.Minute) {
		t.Fatal("metrics fail must disqualify")
	}
	c.MetricsHealthy = true
	c.LastReportAt = time.Now().Add(-20 * time.Minute)
	if c.IsFullyHealthy(15 * time.Minute) {
		t.Fatal("stale report must disqualify")
	}
	c.LastReportAt = time.Now()
	c.Healthy = false
	if c.IsFullyHealthy(15 * time.Minute) {
		t.Fatal("failed tcp probe must disqualify")
	}
}

// TestHealthReportIndependentVerification — ключевой тест безопасности:
// регистратор НЕ доверяет globalping_ok из payload, а сам перезапрашивает
// Globalping API по measurement_id и пересчитывает ratio.
func TestHealthReportIndependentVerification(t *testing.T) {
	// mock Globalping API: measurement where 1/4 probes succeeded (ratio 0.25 -> NOT ok)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "m-123", "type": "http", "target": "1.1.1.1", "status": "finished",
			"measurementOptions": map[string]any{
				"protocol": "HTTPS", "port": 443,
				"request": map[string]any{"host": "front.example.com"},
			},
			"results": []map[string]any{
				{"result": map[string]any{"status": "finished", "statusCode": 200}},
				{"result": map[string]any{"status": "failed", "statusCode": 0}},
				{"result": map[string]any{"status": "failed", "statusCode": 0}},
				{"result": map[string]any{"status": "failed", "statusCode": 0}},
			},
		})
	}))
	defer mock.Close()

	r := newTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	r.register(registerRequest{NodeID: "liar", IP: "1.1.1.1"})

	// node claims globalping_ok=true — registry must override it to false
	payload := HealthReportPayload{
		NodeID:                  "liar",
		IP:                      "1.1.1.1",
		Port:                    443,
		FakeSNI:                 "front.example.com",
		GlobalpingOK:            true,
		GlobalpingMeasurementID: "m-123",
		GlobalpingSuccessRatio:  0.9,
		MetricsOK:               true,
		CheckedAt:               time.Now(),
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	c := r.state.Candidates["liar"]
	if c.GlobalpingOK {
		t.Fatal("registry must distrust node's globalping_ok claim and use its own verification")
	}
	if c.GlobalpingVerifiedRatio != 0.25 {
		t.Fatalf("verified ratio must be 0.25, got %v", c.GlobalpingVerifiedRatio)
	}
	if c.Port != 443 || !c.MetricsOK {
		t.Fatal("port/metrics must be stored from report")
	}
}

// Метрики-отчёт без measurement_id не должен затирать ранее верифицированный
// globalping-статус (тайминги циклов разделены).
func TestMetricsOnlyReportPreservesGlobalping(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "n1", IP: "1.1.1.1"})

	r.mu.Lock()
	r.state.Candidates["n1"].GlobalpingOK = true
	r.state.Candidates["n1"].GlobalpingVerifiedRatio = 0.9
	r.state.Candidates["n1"].GlobalpingMeasurementID = "m-old"
	r.mu.Unlock()

	payload := HealthReportPayload{NodeID: "n1", Port: 443, MetricsOK: true, CheckedAt: time.Now()}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	c := r.state.Candidates["n1"]
	if !c.GlobalpingOK || c.GlobalpingVerifiedRatio != 0.9 || c.GlobalpingMeasurementID != "m-old" {
		t.Fatalf("metrics-only report must preserve verified globalping state, got %+v", c)
	}
	if !c.MetricsOK || c.Port != 443 {
		t.Fatal("metrics/port must be updated")
	}
}

func TestHealthReportRejectsStaleAndDuplicateMeasurement(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "n1", IP: "1.1.1.1"})
	send := func(checked time.Time, measurement string) int {
		payload := HealthReportPayload{NodeID: "n1", IP: "1.1.1.1", Port: 443, FakeSNI: "front.example.com", MetricsOK: true, CheckedAt: checked, GlobalpingMeasurementID: measurement}
		body, _ := json.Marshal(payload)
		rec := httptest.NewRecorder()
		r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
		return rec.Code
	}
	now := time.Now()
	if code := send(now, ""); code != http.StatusOK {
		t.Fatalf("first report status=%d", code)
	}
	if code := send(now.Add(-time.Second), ""); code != http.StatusConflict {
		t.Fatalf("stale report status=%d, want 409", code)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": strings.TrimPrefix(req.URL.Path, "/measurements/"), "type": "http", "target": "1.1.1.1", "status": "finished",
			"measurementOptions": map[string]any{"protocol": "HTTPS", "port": 443, "request": map[string]any{"host": "front.example.com"}},
			"results":            []any{},
		})
	}))
	defer mock.Close()
	r.cfg.Globalping.APIBase = mock.URL
	if code := send(now.Add(time.Second), "measurement-1"); code != http.StatusOK {
		t.Fatalf("first measurement status=%d", code)
	}
	if code := send(now.Add(2*time.Second), "measurement-1"); code != http.StatusConflict {
		t.Fatalf("duplicate measurement status=%d, want 409", code)
	}
}

func TestNewGlobalpingReportCanFinishAfterNewerMetricsReport(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "n1", IP: "1.1.1.1"})
	now := time.Now()
	metrics := HealthReportPayload{NodeID: "n1", IP: "1.1.1.1", Port: 443, MetricsOK: true, CheckedAt: now}
	body, _ := json.Marshal(metrics)
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics report: %d", rec.Code)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id": "new-measurement", "type": "http", "target": "1.1.1.1", "status": "finished",
			"measurementOptions": map[string]any{"protocol": "HTTPS", "port": 443, "request": map[string]any{"host": "front.example.com"}},
			"results":            []any{},
		})
	}))
	defer mock.Close()
	r.cfg.Globalping.APIBase = mock.URL
	gp := HealthReportPayload{NodeID: "n1", IP: "1.1.1.1", Port: 443, FakeSNI: "front.example.com", MetricsOK: true,
		CheckedAt: now.Add(-time.Second), GlobalpingMeasurementID: "new-measurement"}
	body, _ = json.Marshal(gp)
	rec = httptest.NewRecorder()
	r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("new GP report must not be rejected by newer metrics timestamp: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHealthReportDoesNotApplyAfterReregistration(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(map[string]any{
			"id": strings.TrimPrefix(req.URL.Path, "/measurements/"), "type": "http", "target": "1.1.1.1", "status": "finished",
			"measurementOptions": map[string]any{"protocol": "HTTPS", "port": 443, "request": map[string]any{"host": "front.example.com"}},
			"results":            []any{},
		})
	}))
	defer mock.Close()
	r := newTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	r.register(registerRequest{NodeID: "n1", IP: "1.1.1.1"})
	payload := HealthReportPayload{NodeID: "n1", IP: "1.1.1.1", Port: 443, FakeSNI: "front.example.com", MetricsOK: true, CheckedAt: time.Now(), GlobalpingMeasurementID: "slow"}
	body, _ := json.Marshal(payload)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.handleHealthReport(rec, httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body))))
		close(done)
	}()
	<-started
	r.register(registerRequest{NodeID: "n1", IP: "1.1.1.1"})
	close(release)
	<-done
	if rec.Code != http.StatusConflict {
		t.Fatalf("superseded report status=%d, want 409", rec.Code)
	}
	if c := r.state.Candidates["n1"]; c.GlobalpingMeasurementID != "" {
		t.Fatal("slow report must not update the re-registered candidate")
	}
}

func TestHealthReportUnknownNode(t *testing.T) {
	r := newTestRegistry(t)
	payload := HealthReportPayload{NodeID: "ghost"}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown node, got %d", rec.Code)
	}
}

func TestStatePersistAndLoad(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "node-x", IP: "5.6.7.8"})
	persistStateNow(r)

	r2 := newTestRegistry(t)
	r2.cfg.State.File = r.cfg.State.File
	r2.loadState()
	if _, ok := r2.state.Candidates["node-x"]; !ok {
		t.Fatal("state must survive reload")
	}
	info, err := os.Stat(r.cfg.State.File)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state mode = %o, want 0600", info.Mode().Perm())
	}
	// (миграции active_node и её теста больше нет — флотилия
	// давно на per-domain назначениях, state-файлы тех лет не существует.)
}

func TestHTTPServerLimitsAndGracefulShutdown(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.HTTP.Addr = "127.0.0.1:0"
	srv := r.httpServer()
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 || srv.MaxHeaderBytes <= 0 {
		t.Fatalf("HTTP safety limits not configured: %+v", srv)
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveHTTPServer(ctx, srv, ln) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
}

// persistStateNow — тестовый ярлык к persistStateLocked (лок берётся снаружи).
func persistStateNow(r *Registry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.persistStateLocked()
}

// assertSingleChange — ровно одно переназначение домена за проход evaluateAssignments.
func assertSingleChange(t *testing.T, changes []domainChange, domain, from, to string) {
	t.Helper()
	if len(changes) != 1 {
		t.Fatalf("expected exactly one change %s: %q->%q, got %+v", domain, from, to, changes)
	}
	ch := changes[0]
	if ch.Domain != domain || ch.FromID != from || ch.ToID != to {
		t.Fatalf("unexpected change %+v, want %s: %q->%q", ch, domain, from, to)
	}
}

// TestQueueRepositioning — нода, потерявшая мастерство по любой причине,
// при возврате встаёт в КОНЕЦ очереди, а не забирает домен обратно
// по старому RegisteredAt.
func TestQueueRepositioning(t *testing.T) {
	r := newTestRegistry(t)

	makeHealthy := func(id string) {
		c := r.state.Candidates[id]
		c.Healthy = true
		c.GlobalpingOK = true
		c.MetricsOK = true
		c.MetricsHealthy = true
		c.LastReportAt = time.Now()
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeHealthy("A")
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "", "A")
	posA := r.state.Candidates["A"].QueuedAt

	// B приходит позже, тоже здорова — держателем домена остаётся A
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeHealthy("B")
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("A must keep the domain (entered queue first), got changes=%+v", changes)
	}
	posB := r.state.Candidates["B"].QueuedAt
	if !posA.Before(posB) {
		t.Fatal("B must be later in queue than A")
	}

	// telemt на A умирает (metrics fail), агент продолжает heartbeat — кандидат НЕ удаляется
	time.Sleep(2 * time.Millisecond)
	r.state.Candidates["A"].MetricsOK = false
	r.state.Candidates["A"].MetricsHealthy = false // роняем защёлку
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")
	if r.state.Candidates["A"].FullyHealthy || !r.state.Candidates["A"].QueuedAt.IsZero() {
		t.Fatal("A position must be burned when it left healthy set")
	}

	// A чинится — должна встать в КОНЕЦ, домен обратно НЕ забирает
	time.Sleep(2 * time.Millisecond)
	makeHealthy("A")
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("A recovered but must NOT reclaim the domain; B stays, got changes=%+v", changes)
	}
	if r.state.Assignments["d1.example.com"] != "B" {
		t.Fatalf("B must still hold the domain, got %+v", r.state.Assignments)
	}
	posA2 := r.state.Candidates["A"].QueuedAt
	if !posB.Before(posA2) {
		t.Fatalf("A must be behind B after recovery: B=%v A=%v", posB, posA2)
	}

	// Теперь умирает B — очередь отдаёт домен A
	r.state.Candidates["B"].GlobalpingOK = false
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "B", "A")
}

// TestPerDomainMasters — каждый managed-домен держит СВОЕГО мастера.
// Свободная нода НЕ отнимает единственный домен у другого (стабильность
// раскладки: лишний перевод A-записи — это микропотеря клиентов).
func TestPerDomainMasters(t *testing.T) {
	r := newTestRegistry(t)
	// несортированный конфиг — код сам приводит к детерминированному порядку
	r.cfg.Cloudflare.Domains = []string{"d2.example.com", "d1.example.com"}
	makeHealthy := func(id string) {
		c := r.state.Candidates[id]
		c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
		c.LastReportAt = time.Now()
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeHealthy("A")
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeHealthy("B")

	changes := r.evaluateAssignments(time.Now())
	if len(changes) != 2 {
		t.Fatalf("both domains must get masters on first pass, got %+v", changes)
	}
	if r.state.Assignments["d1.example.com"] != "A" || r.state.Assignments["d2.example.com"] != "B" {
		t.Fatalf("1:1 split expected (d1->A, d2->B), got %+v", r.state.Assignments)
	}

	// C встаёт здоровой: ни у кого нет >1 домена — раскладка НЕ трогается
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "C", IP: "3.3.3.3"})
	makeHealthy("C")
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("nobody overful — nothing must move, got %+v", changes)
	}

	// A умирает — d1 достаётся наименее загруженному (C держит 0, B — один)
	r.state.Candidates["A"].MetricsOK = false
	r.state.Candidates["A"].MetricsHealthy = false // роняем защёлку
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "C")

	// A чинится и простаивает — расстановка остаётся (C держит 1, B держит 1)
	r.state.Candidates["A"].MetricsOK = true
	r.state.Candidates["A"].MetricsHealthy = true // поднимаем защёлку
	r.state.Candidates["A"].LastReportAt = time.Now()
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("recovered idle A must not ripple assignments, got %+v", changes)
	}

	// Умирает B — d2 уходит A (0 доменов против одного у C)
	r.state.Candidates["B"].GlobalpingOK = false
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d2.example.com", "B", "A")

	// Умирает и C — оба домена оседают на последней живой ноде
	r.state.Candidates["C"].MetricsOK = false
	r.state.Candidates["C"].MetricsHealthy = false // роняем защёлку
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "C", "A")
	if r.state.Assignments["d1.example.com"] != "A" || r.state.Assignments["d2.example.com"] != "A" {
		t.Fatalf("A must hold both domains, got %+v", r.state.Assignments)
	}

	// Здоровых не осталось вообще — назначения НЕ снимаются (паритет:
	// A-записи остаются на последних мастерах), но stint фиксируем закрытым
	r.state.Candidates["A"].Healthy = false
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("no healthy nodes -> assignments frozen, got %+v", changes)
	}
	if r.state.Assignments["d1.example.com"] != "A" || r.state.Assignments["d2.example.com"] != "A" {
		t.Fatalf("assignments must stay on the last master, got %+v", r.state.Assignments)
	}
	if !r.state.Candidates["A"].MasterSince.IsZero() {
		t.Fatal("stint must be closed even though A-records are left in place")
	}
	orphanLost := false
	r.mu.RLock()
	for _, ev := range r.state.Events {
		if ev.Type == EventMasterLost && ev.NodeID == "A" && strings.Contains(ev.Detail, "no healthy replacement") {
			orphanLost = true
		}
	}
	r.mu.RUnlock()
	if !orphanLost {
		t.Fatal("master_lost 'no healthy replacement' expected for the last standing master")
	}
}

// TestFillEmptyRebalance — fill-empty: нод меньше, чем доменов, — мастер
// дотягивает всех «сирот»; как только появляется свободная здоровая нода,
// один сирота мигрирует к ней (не терять клиентов при падении мастера).
// Полной подгонки под равенство («3/1 → 2/2») НЕТ — стабильность дороже.
func TestFillEmptyRebalance(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{"d1.example.com", "d2.example.com", "d3.example.com"}
	makeHealthy := func(id string) {
		c := r.state.Candidates[id]
		c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
		c.LastReportAt = time.Now()
	}

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeHealthy("A")
	changes := r.evaluateAssignments(time.Now())
	if len(changes) != 3 {
		t.Fatalf("sole node must take all 3 domains, got %+v", changes)
	}
	for _, d := range []string{"d1.example.com", "d2.example.com", "d3.example.com"} {
		if r.state.Assignments[d] != "A" {
			t.Fatalf("A must hold %s, got %+v", d, r.state.Assignments)
		}
	}
	if r.state.Counters.MasterSwitches != 3 {
		t.Fatalf("3 switches expected, got %d", r.state.Counters.MasterSwitches)
	}

	// B встаёт здоровой — к ней мигрирует лексикографически первый сирота (d1)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeHealthy("B")
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")
	if r.state.Assignments["d2.example.com"] != "A" || r.state.Assignments["d3.example.com"] != "A" {
		t.Fatalf("A must keep d2/d3, got %+v", r.state.Assignments)
	}
	// дальше не докручиваем: B уже занята, доноров для fill-empty снова нет
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("no further rebalance expected (B holds d1), got %+v", changes)
	}

	// rebalance-события помечены доменом и причиной
	foundElected, foundLost := false, false
	r.mu.RLock()
	for _, ev := range r.state.Events {
		if ev.Domain != "d1.example.com" {
			continue
		}
		if ev.Type == EventMasterElected && ev.NodeID == "B" && strings.Contains(ev.Detail, "rebalance") {
			foundElected = true
		}
		if ev.Type == EventMasterLost && ev.NodeID == "A" && strings.Contains(ev.Detail, "rebalance") {
			foundLost = true
		}
	}
	r.mu.RUnlock()
	if !foundElected || !foundLost {
		t.Fatalf("rebalance events with domain tag expected, got %v", eventTypes(r))
	}
}

// TestRegisterCarriesAssignments — нода перерегистрировалась с тем же IP
// под новым ID → домены переносятся на новый ID МОЛЧА (IP не изменился,
// A-записи корректны: DNS не трогаем, master_lost/elected не генерируем).
func TestRegisterCarriesAssignments(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "old-id", IP: "1.2.3.4"})
	c := r.state.Candidates["old-id"]
	c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
	c.LastReportAt = time.Now()
	r.evaluateAssignments(time.Now())
	if r.state.Assignments["d1.example.com"] != "old-id" {
		t.Fatalf("old-id must be the master, got %+v", r.state.Assignments)
	}
	sw := r.state.Counters.MasterSwitches

	r.mu.RLock()
	eventsBefore := len(r.state.Events)
	r.mu.RUnlock()
	r.register(registerRequest{NodeID: "new-id", IP: "1.2.3.4"})
	if r.state.Assignments["d1.example.com"] != "new-id" {
		t.Fatalf("domains must be carried to new-id, got %+v", r.state.Assignments)
	}
	if r.state.Counters.MasterSwitches != sw {
		t.Fatal("carry-over must NOT count as a master switch")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, ev := range r.state.Events[eventsBefore:] {
		if ev.Type == EventMasterLost || ev.Type == EventMasterElected {
			t.Fatalf("carry-over must not emit master churn events, got %+v", ev)
		}
	}
}

// TestDomainRemovedFromConfigPruned — домен, вычеркнутый из конфига (hot-edit
// из панели), молча удаляется из назначений; DNS-запись при этом НЕ трогаем
// (именем может завладеть кто-то другой), DNS-событий не генерируем.
func TestDomainRemovedFromConfigPruned(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Cloudflare.Domains = []string{"d1.example.com", "d2.example.com"}
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	c := r.state.Candidates["A"]
	c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
	c.LastReportAt = time.Now()
	r.evaluateAssignments(time.Now())
	if len(r.state.Assignments) != 2 {
		t.Fatalf("both domains must be assigned to the sole node, got %+v", r.state.Assignments)
	}

	r.cfg.Cloudflare.Domains = []string{"d2.example.com"}
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("pruning a config domain must not trigger DNS writes, got %+v", changes)
	}
	if _, ok := r.state.Assignments["d1.example.com"]; ok {
		t.Fatalf("removed domain must be pruned from assignments, got %+v", r.state.Assignments)
	}
	if r.state.Assignments["d2.example.com"] != "A" {
		t.Fatalf("d2 must stay with A, got %+v", r.state.Assignments)
	}
}

func TestOverviewClientsUniqueIPs(t *testing.T) {
	r := newTestRegistry(t)
	report := func(id, ip string, snapshot map[string]float64) {
		t.Helper()
		r.register(registerRequest{NodeID: id, IP: ip})
		req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(
			`{"node_id":"`+id+`","ip":"`+ip+`","port":443,"metrics_ok":true,"healthy":true,"metrics_snapshot":`+toJSON(snapshot)+`}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.handleHealthReport(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("report from %s rejected: %d %s", id, w.Code, w.Body.String())
		}
	}

	// нода с новым агентом: присылает агрегат уникальных IP + per-user серии
	report("node-new", "10.0.0.1", map[string]float64{
		"telemt_me_writers_active_current":         8,
		"telemt_user_unique_ips_current":           5,
		`telemt_user_unique_ips_current{user="a"}`: 2,
		`telemt_user_unique_ips_current{user="b"}`: 3,
	})
	// нода со старым агентом: только writers
	report("node-old", "10.0.0.2", map[string]float64{
		"telemt_me_writers_active_current": 4,
	})
	// нода вообще без метрик (metrics-цикл ещё не отчитался)
	r.register(registerRequest{NodeID: "node-none", IP: "10.0.0.3"})

	ov := r.buildOverview()
	if ov.ClientsUniqueTotal == nil || *ov.ClientsUniqueTotal != 5 {
		t.Fatalf("clients_unique_ips_total must be 5, got %+v", ov.ClientsUniqueTotal)
	}
	if ov.WritersActive != 12 {
		t.Fatalf("writers total must be 8+4=12, got %v", ov.WritersActive)
	}
	byID := map[string]panelNode{}
	for _, n := range ov.Nodes {
		byID[n.NodeID] = n
	}
	if byID["node-new"].ClientsUniqueIPs == nil || *byID["node-new"].ClientsUniqueIPs != 5 {
		t.Fatalf("node-new must carry 5 unique IPs, got %+v", byID["node-new"].ClientsUniqueIPs)
	}
	if byID["node-old"].ClientsUniqueIPs != nil {
		t.Fatal("old agent without aggregate key must yield nil (panel shows dash)")
	}
	if byID["node-none"].ClientsUniqueIPs != nil {
		t.Fatal("node without snapshot must yield nil")
	}
}

func TestOverviewClientsUniqueIPsAllMissing(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "node-old", IP: "10.0.0.9"})
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(
		`{"node_id":"node-old","ip":"10.0.0.9","port":443,"metrics_ok":true,"healthy":true,"metrics_snapshot":{"telemt_me_writers_active_current":3}}`))
	w := httptest.NewRecorder()
	r.handleHealthReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("report rejected: %d", w.Code)
	}
	if ov := r.buildOverview(); ov.ClientsUniqueTotal != nil {
		t.Fatalf("no reporting nodes -> nil total expected, got %v", *ov.ClientsUniqueTotal)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
