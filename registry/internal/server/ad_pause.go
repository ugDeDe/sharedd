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

// Рекламная пауза пула: share_percent процентов времени мастерства каждой
// ноды она показывает рекламный тег пула вместо своего.
//
// Честность:
//   - доля одна для всех нод и задаётся только в конфиге регистратора;
//   - реклама живёт только внутри мастерства (когда нода получает новых
//     клиентов) и чередуется: решение принимается в начале мастерства и
//     затем раз в master_ttl, по накопленному времени — нода с недобором
//     получает рекламный период, с перебором — нет (adPauseDecide);
//   - считается только время, ПОДТВЕРЖДЁННОЕ агентом (ad_pause_state=pool в
//     heartbeat), и всё такое время, включая хвост после конца периода;
//     сбой постановки тега пула ноде долг не создаёт — она просто получит
//     период позже, когда тег встанет;
//   - каждое включение и выключение тега пула пишется в журнал событий.
//
// Поддерживают ноды mtdd с подменой тега на лету (POST/DELETE /v1/ad-tag):
// клиенты не переподключаются. Ноды без поддержки подтверждают
// "unsupported" и в расписание не попадают.

// adPauseDefaultPeriod — длина рекламного периода при выключенном master_ttl.
const adPauseDefaultPeriod = 10 * time.Minute

// adPauseDecide — делать ли следующий период длины period рекламным: да,
// если так доля adSec/masterSec после периода окажется ближе к share, чем
// без него (A + L/2 < s·(M + L)). Самокорректируется по фактическому времени.
func adPauseDecide(adSec, masterSec int64, period time.Duration, share float64) bool {
	l := period.Seconds()
	return float64(adSec)+l/2 < share*(float64(masterSec)+l)
}

type adPauseSettings struct {
	enabled bool
	tag     string
	share   float64
	period  time.Duration
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
		share:   float64(r.cfg.AdPause.SharePercent) / 100,
		period:  period,
	}
}

// scheduleAdPauseLocked — решения по всем кандидатам (тик селекции, под r.mu).
// Возвращает, менялось ли желаемое состояние.
func (r *Registry) scheduleAdPauseLocked(now time.Time, st adPauseSettings) bool {
	changed := false
	for _, c := range r.state.Candidates {
		want, decided := c.AdActive, c.AdDecidedAt
		switch {
		case !st.enabled || c.AdState == machineapi.AdPauseStateUnsupported || c.MasterSince.IsZero():
			want, decided = false, time.Time{}
		case decided.IsZero() || decided.Before(c.MasterSince) || now.Sub(decided) >= st.period:
			want = adPauseDecide(c.AdTimeSec(now), c.MasterTimeSec(now), st.period, st.share)
			decided = now
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
			Detail: fmt.Sprintf("рекламный тег пула включён (за всё время: реклама %s из %s мастерства)",
				fmtDurSec(c.AdTimeSec(now)), fmtDurSec(c.MasterTimeSec(now))),
		})
	case reported != machineapi.AdPauseStatePool && !c.AdSince.IsZero():
		span := int64(now.Sub(c.AdSince).Seconds())
		c.AdSeconds += span
		c.AdSince = time.Time{}
		r.addEventLocked(Event{
			Type: state.EventAdPauseOff, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("рекламный тег пула снят через %s (%s); всего реклама %s из %s мастерства",
				fmtDurSec(span), reported, fmtDurSec(c.AdSeconds), fmtDurSec(c.MasterTimeSec(now))),
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
	NodeID    string  `json:"node_id"`
	IP        string  `json:"ip"`
	NodeType  string  `json:"node_type,omitempty"`
	State     string  `json:"state"`  // подтверждённое агентом (pool/own/unsupported/failed/"")
	Active    bool    `json:"active"` // желаемое сейчас
	Master    bool    `json:"master"`
	MasterSec int64   `json:"master_sec"`
	AdSec     int64   `json:"ad_sec"`
	Share     float64 `json:"share"` // фактическая доля рекламы во времени мастерства, %
}

// adPauseStatusSnapshot — сводка по нодам для панели, по node_id.
func (r *Registry) adPauseStatusSnapshot(now time.Time) []adPauseNodeStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]adPauseNodeStatus, 0, len(r.state.Candidates))
	for _, c := range r.state.Candidates {
		st := adPauseNodeStatus{
			NodeID: c.NodeID, IP: c.IP, NodeType: c.NodeType, State: c.AdState, Active: c.AdActive,
			Master: !c.MasterSince.IsZero(), MasterSec: c.MasterTimeSec(now), AdSec: c.AdTimeSec(now),
		}
		if st.MasterSec > 0 {
			st.Share = float64(st.AdSec) * 100 / float64(st.MasterSec)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}
