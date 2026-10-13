package server

// Детальная карточка ноды в кабинете (docs/specs/plan-ui-batch.md Фаза 2,
// slice 1). Возвращает ТОЛЬКО для ноды-владельца (session + owner-check, как
// DELETE /api/me/nodes/{node_id}) те разделы mtplane node.html, под которые у
// нас уже есть источник данных. Разделы без источника отдаются null, чтобы UI
// рисовал «нет данных» (провайдерские CPU/RAM, per-user трафик, telemt Control
// API — Phase 2 slice 3).

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"net/http"

	"sharedd/registry/internal/config"
	"sharedd/registry/internal/state"
)

type nodeMasterInfo struct {
	IsMaster        bool     `json:"is_master"`
	Domains         []string `json:"domains"`
	SlotsFilled     int      `json:"slots_filled"`
	SlotsTotal      int      `json:"slots_total"`
	TTLRemainingSec *int64   `json:"ttl_remaining_sec,omitempty"`
	MasterStints    int      `json:"master_stints"`
	MasterTimeSec   int64    `json:"master_time_sec"`
}

// meNodeInfo — «Параметры ноды»: параметры + агрегаты здоровья (panelNode)
// плюс регион и время в роли мастера, которых нет в panelNode.
type meNodeInfo struct {
	panelNode
	Region    string         `json:"region,omitempty"`
	UptimeSec int64          `json:"uptime_sec"`
	Status    string         `json:"status"`
	Master    nodeMasterInfo `json:"master"`
}

type nodeDailyAvailability struct {
	Date         string  `json:"date"` // YYYY-MM-DD (UTC)
	TCPOK        int     `json:"tcp_ok"`
	TCPTotal     int     `json:"tcp_total"`
	ReportsOK    int     `json:"reports_ok"`
	ReportsTotal int     `json:"reports_total"`
	OKPct        float64 `json:"ok_pct"`
}

type nodeAvailability struct {
	WindowDays    int                     `json:"window_days"`
	OverallPct    float64                 `json:"overall_pct"`
	ReportsOK     int                     `json:"reports_ok"`
	ReportsTotal  int                     `json:"reports_total"`
	GPChecksOK    int                     `json:"gp_checks_ok"`
	GPChecksTotal int                     `json:"gp_checks_total"`
	GPVerifiedPct float64                 `json:"gp_verified_pct"`
	Daily         []nodeDailyAvailability `json:"daily"`
	TCPHist       []state.TCPPoint        `json:"tcp_hist"`
	GPHist        []state.GPPoint         `json:"gp_hist"`
	ReportHist    []state.ReportPoint     `json:"report_hist"`
}

type nodeCheck struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // tcp | globalping | health_report
	OK     bool      `json:"ok"`
	Detail string    `json:"detail,omitempty"`
}

type nodeHosting struct {
	CPUPercent *float64 `json:"cpu_percent"`
	RAMPercent *float64 `json:"ram_percent"`
	// Сеть — из накопительных счётчиков трафика ноды (metrics_snapshot).
	NetIngressBytes *float64 `json:"net_ingress_bytes_total,omitempty"`
	NetEgressBytes  *float64 `json:"net_egress_bytes_total,omitempty"`
}

type nodeConnPoint struct {
	At      time.Time `json:"at"`
	Clients int       `json:"clients"`
	Writers int       `json:"writers"`
}

type nodeConnections struct {
	CurrentWriters         *float64        `json:"current_writers,omitempty"`
	CurrentUniqueIPs       *float64        `json:"current_unique_ips,omitempty"`
	CurrentUserConnections *float64        `json:"current_user_connections,omitempty"`
	Series                 []nodeConnPoint `json:"series"`
	Note                   string          `json:"note,omitempty"`
}

type nodeUserUniqueIPs struct {
	User      string  `json:"user"`
	UniqueIPs float64 `json:"unique_ips"`
}

type nodeUserTraffic struct {
	User         string `json:"user"`
	IngressBytes int64  `json:"ingress_bytes"`
	EgressBytes  int64  `json:"egress_bytes"`
}

type nodeUsers struct {
	UniqueIPsByUser []nodeUserUniqueIPs `json:"unique_ips_by_user"`
	// TrafficByUser — 24ч-тоталы per-user octets из history (traffic_user).
	// null: БД недоступна (r.db == nil) или пользователей ещё не наблюдали.
	TrafficByUser []nodeUserTraffic `json:"traffic_by_user"`
}

type meNodeDetail struct {
	Node         *meNodeInfo       `json:"node"`
	Availability *nodeAvailability `json:"availability"`
	IPHistory    any               `json:"ip_history"`
	LastChecks   []nodeCheck       `json:"last_checks"`
	Hosting      *nodeHosting      `json:"hosting"`
	Connections  *nodeConnections  `json:"connections_24h"`
	Users        *nodeUsers        `json:"users"`
	Telemt       any               `json:"telemt"`
	Events       []nodeEvent       `json:"events"`
	// Absent — разделы/поля без источника данных (UI → «нет данных»).
	Absent []string `json:"absent"`
}

type nodeEvent struct {
	At     time.Time `json:"at"`
	Type   string    `json:"type"`
	Domain string    `json:"domain,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// handleMeNodeDetail — GET /api/me/nodes/{node_id}/detail.
func (r *Registry) handleMeNodeDetail(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	nodeID := strings.TrimSpace(req.PathValue("node_id"))
	if nodeID == "" {
		controlAPIError(w, http.StatusNotFound, "not_found", "node id required")
		return
	}
	owner, legacy, ok, err := r.control.NodeOwner(nodeID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	// Чужая/legacy/неизвестная нода → 404 (не подтверждаем существование).
	if !ok || legacy || owner != u.ID {
		controlAPIError(w, http.StatusNotFound, "not_found", "node is not owned by the current user")
		return
	}

	// Регион и deleting — по операциям/владельцу пользователя (вне r.mu).
	ops, err := r.control.OperationsByUser(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	region := ""
	for _, op := range ops {
		if op.NodeID == nodeID && op.Region != "" {
			region = op.Region
			break
		}
	}

	now := time.Now()
	absent := []string{}
	resp := meNodeDetail{IPHistory: nil, Absent: absent}

	// 24ч per-user тоталы из history (не под r.mu: чтение БД).
	var userTraffic []nodeUserTraffic
	if r.db != nil {
		if rows, herr := r.db.UserTrafficSince(nodeID, now.Add(-24*time.Hour)); herr == nil {
			sum := map[string]*nodeUserTraffic{}
			for _, row := range rows {
				t := sum[row.User]
				if t == nil {
					t = &nodeUserTraffic{User: row.User}
					sum[row.User] = t
				}
				t.IngressBytes += row.Ingress
				t.EgressBytes += row.Egress
			}
			userTraffic = make([]nodeUserTraffic, 0, len(sum))
			for _, t := range sum {
				userTraffic = append(userTraffic, *t)
			}
			sort.Slice(userTraffic, func(i, j int) bool { return userTraffic[i].User < userTraffic[j].User })
		}
	}

	r.mu.RLock()
	c := r.state.Candidates[nodeID]
	dcsSnap, hasDCS := r.dcStats[nodeID]
	freshness := r.cfg.ReportFreshnessTTL
	controlAPIPresent := false
	if c != nil {
		domains := r.masterDomainsLocked(nodeID)
		pn := r.buildPanelNodeLocked(c, r.queuePositionLocked(c), domains, now)
		slotsTotal := config.ResolveMastersPerDomain(r.cfg.Selection.MastersPerDomain)
		resp.Node = &meNodeInfo{
			panelNode: pn,
			Region:    region,
			UptimeSec: int64(now.Sub(c.RegisteredAt).Seconds()),
			Status:    nodeHealthStatus(c, freshness),
			Master: nodeMasterInfo{
				IsMaster:        len(domains) > 0,
				Domains:         nonNilStrings(domains),
				SlotsFilled:     len(domains),
				SlotsTotal:      slotsTotal,
				TTLRemainingSec: r.masterTTLRemainingLocked(nodeID, domains, now),
				MasterStints:    c.MasterStints,
				MasterTimeSec:   c.MasterTimeSec(now),
			},
		}
		resp.Availability = buildNodeAvailability(c, now)
		resp.LastChecks = buildNodeChecks(c)
		telemtObj := map[string]any{"metrics_snapshot": c.MetricsSnapshot, "collected_at": c.LastReportAt}
		if hasDCS {
			controlAPIPresent = true
			telemtObj["control_api"] = map[string]any{
				"ok":                      true,
				"generated_at_epoch_secs": dcsSnap.GeneratedAtEpochSecs,
				"middle_proxy_enabled":    dcsSnap.MiddleProxyEnabled,
				"dcs":                     dcsSnap.DCs,
			}
		}
		resp.Telemt = telemtObj
		resp.Connections = buildNodeConnections(c)
		resp.Users = buildNodeUsers(c, userTraffic)
		resp.Hosting = &nodeHosting{
			CPUPercent:      metricPtr(c, hostCPUMetric),
			RAMPercent:      metricPtr(c, hostRAMMetric),
			NetIngressBytes: metricPtr(c, trafficIngressMetric),
			NetEgressBytes:  metricPtr(c, trafficEgressMetric),
		}
		for i := len(r.state.Events) - 1; i >= 0 && len(resp.Events) < 50; i-- {
			ev := r.state.Events[i]
			if ev.NodeID != nodeID {
				continue
			}
			resp.Events = append(resp.Events, nodeEvent{
				At: ev.At, Type: ev.Type, Domain: ev.Domain, Detail: sanitizePublicDetail(ev.Detail),
			})
		}
	} else {
		// Кандидат отсутствует (нода вне пула): параметры из control, разделы null.
		resp.Node = &meNodeInfo{
			panelNode: panelNode{NodeID: nodeID},
			Region:    region,
			Status:    "offline",
		}
	}
	r.mu.RUnlock()

	if resp.Events == nil {
		resp.Events = []nodeEvent{}
	}
	if resp.LastChecks == nil {
		resp.LastChecks = []nodeCheck{}
	}

	// История IP — из журнала событий (history DB; может отсутствовать).
	if r.db != nil {
		rows, herr := r.db.NodeIPHistory(nodeID)
		if herr == nil {
			resp.IPHistory = rows
		}
	} else {
		resp.Absent = append(resp.Absent, "ip_history")
	}

	// Разделы без источника данных (честный null вместо нулей).
	if !controlAPIPresent {
		resp.Absent = append(resp.Absent, "telemt_control_api", "dc_endpoints", "quality_health")
	}
	resp.Absent = append(resp.Absent, "actions")
	if resp.Users != nil && resp.Users.TrafficByUser == nil {
		resp.Absent = append(resp.Absent, "users.traffic_by_user")
	}
	if resp.Hosting == nil {
		resp.Absent = append(resp.Absent, "hosting.cpu", "hosting.ram")
	} else {
		if resp.Hosting.CPUPercent == nil {
			resp.Absent = append(resp.Absent, "hosting.cpu")
		}
		if resp.Hosting.RAMPercent == nil {
			resp.Absent = append(resp.Absent, "hosting.ram")
		}
	}
	writeCabinetJSON(w, http.StatusOK, resp)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func metricPtr(c *Candidate, key string) *float64 {
	if v, ok := c.MetricsSnapshot[key]; ok {
		vv := v
		return &vv
	}
	return nil
}

func nodeHealthStatus(c *Candidate, freshness time.Duration) string {
	switch {
	case c.Quarantine != nil:
		return "quarantined"
	case c.IsFullyHealthy(freshness):
		return "healthy"
	default:
		return "unhealthy"
	}
}

// buildNodeAvailability — доступность за окно 7 дней. Дневные бакеты строятся
// по кольцевым буферам tcp/report (bounded, ~6ч при 60с интервале); overall —
// накопительные счётчики кандидата за всё время.
func buildNodeAvailability(c *Candidate, now time.Time) *nodeAvailability {
	a := &nodeAvailability{
		WindowDays:    7,
		OverallPct:    c.AvailabilityPct(),
		ReportsOK:     c.ReportsOK,
		ReportsTotal:  c.ReportsTotal,
		GPChecksOK:    c.GPChecksOK,
		GPChecksTotal: c.GPChecksTotal,
		GPVerifiedPct: c.GPVerifiedPct(),
		TCPHist:       nonNilTCP(c.TCPHist),
		GPHist:        nonNilGP(c.GPHist),
		ReportHist:    nonNilReport(c.ReportHist),
	}
	type day struct{ tcpOK, tcpTotal, repOK, repTotal int }
	days := map[string]*day{}
	get := func(t time.Time) *day {
		k := t.UTC().Format("2006-01-02")
		d := days[k]
		if d == nil {
			d = &day{}
			days[k] = d
		}
		return d
	}
	cutoff := now.Add(-7 * 24 * time.Hour)
	for _, p := range c.TCPHist {
		if p.At.Before(cutoff) {
			continue
		}
		d := get(p.At)
		d.tcpTotal++
		if p.OK {
			d.tcpOK++
		}
	}
	for _, p := range c.ReportHist {
		if p.At.Before(cutoff) {
			continue
		}
		d := get(p.At)
		d.repTotal++
		if p.MetricsOK {
			d.repOK++
		}
	}
	keys := make([]string, 0, len(days))
	for k := range days {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a.Daily = make([]nodeDailyAvailability, 0, len(keys))
	for _, k := range keys {
		d := days[k]
		total := d.tcpTotal + d.repTotal
		ok := d.tcpOK + d.repOK
		pct := 0.0
		if total > 0 {
			pct = float64(ok) * 100 / float64(total)
		}
		a.Daily = append(a.Daily, nodeDailyAvailability{
			Date: k, TCPOK: d.tcpOK, TCPTotal: d.tcpTotal,
			ReportsOK: d.repOK, ReportsTotal: d.repTotal, OKPct: pct,
		})
	}
	return a
}

func nonNilTCP(s []state.TCPPoint) []state.TCPPoint {
	if s == nil {
		return []state.TCPPoint{}
	}
	return s
}
func nonNilGP(s []state.GPPoint) []state.GPPoint {
	if s == nil {
		return []state.GPPoint{}
	}
	return s
}
func nonNilReport(s []state.ReportPoint) []state.ReportPoint {
	if s == nil {
		return []state.ReportPoint{}
	}
	return s
}

// buildNodeChecks — «Последние проверки»: слитые точки tcp/GP/report, новые
// первыми. source/target/availability/ok: target = сама нода (IP:порт), здесь
// отдаём источник, ok и краткий detail.
func buildNodeChecks(c *Candidate) []nodeCheck {
	out := []nodeCheck{}
	for _, p := range c.TCPHist {
		out = append(out, nodeCheck{At: p.At, Source: "tcp", OK: p.OK})
	}
	for _, p := range c.GPHist {
		out = append(out, nodeCheck{
			At: p.At, Source: "globalping", OK: p.OK,
			Detail: fmt.Sprintf("%d/%d probes", p.ProbesOK, p.ProbesTotal),
		})
	}
	for _, p := range c.ReportHist {
		out = append(out, nodeCheck{
			At: p.At, Source: "health_report", OK: p.MetricsOK,
			Detail: fmt.Sprintf("clients=%d writers=%d", p.Clients, p.Writers),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

// buildNodeConnections — «Соединения 24ч»: текущие gauge + серия из
// report_hist (bounded кольцом). Полноценных 24ч-точек у нас нет.
func buildNodeConnections(c *Candidate) *nodeConnections {
	conn := &nodeConnections{
		CurrentWriters:         metricPtr(c, writersMetric),
		CurrentUniqueIPs:       metricPtr(c, uniqueIPsMetric),
		CurrentUserConnections: metricPtr(c, "telemt_user_connections_current"),
		Series:                 make([]nodeConnPoint, 0, len(c.ReportHist)),
	}
	for _, p := range c.ReportHist {
		conn.Series = append(conn.Series, nodeConnPoint{At: p.At, Clients: p.Clients, Writers: p.Writers})
	}
	conn.Note = "series bounded by report ring (не полные 24ч)"
	return conn
}

// buildNodeUsers — «Уникальные IP по пользователям» из per-user серий снапшота
// (telemt_user_unique_ips_current{user="x"}) плюс 24ч-трафик по пользователям
// (traffic, переданный вызывающим из history).
func buildNodeUsers(c *Candidate, traffic []nodeUserTraffic) *nodeUsers {
	const prefix = uniqueIPsMetric + `{user="`
	u := &nodeUsers{UniqueIPsByUser: []nodeUserUniqueIPs{}, TrafficByUser: traffic}
	for k, v := range c.MetricsSnapshot {
		if !strings.HasPrefix(k, prefix) || !strings.HasSuffix(k, `"}`) {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(k, prefix), `"}`)
		u.UniqueIPsByUser = append(u.UniqueIPsByUser, nodeUserUniqueIPs{User: name, UniqueIPs: v})
	}
	sort.Slice(u.UniqueIPsByUser, func(i, j int) bool { return u.UniqueIPsByUser[i].User < u.UniqueIPsByUser[j].User })
	return u
}
