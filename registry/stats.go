package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

//go:embed stats.html
var statsHTML []byte

// ── Публичная статистика ноды (V7.9.4) ──────────────────────────────────
//
// Открытая (без panel-токена) копия страницы подробностей ноды:
//
//	GET /statistics            — HTML-оболочка (список нод или страница ноды,
//	                             клиент разбирает id из URL сам);
//	GET /statistics/<node_id>  — та же оболочка (node_id или hex-суффикс);
//	GET /statistics/api/list   — JSON всех нод (обезличенный);
//	GET /statistics/api/node   — JSON одной ноды (обезличенный), ?id=...
//
// Чувствительное вырезается НА СЕРВЕРЕ, до сериализации ответа:
//   - IP ноды маскируется (первые два октета: 203.0.x.x) — и в карточке, и
//     во всех текстах событий (все IPv4 в detail прогоняются через маску);
//   - measurement_id Globalping НЕ отдаётся нигде: по нему публичное API
//     Globalping отдаёт target — т.е. полный IP ноды (утечка);
//   - в событиях нет поля ip; detail санитизируется (маска IPv4 + вырезание
//     measurement id).
//
// Белый список полей — явный (publicNode/publicEvent/publicGPDetail): новые
// поля приватного API сюда не утекут сами по себе.
//
// Монтируется вместе с панелью: panel.enabled=false выключает и её.

var (
	// любой IPv4 в произвольном тексте (detail событий, report errors и т.п.)
	ipv4TextRe = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	// «measurement <id>» / «measurement_id <id>» в текстах событий
	measurementTextRe = regexp.MustCompile(`(?i)\bmeasurement(?:_id)?[ :=]*\S+`)
)

// maskPublicIP оставляет первые два октета IPv4 (203.0.x.x) / две группы IPv6.
func maskPublicIP(ip string) string {
	if parts := strings.Split(ip, "."); len(parts) == 4 {
		return parts[0] + "." + parts[1] + ".x.x"
	}
	if strings.Contains(ip, ":") {
		groups := strings.Split(ip, ":")
		if len(groups) >= 2 && groups[0] != "" {
			return groups[0] + ":" + groups[1] + ":…"
		}
		return "…"
	}
	return "x.x.x.x"
}

// sanitizePublicDetail — маскирует IPv4 и вырезает measurement id в тексте.
func sanitizePublicDetail(s string) string {
	if s == "" {
		return s
	}
	s = ipv4TextRe.ReplaceAllStringFunc(s, maskPublicIP)
	s = measurementTextRe.ReplaceAllString(s, "measurement …")
	return s
}

// publicNode — явный белый список полей карточки ноды для публичной выдачи.
type publicNode struct {
	NodeID            string    `json:"node_id"`
	IPMasked          string    `json:"ip"` // уже замаскирован — имя поля едино с приватным API
	Port              int       `json:"port"`
	IsMaster          bool      `json:"is_master"`
	MasterDomains     []string  `json:"master_domains,omitempty"`
	QueuePosition     int       `json:"queue_position"`
	Healthy           bool      `json:"healthy"` // tcp-защёлка
	GlobalpingOK      bool      `json:"globalping_ok"`
	MetricsHealthy    bool      `json:"metrics_healthy"` // metrics-защёлка (не сырой последний отчёт)
	MetricsFailStreak int       `json:"metrics_fail_streak,omitempty"`
	GlobalpingRatio   float64   `json:"globalping_ratio"`
	UnhealthyReason   string    `json:"unhealthy_reason,omitempty"` // санитизировано
	RegisteredAt      time.Time `json:"registered_at"`
	HeartbeatAgeSec   int64     `json:"heartbeat_age_sec"`
	ReportAgeSec      int64     `json:"report_age_sec"` // -1 = отчётов не было
	HeartbeatsTotal   int       `json:"heartbeats_total"`
	ReportsTotal      int       `json:"reports_total"`
	ReportsOK         int       `json:"reports_ok"`
	AvailabilityPct   float64   `json:"availability_pct"`
	ClientsUniqueIPs  *float64  `json:"clients_unique_ips,omitempty"`
	GPChecksTotal     int       `json:"gp_checks_total"`
	GPVerifiedPct     float64   `json:"gp_verified_pct"`
	MasterStints      int       `json:"master_stints"`
	MasterTimeSec     int64     `json:"master_time_sec"`
	NodeType          string    `json:"node_type,omitempty"`
	// MasterTTLRemainingSec — см. publicNodeListItem (V7.9.13).
	MasterTTLRemainingSec *int64 `json:"master_ttl_remaining_sec,omitempty"`
}

// publicEvent — событие без ip/measurement id.
type publicEvent struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Domain string    `json:"domain,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// publicGPDetail — деталь последней верификации БЕЗ measurement_id.
type publicGPDetail struct {
	At          time.Time     `json:"at"`
	OK          bool          `json:"ok"`
	Ratio       float64       `json:"ratio"`
	ProbesOK    int           `json:"probes_ok"`
	ProbesTotal int           `json:"probes_total"`
	Probes      []GPProbeLine `json:"probes,omitempty"`
}

// mountStats — публичные страницы/API статистики. Вызывается при
// PanelEnabled (тот же рубильник, что и у панели).
func (r *Registry) mountStats(mux *http.ServeMux) {
	if !r.cfg.PanelEnabled {
		return
	}
	serveStats := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(statsHTML)
	}
	mux.HandleFunc("GET /statistics", serveStats)
	mux.HandleFunc("GET /statistics/", serveStats)

	// JSON-эндпойнты регистрируются ДО точного path-матчинга не нуждаются:
	// /statistics/api/* по right-longest-match побеждает /statistics/.
	mux.HandleFunc("GET /statistics/api/list", r.handleStatsList)
	mux.HandleFunc("GET /statistics/api/node", r.handleStatsNode)
}

// resolvePublicNodeLocked — точный node_id или его hex-суффикс (без «node-»).
func (r *Registry) resolvePublicNodeLocked(seg string) *Candidate {
	if c := r.state.Candidates[seg]; c != nil {
		return c
	}
	seg = strings.TrimPrefix(seg, "node-")
	for id, c := range r.state.Candidates {
		if strings.TrimPrefix(id, "node-") == seg {
			return c
		}
	}
	return nil
}

// masterTTLRemainingLocked — сколько осталось до принудительной ротации
// мастера по таймеру: минимум по доменам ноды. nil — TTL выключен либо
// назначение ещё не отсчитано (since лениво инициализируется в evaluate).
func (r *Registry) masterTTLRemainingLocked(domains []string, now time.Time) *int64 {
	ttl := r.masterTTL()
	if ttl <= 0 {
		return nil
	}
	var best *int64
	for _, d := range domains {
		since := r.state.AssignmentsSince[d]
		if since.IsZero() {
			continue
		}
		rem := int64(ttl.Seconds()) - int64(now.Sub(since).Seconds())
		if best == nil || rem < *best {
			v := rem
			best = &v
		}
	}
	return best
}

func (r *Registry) buildPublicNodeLocked(c *Candidate, queuePos int, domains []string, now time.Time) publicNode {
	n := publicNode{
		NodeID:            c.NodeID,
		IPMasked:          maskPublicIP(c.IP),
		Port:              c.Port,
		IsMaster:          len(domains) > 0,
		MasterDomains:     domains,
		QueuePosition:     queuePos,
		Healthy:           c.Healthy,
		GlobalpingOK:      c.GlobalpingOK,
		MetricsHealthy:    c.MetricsHealthy,
		MetricsFailStreak: c.MetricsFailStreak,
		GlobalpingRatio:   c.GlobalpingVerifiedRatio,
		RegisteredAt:      c.RegisteredAt,
		HeartbeatAgeSec:   int64(now.Sub(c.LastHeartbeat).Seconds()),
		ReportAgeSec:      -1,
		HeartbeatsTotal:   c.HeartbeatsTotal,
		ReportsTotal:      c.ReportsTotal,
		ReportsOK:         c.ReportsOK,
		AvailabilityPct:   c.AvailabilityPct(),
		GPChecksTotal:     c.GPChecksTotal,
		GPVerifiedPct:     c.GPVerifiedPct(),
		MasterStints:      c.MasterStints,
		MasterTimeSec:     c.MasterTimeSec(now),
		NodeType:          c.NodeType,
	}
	n.MasterTTLRemainingSec = r.masterTTLRemainingLocked(domains, now)
	if !c.LastReportAt.IsZero() {
		n.ReportAgeSec = int64(now.Sub(c.LastReportAt).Seconds())
	}
	if n.QueuePosition == 0 {
		// причина может содержать адреса/ошибки ноды — санитизируем
		n.UnhealthyReason = sanitizePublicDetail(c.unhealthyReason(r.cfg.ReportFreshnessTTL))
	}
	if v, ok := c.MetricsSnapshot[uniqueIPsMetric]; ok {
		vv := v
		n.ClientsUniqueIPs = &vv
	}
	return n
}

// publicEventsLocked — последние ≤limit событий ноды, санитизированные.
// ВЫЗЫВАТЬ под r.mu (минимум RLock).
func (r *Registry) publicEventsLocked(nodeID string, limit int) []publicEvent {
	out := make([]publicEvent, 0, 16)
	for i := len(r.state.Events) - 1; i >= 0 && len(out) < limit; i-- {
		ev := r.state.Events[i]
		if ev.NodeID != nodeID {
			continue
		}
		out = append(out, publicEvent{
			At:     ev.At,
			Type:   ev.Type,
			Domain: ev.Domain,
			Detail: sanitizePublicDetail(ev.Detail),
		})
	}
	return out
}

// handleStatsNode — GET /statistics/api/node?id=<node_id|hex>:
// публичная (обезличенная) копия ответа /panel/api/node.
func (r *Registry) handleStatsNode(w http.ResponseWriter, req *http.Request) {
	id := req.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	now := time.Now()
	r.mu.RLock()
	c := r.resolvePublicNodeLocked(id)
	if c == nil {
		r.mu.RUnlock()
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	node := r.buildPublicNodeLocked(c, r.queuePositionLocked(c), r.masterDomainsLocked(c.NodeID), now)
	var gpLast *publicGPDetail
	if g := c.GPLast; g != nil {
		gpLast = &publicGPDetail{
			At: g.At, OK: g.OK, Ratio: g.Ratio,
			ProbesOK: g.ProbesOK, ProbesTotal: g.ProbesTotal,
			Probes: g.Probes, // сеть/ASN ПРОБ (не ноды) — нечувствительно
		}
	}
	resp := map[string]any{
		"node":        node,
		"tcp_hist":    c.TCPHist,
		"gp_hist":     c.GPHist,
		"report_hist": c.ReportHist,
		"gp_last":     gpLast,
		"events":      r.publicEventsLocked(c.NodeID, 100),
		"version":     registryVersion,
		"hc": map[string]int{
			"fail_threshold":    max(1, r.cfg.Healthcheck.FailThreshold),
			"recover_threshold": max(1, r.cfg.Healthcheck.RecoverThreshold),
			"freshness_min":     int(r.cfg.ReportFreshnessTTL.Minutes()),
		},
	}
	r.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

// publicNodeListItem — строка публичного списка нод.
type publicNodeListItem struct {
	NodeID        string   `json:"node_id"`
	IPMasked      string   `json:"ip"`
	Port          int      `json:"port"`
	IsMaster      bool     `json:"is_master"`
	MasterDomains []string `json:"master_domains,omitempty"`
	// MasterTTLRemainingSec (V7.9.13) — через сколько мастер будет
	// переключён по таймеру (минимум по доменам ноды); <0 — лимит истёк,
	// ждём здоровую замену; nil — TTL выключен/не назначен.
	MasterTTLRemainingSec *int64   `json:"master_ttl_remaining_sec,omitempty"`
	QueuePosition         int      `json:"queue_position"`
	Healthy               bool     `json:"healthy"`
	GlobalpingOK          bool     `json:"globalping_ok"`
	MetricsHealthy        bool     `json:"metrics_healthy"`
	AvailabilityPct       float64  `json:"availability_pct"`
	ClientsUniqueIPs      *float64 `json:"clients_unique_ips,omitempty"`
	ReportAgeSec          int64    `json:"report_age_sec"`
	NodeType              string   `json:"node_type,omitempty"`
}

// handleStatsList — GET /statistics/api/list: все ноды (обезличенные).
func (r *Registry) handleStatsList(w http.ResponseWriter, req *http.Request) {
	now := time.Now()
	r.mu.RLock()
	ids := make([]string, 0, len(r.state.Candidates))
	for id := range r.state.Candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]publicNodeListItem, 0, len(ids))
	for _, id := range ids {
		c := r.state.Candidates[id]
		if c.Quarantine != nil {
			continue // V7.9.11: карантин — нода не обслуживает, из публичной статы убрана
		}
		it := publicNodeListItem{
			NodeID:          c.NodeID,
			IPMasked:        maskPublicIP(c.IP),
			Port:            c.Port,
			QueuePosition:   r.queuePositionLocked(c),
			Healthy:         c.Healthy,
			GlobalpingOK:    c.GlobalpingOK,
			MetricsHealthy:  c.MetricsHealthy,
			AvailabilityPct: c.AvailabilityPct(),
			ReportAgeSec:    -1,
			NodeType:        c.NodeType,
		}
		it.MasterDomains = r.masterDomainsLocked(id)
		it.IsMaster = len(it.MasterDomains) > 0
		it.MasterTTLRemainingSec = r.masterTTLRemainingLocked(it.MasterDomains, now)
		if !c.LastReportAt.IsZero() {
			it.ReportAgeSec = int64(now.Sub(c.LastReportAt).Seconds())
		}
		if v, ok := c.MetricsSnapshot[uniqueIPsMetric]; ok {
			vv := v
			it.ClientsUniqueIPs = &vv
		}
		out = append(out, it)
	}
	resp := map[string]any{"version": registryVersion, "nodes": out}
	r.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}
