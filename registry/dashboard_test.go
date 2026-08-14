package main

import (
	"path/filepath"
	"testing"
	"time"
)

// V7.9.11: storage и агрегации вечной истории.

func TestHistoryDBGapChainAndRotation(t *testing.T) {
	db := openHistoryDB(filepath.Join(t.TempDir(), "hist.db"))
	if db == nil {
		t.Fatal("db must open")
	}
	defer db.Close()

	now := time.Now()
	db.recordBan(banRow{TS: now.Add(-3 * time.Hour), NodeID: "n1", IP: "1.1.1.1", Reason: BanReasonIPBan, LifetimeSec: 100})
	db.recordBan(banRow{TS: now.Add(-2 * time.Hour), NodeID: "n2", IP: "2.2.2.2", Reason: BanReasonDead, LifetimeSec: -1})
	db.recordBan(banRow{TS: now.Add(-1 * time.Hour), NodeID: "n3", IP: "3.3.3.3", Reason: BanReasonIPBan, LifetimeSec: 200})

	rows, err := db.bansSince(now.Add(-4*time.Hour), "")
	if err != nil || len(rows) != 3 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].GapSec != -1 {
		t.Fatalf("first ban must have gap=-1, got %d", rows[0].GapSec)
	}
	if rows[1].GapSec != 3600 || rows[2].GapSec != 3600 {
		t.Fatalf("gap chain broken: %+v", rows)
	}
	// фильтр по типу
	gp, _ := db.bansSince(now.Add(-4*time.Hour), BanReasonIPBan)
	if len(gp) != 2 || gp[0].NodeID != "n1" || gp[1].NodeID != "n3" {
		t.Fatalf("reason filter broken: %+v", gp)
	}

	// события: зеркалирование + ротация; баны ротация НЕ трогает
	old := Event{At: now.Add(-40 * 24 * time.Hour), Type: EventNodeRegistered, NodeID: "old", IP: "9.9.9.9"}
	fresh := Event{At: now, Type: EventNodeRegistered, NodeID: "fresh", IP: "9.9.9.8"}
	db.recordEvent(old)
	db.recordEvent(fresh)
	db.pruneEvents(now.Add(-30 * 24 * time.Hour))
	var evLeft int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&evLeft); err != nil || evLeft != 1 {
		t.Fatalf("retention must keep only the fresh event, left=%d err=%v", evLeft, err)
	}
	var bansLeft int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM bans`).Scan(&bansLeft); err != nil || bansLeft != 3 {
		t.Fatalf("bans must NEVER rotate, left=%d err=%v", bansLeft, err)
	}
}

// Три метрики дашборда (все — только по банам GP): число, периодичность,
// среднее время жизни; окна и бакеты.
func TestDashboardAggregates(t *testing.T) {
	r := newTestRegistry(t)
	r.db = openHistoryDB(filepath.Join(t.TempDir(), "hist.db"))
	defer r.db.Close()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// три GP-бана сегодня: 01:10 (life 2h), 05:10 (life 1h), 06:40 (life 0.5h)
	seed := []struct {
		at   time.Time
		life int64
		node string
	}{
		{today.Add(70 * time.Minute), 7200, "n1"},
		{today.Add(5*time.Hour + 10*time.Minute), 3600, "n2"},
		{today.Add(6*time.Hour + 40*time.Minute), 1800, "n3"},
	}
	for _, s := range seed {
		r.db.recordBan(banRow{TS: s.at, NodeID: s.node, IP: "10.0.0.7", Reason: BanReasonIPBan, LifetimeSec: s.life})
	}
	// dead-бан не участвует в метриках GP
	r.db.recordBan(banRow{TS: today.Add(8 * time.Hour), NodeID: "nd", IP: "10.0.0.9", Reason: BanReasonDead, LifetimeSec: 600})
	// бан вчера — за окном today
	r.db.recordBan(banRow{TS: today.Add(-time.Hour), NodeID: "old", IP: "10.0.0.1", Reason: BanReasonIPBan, LifetimeSec: 5})

	d := r.buildDashboard("day", now)
	if !d.HistoryOK {
		t.Fatal("history must be ok")
	}
	if d.KPI.Bans != 3 {
		t.Fatalf("bans in window: got %d want 3", d.KPI.Bans)
	}
	if d.ByReason[BanReasonDead] != 1 {
		t.Fatalf("dead breakdown: %+v", d.ByReason)
	}
	// средняя жизнь = (7200+3600+1800)/3 = 4200
	if d.KPI.AvgLifetimeSec == nil || *d.KPI.AvgLifetimeSec != 4200 {
		t.Fatalf("avg lifetime: %+v", d.KPI.AvgLifetimeSec)
	}
	// периодичность = среднее из (05:10-01:10)=4h и (06:40-05:10)=1.5h → 9900s
	if d.KPI.AvgGapSec == nil || *d.KPI.AvgGapSec != 9900 {
		t.Fatalf("avg gap: %+v", d.KPI.AvgGapSec)
	}
	if len(d.Buckets) != 24 {
		t.Fatalf("day window must have 24 hourly buckets, got %d", len(d.Buckets))
	}
	if d.Buckets[1].Bans != 1 || d.Buckets[5].Bans != 1 || d.Buckets[6].Bans != 1 {
		t.Fatalf("bucket placement broken: %+v", []int{d.Buckets[1].Bans, d.Buckets[5].Bans, d.Buckets[6].Bans})
	}
	// recent: новые первыми, dead не входит (это лента банов GP), ip замаскирован
	if len(d.Recent) != 3 || d.Recent[0].NodeID != "n3" {
		t.Fatalf("recent order/body broken: %+v", d.Recent)
	}
	if d.Recent[0].IPMasked == "10.0.0.7" || d.Recent[0].IPMasked == "" {
		t.Fatalf("ip must be masked in public payload, got %q", d.Recent[0].IPMasked)
	}

	// окно week: вчерашний бан входит (4 bans за 7 суток)
	dw := r.buildDashboard("week", now)
	if dw.KPI.Bans != 4 {
		t.Fatalf("week window must include yesterday's ban, got %d", dw.KPI.Bans)
	}
	if len(dw.Buckets) != 7 {
		t.Fatalf("week must have 7 daily buckets, got %d", len(dw.Buckets))
	}

	// регистратор без БД — пустой, но валидный ответ (history_ok=false)
	r2 := newTestRegistry(t)
	d2 := r2.buildDashboard("day", now)
	if d2.HistoryOK || d2.KPI.Bans != 0 || len(d2.Buckets) != 24 {
		t.Fatalf("no-db registry must degrade gracefully: %+v", d2.KPI)
	}
}

// V7.9.12 #4: интервалы <2 мин между банами — всплеск одного инцидента
// (флап), в «Периодичность банов» (средний интервал) не учитываются.
func TestDashboardGapClusterFilter(t *testing.T) {
	r := newTestRegistry(t)
	r.db = openHistoryDB(filepath.Join(t.TempDir(), "hist.db"))
	defer r.db.Close()

	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	// 00:10 →(+1200с, считается) 00:30 →(+60с, НЕ считается) 00:31 →(+7140с) 02:40
	seed := []time.Time{
		today.Add(10 * time.Minute), today.Add(30 * time.Minute),
		today.Add(31 * time.Minute), today.Add(2*time.Hour + 40*time.Minute),
	}
	for i, ts := range seed {
		r.db.recordBan(banRow{TS: ts, NodeID: "n", IP: "10.0.0.1", Reason: BanReasonIPBan, LifetimeSec: 60 + int64(i)})
	}
	d := r.buildDashboard("day", now)
	if d.KPI.Bans != 4 {
		t.Fatalf("bans: %d", d.KPI.Bans)
	}
	// средняя периодичность = (1200 + 7740) / 2 = 4470; gap=60 отброшен
	if d.KPI.AvgGapSec == nil || *d.KPI.AvgGapSec != 4470 {
		t.Fatalf("cluster gaps must be filtered (<2 min), avg=%+v", d.KPI.AvgGapSec)
	}
	// бакет часа 0: из двух внутрибакетных интервалов (1200 и 60) учтён только 1200
	if d.Buckets[0].AvgGapSec == nil || *d.Buckets[0].AvgGapSec != 1200 {
		t.Fatalf("bucket gap avg must skip the 60s spike: %+v", d.Buckets[0].AvgGapSec)
	}
	// recent хранит фактический gap как есть (сырые данные не выбрасываем)
	if d.Recent[1].GapSec != 60 {
		t.Fatalf("recent must keep the raw gap, got %+v", d.Recent[1])
	}

	// все интервалы короче 2 мин → средней периодичности нет (—)
	r2 := newTestRegistry(t)
	r2.db = openHistoryDB(filepath.Join(t.TempDir(), "hist.db"))
	defer r2.db.Close()
	r2.db.recordBan(banRow{TS: today.Add(time.Hour), NodeID: "a", IP: "10.0.0.2", Reason: BanReasonIPBan, LifetimeSec: 10})
	r2.db.recordBan(banRow{TS: today.Add(time.Hour + 90*time.Second), NodeID: "b", IP: "10.0.0.3", Reason: BanReasonIPBan, LifetimeSec: 10})
	d2 := r2.buildDashboard("day", now)
	if d2.KPI.AvgGapSec != nil {
		t.Fatalf("all gaps <2min must yield nil avg, got %+v", *d2.KPI.AvgGapSec)
	}
}
