package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// specs/dc2-health.md: HealthReport получает dc2_ok/dc2_reason/dc2_diag;
// MetricsOK = dc2_ok; нода с dc2_ok=false не входит в селекцию; восстановление
// — через существующий recover-порог; бан/VPS по dc2 НЕ запускается.

// reportDC2 шлёт metrics-only отчёт с dc2-полями (как dc2-агент).
func reportDC2(t *testing.T, r *Registry, id string, dc2OK bool, reason string, metricsOK bool) {
	t.Helper()
	payload := HealthReportPayload{
		NodeID: id, Port: 443, CheckedAt: time.Now(),
		DC2OK: dc2OK, DC2Reason: reason, MetricsOK: metricsOK,
	}
	if reason != "" {
		payload.Error = "dc2: " + reason
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dc2 report for %s: expected 200, got %d (%s)", id, rec.Code, rec.Body.String())
	}
}

// TestDC2FieldsAcceptedOnWire — новые поля не должны отваливаться
// DisallowUnknownFields (иначе каждый dc2-агент получал бы 400).
func TestDC2FieldsAcceptedOnWire(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	body := `{"node_id":"A","ip":"1.1.1.1","port":443,"metrics_ok":false,"error":"dc2: dc2_no_fresh",` +
		`"dc2_ok":false,"dc2_reason":"dc2_no_fresh","dc2_diag":{"dc2_alive":7,"dc2_fresh":0},"checked_at":"` +
		time.Now().UTC().Format(time.RFC3339Nano) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dc2 fields must decode: got %d (%s)", rec.Code, rec.Body.String())
	}
	c := r.state.Candidates["A"]
	if c.MetricsOK {
		t.Fatal("dc2_no_fresh report must count as failed (MetricsOK = dc2_ok)")
	}
	if c.ReportError != "dc2: dc2_no_fresh" {
		t.Fatalf("report error must carry the reason, got %q", c.ReportError)
	}
}

// TestDC2DerivationRule — реестр доверяет dc2_ok, когда агент прислал dc2-
// вердикт (непустой dc2_reason); старые агенты (без dc2-полей) — прежняя логика.
func TestDC2DerivationRule(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	r.register(registerRequest{NodeID: "legacy", IP: "2.2.2.2"})

	// dc2-агент: противоречивый отчёт (metrics_ok=true, dc2_ok=false) — верит dc2.
	reportDC2(t, r, "A", false, "dc2_absent", true)
	if r.state.Candidates["A"].MetricsOK {
		t.Fatal("dc2_ok must win over metrics_ok for dc2-aware agents")
	}

	// Старый агент: metrics_ok=true без dc2-полей — засчитывается как раньше.
	payload := HealthReportPayload{NodeID: "legacy", Port: 443, MetricsOK: true, CheckedAt: time.Now()}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy report: got %d", rec.Code)
	}
	if !r.state.Candidates["legacy"].MetricsOK {
		t.Fatal("old-agent report must count as ok")
	}
}

// TestDC2FailureBlocksSelectionRecoversByLatch — dc2_ok=false гасит ноду
// через существующую metrics-защёлку (fail/recover пороги не тронуты);
// восстановление возвращает ноду в очередь после recover_threshold.
// Бан/карантин/terminate по dc2 не срабатывают.
func TestDC2FailureBlocksSelectionRecoversByLatch(t *testing.T) {
	r := newTestRegistry(t) // fail_threshold=3, recover_threshold=2
	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	liftToHealthy(r, "A")
	liftToHealthy(r, "B")
	r.evaluateAssignments(time.Now())
	if firstMaster(r, "d1.example.com") != "A" {
		t.Fatalf("A must be the initial master, got %+v", r.state.Assignments)
	}

	// Серия dc2-фейлов: на третьем защёлка гаснет, домен уходит B.
	reportDC2(t, r, "A", false, "dc2_no_fresh", false)
	reportDC2(t, r, "A", false, "dc2_no_fresh", false)
	c := r.state.Candidates["A"]
	if !c.MetricsHealthy {
		t.Fatal("two bad dc2 reports (fail=3) must keep the latch up")
	}
	if firstMaster(r, "d1.example.com") != "A" {
		t.Fatal("master must survive below the fail threshold")
	}
	reportDC2(t, r, "A", false, "dc2_no_fresh", false)
	if c.MetricsHealthy {
		t.Fatal("latch must close on fail_threshold consecutive dc2 failures")
	}
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")

	// Ни бана, ни карантина по dc2-фейлу быть не должно.
	if c.Quarantine != nil {
		t.Fatalf("dc2 failure must not trigger gp quarantine: %+v", c.Quarantine)
	}
	if len(r.state.Terminated) != 0 {
		t.Fatalf("dc2 failure must never terminate a node: %v", r.state.Terminated)
	}

	// Восстановление: recover_threshold=2 подряд dc2_ok=true.
	reportDC2(t, r, "A", true, "", true)
	if c.MetricsHealthy {
		t.Fatal("one good report (recover=2) must not reopen the latch")
	}
	reportDC2(t, r, "A", true, "", true)
	if !c.MetricsHealthy {
		t.Fatal("latch must reopen on recover_threshold consecutive dc2-ok reports")
	}
	r.evaluateAssignments(time.Now())
	if !c.FullyHealthy {
		t.Fatal("recovered node must rejoin the healthy queue")
	}
}
