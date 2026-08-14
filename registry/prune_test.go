package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// V7.9.6: рипер неактивных — нода, непрерывно вне очереди здоровых дольше
// prune_unhealthy_min, удаляется из пула (живой агент с мёртвым прокси
// больше не висит в списке вечно, «вся красная»).

func TestResolvePruneUnhealthyMinutes(t *testing.T) {
	if got := resolvePruneUnhealthyMinutes(nil); got != defaultPruneUnhealthyMinutes {
		t.Fatalf("nil must resolve to default %d, got %d", defaultPruneUnhealthyMinutes, got)
	}
	if got := resolvePruneUnhealthyMinutes(ptrInt(0)); got != 0 {
		t.Fatalf("explicit 0 = off, got %d", got)
	}
	if got := resolvePruneUnhealthyMinutes(ptrInt(90)); got != 90 {
		t.Fatalf("90 must pass through, got %d", got)
	}
}

func TestPruneUnhealthyNode(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "sick", IP: "1.1.1.1"})
	c := r.state.Candidates["sick"]

	// агент жив (свежий heartbeat), но всё красное уже 2 часа
	c.LastHeartbeat = time.Now()
	c.UnhealthySince = time.Now().Add(-2 * time.Hour)

	r.sweepExpired(time.Now())
	if _, ok := r.state.Candidates["sick"]; ok {
		t.Fatal("node unhealthy for 2h (window 1h) must be pruned")
	}
	if !hasEvent(eventTypes(r), EventNodePruned) {
		t.Fatalf("node_pruned event expected, got %v", eventTypes(r))
	}
	// detail должна объяснять причину и длительность
	for i := len(r.state.Events) - 1; i >= 0; i-- {
		ev := r.state.Events[i]
		if ev.Type == EventNodePruned {
			if !strings.Contains(ev.Detail, "2h") {
				t.Fatalf("prune detail must carry the unhealthy duration, got %q", ev.Detail)
			}
			break
		}
	}
}

func TestPruneKeepsRecentlyUnhealthyAndHealthy(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "fresh-sick", IP: "1.1.1.1"})
	r.register(registerRequest{NodeID: "fit", IP: "2.2.2.2"})

	// нездорова, но окно не вышло — живёт
	r.state.Candidates["fresh-sick"].LastHeartbeat = time.Now()
	r.state.Candidates["fresh-sick"].UnhealthySince = time.Now().Add(-30 * time.Minute)
	// здорова и в очереди — рипер не трогает никогда, даже со старым UnhealthySince
	// (у здоровой UnhealthySince нулевой; гарантируем семантику)
	makeFullyHealthyNow(r, "fit")
	r.state.Candidates["fit"].LastHeartbeat = time.Now()

	r.sweepExpired(time.Now())
	if _, ok := r.state.Candidates["fresh-sick"]; !ok {
		t.Fatal("recently unhealthy node must survive (30m < 1h window)")
	}
	if _, ok := r.state.Candidates["fit"]; !ok {
		t.Fatal("healthy node must never be pruned")
	}
}

func TestPruneDisabledKeepsNode(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PruneUnhealthyTTL = 0 // явный 0 = выключен

	r.register(registerRequest{NodeID: "sick", IP: "1.1.1.1"})
	c := r.state.Candidates["sick"]
	c.LastHeartbeat = time.Now()
	c.UnhealthySince = time.Now().Add(-72 * time.Hour)

	r.sweepExpired(time.Now())
	if _, ok := r.state.Candidates["sick"]; !ok {
		t.Fatal("prune disabled (0) must keep even long-dead nodes")
	}
	if hasEvent(eventTypes(r), EventNodePruned) {
		t.Fatal("no node_pruned event expected while disabled")
	}
}

func TestHeartbeatExpiryStillWorks(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "ghost", IP: "1.1.1.1"})
	// heartbeat протух (агент умер) — классический путь удаления, рипер не нужен
	r.state.Candidates["ghost"].LastHeartbeat = time.Now().Add(-5 * time.Minute)

	r.sweepExpired(time.Now())
	if _, ok := r.state.Candidates["ghost"]; ok {
		t.Fatal("heartbeat-expired node must be removed")
	}
	if !hasEvent(eventTypes(r), EventNodeExpired) {
		t.Fatalf("node_expired event expected, got %v", eventTypes(r))
	}
}

// Миграция поля: нода была нездорова на момент апгрейда (UnhealthySince в
// state ещё не существовало) — часы рипера стартуют с ближайшего evaluate.
func TestUnhealthySinceLazyInit(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "old-sick", IP: "1.1.1.1"})
	c := r.state.Candidates["old-sick"]
	// нездорова (ничего не поднимали), FullyHealthy=false, UnhealthySince=ноль
	if !c.UnhealthySince.IsZero() {
		t.Fatal("precondition: UnhealthySince must be zero")
	}
	r.evaluateAssignments(time.Now())
	if c.UnhealthySince.IsZero() {
		t.Fatal("evaluate must lazily arm UnhealthySince for pre-existing unhealthy nodes")
	}

	// выздоровела — часы обнуляются
	makeFullyHealthyNow(r, "old-sick")
	r.evaluateAssignments(time.Now())
	if !c.UnhealthySince.IsZero() {
		t.Fatal("UnhealthySince must be cleared when the node rejoins the healthy queue")
	}
}

// Живой агент удалённой (prune/expiry) ноды ДОЛЖЕН узнать об удалении:
// heartbeat по неизвестному node_id → 410, агент на любой !=200
// пере-регистрируется. Без этого prune убивал бы живую ноду навсегда.
func TestHeartbeatUnknownNodeGetsGone(t *testing.T) {
	r := newTestRegistry(t)
	r.register(registerRequest{NodeID: "alive", IP: "1.1.1.1"})

	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()

	post := func(id string) int {
		resp, err := http.Post(srv.URL+"/heartbeat", "application/json",
			strings.NewReader(`{"node_id":"`+id+`"}`))
		if err != nil {
			t.Fatalf("heartbeat post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := post("alive"); got != http.StatusOK {
		t.Fatalf("known node heartbeat must be 200, got %d", got)
	}
	if got := post("ghost"); got != http.StatusGone {
		t.Fatalf("unknown node heartbeat must be 410 Gone, got %d", got)
	}

	// пере-регистрация после 410 снова делает heartbeat валидным
	r.register(registerRequest{NodeID: "ghost", IP: "2.2.2.2"})
	if got := post("ghost"); got != http.StatusOK {
		t.Fatalf("re-registered node heartbeat must be 200, got %d", got)
	}
}

// ---------- V7.9.7: карантин после prune ----------

func TestPruneBanSchedule(t *testing.T) {
	want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 3 * time.Hour, 3 * time.Hour}
	for i, w := range want {
		if got := pruneBanFor(i + 1); got != w {
			t.Fatalf("strike %d: want %v, got %v", i+1, w, got)
		}
	}
	if got := pruneBanFor(0); got != 15*time.Minute {
		t.Fatalf("strike 0 must clamp to first step, got %v", got)
	}
	if got := pruneBanFor(64); got != 3*time.Hour {
		t.Fatalf("huge strike must clamp to cap, got %v", got)
	}
}

func pruneSickNode(t *testing.T, r *Registry, id, ip string) {
	t.Helper()
	r.register(registerRequest{NodeID: id, IP: ip})
	c := r.state.Candidates[id]
	c.LastHeartbeat = time.Now()
	c.UnhealthySince = time.Now().Add(-2 * time.Hour)
	r.sweepExpired(time.Now())
	if _, ok := r.state.Candidates[id]; ok {
		t.Fatalf("precondition: %s must be pruned", id)
	}
}

// prune кладёт карантин: немедленная пере-регистрация отклоняется
// (вертящаяся дверь V7.9.6 закрыта).
func TestPruneCreatesTombstoneAndBlocksRegister(t *testing.T) {
	r := newTestRegistry(t)
	pruneSickNode(t, r, "dead", "1.1.1.1")

	tb := r.state.PruneStrikes["dead"]
	if tb == nil {
		t.Fatal("tombstone must be created on prune")
	}
	if tb.Strikes != 1 || tb.IP != "1.1.1.1" {
		t.Fatalf("tombstone = %+v, want strikes=1 ip set", tb)
	}
	until, banned := r.register(registerRequest{NodeID: "dead", IP: "1.1.1.1"})
	if !banned {
		t.Fatal("immediate re-register after prune must be rejected (429 path)")
	}
	if d := time.Until(until); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("first strike ban must be ~15m, got %v", d)
	}
	if _, ok := r.state.Candidates["dead"]; ok {
		t.Fatal("banned register must NOT create a candidate entry")
	}
	// в detail prune-события — длина карантина и номер strike
	// (забаненная регистрация новых событий не плодит — проверяется тем,
	// что последним событием остаётся node_pruned)
	last := r.state.Events[len(r.state.Events)-1]
	if last.Type != EventNodePruned || !strings.Contains(last.Detail, "banned for 15m0s") || !strings.Contains(last.Detail, "strike 1") {
		t.Fatalf("prune detail must carry ban duration and strike, got %q", last.Detail)
	}
}

// Серия растёт через циклы prune (агент пережил карантин, так и не вылечился):
// strike 2 → карантин 30 минут.
func TestTombstoneEscalatesAcrossCycles(t *testing.T) {
	r := newTestRegistry(t)
	pruneSickNode(t, r, "dead", "1.1.1.1")

	// карантин истёк — регистрация снова пускает
	tb := r.state.PruneStrikes["dead"]
	tb.BannedUntil = time.Now().Add(-time.Minute)
	if _, banned := r.register(registerRequest{NodeID: "dead", IP: "1.1.1.1"}); banned {
		t.Fatal("register must pass after ban expiry")
	}
	pruneSickNode(t, r, "dead", "1.1.1.1") // вторая порция: 2 часа красного → prune
	// pruneSickNode регистрирует заново — но карантин strike 1 истёк выше, ок.
	tb2 := r.state.PruneStrikes["dead"]
	if tb2.Strikes != 2 {
		t.Fatalf("strikes must escalate to 2, got %d", tb2.Strikes)
	}
	if d := time.Until(tb2.BannedUntil); d < 29*time.Minute || d > 31*time.Minute {
		t.Fatalf("strike 2 ban must be ~30m, got %v", d)
	}
}

// Переустановка агента = новый node_id, тот же IP — карантин и серия
// наследуются по IP, без обнуления.
func TestTombstoneInheritedByIP(t *testing.T) {
	r := newTestRegistry(t)
	pruneSickNode(t, r, "dead-old", "1.1.1.1")

	if until, banned := r.register(registerRequest{NodeID: "dead-new", IP: "1.1.1.1"}); !banned {
		t.Fatal("same-IP re-register with fresh node_id must inherit the ban")
	} else if !until.Equal(r.state.PruneStrikes["dead-old"].BannedUntil) {
		t.Fatal("inherited ban should come from the old-IP tombstone")
	}

	// серия тоже наследуется: prune нового id = strike 2, а не 1
	tb := r.state.PruneStrikes["dead-old"]
	tb.BannedUntil = time.Now().Add(-time.Minute)
	pruneSickNode(t, r, "dead-new", "1.1.1.1")
	if got := r.state.PruneStrikes["dead-new"].Strikes; got != 2 {
		t.Fatalf("IP-inherited strike series must continue: want 2, got %d", got)
	}
	if _, ok := r.state.PruneStrikes["dead-old"]; ok {
		t.Fatal("weaker same-IP tombstone must be absorbed by the stronger one")
	}
}

// Нода вылечилась и реально вошла в очередь здоровых — серия обнуляется.
func TestHealthyJoinClearsTombstone(t *testing.T) {
	r := newTestRegistry(t)
	pruneSickNode(t, r, "dead", "1.1.1.1")

	tb := r.state.PruneStrikes["dead"]
	tb.BannedUntil = time.Now().Add(-time.Minute)
	r.register(registerRequest{NodeID: "dead", IP: "1.1.1.1"})
	makeFullyHealthyNow(r, "dead")
	r.evaluateAssignments(time.Now())

	if len(r.state.PruneStrikes) != 0 {
		t.Fatalf("healthy join must clear prune tombstones, still have %+v", r.state.PruneStrikes)
	}
}

// HTTP-слой: 429 + Retry-After + тело с retry_after_sec для агента V7.9.7+.
func TestRegisterEndpointReturns429DuringBan(t *testing.T) {
	r := newTestRegistry(t)
	r.state.PruneStrikes = map[string]*PruneTombstone{
		"dead": {NodeID: "dead", IP: "1.1.1.1", Strikes: 2, LastPruned: time.Now(),
			BannedUntil: time.Now().Add(30 * time.Minute)},
	}
	srv := httptest.NewServer(r.buildMux())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"node_id":"dead","ip":"1.1.1.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("banned register must be 429, got %d", resp.StatusCode)
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		t.Fatal("429 must carry Retry-After header")
	}
	// после дедлайна — обычный 200
	r.state.PruneStrikes["dead"].BannedUntil = time.Now().Add(-time.Minute)
	resp2, err := http.Post(srv.URL+"/register", "application/json",
		strings.NewReader(`{"node_id":"dead","ip":"1.1.1.1"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("register after ban expiry must be 200, got %d", resp2.StatusCode)
	}
}
