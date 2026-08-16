package main

import (
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed panel.html
var panelHTML []byte

// writersMetric — gauge активных ME-райтеров telemt (апстрим-подключения к
// Telegram middle-end, приходит в metrics_snapshot от нод). ВАЖНО: это НЕ
// клиенты — раньше панель ошибочно подавала сумму writers как "клиентов".
const writersMetric = "telemt_me_writers_active_current"

// uniqueIPsMetric — агрегат уникальных активных клиентских IP ноды
// (нода суммирует per-user серии telemt_user_unique_ips_current{user="..."};
// ключ без labels присутствует в снапшоте только если telemt эмитит per-user
// метрики, т.е. [general.telemetry] user_enabled=true).
const uniqueIPsMetric = "telemt_user_unique_ips_current"

// panelAuthorized — общая проверка для API панели и /status.
// Пустой токен в конфиге = dev-режим без авторизации (warning при старте).
// Токен читается под cfgMu — панель может сменить его на лету.
func (r *Registry) panelAuthorized(req *http.Request) bool {
	r.cfgMu.RLock()
	tok := r.cfg.Panel.Token
	r.cfgMu.RUnlock()
	if tok == "" {
		return true
	}
	got, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}

// mountPanel подключает веб-панель (HTML + JSON API) к mux.
func (r *Registry) mountPanel(mux *http.ServeMux) {
	if !r.cfg.PanelEnabled {
		return
	}

	serveIndex := func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(panelHTML)
	}
	// пустой path-суффикс отдаёт index; /panel/api/* регистрируется ниже
	// и побеждает по right-longest-match
	mux.HandleFunc("GET /panel", serveIndex)
	mux.HandleFunc("GET /panel/", serveIndex)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/" {
			http.NotFound(w, req)
			return
		}
		http.Redirect(w, req, "/panel", http.StatusFound)
	})

	mux.HandleFunc("GET /panel/api/overview", func(w http.ResponseWriter, req *http.Request) {
		if !r.panelAuthorized(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(r.buildOverview())
	})

	// редактор конфигурации + ручной DNS-push + детальная статистика ноды
	mux.HandleFunc("GET /panel/api/config", r.handleGetConfig)
	mux.HandleFunc("PUT /panel/api/config", r.handlePutConfig)
	mux.HandleFunc("POST /panel/api/dns-push", r.handleDNSPush)
	mux.HandleFunc("GET /panel/api/node", r.handlePanelNode)
	// СРМД: перевод домена ручной ⇄ под контролем СРМД
	mux.HandleFunc("POST /panel/api/srmd-domain", r.handleSRMDDomain)

	mux.HandleFunc("GET /panel/api/events", func(w http.ResponseWriter, req *http.Request) {
		if !r.panelAuthorized(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		limit := 200
		if v := req.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = min(n, 1000)
			}
		}
		typeFilter := req.URL.Query().Get("type")

		r.mu.RLock()
		out := make([]Event, 0, min(limit, len(r.state.Events)))
		for i := len(r.state.Events) - 1; i >= 0 && len(out) < limit; i-- {
			ev := r.state.Events[i]
			if typeFilter != "" && ev.Type != typeFilter {
				continue
			}
			out = append(out, ev)
		}
		r.mu.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(map[string]any{"events": out})
	})
}

type panelNode struct {
	NodeID        string   `json:"node_id"`
	IP            string   `json:"ip"`
	Port          int      `json:"port"`
	IsMaster      bool     `json:"is_master"` // держит ≥1 домен
	MasterDomains []string `json:"master_domains,omitempty"`
	QueuePosition int      `json:"queue_position"` // 0 = вне очереди мастерства
	Healthy       bool     `json:"healthy"`        // tcp
	FullyHealthy  bool     `json:"fully_healthy"`
	GlobalpingOK  bool     `json:"globalping_ok"`
	MetricsOK     bool     `json:"metrics_ok"` // вердикт ПОСЛЕДНЕГО отчёта (сырой)
	// MetricsHealthy — защёлка по fail/recover-порогам; именно она
	// участвует в fully-healthy и ротации мастеров. MetricsFailStreak —
	// текущая серия подряд плохих отчётов (для подсказок «серия 1 из 3»).
	MetricsHealthy    bool      `json:"metrics_healthy"`
	MetricsFailStreak int       `json:"metrics_fail_streak,omitempty"`
	GlobalpingRatio   float64   `json:"globalping_ratio"`
	UnhealthyReason   string    `json:"unhealthy_reason,omitempty"`
	RegisteredAt      time.Time `json:"registered_at"`
	HeartbeatAgeSec   int64     `json:"heartbeat_age_sec"`
	ReportAgeSec      int64     `json:"report_age_sec"` // -1 = отчётов не было
	ReportError       string    `json:"report_error,omitempty"`
	HeartbeatsTotal   int       `json:"heartbeats_total"`
	ReportsTotal      int       `json:"reports_total"`
	ReportsOK         int       `json:"reports_ok"`
	AvailabilityPct   float64   `json:"availability_pct"`
	// ClientsUniqueIPs — уникальные активные клиентские IP по метрикам ноды
	// (см. uniqueIPsMetric). nil = старый агент/telemetry выключен — нет данных.
	ClientsUniqueIPs *float64 `json:"clients_unique_ips,omitempty"`
	GPChecksTotal    int      `json:"gp_checks_total"`
	GPVerifiedPct    float64  `json:"gp_verified_pct"`
	MasterStints     int      `json:"master_stints"`
	MasterTimeSec    int64    `json:"master_time_sec"`
	// NodeType: classic/mtproxyl/meko — бейдж типа менеджера в панели.
	NodeType string `json:"node_type,omitempty"`
	// Quarantine: не-nil — нода в GP-карантине, рендерится
	// отдельной таблицей (не в основном списке нод).
	Quarantine *panelQuarantine `json:"quarantine,omitempty"`
}

// panelQuarantine — состояние GP-карантина ноды для панели.
type panelQuarantine struct {
	Attempt   int       `json:"attempt"`            // текущая неудачная попытка (1-based)
	Max       int       `json:"max"`                // quarantine_attempts — предел до бана
	EnteredAt time.Time `json:"entered_at"`         // когда посажена
	Reverify  bool      `json:"reverify,omitempty"` // перепроверка старого забаненного ip
}

// PanelMaster — кому назначен managed-домен.
type panelMaster struct {
	Domain   string `json:"domain"`
	NodeID   string `json:"node_id,omitempty"`
	IP       string `json:"ip,omitempty"`
	Dead     bool   `json:"dead,omitempty"` // назначен, но нода пропала/нездорова
	NodeType string `json:"node_type,omitempty"`
	// (TTL мастерства): TTLSec — действующий лимит (0 = выкл);
	// TTLRemainingSec — сколько осталось до принудительной ротации
	// (<0 = лимит истёк, ждём здоровую замену).
	TTLSec          int64 `json:"ttl_sec,omitempty"`
	TTLRemainingSec int64 `json:"ttl_remaining_sec,omitempty"`
	// (СРМД): Clients — последние известные активные клиенты домена
	// (уникальные IP по общему секрету; nil = не измерялось); CNameTarget —
	// домен свёрнут СРМД и живёт CNAME-записью на этот домен (мастера нет).
	Clients     *int   `json:"clients,omitempty"`
	CNameTarget string `json:"cname_target,omitempty"`
}

// panelGPBan — один «бан» Globalping. Бан = globalping_blocked, после
// которого адрес НЕ восстановился: либо нода ушла из пула заблокированной
// (закрыт node_expired/node_replaced/node_pruned/node_terminated), либо он
// всё ещё заблокирован (active). Пара blocked→recovered баном НЕ считается —
// это самоустранившийся сбой. ОДИН IP считается баном ОДИН РАЗ: повторные
// баны того же адреса (перепроверки) новую запись не создают, а
// восстановление адреса (gp снова зелёный / reverify прошёл) убирает запись
// из статистики вовсе.
type panelGPBan struct {
	NodeID      string    `json:"node_id"`
	IP          string    `json:"ip,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	Active      bool      `json:"active"`
	DurationSec int64     `json:"duration_sec"`
	Cause       string    `json:"cause,omitempty"`
	ClosedBy    string    `json:"closed_by,omitempty"` // node_expired / node_replaced
}

// panelStats — агрегаты по журналу событий: баны GP, периодичность
// банов и DNS-смен, среднее время мастерства. Всё считается форвард-проходом
// по state.Events — метрики соответствуют тому, что админ видит в журнале.
type panelStats struct {
	EventsWindow int `json:"events_window"` // сколько событий анализировали

	GPBansTotal      int          `json:"gp_bans_total"`
	GPBansActive     int          `json:"gp_bans_active"`
	GPBansNodes      int          `json:"gp_bans_nodes"`
	GPBanIntervalSec *float64     `json:"gp_ban_interval_sec,omitempty"` // средний промежуток между банами
	GPBanHistory     []panelGPBan `json:"gp_ban_history,omitempty"`      // новые первыми, ≤15

	DNSSwitchIntervalSec *float64 `json:"dns_switch_interval_sec,omitempty"` // средний промежуток между DNS-сменами
	MasterAvgSec         *float64 `json:"master_avg_sec,omitempty"`          // средний stint мастерства
}

// avgGapSec — средний промежуток между соседними моментами (telescoping: span
// делится на n-1). nil, если точек меньше двух — метрику не показываем.
func avgGapSec(times []time.Time) *float64 {
	if len(times) < 2 {
		return nil
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	avg := times[len(times)-1].Sub(times[0]).Seconds() / float64(len(times)-1)
	return &avg
}

// gpBanHistoryCap — сколько последних банов отдаём в панель.
const gpBanHistoryCap = 15

// buildPanelStatsLocked — агрегаты по журналу: баны GP, периодичности,
// мастерство. ВЫЗЫВАТЬ под r.mu (минимум RLock). Чистый форвард-проход,
// O(events); журнал ограничен events_max, так что это дёшево.
//
// Семантика банов: ОДИН IP = ОДИН бан. Повторный globalping_blocked того же
// адреса (перепроверка, ре-бан) новую запись не открывает — адрес уже учтён.
// Восстановление адреса (globalping_recovered либо ban_lifted после
// reverify) убирает его бан из статистики полностью, даже если нода с этим
// адресом уже ушла из пула. Уход ноды (expired/replaced/pruned/terminated)
// бан НЕ убирает: адрес остаётся заблокированным, запись просто закрывается.
func (r *Registry) buildPanelStatsLocked(now time.Time) panelStats {
	st := panelStats{EventsWindow: len(r.state.Events)}

	type ipBan struct {
		ip       string
		start    time.Time
		nodeID   string
		cause    string
		closedAt time.Time // нода ушла из пула; ноль — ещё активен
		closedBy string
	}
	byIP := map[string]*ipBan{}   // адрес → его бан (пока не восстановится)
	byNode := map[string]*ipBan{} // nodeID → бан его адреса
	nodesSeen := map[string]bool{}
	var banStarts []time.Time // начала банов (для периодичности)
	var dnsTimes []time.Time  // per-domain смены DNS

	openBan := func(ev Event) {
		if ev.IP == "" || byIP[ev.IP] != nil {
			return // без адреса не пишем; этот адрес уже учтён как бан
		}
		b := &ipBan{ip: ev.IP, start: ev.At, nodeID: ev.NodeID, cause: ev.Detail}
		byIP[ev.IP] = b
		if ev.NodeID != "" {
			byNode[ev.NodeID] = b
		}
	}
	liftBan := func(ev Event) {
		b := byIP[ev.IP]
		if b == nil {
			// восстановление без ip в событии — ищем по ноде
			b = byNode[ev.NodeID]
		}
		if b == nil {
			return
		}
		delete(byIP, b.ip)
		if byNode[b.nodeID] == b {
			delete(byNode, b.nodeID)
		}
	}

	type stintKey struct{ node, domain string }
	openStints := map[stintKey]time.Time{}
	stintCount := 0
	stintTotal := 0.0

	for _, ev := range r.state.Events {
		switch ev.Type {
		case EventGlobalpingBlocked:
			openBan(ev)
		case EventGlobalpingRecovered:
			// адрес снова зелёный — бана нет (сбой самоустранился, либо
			// восстановился уже забаненный адрес — запись убирается)
			liftBan(ev)
		case EventBanLifted:
			// reverify прошёл — адрес восстановлен, бан из статистики долой
			liftBan(ev)
		case EventNodeExpired, EventNodeReplaced, EventNodePruned, EventNodeTerminated:
			// нода ушла из пула заблокированной — бан адреса остаётся,
			// запись закрывается (адрес ведь не восстановился)
			if b := byNode[ev.NodeID]; b != nil && b.closedAt.IsZero() {
				b.closedAt = ev.At
				b.closedBy = ev.Type
			}
		case EventDNSUpdated:
			// только per-domain смены; агрегированные manual-push без домена
			// в периодичность не входят (оно бы исказило среднее)
			if ev.Domain != "" {
				dnsTimes = append(dnsTimes, ev.At)
			}
		case EventMasterElected:
			openStints[stintKey{ev.NodeID, ev.Domain}] = ev.At
		case EventMasterLost:
			key := stintKey{ev.NodeID, ev.Domain}
			start, ok := openStints[key]
			if !ok {
				// master_lost без домена (stint-reconcile «no healthy
				// replacement») — закрывает самый ранний stint этой ноды
				for k, at := range openStints {
					if k.node == ev.NodeID && (!ok || at.Before(start)) {
						key, start, ok = k, at, true
					}
				}
			}
			if ok {
				delete(openStints, key)
				stintTotal += ev.At.Sub(start).Seconds()
				stintCount++
			}
		}
	}

	// собираем не восстановившиеся баны: закрытые (нода ушла) и активные
	var bans []panelGPBan
	for ip, b := range byIP {
		pb := panelGPBan{NodeID: b.nodeID, IP: ip, StartedAt: b.start, Cause: b.cause}
		if b.closedAt.IsZero() {
			pb.Active = true
			pb.DurationSec = int64(now.Sub(b.start).Seconds())
		} else {
			pb.DurationSec = int64(b.closedAt.Sub(b.start).Seconds())
			pb.ClosedBy = b.closedBy
		}
		bans = append(bans, pb)
		banStarts = append(banStarts, b.start)
		if b.nodeID != "" {
			nodesSeen[b.nodeID] = true
		}
	}
	// незакрытые stint'ы — текущие мастера: время до «сейчас»
	for _, at := range openStints {
		stintTotal += now.Sub(at).Seconds()
		stintCount++
	}

	st.GPBansTotal = len(bans)
	st.GPBansNodes = len(nodesSeen)
	st.GPBanIntervalSec = avgGapSec(banStarts)
	st.DNSSwitchIntervalSec = avgGapSec(dnsTimes)
	if stintCount > 0 {
		avg := stintTotal / float64(stintCount)
		st.MasterAvgSec = &avg
	}
	for _, b := range bans {
		if b.Active {
			st.GPBansActive++
		}
	}
	// история новыми вперёд, обрезка по капу
	sort.Slice(bans, func(i, j int) bool { return bans[i].StartedAt.After(bans[j].StartedAt) })
	if len(bans) > gpBanHistoryCap {
		bans = bans[:gpBanHistoryCap]
	}
	st.GPBanHistory = bans
	return st
}

type panelOverview struct {
	StartedAt time.Time     `json:"started_at"`
	UptimeSec int64         `json:"uptime_sec"`
	Now       time.Time     `json:"now"`
	Domains   []string      `json:"domains"`
	TLSDomain string        `json:"tls_domain"`
	Masters   []panelMaster `json:"masters"`
	// MasterTTLSec: действующий лимит мастерства, сек (0 = выкл.).
	MasterTTLSec      int64   `json:"master_ttl_sec"`
	NodesTotal        int     `json:"nodes_total"`
	NodesTCPOK        int     `json:"nodes_tcp_ok"`
	NodesGPVerified   int     `json:"nodes_gp_verified"`
	NodesMetricsOK    int     `json:"nodes_metrics_ok"`
	NodesFullyHealthy int     `json:"nodes_fully_healthy"`
	NodesQuarantined  int     `json:"nodes_quarantined"` // в GP-карантине
	QueueSize         int     `json:"queue_size"`
	WritersActive     float64 `json:"writers_active_total"`
	// ClientsUniqueTotal — сумма unique-IP клиентов по нодам, приславшим метрику.
	// nil = ни одна нода её не присылает (старые агенты / user_enabled=false).
	ClientsUniqueTotal *float64 `json:"clients_unique_ips_total"`
	Counters           Counters `json:"counters"`
	// Stats: баны GP, периодичность банов/DNS-смен, среднее мастерство.
	Stats panelStats  `json:"stats"`
	Nodes []panelNode `json:"nodes"`
	// SRMD: система распределения и масштабирования доменов —
	// таблица «домен | активные пользователи» + её настройки/алерт.
	SRMD *panelSRMD `json:"srmd,omitempty"`
}

// buildOverview собирает снимок состояния пула для панели (под RLock).
func (r *Registry) buildOverview() panelOverview {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	// hot-поля cfg (панель их правит) читаем под cfgMu — порядок mu→cfgMu соблюдён
	r.cfgMu.RLock()
	cfgDomains := append([]string(nil), r.cfg.Cloudflare.Domains...)
	cfgTLSDomain := r.cfg.SharedProxy.TLSDomain
	ttl := time.Duration(resolveMasterTTLMinutes(r.cfg.Rotation.MasterTTLMinutes)) * time.Minute
	// Настройки СРМД (тоже hot-apply)
	srmdEnabled := r.cfg.SRMD.Enabled != nil && *r.cfg.SRMD.Enabled
	srmdBase := strings.TrimSpace(r.cfg.SRMD.BaseDomain)
	srmdMaxN := resolveSRMDMaxNodes(r.cfg.SRMD.MaxNodesPerDomain)
	r.cfgMu.RUnlock()
	ov := panelOverview{
		StartedAt:    r.startedAt,
		UptimeSec:    int64(now.Sub(r.startedAt).Seconds()),
		Now:          now,
		Domains:      cfgDomains,
		TLSDomain:    cfgTLSDomain,
		Nodes:        make([]panelNode, 0, len(r.state.Candidates)),
		Counters:     r.state.Counters,
		MasterTTLSec: int64(ttl.Seconds()),
	}

	// очередь мастерства: тот же порядок, что и у evaluateAssignments
	queue := make([]*Candidate, 0, len(r.state.Candidates))
	for _, c := range r.state.Candidates {
		if c.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
			queue = append(queue, c)
		}
	}
	sort.Slice(queue, func(a, b int) bool {
		x, y := queue[a], queue[b]
		if !x.QueuedAt.Equal(y.QueuedAt) {
			return x.QueuedAt.Before(y.QueuedAt)
		}
		if !x.RegisteredAt.Equal(y.RegisteredAt) {
			return x.RegisteredAt.Before(y.RegisteredAt)
		}
		return x.NodeID < y.NodeID
	})
	queuePos := make(map[*Candidate]int, len(queue))
	for i, c := range queue {
		queuePos[c] = i + 1
	}

	// Per-domain мастера. holdsBy — nodeID → его домены (для роли ноды),
	// masters — карточка раскладки по доменам из конфига.
	holdsBy := make(map[string][]string, len(r.state.Assignments))
	for d, id := range r.state.Assignments {
		holdsBy[id] = append(holdsBy[id], d)
	}
	for id := range holdsBy {
		sort.Strings(holdsBy[id])
	}
	domainsSeen := make(map[string]bool, len(ov.Domains))
	for _, d := range ov.Domains {
		d = strings.TrimSpace(d)
		if d == "" || domainsSeen[d] {
			continue
		}
		domainsSeen[d] = true
		m := panelMaster{Domain: d}
		// Таблица клиентов СРМД и CNAME-статус домена
		if cl, ok := r.state.SRMD.DomainClients[d]; ok {
			v := cl
			m.Clients = &v
		}
		if cn := r.state.SRMD.CNames[d]; cn != "" {
			m.CNameTarget = cn
		}
		if id := r.state.Assignments[d]; id != "" {
			m.NodeID = id
			if c := r.state.Candidates[id]; c != nil {
				m.IP = c.IP
				m.NodeType = c.NodeType
				m.Dead = !c.IsFullyHealthy(r.cfg.ReportFreshnessTTL)
			} else {
				m.Dead = true
			}
			// TTL мастерства: заполняем, только когда назначение отсчитано
			// (лениво инициализирует since на первом evaluate).
			if since := r.state.AssignmentsSince[d]; ttl > 0 && !since.IsZero() {
				m.TTLSec = int64(ttl.Seconds())
				m.TTLRemainingSec = int64(ttl.Seconds()) - int64(now.Sub(since).Seconds())
			}
		}
		ov.Masters = append(ov.Masters, m)
	}
	sort.Slice(ov.Masters, func(i, j int) bool { return ov.Masters[i].Domain < ov.Masters[j].Domain })

	ids := make([]string, 0, len(r.state.Candidates))
	for id := range r.state.Candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var clientsSum float64
	clientsAny := false

	for _, id := range ids {
		c := r.state.Candidates[id]
		n := r.buildPanelNodeLocked(c, queuePos[c], holdsBy[id], now)

		ov.NodesTotal++
		if c.Healthy {
			ov.NodesTCPOK++
		}
		if c.GlobalpingOK {
			ov.NodesGPVerified++
		}
		if c.MetricsHealthy { // счётчик по защёлке, а не по сырому последнему отчёту
			ov.NodesMetricsOK++
		}
		if n.FullyHealthy {
			ov.NodesFullyHealthy++
		}
		if n.Quarantine != nil {
			ov.NodesQuarantined++
		}
		if w, ok := c.MetricsSnapshot[writersMetric]; ok {
			ov.WritersActive += w
		}
		if n.ClientsUniqueIPs != nil {
			clientsSum += *n.ClientsUniqueIPs
			clientsAny = true
		}
		ov.Nodes = append(ov.Nodes, n)
	}

	if clientsAny {
		ov.ClientsUniqueTotal = &clientsSum
	}

	ov.QueueSize = len(queue)
	ov.Stats = r.buildPanelStatsLocked(now)
	// Снимок СРМД (таблица доменов, нужное число доменов, алерт)
	ov.SRMD = r.buildPanelSRMDLocked(len(queue), srmdEnabled, srmdBase, srmdMaxN)
	return ov
}

// buildPanelNodeLocked собирает panelNode по кандидату — общий конструктор для
// overview и страницы ноды. Вызывать под r.mu (минимум RLock).
func (r *Registry) buildPanelNodeLocked(c *Candidate, queuePos int, masterDomains []string, now time.Time) panelNode {
	n := panelNode{
		NodeID:            c.NodeID,
		IP:                c.IP,
		Port:              c.Port,
		IsMaster:          len(masterDomains) > 0,
		MasterDomains:     masterDomains,
		QueuePosition:     queuePos,
		Healthy:           c.Healthy,
		FullyHealthy:      c.IsFullyHealthy(r.cfg.ReportFreshnessTTL),
		GlobalpingOK:      c.GlobalpingOK,
		MetricsOK:         c.MetricsOK,
		MetricsHealthy:    c.MetricsHealthy,
		MetricsFailStreak: c.MetricsFailStreak,
		GlobalpingRatio:   c.GlobalpingVerifiedRatio,
		RegisteredAt:      c.RegisteredAt,
		HeartbeatAgeSec:   int64(now.Sub(c.LastHeartbeat).Seconds()),
		ReportAgeSec:      -1,
		ReportError:       c.ReportError,
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
	if c.Quarantine != nil { //
		n.Quarantine = &panelQuarantine{
			Attempt: c.Quarantine.Attempts, Max: r.cfg.QuarantineAttempts,
			EnteredAt: c.Quarantine.EnteredAt, Reverify: c.Quarantine.Reverify,
		}
	}
	if !c.LastReportAt.IsZero() {
		n.ReportAgeSec = int64(now.Sub(c.LastReportAt).Seconds())
	}
	if n.QueuePosition == 0 {
		n.UnhealthyReason = c.unhealthyReason(r.cfg.ReportFreshnessTTL)
	}
	if v, ok := c.MetricsSnapshot[uniqueIPsMetric]; ok {
		vv := v
		n.ClientsUniqueIPs = &vv
	}
	return n
}

// queuePositionLocked — позиция ноды в очереди мастерства (0 = вне очереди).
// Тот же порядок, что у evaluateAssignments (нод мало, полный проход дёшев
// для точечного запроса). ВЫЗЫВАТЬ под r.mu (минимум RLock).
func (r *Registry) queuePositionLocked(c *Candidate) int {
	if !c.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
		return 0
	}
	pos := 1
	for _, other := range r.state.Candidates {
		if other == c || !other.IsFullyHealthy(r.cfg.ReportFreshnessTTL) {
			continue
		}
		if other.QueuedAt.Before(c.QueuedAt) ||
			(other.QueuedAt.Equal(c.QueuedAt) && other.RegisteredAt.Before(c.RegisteredAt)) ||
			(other.QueuedAt.Equal(c.QueuedAt) && other.RegisteredAt.Equal(c.RegisteredAt) && other.NodeID < c.NodeID) {
			pos++
		}
	}
	return pos
}

// masterDomainsLocked — отсортированный список доменов, которые держит нода.
// ВЫЗЫВАТЬ под r.mu (минимум RLock).
func (r *Registry) masterDomainsLocked(id string) []string {
	domains := make([]string, 0, 1)
	for d, nid := range r.state.Assignments {
		if nid == id {
			domains = append(domains, d)
		}
	}
	sort.Strings(domains)
	return domains
}

// handlePanelNode — GET /panel/api/node?id=…: детальная статистика ноды.
// Агрегаты + истории TCP/GP/metrics-проверок (кольцевые буферы), деталь
// последней верификации Globalping по площадкам и журнал событий этой ноды.
func (r *Registry) handlePanelNode(w http.ResponseWriter, req *http.Request) {
	if !r.panelAuthorized(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := req.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}

	now := time.Now()
	r.mu.RLock()
	c := r.state.Candidates[id]
	if c == nil {
		r.mu.RUnlock()
		http.Error(w, "unknown node", http.StatusNotFound)
		return
	}
	queuePos := r.queuePositionLocked(c)
	domains := r.masterDomainsLocked(id)
	events := make([]Event, 0, 16)
	for i := len(r.state.Events) - 1; i >= 0 && len(events) < 100; i-- {
		if r.state.Events[i].NodeID == id {
			events = append(events, r.state.Events[i])
		}
	}
	resp := map[string]any{
		"node":        r.buildPanelNodeLocked(c, queuePos, domains, now),
		"tcp_hist":    c.TCPHist,
		"gp_hist":     c.GPHist,
		"report_hist": c.ReportHist,
		"gp_last":     c.GPLast,
		"events":      events,
		// hc: пороги защёлок для подсказок панели («серия 1 из 3»)
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
