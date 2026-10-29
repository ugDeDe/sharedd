package server

// Табличные тесты чистого ядра селекции (selection_core.go). Никакого
// Registry: только view + sink. Поведение зафиксировано как контракт —
// регрессии в приоритетах очереди/загрузки ловятся здесь.
//
// Мульти-мастер: слот = индекс в Assignments[domain]; разные слоты одного
// домена — разные ноды с разными IP. См. specs/multi-master-dns.md.

import (
	"slices"
	"testing"
	"time"

	"sharedd/registry/internal/state"
)

// firstMaster/firstSince — первый слот домена. Хелперы для тестов уровня
// Registry (main/ttl/srmd/…): при M=1 «первый слот» = прежнее одиночное
// значение, сравнения остаются читаемыми.
func firstMaster(r *Registry, domain string) string {
	if s := r.state.Assignments[domain]; len(s) > 0 {
		return s[0]
	}
	return ""
}

func firstSince(r *Registry, domain string) time.Time {
	if s := r.state.AssignmentsSince[domain]; len(s) > 0 {
		return s[0]
	}
	return time.Time{}
}

func mkCand(id string, queuedAt time.Time) *Candidate {
	return &Candidate{NodeID: id, IP: "203.0.113." + id[len(id)-1:], QueuedAt: queuedAt}
}

func newView(cands ...*Candidate) *selectionView {
	st := &selectionView{
		healthy:          map[string]*Candidate{},
		all:              map[string]*Candidate{},
		holds:            map[string][]string{},
		Assignments:      map[string]state.AssignmentSlots{},
		AssignmentsSince: map[string]state.SinceSlots{},
		TTLOverdue:       map[string]bool{},
		Masters:          1,
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

func TestPickSlotCandidateLeastLoaded(t *testing.T) {
	base := time.Unix(1700000000, 0)
	x := mkCand("x", base)
	y := mkCand("y", base.Add(time.Second))
	z := mkCand("z", base.Add(2*time.Second))
	st := newView(x, y, z)
	st.holds["y"] = []string{"d1"}
	st.holds["z"] = []string{"d2"}

	if got := pickSlotCandidate(st, "", ""); got.NodeID != "x" {
		t.Fatalf("least loaded must be x (0 domains), got %q", got.NodeID)
	}
	if got := pickSlotCandidate(st, "", "x"); got.NodeID != "y" {
		t.Fatalf("excluding x must pick y (1 domain, older in queue), got %q", got.NodeID)
	}
}

func TestRotateByTTLOldestHolderSurrenders(t *testing.T) {
	now := time.Unix(1700000000, 0)
	old := mkCand("old", now.Add(-time.Hour))
	fresh := mkCand("fresh", now)
	st := newView(old, fresh)
	st.Assignments["d"] = state.AssignmentSlots{"old"}
	st.AssignmentsSince["d"] = state.SinceSlots{now.Add(-2 * time.Hour)} // age > ttl

	var sink selectionSink
	rotateByTTL(st, []string{"d"}, 30*time.Minute, now, &sink)

	if firstSlot(st.Assignments["d"]) != "fresh" {
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
	st.Assignments["d"] = state.AssignmentSlots{"solo"}
	st.AssignmentsSince["d"] = state.SinceSlots{now.Add(-time.Hour)}

	var sink selectionSink
	rotateByTTL(st, []string{"d"}, 30*time.Minute, now, &sink)

	if firstSlot(st.Assignments["d"]) != "solo" {
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
	st.Assignments["d"] = state.AssignmentSlots{"gone"}

	var sink selectionSink
	reassignDead(st, []string{"d"}, time.Minute, now, &sink)

	if firstSlot(st.Assignments["d"]) != "live" {
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
	st.Assignments["a"] = state.AssignmentSlots{"busy"}
	st.Assignments["b"] = state.AssignmentSlots{"busy"}

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

// Multi-slot: idle-нода получает РОВНО один слот домена, а не весь домен —
// иначе домен на M слотов схлопывается в 1 (DNS-чехарда).
func TestFillEmptyIdleMovesOneSlotOnly(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := &Candidate{NodeID: "a", IP: "10.0.0.1", QueuedAt: now}
	b := &Candidate{NodeID: "b", IP: "10.0.0.2", QueuedAt: now.Add(time.Millisecond)}
	c := &Candidate{NodeID: "c", IP: "10.0.0.3", QueuedAt: now.Add(2 * time.Millisecond)}
	d := &Candidate{NodeID: "d", IP: "10.0.0.4", QueuedAt: now.Add(3 * time.Millisecond)}
	st := newView(a, b, c, d)
	st.Masters = 2
	st.Assignments["d1"] = state.AssignmentSlots{"a", "b"} // M=2
	st.Assignments["d2"] = state.AssignmentSlots{"a", "d"} // у a два домена, c пуст
	st.holds["a"] = []string{"d1", "d2"}
	st.holds["b"] = []string{"d1"}
	st.holds["d"] = []string{"d2"}

	var sink selectionSink
	fillEmpty(st, now, &sink)

	if len(st.Assignments["d1"]) != 2 {
		t.Fatalf("multi-slot domain must not collapse, got %v", st.Assignments["d1"])
	}
	if !slices.Contains(st.Assignments["d1"], "c") {
		t.Fatalf("idle node must receive exactly one slot of d1, got %v", st.Assignments["d1"])
	}
	if len(sink.changes) != 1 {
		t.Fatalf("exactly one change expected, got %+v", sink.changes)
	}
	seen := map[string]bool{}
	for _, id := range st.Assignments["d1"] {
		if h := st.all[id]; h != nil {
			seen[h.IP] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("distinct-IP invariant violated: %v", st.Assignments["d1"])
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

// firstSlot — первый слот или "" (локальный хелпер для view-тестов).
func firstSlot(nodes state.AssignmentSlots) string {
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

// ── обязательные тесты 1–3 (specs/multi-master-dns.md) ──────────────

// Тест 1: fillEmpty раскладывает M=2 разных ноды с РАЗНЫМИ IP; нода с IP,
// уже занятым слотом домена, кандидатом не считается.
func TestFillSlotsDistinctNodesDistinctIPs(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := &Candidate{NodeID: "a", IP: "10.0.0.1", QueuedAt: now}
	b := &Candidate{NodeID: "b", IP: "10.0.0.2", QueuedAt: now.Add(time.Millisecond)}
	c := &Candidate{NodeID: "c", IP: "10.0.0.1", QueuedAt: now.Add(2 * time.Millisecond)} // IP дублирует a
	st := newView(a, b, c)
	st.Masters = 2
	st.Assignments["d"] = state.AssignmentSlots{} // домен есть, слотов нет

	var sink selectionSink
	fillEmpty(st, now, &sink)

	slots := st.Assignments["d"]
	if len(slots) != 2 {
		t.Fatalf("M=2 must fill two slots, got %v", slots)
	}
	if slots[0] == slots[1] {
		t.Fatalf("slots must hold distinct nodes, got %v", slots)
	}
	ipSeen := map[string]bool{}
	for _, id := range slots {
		if c := st.all[id]; c != nil {
			if ipSeen[c.IP] {
				t.Fatalf("duplicate IP %q on domain slots %v", c.IP, slots)
			}
			ipSeen[c.IP] = true
		}
	}
	if len(ipSeen) != 2 {
		t.Fatalf("expected two distinct IPs, got %v", slots)
	}
	if len(sink.changes) != 2 {
		t.Fatalf("two fill changes expected, got %+v", sink.changes)
	}
}

// Тест 1b: если свободные ноды делят IP, слот заполняется один — дубль IP
// не допускается даже ценой неполного набора.
func TestFillSlotsRejectsDuplicateIP(t *testing.T) {
	now := time.Unix(1700000000, 0)
	a := &Candidate{NodeID: "a", IP: "10.0.0.1", QueuedAt: now}
	c := &Candidate{NodeID: "c", IP: "10.0.0.1", QueuedAt: now.Add(time.Millisecond)}
	st := newView(a, c)
	st.Masters = 2
	st.Assignments["d"] = state.AssignmentSlots{}

	var sink selectionSink
	fillEmpty(st, now, &sink)

	if slots := st.Assignments["d"]; len(slots) != 1 {
		t.Fatalf("same-IP holders must not fill the second slot, got %v", slots)
	}
}

// Тест 2: смерть одного из двух мастеров — заменяется ТОЛЬКО его слот,
// живой держатель второго слота не трогается.
func TestReassignDeadReplacesOnlyDeadSlot(t *testing.T) {
	now := time.Unix(1700000000, 0)
	live1 := &Candidate{NodeID: "live1", IP: "10.0.0.1", QueuedAt: now}
	live2 := &Candidate{NodeID: "live2", IP: "10.0.0.3", QueuedAt: now.Add(2 * time.Millisecond)}
	dead := &Candidate{NodeID: "dead", IP: "10.0.0.2"}
	st := newView(live1, live2) // очередь = только живые
	st.all["dead"] = dead       // мёртвая нода ещё в пуле (для IP/причины)
	st.holds["live1"] = []string{"d"}
	st.holds["dead"] = []string{"d"}
	st.Masters = 2
	st.Assignments["d"] = state.AssignmentSlots{"live1", "dead"}
	st.AssignmentsSince["d"] = state.SinceSlots{now, now}

	var sink selectionSink
	reassignDead(st, []string{"d"}, time.Minute, now, &sink)

	slots := st.Assignments["d"]
	if len(slots) != 2 || slots[0] != "live1" {
		t.Fatalf("live slot 0 must stay, got %v", slots)
	}
	if slots[1] != "live2" {
		t.Fatalf("dead slot 1 must be replaced by live2, got %v", slots)
	}
	if len(sink.changes) != 1 || sink.changes[0].FromID != "dead" || sink.changes[0].ToID != "live2" {
		t.Fatalf("exactly the dead slot change expected, got %+v", sink.changes)
	}
}

// Тест 3: TTL считается по слоту — за тик ротируется РОВНО один слот, у
// каждого слота свой AssignmentsSince.
func TestRotateByTTLOneSlotPerTick(t *testing.T) {
	now := time.Unix(1700000000, 0)
	h0 := &Candidate{NodeID: "h0", IP: "10.0.0.1", QueuedAt: now.Add(-2 * time.Hour)}
	h1 := &Candidate{NodeID: "h1", IP: "10.0.0.2", QueuedAt: now.Add(-time.Hour)}
	spare := &Candidate{NodeID: "spare", IP: "10.0.0.3", QueuedAt: now}
	st := newView(h0, h1, spare)
	st.Masters = 2
	recent := now.Add(-time.Minute)
	st.Assignments["d"] = state.AssignmentSlots{"h0", "h1"}
	st.AssignmentsSince["d"] = state.SinceSlots{now.Add(-2 * time.Hour), recent} // слот 0 просрочен, слот 1 свежий

	var sink selectionSink
	rotateByTTL(st, []string{"d"}, 30*time.Minute, now, &sink)

	slots := st.Assignments["d"]
	if slots[0] != "spare" || slots[1] != "h1" {
		t.Fatalf("only slot 0 must rotate, got %v", slots)
	}
	since := st.AssignmentsSince["d"]
	if len(since) != 2 || !since[0].Equal(now) || !since[1].Equal(recent) {
		t.Fatalf("per-slot since wrong: %+v", since)
	}
	if sink.ttlRotations != 1 {
		t.Fatalf("exactly one rotation per tick expected, got %d", sink.ttlRotations)
	}
}

// Тест 6 (паритет M=1): один слот заполняется как прежний одиночный мастер.
func TestFillEmptySingleMasterParity(t *testing.T) {
	now := time.Unix(1700000000, 0)
	solo := mkCand("solo", now)
	st := newView(solo)
	st.Masters = 1
	st.Assignments["d"] = state.AssignmentSlots{}

	var sink selectionSink
	fillEmpty(st, now, &sink)

	if slots := st.Assignments["d"]; len(slots) != 1 || slots[0] != "solo" {
		t.Fatalf("M=1 must fill exactly one slot, got %v", slots)
	}
	if st.slotDetail(0) != "" {
		t.Fatalf("M=1 must not emit slot detail, got %q", st.slotDetail(0))
	}
}
