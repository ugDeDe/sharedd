package server

// Табличные тесты чистого ядра селекции (selection_core.go). Никакого
// Registry: только view + sink. Поведение зафиксировано как контракт —
// регрессии в приоритетах очереди/загрузки ловятся здесь.

import (
	"testing"
	"time"
)

func mkCand(id string, queuedAt time.Time) *Candidate {
	return &Candidate{NodeID: id, IP: "203.0.113." + id[len(id)-1:], QueuedAt: queuedAt}
}

func newView(cands ...*Candidate) *selectionView {
	st := &selectionView{
		healthy:          map[string]*Candidate{},
		all:              map[string]*Candidate{},
		holds:            map[string][]string{},
		Assignments:      map[string]string{},
		AssignmentsSince: map[string]time.Time{},
		TTLOverdue:       map[string]bool{},
	}
	for _, c := range cands {
		st.queue = append(st.queue, c)
		st.healthy[c.NodeID] = c
		st.all[c.NodeID] = c
	}
	sortQueue(st.queue)
	return st
}

func TestSortQueueOrder(t *testing.T) {
	base := time.Unix(1700000000, 0)
	a := mkCand("a", base)
	a.RegisteredAt = base
	b := mkCand("b", base.Add(time.Second))
	b.RegisteredAt = base
	c := &Candidate{NodeID: "c", QueuedAt: base, RegisteredAt: base.Add(-time.Hour)} // тай-брейк по RegisteredAt
	d := &Candidate{NodeID: "d", QueuedAt: base, RegisteredAt: base}                 // дальше тай-брейк по NodeID

	got := []*Candidate{b, d, a, c}
	sortQueue(got)
	want := []string{"c", "a", "d", "b"}
	for i, w := range want {
		if got[i].NodeID != w {
			t.Fatalf("position %d: want %q got %q", i, w, got[i].NodeID)
		}
	}
}

func TestPickLeastLoaded(t *testing.T) {
	base := time.Unix(1700000000, 0)
	x := mkCand("x", base)
	y := mkCand("y", base.Add(time.Second))
	z := mkCand("z", base.Add(2*time.Second))
	st := newView(x, y, z)
	st.holds["y"] = []string{"d1"}
	st.holds["z"] = []string{"d2"}

	if got := pickLeastLoaded(st, ""); got.NodeID != "x" {
		t.Fatalf("least loaded must be x (0 domains), got %q", got.NodeID)
	}
	if got := pickLeastLoaded(st, "x"); got.NodeID != "y" {
		t.Fatalf("excluding x must pick y (1 domain, older in queue), got %q", got.NodeID)
	}
}

func TestRotateByTTLOldestHolderSurrenders(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := mkCand("old", now.Add(-time.Hour))
	fresh := mkCand("fresh", now)
	st := newView(old, fresh)
	st.Assignments["d"] = "old"
	st.AssignmentsSince["d"] = now.Add(-2 * time.Hour) // age > ttl

	var sink selectionSink
	rotateByTTL(st, []string{"d"}, 30*time.Minute, now, &sink)

	if st.Assignments["d"] != "fresh" {
		t.Fatalf("domain must move to fresh holder, got %q", st.Assignments["d"])
	}
	if len(sink.changes) != 1 || sink.changes[0].FromID != "old" || sink.changes[0].ToID != "fresh" {
		t.Fatalf("change not recorded: %+v", sink.changes)
	}
	if sink.switches != 1 || sink.ttlRotations != 1 {
		t.Fatalf("counters: switches=%d ttl=%d", sink.switches, sink.ttlRotations)
	}
	if old.QueuedAt != now {
		t.Fatal("surrendered holder must be moved to queue tail")
	}
}

func TestRotateByTTLNoReplacementKeepsDomain(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solo := mkCand("solo", now)
	st := newView(solo)
	st.Assignments["d"] = "solo"
	st.AssignmentsSince["d"] = now.Add(-time.Hour)

	var sink selectionSink
	rotateByTTL(st, []string{"d"}, 30*time.Minute, now, &sink)

	if st.Assignments["d"] != "solo" {
		t.Fatalf("domain must stay on sole healthy node")
	}
	if !st.TTLOverdue["d"] {
		t.Fatal("overdue flag must arm for one-time log")
	}
	if len(sink.changes) != 0 || sink.switches != 0 {
		t.Fatal("no rotation expected without replacement")
	}
}

func TestReassignDeadMovesOrphan(t *testing.T) {
	now := time.Unix(1700000000, 0)
	live := mkCand("live", now)
	st := newView(live)
	delete(st.all, "gone") // держатель вычищен из пула
	st.Assignments["d"] = "gone"

	var sink selectionSink
	reassignDead(st, []string{"d"}, time.Minute, now, &sink)

	if st.Assignments["d"] != "live" {
		t.Fatalf("orphan domain must go to live node, got %q", st.Assignments["d"])
	}
	if len(sink.changes) != 1 || sink.changes[0].ToID != "live" {
		t.Fatalf("change missing: %+v", sink.changes)
	}
}

func TestFillEmptyMigratesOneDomainToIdle(t *testing.T) {
	now := time.Unix(1700000000, 0)
	busy := mkCand("busy", now)
	idle := mkCand("idle", now.Add(time.Second))
	st := newView(busy, idle)
	st.holds["busy"] = []string{"a", "b"}
	st.Assignments["a"] = "busy"
	st.Assignments["b"] = "busy"

	var sink selectionSink
	fillEmpty(st, now, &sink)

	total := 0
	for _, h := range st.holds {
		total += len(h)
	}
	if total != 2 {
		t.Fatalf("domains lost/gained: %+v", st.holds)
	}
	if len(st.holds["idle"]) != 1 || len(st.holds["busy"]) != 1 {
		t.Fatalf("expected 1+1 split, got busy=%v idle=%v", st.holds["busy"], st.holds["idle"])
	}
	if len(sink.changes) != 1 {
		t.Fatalf("exactly one migration expected, got %d", len(sink.changes))
	}
}

func TestReconcileStintsOpenClose(t *testing.T) {
	now := time.Unix(1700000000, 0)
	master := mkCand("m", now)
	outside := mkCand("o", now)
	cands := map[string]*Candidate{"m": master, "o": outside}
	healthy := map[string]*Candidate{"m": master}
	holds := map[string][]string{"m": {"d"}}

	var sink selectionSink
	reconcileStints(cands, healthy, holds, now, &sink)
	if master.MasterStints != 1 || master.MasterSince.IsZero() {
		t.Fatalf("stint must open: %+v", master)
	}
	if outside.MasterStints != 0 {
		t.Fatal("non-holder must not open stint")
	}

	later := now.Add(10 * time.Minute)
	reconcileStints(cands, map[string]*Candidate{}, holds, later, &sink)
	if !master.MasterSince.IsZero() {
		t.Fatal("unhealthy holder stint must close")
	}
	if len(sink.events) != 1 {
		t.Fatal("empty-queue unhealthy holder must emit MasterLost")
	}
}
