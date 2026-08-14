package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func ptrInt(v int) *int { return &v }

// makeFullyHealthyNow — минимально достаточный fully-healthy кандидат
// (паттерн из TestQueueRepositioning).
func makeFullyHealthyNow(r *Registry, id string) {
	c := r.state.Candidates[id]
	c.Healthy = true
	c.GlobalpingOK = true
	c.MetricsOK = true
	c.MetricsHealthy = true // V7.9.4: fully-healthy судится по защёлке
	c.LastReportAt = time.Now()
}

func TestResolveMasterTTLMinutes(t *testing.T) {
	if got := resolveMasterTTLMinutes(nil); got != defaultMasterTTLMinutes {
		t.Fatalf("nil ptr must resolve to default %d, got %d", defaultMasterTTLMinutes, got)
	}
	if got := resolveMasterTTLMinutes(ptrInt(0)); got != 0 {
		t.Fatalf("explicit 0 must stay 0 (limit off), got %d", got)
	}
	if got := resolveMasterTTLMinutes(ptrInt(45)); got != 45 {
		t.Fatalf("45 must pass through, got %d", got)
	}
}

func TestMasterTTLForcedRotation(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Rotation.MasterTTLMinutes = ptrInt(30)

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeFullyHealthyNow(r, "A")
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "", "A")
	if r.state.AssignmentsSince["d1.example.com"].IsZero() {
		t.Fatal("assignment since must be recorded")
	}

	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeFullyHealthyNow(r, "B")
	if ch := r.evaluateAssignments(time.Now()); len(ch) != 0 {
		t.Fatalf("healthy master within TTL must not be rotated, got %+v", ch)
	}

	// мастер просидел дольше лимита → принудительная ротация на B
	r.state.AssignmentsSince["d1.example.com"] = time.Now().Add(-31 * time.Minute)
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")
	if r.state.Counters.MasterTTLRotations != 1 {
		t.Fatalf("MasterTTLRotations = %d, want 1", r.state.Counters.MasterTTLRotations)
	}
	if time.Since(r.state.AssignmentsSince["d1.example.com"]) > time.Minute {
		t.Fatal("since must restart on TTL rotation")
	}
	// событие master_lost помечено как TTL-ротация
	found := false
	for i := len(r.state.Events) - 1; i >= 0; i-- {
		ev := r.state.Events[i]
		if ev.Type == EventMasterLost && ev.Domain == "d1.example.com" && ev.NodeID == "A" {
			if !strings.Contains(ev.Detail, "TTL") {
				t.Fatalf("master_lost detail must mention TTL, got %q", ev.Detail)
			}
			found = true
			break
		}
	}
	if !found {
		t.Fatal("master_lost event for TTL rotation missing")
	}
	// следующий тик — тишина (возраст нового мастера мал)
	if ch := r.evaluateAssignments(time.Now()); len(ch) != 0 {
		t.Fatalf("fresh master must not be rotated, got %+v", ch)
	}

	// round-robin: B тоже пересидел — домен возвращается к A
	r.state.AssignmentsSince["d1.example.com"] = time.Now().Add(-31 * time.Minute)
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "B", "A")
	if r.state.Counters.MasterTTLRotations != 2 {
		t.Fatalf("MasterTTLRotations = %d, want 2", r.state.Counters.MasterTTLRotations)
	}
}

func TestMasterTTLNoReplacementKeepsMaster(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Rotation.MasterTTLMinutes = ptrInt(30)

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeFullyHealthyNow(r, "A")
	r.evaluateAssignments(time.Now())
	r.state.AssignmentsSince["d1.example.com"] = time.Now().Add(-2 * time.Hour)

	// замены нет — мастер остаётся, счётчик не двигается, overdue взведён
	if ch := r.evaluateAssignments(time.Now()); len(ch) != 0 {
		t.Fatalf("no replacement → no rotation, got %+v", ch)
	}
	if r.state.Assignments["d1.example.com"] != "A" {
		t.Fatal("A must keep the domain while it is the only healthy node")
	}
	if r.state.Counters.MasterTTLRotations != 0 {
		t.Fatal("no TTL rotation may be counted without a successor")
	}
	if !r.ttlOverdue["d1.example.com"] {
		t.Fatal("overdue flag must be set (anti-spam marker)")
	}
	// второй тик — то же самое, никаких дублей событий master_lost
	r.evaluateAssignments(time.Now())
	lostCnt := 0
	for _, ev := range r.state.Events {
		if ev.Type == EventMasterLost && ev.Domain == "d1.example.com" {
			lostCnt++
		}
	}
	if lostCnt != 0 {
		t.Fatalf("overdue-without-replacement must NOT emit master_lost, got %d", lostCnt)
	}

	// появилась здоровая B — истёкший мастер сдаёт домен на ближайшем тике
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeFullyHealthyNow(r, "B")
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")
	if r.ttlOverdue["d1.example.com"] {
		t.Fatal("overdue flag must be cleared after rotation")
	}
}

func TestMasterTTLDisabled(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.Rotation.MasterTTLMinutes = ptrInt(0) // явный ноль = выкл (не дефолт!)

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeFullyHealthyNow(r, "A")
	r.evaluateAssignments(time.Now())
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeFullyHealthyNow(r, "B")

	r.state.AssignmentsSince["d1.example.com"] = time.Now().Add(-72 * time.Hour)
	if ch := r.evaluateAssignments(time.Now()); len(ch) != 0 {
		t.Fatalf("TTL disabled (0) — rotation must not happen, got %+v", ch)
	}
	if r.state.Counters.MasterTTLRotations != 0 {
		t.Fatal("no TTL rotations allowed when disabled")
	}
}

func TestMasterTTLRoundRobinAcrossAllNodes(t *testing.T) {
	// V7.9.5: регрессионный — раньше домен ходил только между двумя старшими
	// нодами очереди: истёкший мастер СВОЮ позицию сохранял, а pickLeastLoaded
	// при равной загрузке брал раннего. Теперь сдавший домен — в конец очереди.
	r := newTestRegistry(t)
	r.cfg.Rotation.MasterTTLMinutes = ptrInt(30)

	r.register(registerRequest{NodeID: "A", IP: "1.1.1.1"})
	makeFullyHealthyNow(r, "A")
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "B", IP: "2.2.2.2"})
	makeFullyHealthyNow(r, "B")
	time.Sleep(2 * time.Millisecond)
	r.register(registerRequest{NodeID: "C", IP: "3.3.3.3"})
	makeFullyHealthyNow(r, "C")

	r.evaluateAssignments(time.Now())
	if r.state.Assignments["d1.example.com"] != "A" {
		t.Fatalf("A (старшая в очереди) должен получить домен, got %+v", r.state.Assignments)
	}

	expire := func() {
		r.state.AssignmentsSince["d1.example.com"] = time.Now().Add(-31 * time.Minute)
	}
	// TTL A истёк → домен к B, A — в конец очереди
	expire()
	changes := r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "A", "B")
	if got := r.queuePositionLocked(r.state.Candidates["A"]); got != 3 {
		t.Fatalf("ex-master A must be demoted to queue tail, got position %d", got)
	}
	// TTL B истёк → домен к C (НЕ обратно к A — в этом и был баг)
	expire()
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "B", "C")
	if got := r.queuePositionLocked(r.state.Candidates["B"]); got != 3 {
		t.Fatalf("ex-master B must be demoted to queue tail, got position %d", got)
	}
	// TTL C истёк → домен снова к A (полный круг round-robin)
	expire()
	changes = r.evaluateAssignments(time.Now())
	assertSingleChange(t, changes, "d1.example.com", "C", "A")
	if r.state.Counters.MasterTTLRotations != 3 {
		t.Fatalf("MasterTTLRotations = %d, want 3", r.state.Counters.MasterTTLRotations)
	}
}

func TestRotationEditorRoundTrip(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)

	// GET: дефолт 30 (ключа в файле нет)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admintoken")
	rec := httptest.NewRecorder()
	r.handleGetConfig(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET config: %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	cfgMap, _ := got["config"].(map[string]any)
	rot, _ := cfgMap["rotation"].(map[string]any)
	if rot["master_ttl_minutes"] != float64(defaultMasterTTLMinutes) {
		t.Fatalf("default master_ttl_minutes = %v, want %d", rot["master_ttl_minutes"], defaultMasterTTLMinutes)
	}

	// PUT 0 (выкл) — валидно, записывается и в рантайм, и в файл
	rec2, resp2 := putConfig(t, r, `{"rotation":{"master_ttl_minutes":0}}`, "")
	if rec2.Code != 200 || resp2["ok"] != true {
		t.Fatalf("PUT rotation 0: code=%d resp=%v", rec2.Code, resp2)
	}
	if p := r.cfg.Rotation.MasterTTLMinutes; p == nil || *p != 0 {
		t.Fatalf("runtime rotation must be explicit 0, got %v", p)
	}
	data, err := os.ReadFile(r.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "master_ttl_minutes = 0") {
		t.Fatalf("persisted config must contain explicit 0, got:\n%s", data)
	}

	// PUT 45 — тоже валидно
	rec3, _ := putConfig(t, r, `{"rotation":{"master_ttl_minutes":45}}`, "")
	if rec3.Code != 200 {
		t.Fatalf("PUT rotation 45: %d", rec3.Code)
	}
	if p := r.cfg.Rotation.MasterTTLMinutes; p == nil || *p != 45 {
		t.Fatalf("runtime rotation must be 45, got %v", p)
	}

	// отрицательный и гигантский — 400
	if rec4, _ := putConfig(t, r, `{"rotation":{"master_ttl_minutes":-5}}`, ""); rec4.Code != 400 {
		t.Fatalf("negative TTL must be rejected, got %d", rec4.Code)
	}
	if rec5, _ := putConfig(t, r, `{"rotation":{"master_ttl_minutes":999999}}`, ""); rec5.Code != 400 {
		t.Fatalf("huge TTL must be rejected, got %d", rec5.Code)
	}
}
