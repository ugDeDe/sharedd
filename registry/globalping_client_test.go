package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// V7.9.1: measurement ещё выполняется — верификация обязана ДОЖДАТЬСЯ
// завершения, а не оценивать частичные результаты (источник «ratio 0»).
func TestFetchFinishedWaitsForCompletion(t *testing.T) {
	var calls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) < 3 {
			json.NewEncoder(w).Encode(map[string]any{"id": "m-wait", "status": "in-progress", "results": []any{}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id": "m-wait", "status": "finished",
			"results": []map[string]any{
				{"result": map[string]any{"status": "finished", "statusCode": 200}},
				{"result": map[string]any{"status": "finished", "statusCode": 200}},
			},
		})
	}))
	defer mock.Close()

	gp := NewGlobalpingChecker(mock.URL)
	m, err := gp.FetchFinished("m-wait", 30*time.Second)
	if err != nil {
		t.Fatalf("FetchFinished: %v", err)
	}
	if m.Status != "finished" || calls.Load() < 3 {
		t.Fatalf("expected finished after polling, status=%s calls=%d", m.Status, calls.Load())
	}
	if ratio := evaluateSuccessRatio(m); ratio != 1.0 {
		t.Fatalf("ratio must be 1.0, got %v", ratio)
	}
}

// V7.9.1: measurement так и остался in-progress за таймаут — ошибка + снапшот
// (вызывающий код трактует верификацию как несостоявшуюся).
func TestFetchFinishedTimeoutOnStuckMeasurement(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "m-stuck", "status": "in-progress", "results": []any{}})
	}))
	defer mock.Close()

	gp := NewGlobalpingChecker(mock.URL)
	_, err := gp.FetchFinished("m-stuck", 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "in-progress") {
		t.Fatalf("expected in-progress timeout error, got %v", err)
	}
}

// V7.9.1: верификация не состоялась (API globalping недоступно) — GP-статус
// ноды НЕ трогаем: ни блокировки, ни счётчиков. Иначе сетевой чих рисовал 0.
func TestHealthReportInconclusiveKeepsGPState(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer mock.Close()

	r := newTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	r.register(registerRequest{NodeID: "n1", IP: "1.2.3.4"})

	r.mu.Lock()
	r.state.Candidates["n1"].GlobalpingOK = true
	r.state.Candidates["n1"].GlobalpingVerifiedRatio = 0.9
	r.state.Candidates["n1"].GlobalpingMeasurementID = "m-old"
	r.mu.Unlock()

	payload := HealthReportPayload{
		NodeID: "n1", IP: "1.2.3.4", Port: 443,
		GlobalpingOK: true, GlobalpingMeasurementID: "m-dead", MetricsOK: true,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	c := r.state.Candidates["n1"]
	if !c.GlobalpingOK || c.GlobalpingVerifiedRatio != 0.9 || c.GlobalpingMeasurementID != "m-old" {
		t.Fatalf("inconclusive verification must keep previous GP state, got %+v", c)
	}
	if c.GPChecksTotal != 0 {
		t.Fatalf("inconclusive verification must not count as a GP check, got %d", c.GPChecksTotal)
	}
	if r.state.Counters.GPBlocked != 0 || hasEvent(eventTypes(r), EventGlobalpingBlocked) {
		t.Fatal("inconclusive verification must not block the node")
	}
	// а вот доступность отчёт не портит: metrics были ok
	if c.ReportsOK != 1 || c.ReportsTotal != 1 {
		t.Fatalf("report with ok metrics must count despite inconclusive GP, got ok=%d total=%d", c.ReportsOK, c.ReportsTotal)
	}
}
