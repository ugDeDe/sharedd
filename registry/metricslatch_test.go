package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Metrics-здоровье — защёлка по fail/recover-порогам, а не мгновенный
// флаг последнего отчёта. Одиночный сбойный отчёт fully-healthy (и мастерство)
// НЕ роняет; роняет серия из fail_threshold подряд; обратно — recover_threshold
// подряд хороших.

// reportMetrics шлёт metrics-only отчёт (без measurement_id → GP-верификация
// и сеть не задействованы).
func reportMetrics(t *testing.T, r *Registry, id string, ok bool, errText string) {
	t.Helper()
	payload := HealthReportPayload{NodeID: id, Port: 443, MetricsOK: ok, Error: errText, CheckedAt: time.Now()}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("report for %s: expected 200, got %d", id, rec.Code)
	}
}

// liftToHealthy — ручной подъём всех защёлок (как makeHealthy в main_test,
// но локально, чтобы не зависеть от порядка файлов).
func liftToHealthy(r *Registry, id string) {
	c := r.state.Candidates[id]
	c.Healthy, c.GlobalpingOK, c.MetricsOK, c.MetricsHealthy = true, true, true, true
	c.LastReportAt = time.Now()
}

func TestMetricsLatchMasterSurvivesSingleBadReports(t *testing.T) {
	r := newTestRegistry(t) // fail_threshold=3, recover_threshold=2
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	liftToHealthy(r, "A")
	liftToHealthy(r, "B")
	r.evaluateAssignments(time.Now())
	if r.state.Assignments["d1.example.com"] != "A" {
		t.Fatalf("A must be the initial master, got %+v", r.state.Assignments)
	}

	// Два подряд плохих отчёта (fail=3): защёлка жива, мастерство не тронуто,
	// события metrics_down ещё нет.
	reportMetrics(t, r, "A", false, "metrics: fetch error: connection refused")
	reportMetrics(t, r, "A", false, "metrics: fetch error: connection refused")
	c := r.state.Candidates["A"]
	if !c.MetricsHealthy || c.MetricsFailStreak != 2 {
		t.Fatalf("two bad reports (fail=3) must keep the latch up, got healthy=%v streak=%d",
			c.MetricsHealthy, c.MetricsFailStreak)
	}
	if !c.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
		t.Fatal("node must stay fully healthy below the fail threshold")
	}
	if changes := r.evaluateAssignments(time.Now()); len(changes) != 0 {
		t.Fatalf("single/double bad report must not rotate the master, got %+v", changes)
	}
	if hasEvent(eventTypes(r), EventMetricsDown) {
		t.Fatal("metrics_down must not fire below fail_threshold")
	}

	// Третий подряд — защёлка захлопнулась: событие, выход из очереди,
	// домен уходит запасной ноде.
	reportMetrics(t, r, "A", false, "metrics: fetch error: connection refused")
	if c.MetricsHealthy {
		t.Fatal("latch must close exactly on fail_threshold consecutive bad reports")
	}
	if !hasEvent(eventTypes(r), EventMetricsDown) {
		t.Fatalf("metrics_down event expected, got %v", eventTypes(r))
	}
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")

	// Один хороший отчёт (recover=2) — защёлка ещё закрыта, A вне очереди.
	reportMetrics(t, r, "A", true, "")
	if c.MetricsHealthy || c.MetricsOKStreak != 1 {
		t.Fatalf("one good report (recover=2) must not reopen the latch, got healthy=%v streak=%d",
			c.MetricsHealthy, c.MetricsOKStreak)
	}
	if hasEvent(eventTypes(r), EventMetricsUp) {
		t.Fatal("metrics_up must not fire below recover_threshold")
	}

	// Второй подряд хороший — защёлка открылась, A встал в очередь (в конец).
	reportMetrics(t, r, "A", true, "")
	if !c.MetricsHealthy {
		t.Fatal("latch must reopen on recover_threshold consecutive good reports")
	}
	if !hasEvent(eventTypes(r), EventMetricsUp) {
		t.Fatalf("metrics_up event expected, got %v", eventTypes(r))
	}
	r.evaluateAssignments(time.Now())
	if !c.FullyHealthy {
		t.Fatal("recovered node must rejoin the healthy queue")
	}
}

func TestMetricsLatchFreshNodeWaitsRecover(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "node-new", IP: "10.0.0.1"})
	c := r.state.Candidates["node-new"]
	// изолируем metrics-защёлку: остальные условия здоровья выполняем сразу
	c.Healthy = true
	c.GlobalpingOK = true

	reportMetrics(t, r, "node-new", true, "")
	if c.MetricsHealthy || c.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
		t.Fatal("fresh node must NOT be fully healthy after a single good report (recover_threshold=2)")
	}
	reportMetrics(t, r, "node-new", true, "")
	if !c.MetricsHealthy || !c.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
		t.Fatal("fresh node must become fully healthy after recover_threshold good reports")
	}
}

func TestMetricsLatchStreakReset(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	liftToHealthy(r, "A")
	c := r.state.Candidates["A"]

	// fail, fail, ok, fail, fail, ok… — серия прерывается, защёлка вечно жива
	for i := 0; i < 4; i++ {
		reportMetrics(t, r, "A", false, "boom")
		reportMetrics(t, r, "A", false, "boom")
		reportMetrics(t, r, "A", true, "")
		if !c.MetricsHealthy {
			t.Fatalf("interleaved ok must reset the streak; iteration %d killed the latch", i)
		}
		if c.MetricsFailStreak != 0 {
			t.Fatalf("fail streak must reset to 0 after a good report, got %d", c.MetricsFailStreak)
		}
	}
}

// Миграция при апгрейде: у старого state защёлки нет — поднимаем её из
// последнего MetricsOK, чтобы апгрейд не ронял весь пул.
func TestMetricsLatchMigrationOnLoad(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "node-x", IP: "5.6.7.8"})
	// «старое» состояние: был живой по последнему отчёту, защёлки не существовало
	r.state.Candidates["node-x"].MetricsOK = true
	persistStateNow(r)

	r2 := newTestRegistry(t)
	r2.cfg.State.File = r.cfg.State.File
	r2.loadState()
	c := r2.state.Candidates["node-x"]
	if c == nil {
		t.Fatal("state must reload")
	}
	if !c.MetricsHealthy {
		t.Fatal("migration must lift MetricsHealthy from legacy MetricsOK")
	}

	// нода с MetricsOK=false мигрирует закрытой защёлкой (намеренная гистерезис)
	r3 := newTestRegistry(t)
	r3.register(registerRequest{NodeID: "node-y", IP: "5.6.7.9"})
	r3.state.Candidates["node-y"].MetricsOK = false
	persistStateNow(r3)
	r4 := newTestRegistry(t)
	r4.cfg.State.File = r3.cfg.State.File
	r4.loadState()
	if r4.state.Candidates["node-y"].MetricsHealthy {
		t.Fatal("legacy unhealthy node must stay latched-down until recover_threshold good reports")
	}
}
