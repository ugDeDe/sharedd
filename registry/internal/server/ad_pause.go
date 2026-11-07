package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"sharedd/registry/internal/machineapi"
	"sharedd/registry/internal/state"
)

// Рекламная пауза пула: [ad_pause] minutes_per_day минут в сутки на весь
// пул ноды показывают рекламный тег пула вместо своего.
//
// Расписание:
//   - бюджет суток делится поровну между нодами, умеющими подмену тега
//     (adPauseCapable), и округляется вверх до целых окон длиной master_ttl —
//     меньше одного окна нода не получает (adPauseQuota). Ноды считаются на
//     каждом решении, поэтому добавление и удаление ноды сразу
//     перераспределяет доли;
//   - реклама живёт только внутри мастерства, окнами master_ttl: решение в
//     начале мастерства и затем раз в окно. Окна ноды чередуются —
//     рекламное, своё, рекламное… — сквозь все её мастерства: при ротации
//     мастерство обычно и есть одно окно, и сброс на каждом мастерстве
//     делал бы рекламным каждое;
//   - окно рекламное, пока у ноды не выбрана её доля за сутки и пул не
//     выбрал общий бюджет (с учётом уже идущих окон). Набрался бюджет —
//     до конца суток (полночь по часам регистратора) рекламы нет;
//   - считается только время, ПОДТВЕРЖДЁННОЕ агентом (ad_pause_state=pool в
//     heartbeat); сбой постановки тега долг не создаёт;
//   - каждое включение и выключение тега пула пишется в журнал событий.
//
// Поддерживают ноды mtdd (POST/DELETE /v1/ad-tag, сразу для всех клиентов)
// и MTProxyL (персональная метка общих пользователей пула, горячая
// перезагрузка; клиенты получают тег при переподключении). Ноды без
// поддержки подтверждают "unsupported" и в расписание и делёж бюджета не
// попадают.

// adPauseDefaultPeriod — длина рекламного окна при выключенном master_ttl.
const adPauseDefaultPeriod = 10 * time.Minute

type adPauseSettings struct {
	enabled bool
	tag     string
	daily   time.Duration // бюджет пула на сутки
	period  time.Duration // окно = master_ttl
}

func (r *Registry) adPauseSettings() adPauseSettings {
	period := r.masterTTL()
	if period <= 0 {
		period = adPauseDefaultPeriod
	}
	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	return adPauseSettings{
		enabled: r.cfg.AdPause.Enabled != nil && *r.cfg.AdPause.Enabled && r.cfg.AdPause.Tag != "",
		tag:     strings.ToLower(r.cfg.AdPause.Tag),
		daily:   time.Duration(r.cfg.AdPause.MinutesPerDay) * time.Minute,
		period:  period,
	}
}

// budget — бюджет суток, не меньше одного окна.
func (st adPauseSettings) budget() time.Duration {
	return max(st.daily, st.period)
}

// adPauseCapable — агент подтвердил, что умеет подмену тега (pool/own/failed).
// Ноды без данных (старый агент) и "unsupported" бюджет не делят.
func adPauseCapable(c *Candidate) bool {
	switch c.AdState {
	case machineapi.AdPauseStatePool, machineapi.AdPauseStateOwn, machineapi.AdPauseStateFailed:
		return true
	}
	return false
}

// adPauseQuota — доля ноды за сутки: бюджет поровну на nodes нод, вверх до
// целых окон, минимум одно окно.
func adPauseQuota(daily, period time.Duration, nodes int) time.Duration {
	if nodes < 1 || period <= 0 {
		return 0
	}
	per := daily / time.Duration(nodes)
	windows := max((per+period-1)/period, 1)
	return windows * period
}

// adDay — текущие сутки регистратора (зона now): ключ и полночь.
func adDay(now time.Time) (string, time.Time) {
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	return start.Format("2006-01-02"), start
}

// adTodaySec — подтверждённая реклама ноды за текущие сутки: закрытые
// отрезки плюс часть открытого после полуночи.
func adTodaySec(c *Candidate, now time.Time) int64 {
	key, start := adDay(now)
	var total int64
	if c.AdDay == key {
		total = c.AdDaySec
	}
	if !c.AdSince.IsZero() {
		if from := maxTime(c.AdSince, start); now.After(from) {
			total += int64(now.Sub(from).Seconds())
		}
	}
	return total
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// retireAdPauseLocked — нода удаляется из пула: её реклама за сутки остаётся
// в расходе бюджета пула. Звать перед delete(r.state.Candidates, …).
func (r *Registry) retireAdPauseLocked(c *Candidate, now time.Time) {
	if c == nil {
		return
	}
	sec := adTodaySec(c, now)
	if sec <= 0 {
		return
	}
	if key, _ := adDay(now); r.state.AdRetiredDay != key {
		r.state.AdRetiredDay, r.state.AdRetiredSec = key, 0
	}
	r.state.AdRetiredSec += sec
}

// adRetiredTodaySec — реклама удалённых за сутки нод.
func adRetiredTodaySec(st *State, now time.Time) int64 {
	if key, _ := adDay(now); st.AdRetiredDay == key {
		return st.AdRetiredSec
	}
	return 0
}

// adPauseCommittedSec — расход бюджета пула за сутки: подтверждённое время
// всех нод (и удалённых за сутки) плюс остаток уже назначенных окон.
func adPauseCommittedSec(st *State, now time.Time, period time.Duration) float64 {
	used := float64(adRetiredTodaySec(st, now))
	cands := st.Candidates
	for _, c := range cands {
		used += float64(adTodaySec(c, now))
		if c.AdActive && !c.AdDecidedAt.IsZero() {
			if rest := period - now.Sub(c.AdDecidedAt); rest > 0 {
				used += rest.Seconds()
			}
		}
	}
	return used
}

// scheduleAdPauseLocked — решения по всем кандидатам (тик селекции, под r.mu).
// Возвращает, менялось ли желаемое состояние.
func (r *Registry) scheduleAdPauseLocked(now time.Time, st adPauseSettings) bool {
	nodes := 0
	for _, c := range r.state.Candidates {
		if adPauseCapable(c) {
			nodes++
		}
	}
	quota := adPauseQuota(st.daily, st.period, nodes).Seconds()
	budget := st.budget().Seconds()
	// Полокна допуска: подтверждение идёт heartbeat'ами, и выбранное окно
	// засчитывается чуть короче периода.
	half := st.period.Seconds() / 2
	committed := adPauseCommittedSec(&r.state, now, st.period)

	changed := false
	for _, c := range r.state.Candidates {
		want, decided := c.AdActive, c.AdDecidedAt
		switch {
		case !st.enabled || !adPauseCapable(c) || c.MasterSince.IsZero():
			want, decided = false, time.Time{}
		case decided.IsZero() || decided.Before(c.MasterSince) || now.Sub(decided) >= st.period:
			want = !c.AdLastAd &&
				float64(adTodaySec(c, now))+half < quota &&
				committed+half < budget
			if want {
				committed += st.period.Seconds() // резерв окна для следующих нод тика
			}
			decided = now
			c.AdLastAd = want
		}
		if want != c.AdActive || !decided.Equal(c.AdDecidedAt) {
			c.AdActive, c.AdDecidedAt = want, decided
			changed = true
		}
	}
	return changed
}

// adPauseDirectiveLocked — ответ heartbeat: какой тег держать сейчас.
func adPauseDirectiveLocked(c *Candidate, st adPauseSettings) *machineapi.AdPauseDirective {
	if st.enabled && c.AdActive {
		return &machineapi.AdPauseDirective{Active: true, Tag: st.tag}
	}
	return &machineapi.AdPauseDirective{Active: false}
}

// noteAdPauseStateLocked — подтверждение агента из heartbeat: учёт времени
// с тегом пула и журнал переключений.
func (r *Registry) noteAdPauseStateLocked(c *Candidate, reported string, now time.Time) {
	switch reported {
	case machineapi.AdPauseStatePool, machineapi.AdPauseStateOwn,
		machineapi.AdPauseStateUnsupported, machineapi.AdPauseStateFailed:
	default:
		return // старый агент или мусор — состояние не трогаем
	}
	prev := c.AdState
	c.AdState = reported
	switch {
	case reported == machineapi.AdPauseStatePool && c.AdSince.IsZero():
		c.AdSince = now
		r.addEventLocked(Event{
			Type: state.EventAdPauseOn, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("рекламный тег пула включён (за сутки у ноды: %s)",
				fmtDurSec(adTodaySec(c, now))),
		})
	case reported != machineapi.AdPauseStatePool && !c.AdSince.IsZero():
		span := int64(now.Sub(c.AdSince).Seconds())
		c.AdSeconds += span
		key, start := adDay(now)
		if c.AdDay != key {
			c.AdDay, c.AdDaySec = key, 0
		}
		if from := maxTime(c.AdSince, start); now.After(from) {
			c.AdDaySec += int64(now.Sub(from).Seconds())
		}
		c.AdSince = time.Time{}
		r.addEventLocked(Event{
			Type: state.EventAdPauseOff, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("рекламный тег пула снят через %s (%s); за сутки у ноды %s",
				fmtDurSec(span), reported, fmtDurSec(c.AdDaySec)),
		})
	}
	if prev != reported {
		r.persistStateLocked()
	}
}

func fmtDurSec(sec int64) string {
	return (time.Duration(sec) * time.Second).String()
}

// writeHeartbeatOK — 200 с указанием по тегу. Прежние агенты тело
// игнорируют; новые по наличию ad_pause узнают, что регистратор понимает
// ad_pause_state.
func writeHeartbeatOK(w http.ResponseWriter, directive *machineapi.AdPauseDirective) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(machineapi.HeartbeatResponse{AdPause: directive})
}

type adPauseNodeStatus struct {
	NodeID   string `json:"node_id"`
	IP       string `json:"ip"`
	NodeType string `json:"node_type,omitempty"`
	State    string `json:"state"`  // подтверждённое агентом (pool/own/unsupported/failed/"")
	Active   bool   `json:"active"` // желаемое сейчас
	Master   bool   `json:"master"`
	TodaySec int64  `json:"today_sec"` // подтверждённая реклама за сутки
	QuotaSec int64  `json:"quota_sec"` // доля ноды за сутки (0 — не участвует)
	AdSec    int64  `json:"ad_sec"`    // за всё время
}

type adPauseSummary struct {
	Day       string `json:"day"`
	Nodes     int    `json:"nodes"`      // делят бюджет
	BudgetSec int64  `json:"budget_sec"` // бюджет суток (не меньше окна)
	UsedSec   int64  `json:"used_sec"`   // подтверждено за сутки
	PeriodSec int64  `json:"period_sec"` // окно = master_ttl
}

// adPauseStatusSnapshot — сводка по нодам (по node_id) и по пулу для панели.
func (r *Registry) adPauseStatusSnapshot(now time.Time) ([]adPauseNodeStatus, adPauseSummary) {
	st := r.adPauseSettings()
	r.mu.RLock()
	defer r.mu.RUnlock()
	nodes := 0
	for _, c := range r.state.Candidates {
		if adPauseCapable(c) {
			nodes++
		}
	}
	quota := int64(adPauseQuota(st.daily, st.period, nodes).Seconds())
	day, _ := adDay(now)
	sum := adPauseSummary{Day: day, Nodes: nodes, BudgetSec: int64(st.budget().Seconds()), PeriodSec: int64(st.period.Seconds()),
		UsedSec: adRetiredTodaySec(&r.state, now)}
	out := make([]adPauseNodeStatus, 0, len(r.state.Candidates))
	for _, c := range r.state.Candidates {
		ns := adPauseNodeStatus{
			NodeID: c.NodeID, IP: c.IP, NodeType: c.NodeType, State: c.AdState, Active: c.AdActive,
			Master: !c.MasterSince.IsZero(), TodaySec: adTodaySec(c, now), AdSec: c.AdTimeSec(now),
		}
		if adPauseCapable(c) {
			ns.QuotaSec = quota
		}
		sum.UsedSec += ns.TodaySec
		out = append(out, ns)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, sum
}
