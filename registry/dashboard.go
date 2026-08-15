package main

// Публичный дашборд блокировок: /dashboard — страница с
// интерактивными графиками, /dashboard/api — JSON-агрегации из SQLite
// (historyDB). Публичный, как /statistics: IP нод маскируются тем же
// maskPublicIP, id нод — непрозрачные хеши.
//
// Три метрики (ТЗ, все — ТОЛЬКО по банам GP = reason ip_ban — «без
// восстановления»; терминальность гарантирует terminate.go):
// 1. Баны GP — число терминальных блокировок по IP за окно.
// 2. Периодичность банов — средний интервал между соседними блокировками
// (любых нод) внутри окна; интервалы хранятся и в самой таблице
// (bans.gap_sec), здесь считаем по gp-ряду окна.
// 3. Время жизни ноды — среднее (регистрация → бан) по банам окна;
// lifetime=-1 (retire после выпадения) из среднего исключён.
//
// Окна: today = текущие календарные сутки (локаль регистратора), week =
// последние 7 суток, month = последние 30 суток. Бакеты: часы (24) для
// today, сутки (7/30) для week/month. Граница AvgGap — только пары, обе
// точки которых внутри окна (первая точка окна интервала не имеет).
// Интервалы < gapClusterMin (2 мин) в среднюю периодичность НЕ
// идут — это всплеск одного инцидента (флап серии нод подряд), а не
// периодичность блокировок.

import (
	"encoding/json"
	"math"
	"net/http"
	"sort"
	"time"

	_ "embed"
)

//go:embed dashboard.html
var dashboardHTML []byte

// gapClusterMin — интервалы между банами короче этого порога
// в среднюю периодичность не учитываются (п.4 ревью).
const gapClusterMin = 2 * time.Minute

type dashKPI struct {
	Bans           int      `json:"bans"`
	AvgLifetimeSec *float64 `json:"avg_lifetime_sec,omitempty"`
	AvgGapSec      *float64 `json:"avg_gap_sec,omitempty"`
}

type dashBucket struct {
	StartTS        int64    `json:"start_ts"`
	Bans           int      `json:"bans"`
	AvgLifetimeSec *float64 `json:"avg_lifetime_sec,omitempty"`
	AvgGapSec      *float64 `json:"avg_gap_sec,omitempty"`
}

type dashBan struct {
	TS          time.Time `json:"ts"`
	NodeID      string    `json:"node_id"`
	IPMasked    string    `json:"ip_masked"`
	LifetimeSec int64     `json:"lifetime_sec"`
	GapSec      int64     `json:"gap_sec"`
}

type dashResponse struct {
	Range     string         `json:"range"`
	Now       time.Time      `json:"now"`
	From      time.Time      `json:"from"`
	StepSec   int64          `json:"step_sec"`
	KPI       dashKPI        `json:"kpi"`
	ByReason  map[string]int `json:"by_reason"`
	Buckets   []dashBucket   `json:"buckets"`
	Recent    []dashBan      `json:"recent"` // ≤30 новых банов GP в окне
	HistoryOK bool           `json:"history_ok"`
}

// dashWindow — рамки окна и шаг бакетов по имени диапазона.
func dashWindow(name string, now time.Time) (from time.Time, buckets int, step time.Duration) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch name {
	case "week":
		return today.AddDate(0, 0, -6), 7, 24 * time.Hour
	case "month":
		return today.AddDate(0, 0, -29), 30, 24 * time.Hour
	default: // "day"/today
		return today, 24, time.Hour
	}
}

func (r *Registry) buildDashboard(name string, now time.Time) dashResponse {
	from, nB, step := dashWindow(name, now)
	resp := dashResponse{
		Range:    name,
		Now:      now,
		From:     from,
		StepSec:  int64(step.Seconds()),
		ByReason: map[string]int{BanReasonIPBan: 0, BanReasonDead: 0},
		Buckets:  make([]dashBucket, nB),
		Recent:   []dashBan{},
	}
	for i := range resp.Buckets {
		resp.Buckets[i].StartTS = from.Add(time.Duration(i) * step).Unix()
	}
	if r.db == nil {
		return resp // HistoryOK=false — страница покажет «история недоступна»
	}
	resp.HistoryOK = true

	// окно снапшотим под коротким db-запросом (sql.DB сам сериализует)
	all, err := r.db.bansSince(from, "")
	if err != nil {
		return resp
	}
	gp := make([]banRow, 0, len(all))
	for _, b := range all {
		resp.ByReason[b.Reason]++
		if b.Reason == BanReasonIPBan {
			gp = append(gp, b)
		}
	}
	sort.Slice(gp, func(i, j int) bool { return gp[i].TS.Before(gp[j].TS) })

	resp.KPI.Bans = len(gp)
	// KPI: средняя жизнь / средний интервал (пары внутри окна)
	var lifeSum float64
	var lifeN int
	gaps := make([]float64, 0, len(gp))
	bucketLife := make([]float64, nB)
	bucketLifeN := make([]int, nB)
	bucketGap := make([]float64, nB)
	bucketGapN := make([]int, nB)
	for i, b := range gp {
		bi := int(b.TS.Sub(from) / step)
		if bi < 0 {
			bi = 0
		}
		if bi >= nB {
			bi = nB - 1
		}
		resp.Buckets[bi].Bans++
		if b.LifetimeSec >= 0 {
			lifeSum += float64(b.LifetimeSec)
			lifeN++
			bucketLife[bi] += float64(b.LifetimeSec)
			bucketLifeN[bi]++
		}
		if i > 0 { // предыдущий gp-бан тоже в окне — интервал легален
			d := b.TS.Sub(gp[i-1].TS).Seconds()
			if b.TS.Sub(gp[i-1].TS) >= gapClusterMin { // <2 мин — всплеск одного инцидента, не периодичность
				gaps = append(gaps, d)
				bucketGap[bi] += d
				bucketGapN[bi]++
			}
		}
	}
	if lifeN > 0 {
		v := lifeSum / float64(lifeN)
		resp.KPI.AvgLifetimeSec = &v
	}
	if len(gaps) > 0 {
		s := 0.0
		for _, g := range gaps {
			s += g
		}
		v := s / float64(len(gaps))
		resp.KPI.AvgGapSec = &v
	}
	for i := range resp.Buckets {
		if bucketLifeN[i] > 0 {
			v := math.Round(bucketLife[i] / float64(bucketLifeN[i]))
			resp.Buckets[i].AvgLifetimeSec = &v
		}
		if bucketGapN[i] > 0 {
			v := math.Round(bucketGap[i] / float64(bucketGapN[i]))
			resp.Buckets[i].AvgGapSec = &v
		}
	}

	// нитка последних банов (новые первыми, IP замаскирован)
	for i := len(gp) - 1; i >= 0 && len(resp.Recent) < 30; i-- {
		resp.Recent = append(resp.Recent, dashBan{
			TS: gp[i].TS, NodeID: gp[i].NodeID, IPMasked: maskPublicIP(gp[i].IP),
			LifetimeSec: gp[i].LifetimeSec, GapSec: gp[i].GapSec,
		})
	}
	return resp
}

func (r *Registry) mountDashboard(mux *http.ServeMux) {
	if !r.cfg.PanelEnabled {
		return
	}
	serveDash := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(dashboardHTML)
	}
	mux.HandleFunc("GET /dashboard", serveDash)
	mux.HandleFunc("GET /dashboard/", serveDash)
	mux.HandleFunc("GET /dashboard/api", func(w http.ResponseWriter, req *http.Request) {
		name := req.URL.Query().Get("range")
		if name != "day" && name != "week" && name != "month" {
			name = "day"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(r.buildDashboard(name, time.Now()))
	})
}
