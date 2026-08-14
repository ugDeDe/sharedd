package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// V7.8: статистика пула по журналу событий — баны GP (recovered ⇒ не бан),
// периодичность банов и DNS-смен, среднее время мастерства.
func TestBuildPanelStats(t *testing.T) {
	r := newTestRegistry(t)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	add := func(atSec int, ev Event) {
		r.mu.Lock()
		ev.At = base.Add(time.Duration(atSec) * time.Second)
		r.addEventLocked(ev)
		r.mu.Unlock()
	}

	// nA: blocked@0 → recovered@5 ⇒ НЕ бан (самоустранившийся сбой)
	add(0, Event{Type: EventGlobalpingBlocked, NodeID: "nA", IP: "1.1.1.1"})
	add(5, Event{Type: EventGlobalpingRecovered, NodeID: "nA"})
	// nB: blocked@10, recovered нет ⇒ активный бан
	add(10, Event{Type: EventGlobalpingBlocked, NodeID: "nB", IP: "2.2.2.2", Detail: "verified ratio 0.10"})
	// nC: blocked@20 (+ повторный @25 — начало НЕ сдвигается), expired@30 ⇒ бан 10с
	add(20, Event{Type: EventGlobalpingBlocked, NodeID: "nC", IP: "3.3.3.3"})
	add(25, Event{Type: EventGlobalpingBlocked, NodeID: "nC", IP: "3.3.3.3"})
	add(30, Event{Type: EventNodeExpired, NodeID: "nC"})
	// DNS-смены: @40, @70, @130 ⇒ средний промежуток 45с; агрегированный
	// manual-push (без домена) в периодичность НЕ входит
	add(40, Event{Type: EventDNSUpdated, Domain: "d1.example.com"})
	add(70, Event{Type: EventDNSUpdated, Domain: "d1.example.com"})
	add(130, Event{Type: EventDNSUpdated, Domain: "d1.example.com"})
	add(135, Event{Type: EventDNSUpdated, Detail: "d1.example.com → 1.2.3.4 (manual push)"})
	// мастерство: закрытый stint 60с (elected@100 → lost@160) + открытый
	// elected@540 (60с до now=base+600) ⇒ среднее 60с
	add(100, Event{Type: EventMasterElected, NodeID: "m1", Domain: "d1.example.com"})
	add(160, Event{Type: EventMasterLost, NodeID: "m1", Domain: "d1.example.com"})
	add(540, Event{Type: EventMasterElected, NodeID: "m2", Domain: "d1.example.com"})

	now := base.Add(600 * time.Second)
	r.mu.RLock()
	st := r.buildPanelStatsLocked(now)
	r.mu.RUnlock()

	if st.EventsWindow != 13 {
		t.Fatalf("events window: 13 expected, got %d", st.EventsWindow)
	}
	if st.GPBansTotal != 2 {
		t.Fatalf("bans: 2 expected (nC closed + nB active), got %d", st.GPBansTotal)
	}
	if st.GPBansActive != 1 {
		t.Fatalf("active bans: 1 expected, got %d", st.GPBansActive)
	}
	if st.GPBansNodes != 2 {
		t.Fatalf("banned nodes: 2 expected (nA recovered — не бан), got %d", st.GPBansNodes)
	}
	if st.GPBanIntervalSec == nil || *st.GPBanIntervalSec != 10 {
		t.Fatalf("ban interval: 10s expected (starts @10/@20), got %v", st.GPBanIntervalSec)
	}
	if st.DNSSwitchIntervalSec == nil || *st.DNSSwitchIntervalSec != 45 {
		t.Fatalf("dns interval: 45s expected (span 90 / 2 gaps), got %v", st.DNSSwitchIntervalSec)
	}
	if st.MasterAvgSec == nil || *st.MasterAvgSec != 60 {
		t.Fatalf("master avg: 60s expected (60 closed + 60 open), got %v", st.MasterAvgSec)
	}

	// история новыми вперёд: nC (старт @20) идёт первым, nB (@10) — вторым
	if len(st.GPBanHistory) != 2 {
		t.Fatalf("ban history: 2 entries expected, got %+v", st.GPBanHistory)
	}
	b0 := st.GPBanHistory[0]
	if b0.NodeID != "nC" || b0.Active || b0.DurationSec != 10 || b0.ClosedBy != EventNodeExpired {
		t.Fatalf("history[0] must be closed nC ban (10s, node_expired), got %+v", b0)
	}
	b1 := st.GPBanHistory[1]
	if b1.NodeID != "nB" || !b1.Active || b1.DurationSec != 590 || b1.Cause != "verified ratio 0.10" {
		t.Fatalf("history[1] must be active nB ban (590s ongoing, cause kept), got %+v", b1)
	}
	if !b1.Active {
		t.Fatal("active ban must stay active")
	}
}

// V7.8: master_lost БЕЗ домена (stint-reconcile «no healthy replacement»)
// закрывает открытый stint ноды — иначе мастерство завышалось бы.
func TestPanelStatsDomainlessMasterLost(t *testing.T) {
	r := newTestRegistry(t)
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	add := func(atSec int, ev Event) {
		r.mu.Lock()
		ev.At = base.Add(time.Duration(atSec) * time.Second)
		r.addEventLocked(ev)
		r.mu.Unlock()
	}
	add(0, Event{Type: EventMasterElected, NodeID: "m", Domain: "d1.example.com"})
	add(120, Event{Type: EventMasterLost, NodeID: "m"}) // без домена!

	r.mu.RLock()
	st := r.buildPanelStatsLocked(base.Add(300 * time.Second))
	r.mu.RUnlock()
	if st.MasterAvgSec == nil || *st.MasterAvgSec != 120 {
		t.Fatalf("domainless master_lost must close the stint at 120s, got %v", st.MasterAvgSec)
	}
}

// V7.8: пустой журнал — метрики отсутствуют (nil), банов нет, всё нули.
func TestPanelStatsEmptyJournal(t *testing.T) {
	r := newTestRegistry(t)
	r.mu.RLock()
	st := r.buildPanelStatsLocked(time.Now())
	r.mu.RUnlock()
	if st.EventsWindow != 0 || st.GPBansTotal != 0 || st.GPBansNodes != 0 || st.GPBansActive != 0 {
		t.Fatalf("empty journal must give zeroed bans, got %+v", st)
	}
	if st.GPBanIntervalSec != nil || st.DNSSwitchIntervalSec != nil || st.MasterAvgSec != nil {
		t.Fatalf("empty journal: intervals must be nil (dash in panel), got %+v", st)
	}
	// одиночные события — периодичности тоже нет
	r.mu.Lock()
	r.addEventLocked(Event{Type: EventDNSUpdated, Domain: "d"})
	r.mu.Unlock()
	r.mu.RLock()
	st = r.buildPanelStatsLocked(time.Now())
	r.mu.RUnlock()
	if st.DNSSwitchIntervalSec != nil {
		t.Fatal("single DNS switch — no interval to compute")
	}
}

// V7.8: overview отдаёт stats блоком; панельный эндпоинт несёт его в JSON.
func TestOverviewCarriesStats(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	r.mu.Lock()
	r.addEventLocked(Event{
		At: time.Now().Add(-time.Minute), Type: EventGlobalpingBlocked,
		NodeID: "nB", IP: "2.2.2.2",
	})
	r.mu.Unlock()

	mux := r.buildMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("overview: %d", rec.Code)
	}
	var resp struct {
		Stats struct {
			EventsWindow int `json:"events_window"`
			GPBansTotal  int `json:"gp_bans_total"`
			GPBansActive int `json:"gp_bans_active"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if resp.Stats.GPBansTotal != 1 || resp.Stats.GPBansActive != 1 || resp.Stats.EventsWindow == 0 {
		t.Fatalf("stats in overview: %+v", resp.Stats)
	}
}
