package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"sharedd/registry/internal/machineapi"
)

const testPoolTag = "00112233445566778899aabbccddeeff"

func TestAdPauseQuota(t *testing.T) {
	period := 10 * time.Minute
	for _, tc := range []struct {
		daily time.Duration
		nodes int
		want  time.Duration
	}{
		{50 * time.Minute, 10, 10 * time.Minute},  // 5 мин на ноду → одно окно
		{300 * time.Minute, 10, 30 * time.Minute}, // ровно три окна
		{310 * time.Minute, 10, 40 * time.Minute}, // 31 мин → вверх до четырёх
		{5 * time.Minute, 1, 10 * time.Minute},    // меньше окна → окно
		{60 * time.Minute, 0, 0},
	} {
		if got := adPauseQuota(tc.daily, period, tc.nodes); got != tc.want {
			t.Fatalf("quota(%v, %d nodes) = %v, want %v", tc.daily, tc.nodes, got, tc.want)
		}
	}
}

func adPauseRegistry(t *testing.T, minutesPerDay int) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	on := true
	r.cfg.AdPause.Enabled = &on
	r.cfg.AdPause.Tag = testPoolTag
	r.cfg.AdPause.MinutesPerDay = minutesPerDay
	ttl := 10
	r.cfg.Rotation.MasterTTLMinutes = &ttl
	return r
}

// adPauseConfirm — агент подтверждает желаемое состояние (как heartbeat).
func adPauseConfirm(r *Registry, now time.Time) {
	for _, c := range r.state.Candidates {
		if !adPauseCapable(c) {
			continue
		}
		if c.AdActive {
			r.noteAdPauseStateLocked(c, machineapi.AdPauseStatePool, now)
		} else {
			r.noteAdPauseStateLocked(c, machineapi.AdPauseStateOwn, now)
		}
	}
}

// Полдень фиксированного дня в зоне регистратора: детерминированно и далеко
// от полуночи.
func adPauseNoon() time.Time {
	return time.Date(2026, 11, 7, 12, 0, 0, 0, time.Local)
}

func TestAdPauseScheduleOnlyInsideMasteryAndCapable(t *testing.T) {
	r := adPauseRegistry(t, 60)
	now := adPauseNoon()
	r.state.Candidates["m"] = &Candidate{NodeID: "m", MasterSince: now.Add(-time.Minute), AdState: machineapi.AdPauseStateOwn}
	r.state.Candidates["idle"] = &Candidate{NodeID: "idle", AdState: machineapi.AdPauseStateOwn}
	r.state.Candidates["old"] = &Candidate{NodeID: "old", MasterSince: now.Add(-time.Minute),
		AdState: machineapi.AdPauseStateUnsupported}
	r.state.Candidates["new"] = &Candidate{NodeID: "new", MasterSince: now.Add(-time.Minute)} // агент ещё не подтвердил
	st := r.adPauseSettings()
	if !r.scheduleAdPauseLocked(now, st) {
		t.Fatal("first decision must change state")
	}
	for id, want := range map[string]bool{"m": true, "idle": false, "old": false, "new": false} {
		if r.state.Candidates[id].AdActive != want {
			t.Fatalf("%s: active=%v want %v", id, r.state.Candidates[id].AdActive, want)
		}
	}
	// Внутри окна решение не пересматривается.
	if r.scheduleAdPauseLocked(now.Add(5*time.Minute), st) {
		t.Fatal("decision must hold for the whole window")
	}
	// Конец мастерства снимает рекламу сразу.
	r.state.Candidates["m"].MasterSince = time.Time{}
	r.scheduleAdPauseLocked(now.Add(6*time.Minute), st)
	if r.state.Candidates["m"].AdActive {
		t.Fatal("ad must stop when mastery ends")
	}
	// Выключение паузы снимает рекламу у всех.
	r.state.Candidates["m"].MasterSince = now.Add(7 * time.Minute)
	off := false
	r.cfg.AdPause.Enabled = &off
	r.scheduleAdPauseLocked(now.Add(8*time.Minute), r.adPauseSettings())
	if r.state.Candidates["m"].AdActive {
		t.Fatal("disabled ad pause must not keep the pool tag")
	}
}

// adPauseRun — сутки мастерства без перерыва у всех нод: окна по 10 минут
// с подтверждением; возвращает рекламные окна каждой ноды по порядку.
func adPauseRun(t *testing.T, r *Registry, from time.Time, windows int) map[string][]bool {
	t.Helper()
	got := map[string][]bool{}
	st := r.adPauseSettings()
	for i := 0; i < windows; i++ {
		now := from.Add(time.Duration(i) * st.period)
		adPauseConfirm(r, now) // закрыть прошлое окно фактическим временем
		r.scheduleAdPauseLocked(now, st)
		adPauseConfirm(r, now)
		for id, c := range r.state.Candidates {
			got[id] = append(got[id], c.AdActive)
		}
	}
	return got
}

func countTrue(xs []bool) int {
	n := 0
	for _, x := range xs {
		if x {
			n++
		}
	}
	return n
}

// 50 минут на 10 нод: каждой по одному окну, итого 50 минут — и тишина.
func TestAdPauseBudgetSplitsEvenlyAndStops(t *testing.T) {
	r := adPauseRegistry(t, 50)
	now := adPauseNoon()
	ids := []string{"n0", "n1", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9"}
	for _, id := range ids {
		r.state.Candidates[id] = &Candidate{NodeID: id, MasterSince: now, AdState: machineapi.AdPauseStateOwn}
	}
	got := adPauseRun(t, r, now, 12)
	total := 0
	for _, id := range ids {
		n := countTrue(got[id])
		if n > 1 {
			t.Fatalf("%s got %d windows, quota is one", id, n)
		}
		total += n
	}
	if total != 5 {
		t.Fatalf("pool budget 50 min = 5 windows, got %d", total)
	}
	_, sum := r.adPauseStatusSnapshot(now.Add(12 * 10 * time.Minute))
	if sum.UsedSec < 5*600-60 || sum.UsedSec > 5*600 || sum.Nodes != 10 {
		t.Fatalf("summary: %+v", sum)
	}
}

// 30 минут на ноду при окне 10: реклама, своё, реклама, своё, реклама.
func TestAdPauseAlternatesWithinQuota(t *testing.T) {
	r := adPauseRegistry(t, 60)
	now := adPauseNoon()
	r.state.Candidates["a"] = &Candidate{NodeID: "a", MasterSince: now, AdState: machineapi.AdPauseStateOwn}
	r.state.Candidates["b"] = &Candidate{NodeID: "b", AdState: machineapi.AdPauseStateOwn}
	got := adPauseRun(t, r, now, 8)["a"]
	want := []bool{true, false, true, false, true, false, false, false}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("windows %v, want %v", got, want)
		}
	}
}

// Ушла нода — её доля перераспределяется на оставшиеся.
func TestAdPauseQuotaFollowsNodeCount(t *testing.T) {
	r := adPauseRegistry(t, 40)
	now := adPauseNoon()
	for _, id := range []string{"a", "b", "c", "d"} {
		r.state.Candidates[id] = &Candidate{NodeID: id, AdState: machineapi.AdPauseStateOwn}
	}
	r.state.Candidates["a"].MasterSince = now
	if got := countTrue(adPauseRun(t, r, now, 6)["a"]); got != 1 {
		t.Fatalf("4 nodes × 10 min: a got %d windows", got)
	}
	delete(r.state.Candidates, "c")
	delete(r.state.Candidates, "d")
	// 40 мин на две ноды: у a теперь доля 20 минут — ещё одно окно.
	if got := countTrue(adPauseRun(t, r, now.Add(time.Hour), 6)["a"]); got != 1 {
		t.Fatalf("2 nodes × 20 min: a must get one more window, got %d", got)
	}
}

// Новые сутки — новый бюджет.
func TestAdPauseBudgetResetsAtMidnight(t *testing.T) {
	r := adPauseRegistry(t, 10)
	day := time.Date(2026, 11, 7, 23, 30, 0, 0, time.Local)
	r.state.Candidates["a"] = &Candidate{NodeID: "a", MasterSince: day, AdState: machineapi.AdPauseStateOwn}
	if got := countTrue(adPauseRun(t, r, day, 3)["a"]); got != 1 {
		t.Fatalf("day 1: %d windows", got)
	}
	if got := countTrue(adPauseRun(t, r, day.Add(time.Hour), 3)["a"]); got != 1 {
		t.Fatalf("day 2 must get a fresh window, got %d", got)
	}
}

func heartbeat(t *testing.T, r *Registry, body string) machineapi.HeartbeatResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	r.buildMux().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/heartbeat", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat %s: %d %s", body, rec.Code, rec.Body.String())
	}
	var resp machineapi.HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("heartbeat body: %v", err)
	}
	return resp
}

func TestAdPauseHeartbeatDirectiveAndConfirmedAccounting(t *testing.T) {
	r := adPauseRegistry(t, 60)
	r.state.Candidates["node-a1b2c"] = &Candidate{NodeID: "node-a1b2c", IP: "1.2.3.4", AdActive: true}

	resp := heartbeat(t, r, `{"node_id":"node-a1b2c"}`) // старый агент: поле не шлёт
	if resp.AdPause == nil || !resp.AdPause.Active || resp.AdPause.Tag != testPoolTag {
		t.Fatalf("directive must carry the pool tag: %+v", resp.AdPause)
	}
	if c := r.state.Candidates["node-a1b2c"]; !c.AdSince.IsZero() || c.AdState != "" {
		t.Fatal("without confirmation nothing may be counted")
	}

	heartbeat(t, r, `{"node_id":"node-a1b2c","ad_pause_state":"pool"}`)
	c := r.state.Candidates["node-a1b2c"]
	if c.AdSince.IsZero() || c.AdState != machineapi.AdPauseStatePool {
		t.Fatalf("confirmed pool tag must open a span: %+v", c)
	}
	c.AdSince = c.AdSince.Add(-90 * time.Second) // время прошло
	c.AdActive = false
	resp = heartbeat(t, r, `{"node_id":"node-a1b2c","ad_pause_state":"own"}`)
	if resp.AdPause == nil || resp.AdPause.Active {
		t.Fatalf("directive must return the own tag: %+v", resp.AdPause)
	}
	if c.AdSeconds < 90 || c.AdDaySec < 90 || !c.AdSince.IsZero() {
		t.Fatalf("closed span must be counted: sec=%d since=%v", c.AdSeconds, c.AdSince)
	}
	var on, off int
	for _, ev := range r.state.Events {
		switch ev.Type {
		case "ad_pause_on":
			on++
		case "ad_pause_off":
			off++
		}
	}
	if on != 1 || off != 1 {
		t.Fatalf("each switch must be journaled once: on=%d off=%d", on, off)
	}
}

func TestAdPausePanelValidation(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	rec, _ := putConfig(t, r, `{"ad_pause":{"enabled":true,"tag":" 00112233445566778899AABBCCDDEEFF ","minutes_per_day":45}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT ad_pause: %d %s", rec.Code, rec.Body.String())
	}
	if r.cfg.AdPause.Tag != testPoolTag || r.cfg.AdPause.MinutesPerDay != 45 || !*r.cfg.AdPause.Enabled {
		t.Fatalf("ad_pause not applied: %+v", r.cfg.AdPause)
	}
	for _, body := range []string{
		`{"ad_pause":{"enabled":true,"tag":"","minutes_per_day":10}}`,
		`{"ad_pause":{"enabled":true,"tag":"00000000000000000000000000000000","minutes_per_day":10}}`,
		`{"ad_pause":{"enabled":false,"tag":"","minutes_per_day":0}}`,
		`{"ad_pause":{"enabled":false,"tag":"","minutes_per_day":1441}}`,
		`{"ad_pause":{"enabled":false,"tag":"","share_percent":10}}`,
	} {
		if rec, _ := putConfig(t, r, body, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid ad_pause %s accepted: %d", body, rec.Code)
		}
	}
}
