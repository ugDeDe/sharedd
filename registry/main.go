package main

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudflare/cloudflare-go"
)

type Candidate struct {
	NodeID          string    `json:"node_id"`
	IP              string    `json:"ip"`
	RegisteredAt    time.Time `json:"registered_at"`
	LastHeartbeat   time.Time `json:"last_heartbeat"`
	Healthy         bool      `json:"healthy"`
	ConsecutiveFail int       `json:"-"`
	ConsecutiveOK   int       `json:"-"`

	// Очередь мастерства считается по НЕПРЕРЫВНОМУ здоровью, а не по RegisteredAt:
	// QueuedAt — момент последнего входа в fully-healthy состояние; пока нода
	// здорова, позиция не меняется. Выпала из fully-healthy по любой причине
	// (telemt умер, globalping упал, отчёты протухли, TCP не отвечает) — при
	// возврате встаёт в конец очереди. См. evaluateAssignments.
	FullyHealthy bool      `json:"fully_healthy"`
	QueuedAt     time.Time `json:"queued_at"`

	// UnhealthySince (V7.9.6) — момент последнего выхода из очереди здоровых
	// (= старт непрерывного «всё красное» эпизода). Ноль — нода в очереди.
	// Используется рипером prune (sweepExpired): непрерывно вне очереди
	// дольше PruneUnhealthyTTL → удаление из пула. Персистится в state.
	// Вооружается в evaluateAssignments (lazy-arm для нод, бывших
	// нездоровыми на момент апгрейда, и свежезарегистрированных).
	UnhealthySince time.Time `json:"unhealthy_since,omitempty"`

	// DeadBothSince (V7.9.11) — старт непрерывного окна класса dead:
	// TCP-защёлка красная И метрик нет (отчёты протухли или красные).
	// Достиг TerminateDeadTTL → терминальное завершение (sweepExpired).
	DeadBothSince time.Time `json:"dead_both_since,omitempty"`
	// Quarantine (V7.9.11) — нода в GP-карантине («всё зелёное, кроме
	// globalping»): панель показывает отдельной таблицей, prune-рипер не
	// трогает; вердикт — счётчик попыток → бан по IP или восстановление.
	Quarantine *QuarantineState `json:"quarantine,omitempty"`

	GlobalpingOK            bool    `json:"globalping_ok"`
	GlobalpingMeasurementID string  `json:"globalping_measurement_id"`
	GlobalpingVerifiedRatio float64 `json:"globalping_verified_ratio"`
	MetricsOK               bool    `json:"metrics_ok"` // сырой вердикт ПОСЛЕДНЕГО отчёта (мигает от любого чиха)
	// MetricsHealthy (V7.9.4) — защёлка metrics-здоровья по fail/recover-
	// порогам, полный аналог TCP-защёлки Healthy: в fully-healthy входит
	// ИМЕННО она, а не MetricsOK последнего отчёта. false — только после
	// FailThreshold ПОДРЯД неудачных отчётов; обратно true — после
	// RecoverThreshold подряд удачных. Одиночный сбойный отчёт мастерство
	// больше не роняет. Серии персистить не нужно (как ConsecutiveFail —
	// рестарт регистратора честно обнуляет серию).
	MetricsHealthy    bool               `json:"metrics_healthy"`
	MetricsFailStreak int                `json:"-"`
	MetricsOKStreak   int                `json:"-"`
	MetricsSnapshot   map[string]float64 `json:"metrics_snapshot,omitempty"`
	LastReportAt      time.Time          `json:"last_report_at"`
	ReportError       string             `json:"report_error,omitempty"`

	Port int `json:"port"`

	// NodeType (V7.9): classic/mtproxyl/meko — тип менеджера прокси на ноде,
	// информационный бейдж в панели.
	NodeType string `json:"node_type,omitempty"`

	// Накопительная статистика ноды (для панели/доступности).
	HeartbeatsTotal int `json:"heartbeats_total"`
	ReportsTotal    int `json:"reports_total"`
	ReportsOK       int `json:"reports_ok"`
	GPChecksTotal   int `json:"gp_checks_total"` // сколько раз независимо проверяли Globalping
	GPChecksOK      int `json:"gp_checks_ok"`

	// Учёт времени в роли мастера: MasterSince — начало текущего stint'а,
	// MasterSeconds — сумма закрытых stint'ов (сек).
	MasterStints  int       `json:"master_stints"`
	MasterSeconds int64     `json:"master_seconds"`
	MasterSince   time.Time `json:"master_since,omitempty"`

	// История для детальной страницы ноды (V7.7): кольцевые буферы точек,
	// персистятся вместе с State. GPLast — деталь последнего measurement'а
	// Globalping (результаты по площадкам), заполняется только при прохождении
	// независимой верификации (measurement успешно скачан и распарсен).
	TCPHist    []TCPPoint    `json:"tcp_hist,omitempty"`
	GPHist     []GPPoint     `json:"gp_hist,omitempty"`
	ReportHist []ReportPoint `json:"report_hist,omitempty"`
	GPLast     *GPDetail     `json:"gp_last,omitempty"`
}

// ── история точек проверок (V7.7) ───────────────────────────────────────

const (
	tcpHistCap    = 360 // тик probeLoop = ProbeInterval (10с по дефолту) → ~1 час
	gpHistCap     = 288 // globalping_ms (5 мин) → ~24 часа
	reportHistCap = 360 // metrics_ms (60с) → ~6 часов
)

type TCPPoint struct {
	At time.Time `json:"at"`
	OK bool      `json:"ok"`
}

type GPPoint struct {
	At          time.Time `json:"at"`
	OK          bool      `json:"ok"`
	Ratio       float64   `json:"ratio"`
	ProbesOK    int       `json:"probes_ok"`
	ProbesTotal int       `json:"probes_total"`
}

type ReportPoint struct {
	At        time.Time `json:"at"`
	MetricsOK bool      `json:"metrics_ok"`
	Clients   int       `json:"clients"` // -1 = метрики нет (старый агент / user_enabled=false)
	Writers   int       `json:"writers"`
}

type GPProbeLine struct {
	Country  string `json:"country"`
	City     string `json:"city,omitempty"`
	Network  string `json:"network,omitempty"`
	ASN      int    `json:"asn,omitempty"`
	OK       bool   `json:"ok"`
	Status   string `json:"status"`
	HTTPCode int    `json:"http_code,omitempty"`
}

type GPDetail struct {
	At            time.Time     `json:"at"`
	MeasurementID string        `json:"measurement_id"`
	OK            bool          `json:"ok"`
	Ratio         float64       `json:"ratio"`
	ProbesOK      int           `json:"probes_ok"`
	ProbesTotal   int           `json:"probes_total"`
	Probes        []GPProbeLine `json:"probes,omitempty"`
}

// pushRing — append в кольцевой буфер с лимитом длины (старшие точки вытесняются).
func pushRing[T any](s []T, v T, limit int) []T {
	s = append(s, v)
	if n := len(s) - limit; n > 0 {
		copy(s, s[n:])
		s = s[:limit]
	}
	return s
}

type State struct {
	Candidates map[string]*Candidate `json:"candidates"`
	// Assignments — V7: per-domain мастера, domain → node_id. Каждый managed-
	// домен держит свою ноду; при дефиците нод мастера забирают «сиротские»
	// домены (fill-empty), при появлении свободной ноды сирота отдаётся ей.
	Assignments map[string]string `json:"assignments,omitempty"`
	// AssignmentsSince — V7.9.3: domain → момент текущего назначения. База TTL
	// мастерства ([rotation] master_ttl_minutes); персистится — отсчёт не
	// сбрасывается рестартом регистратора. Назначениям, доставшимся от версий
	// до V7.9.3, поле инициализируется лениво на ближайшем evaluate (с этого
	// момента и начнётся отсчёт лимита).
	AssignmentsSince map[string]time.Time `json:"assignments_since,omitempty"`
	Events           []Event              `json:"events,omitempty"`
	Counters         Counters             `json:"counters"`
	// PruneStrikes — V7.9.7: карантин вычищенных рипером нод (node_id →
	// запись). До BannedUntil /register отклоняется 429; серия strikes
	// наращивает карантин, вход ноды в очередь здоровых серию обнуляет.
	PruneStrikes map[string]*PruneTombstone `json:"prune_strikes,omitempty"`
	// ManagedDomains (V7.8) — все домены, которыми регистратор когда-либо
	// управлял (бывшие домены конфига ∪ ключи назначений). Только для них
	// разрешена зачистка DNS при удалении из конфига — чужие записи зоны
	// (другие сервисы) сюда не попадают и не трогаются никогда.
	ManagedDomains []string `json:"managed_domains,omitempty"`
	// Terminated — V7.9.11: оперативный блок-лист убитых нод (node_id →
	// запись). Обращение под таким id получает 403+terminate (нода пишет
	// Message в лог и останавливается); ip_ban с НОВОГО ip не блокируется.
	// Вечная история всех банов — в SQLite (bans), сюда она не нужна.
	Terminated map[string]*TerminatedRecord `json:"terminated,omitempty"`
}

type Registry struct {
	cfg   *resolvedRegistryConfig
	cf    cfDNSAPI     // заменяется при смене api_token через панель
	mu    sync.RWMutex // state (кандидаты, журнал, счётчики)
	state State

	// db — V7.9.11: вечная история (SQLite). nil = работаем без неё
	// (файл не открылся / отключено) — пул и панель не страдают.
	db *historyDB

	// cfgMu защищает "горячие" поля cfg (панель их правит на лету).
	// ПОРЯДОК ЛОКОВ: сначала mu, потом cfgMu — никогда наоборот.
	cfgMu sync.RWMutex

	startedAt   time.Time
	eventsDirty bool // журнал изменился с последнего persistStateLocked (под mu)

	// ttlOverdue — V7.9.3: домены с истёкшим TTL мастерства, для которых УЖЕ
	// залогировано «нет здоровой замены» (in-memory anti-spam: лог раз за
	// эпизод, сбрасывается при ротации/смене держателя или рестарте процесса).
	// Пишется только из evaluateAssignments — под r.mu.
	ttlOverdue map[string]bool

	// banLogTick — V7.9.7: последний лог отказа /register по карантину на
	// node_id (in-memory anti-spam: старые агенты без Retry-After долбят
	// register по каждому heartbeat — в журнал не чаще раза в минуту).
	banLogTick map[string]time.Time
}

// newCFClient — фабрика Cloudflare-клиента (var ради подмены в тестах).
var newCFClient = func(token string) (cfDNSAPI, error) {
	return cloudflare.NewWithAPIToken(token)
}

func main() {
	cfg, err := loadRegistryConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	cf, err := newCFClient(cfg.Cloudflare.APIToken)
	if err != nil {
		log.Fatalf("cloudflare client error: %v", err)
	}

	reg := &Registry{
		cfg: cfg,
		cf:  cf,
		state: State{
			Candidates: make(map[string]*Candidate),
		},
		startedAt:  time.Now(),
		ttlOverdue: make(map[string]bool),
	}
	reg.loadState()
	if cfg.DBEnabled {
		reg.db = openHistoryDB(cfg.Database.File)
	}

	reg.mu.Lock()
	reg.addEventLocked(Event{
		Type:   EventRegistryStarted,
		Detail: fmt.Sprintf("loaded %d candidates, %d domain assignments, listen=%s", len(reg.state.Candidates), len(reg.state.Assignments), cfg.HTTP.Addr),
	})
	reg.persistStateLocked()
	reg.mu.Unlock()

	if reg.cfg.PanelEnabled && reg.cfg.Panel.Token == "" {
		log.Printf("WARNING: panel token is empty — /panel API and /status are unauthenticated (set [panel] token)")
	}

	go reg.probeLoop()
	go reg.selectionLoop()
	go reg.expiryLoop()
	go reg.eventPersistLoop()
	go reg.historyDBLoop() // V7.9.11: ротация событий в SQLite (баны вечны)
	reg.serveHTTP()
}

type registerRequest struct {
	NodeID string `json:"node_id"`
	IP     string `json:"ip"`
	// NodeType (V7.9): classic/mtproxyl/meko — информационный бейдж в панели.
	NodeType string `json:"node_type,omitempty"`
}

type nodeIntervals struct {
	HeartbeatMs  int `json:"heartbeat_ms"`
	GlobalpingMs int `json:"globalping_ms"`
	MetricsMs    int `json:"metrics_ms"`
	SyncMs       int `json:"sync_ms"`
}

type sharedConfigResponse struct {
	TLSDomain string            `json:"tls_domain"`
	Users     map[string]string `json:"users"`
	Intervals nodeIntervals     `json:"intervals"`
}

func (r *Registry) serveHTTP() {
	log.Printf("registry HTTP listening on %s", r.cfg.HTTP.Addr)
	log.Fatal(http.ListenAndServe(r.cfg.HTTP.Addr, r.buildMux()))
}

// buildMux собирает все маршруты. Для Go-1.22+ mux все паттерны метод-квалифицированы —
// иначе "GET /" из панели конфликтует с unqualified "/register" (panic на старте).
func (r *Registry) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /register", func(w http.ResponseWriter, req *http.Request) {
		var body registerRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if body.NodeID == "" || body.IP == "" {
			http.Error(w, "node_id, ip required", http.StatusBadRequest)
			return
		}
		// V7.9.11: терминально убитая нода; V7.9.12: ip_ban по ТОМУ ЖЕ ip
		// не вечен — даём одну GP-перепроверку (reverify в registerWithReverify;
		// ложные глобалпинги, прокси был выключен при отладке). dead —
		// бессрочен, агенту kill-сигнал. Совпадение по ip с ЧУЖОЙ записью —
		// тоже reverify (переустановка агента с новым id старый ip не отмывает).
		r.mu.RLock()
		rec := r.terminatedBlockingLocked(body.NodeID, body.IP)
		if rec == nil && body.IP != "" {
			rec = r.terminatedIPBanByIPLocked(body.IP)
		}
		r.mu.RUnlock()
		if rec != nil && !reverifyOpenLocked(rec, time.Now()) {
			log.Printf("register from terminated node %s (%s) rejected: %s (reverify_failed=%t)",
				body.NodeID, body.IP, rec.Reason, rec.ReverifyFailed)
			r.writeTerminate(w, rec)
			return
		}
		if banUntil, banned := r.registerWithReverify(body, rec); banned {
			// V7.9.7: карантин после prune — 429 + Retry-After. Агенты V7.9.7+
			// уважают Retry-After и молчат до дедлайна; старые продолжат
			// долбиться по heartbeat'ам — отказ дешёвый, лог троттлится.
			retryAfter := int(time.Until(banUntil).Seconds()) + 1
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintf(w, `{"error":"node was pruned as inactive, re-registration deferred","retry_after_sec":%d}`+"\n", retryAfter)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /heartbeat", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			NodeID string `json:"node_id"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.NodeID == "" {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		r.mu.Lock()
		c, ok := r.state.Candidates[body.NodeID]
		if ok {
			c.LastHeartbeat = time.Now()
			c.HeartbeatsTotal++
			r.state.Counters.Heartbeats++
		}
		// V7.9.11: heartbeat от терминально убитой ноды — kill-сигнал
		// (403+terminate). IP берём из соединения: для ip_ban сменившийся ip
		// блока не имеет — обычный 410 «перерегистрируйся» (register,
		// пусть и перезаписью, снимет терминальную запись по смене ip).
		var rec *TerminatedRecord
		if !ok {
			host, _, _ := net.SplitHostPort(req.RemoteAddr)
			rec = r.terminatedBlockingLocked(body.NodeID, host)
		}
		r.mu.Unlock()
		if rec != nil {
			log.Printf("heartbeat from terminated node %s — sending kill (%s)", body.NodeID, rec.Reason)
			r.writeTerminate(w, rec)
			return
		}
		if !ok {
			// V7.9.6: ноду удалили (heartbeat-expiry / prune-рипер). Без явного
			// отказа живой агент удалённой ноды слал бы heartbeat'ы в пустоту
			// вечно и никогда не вернулся бы в пул. Агент на любой статус !=200
			// пере-регистрируется (node/main.go heartbeatLoop).
			log.Printf("heartbeat from unknown node %s (pruned/expired?) — asking to re-register", body.NodeID)
			http.Error(w, "unknown node_id, re-register", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /report", r.handleHealthReport)

	// POST /retire (V7.9.11) — агент сообщает о само-завершении по классу
	// dead (локальные проверки красные > terminate_dead_min): регистратор
	// обязан записать бан в вечную историю, даже если кандидат уже выпал
	// по heartbeat-TTL, пока нода молчала. Привязка доверия — RemoteAddr ==
	// заявленный ip (модель угроз как у открытого /register; стучаться в
	// retire от чужого имени без его маршрутизируемого адреса нельзя).
	mux.HandleFunc("POST /retire", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			NodeID string `json:"node_id"`
			IP     string `json:"ip"`
			Reason string `json:"reason"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.NodeID == "" || body.IP == "" {
			http.Error(w, "node_id, ip required", http.StatusBadRequest)
			return
		}
		host, _, _ := net.SplitHostPort(req.RemoteAddr)
		if host != body.IP {
			http.Error(w, "ip mismatch", http.StatusForbidden)
			return
		}
		if body.Reason != BanReasonIPBan && body.Reason != BanReasonDead {
			body.Reason = BanReasonDead
		}
		now := time.Now()
		r.mu.Lock()
		if c, ok := r.state.Candidates[body.NodeID]; ok {
			c.IP = body.IP // на случай дрифта после регистрации
			// V7.9.12: само-завершение ноды ВО ВРЕМЯ карантина — это выход
			// из карантина, т.е. бан по ip (её класс уже определён GP).
			if c.Quarantine != nil && body.Reason == BanReasonDead {
				body.Reason = BanReasonIPBan
			}
			r.terminateNodeLocked(c, now, body.Reason, "self-retire")
		} else {
			r.terminateRetiredLocked(body.NodeID, body.IP, now, body.Reason)
		}
		r.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /config", func(w http.ResponseWriter, req *http.Request) {
		// снапшот под cfgMu: эти секции панель может править на лету
		r.cfgMu.RLock()
		resp := sharedConfigResponse{
			TLSDomain: r.cfg.SharedProxy.TLSDomain,
			Users:     maps.Clone(r.cfg.SharedProxy.Users),
			Intervals: nodeIntervals{
				HeartbeatMs:  r.cfg.NodeDefaults.HeartbeatMs,
				GlobalpingMs: r.cfg.NodeDefaults.GlobalpingMs,
				MetricsMs:    r.cfg.NodeDefaults.MetricsMs,
				SyncMs:       r.cfg.NodeDefaults.SyncMs,
			},
		}
		r.cfgMu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// /status отдаёт всё состояние (включая IP нод) — когда панель защищена
	// токеном, /status защищается тем же токеном; без токена — как раньше, открыт.
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, req *http.Request) {
		if !r.panelAuthorized(req) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(r.state)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	r.mountPanel(mux)
	r.mountStats(mux)     // V7.9.4: публичная статистика нод (/statistics/...)
	r.mountDashboard(mux) // V7.9.11: публичный дашборд блокировок (/dashboard/...)

	return mux
}

// writeTerminate — kill-сигнал агенту (V7.9.11): 403 + флаг terminate.
// Агент пишет Message в лог дословно, кладёт tombstone и останавливает
// службу (node/terminate.go); повторных регистраций быть не должно.
func (r *Registry) writeTerminate(w http.ResponseWriter, rec *TerminatedRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	json.NewEncoder(w).Encode(map[string]any{
		"terminate": true,
		"reason":    rec.Reason,
		"message":   rec.Message,
	})
}

// register — /register для ноды. Возвращает (banUntil, true), если нода под
// карантином V7.9.7 (вычищена рипером и BannedUntil ещё не истёк) — хендлер
// отвечает 429 + Retry-After, в state ничего не пишется (событие тоже не
// плодим: отказов много, лента одна — подробности уже есть в node_pruned).
func (r *Registry) register(body registerRequest) (time.Time, bool) {
	return r.registerWithReverify(body, nil)
}

// registerWithReverify — register + reverify-трек V7.9.12: rec не-nil,
// когда нода регистрируется со СТАРОГО gp-забаненного ip (своего или чужого
// по наследству IP). Терминальная запись снимается, кандидат уходит в
// карантин с одной решающей попыткой (applyReverifyLocked).
func (r *Registry) registerWithReverify(body registerRequest, reverify *TerminatedRecord) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// V7.9.11: ip_ban-запись снимается регистрацией с НОВОГО ip — оператор
	// выполнил инструкцию «запустите службу заново после его смены». Бан в
	// истории БД при этом остаётся (он был «без восстановления»).
	r.terminateLiftIfIPChangedLocked(body.NodeID, body.IP, now)

	if existing, ok := r.state.Candidates[body.NodeID]; ok {
		detail := "re-registered"
		if existing.IP != body.IP {
			// V7.9.13: смена ip В КАРАНТИНЕ — старый ip фиксируем как
			// блокировку (bans row + stale-запись), нода живёт на новом.
			if existing.Quarantine != nil {
				r.quarantineIPChangeLocked(existing, body.IP, now)
			}
			detail = fmt.Sprintf("re-registered, ip changed %s -> %s", existing.IP, body.IP)
		}
		if body.NodeType != "" && body.NodeType != existing.NodeType {
			detail += fmt.Sprintf(", type %s -> %s", existing.NodeType, body.NodeType)
		}
		existing.IP = body.IP
		if body.NodeType != "" {
			existing.NodeType = body.NodeType
		}
		existing.LastHeartbeat = now
		if reverify != nil { // V7.9.12 (крайний случай: кандидат ещё жив)
			r.applyReverifyLocked(existing, reverify, now)
		}
		r.state.Counters.Registrations++
		r.addEventLocked(Event{Type: EventNodeRegistered, NodeID: body.NodeID, IP: body.IP, Detail: detail})
		r.persistStateLocked()
		log.Printf("candidate re-registered: %s (%s)", body.NodeID, body.IP)
		return time.Time{}, false
	}

	// V7.9.7: карантин после prune. Нода (либо предыдущая регистрация с тем
	// же IP — переустановка агента с новым id серию не отмывает) вычищена
	// рипером, карантин ещё не истёк → регистрация отклоняется. Лог
	// троттлим: старые агенты без Retry-After долбятся по каждому heartbeat.
	if tb := r.effectiveTombstoneLocked(body.NodeID, body.IP, now); tb != nil {
		if r.banLogTick == nil {
			r.banLogTick = make(map[string]time.Time)
		}
		if now.Sub(r.banLogTick[body.NodeID]) >= time.Minute {
			r.banLogTick[body.NodeID] = now
			log.Printf("register from %s (%s) rejected: pruned as inactive, banned for another %s (strike %d)",
				body.NodeID, body.IP, time.Until(tb.BannedUntil).Round(time.Second), tb.Strikes)
		}
		return tb.BannedUntil, true
	}

	for oldID, c := range r.state.Candidates {
		if c.IP == body.IP {
			log.Printf("candidate %s had same IP %s as new registration %s — replacing", oldID, body.IP, body.NodeID)
			// Stint старой записи не закрываем вручную: централизованный
			// reconcile в evaluateAssignments. А вот назначения доменов
			// переносим на новый ID БЕЗ master_lost/elected: IP тот же,
			// A-записи остаются корректными — DNS-чехарда не нужна.
			for d, id := range r.state.Assignments {
				if id == oldID {
					r.state.Assignments[d] = body.NodeID
				}
			}
			r.addEventLocked(Event{
				Type: EventNodeReplaced, NodeID: oldID, IP: body.IP,
				Detail: fmt.Sprintf("same IP as new node %s — old entry removed, domains carried over", body.NodeID),
			})
			delete(r.state.Candidates, oldID)
		}
	}

	r.state.Candidates[body.NodeID] = &Candidate{
		NodeID:        body.NodeID,
		IP:            body.IP,
		RegisteredAt:  now,
		LastHeartbeat: now,
		Healthy:       false,
		// V7.9.4: с нуля защёлка закрыта — в очередь здоровых войдёт после
		// recover_threshold подряд удачных отчётов (~2 × metrics_ms).
		MetricsHealthy: false,
		NodeType:       body.NodeType,
	}
	if reverify != nil { // V7.9.12: тот же забаненный ip — одна попытка
		r.applyReverifyLocked(r.state.Candidates[body.NodeID], reverify, now)
	}
	r.state.Counters.Registrations++
	r.addEventLocked(Event{Type: EventNodeRegistered, NodeID: body.NodeID, IP: body.IP})
	log.Printf("new candidate registered: %s (%s), order=%s", body.NodeID, body.IP, now.Format(time.RFC3339))
	// NOTE: persistStateLocked, а не persistState — write-lock уже держит эта
	// функция, повторный Lock был бы дедлоком.
	r.persistStateLocked()
	return time.Time{}, false
}

func (r *Registry) probeLoop() {
	ticker := time.NewTicker(r.cfg.ProbeInterval)
	defer ticker.Stop()
	for range ticker.C {
		r.mu.RLock()
		targets := make([]*Candidate, 0, len(r.state.Candidates))
		for _, c := range r.state.Candidates {
			targets = append(targets, c)
		}
		r.mu.RUnlock()

		var wg sync.WaitGroup
		for _, c := range targets {
			if c.Port == 0 {
				continue
			}
			wg.Add(1)
			go func(c *Candidate) {
				defer wg.Done()
				ok := tcpProbe(c.IP, c.Port, r.cfg.ProbeTimeout)
				r.mu.Lock()
				// V7.9.10: общая анти-флап защёлка (streakStep) — та же
				// машина, что и у metrics-отчётов; события/тексты как раньше.
				var changed bool
				c.Healthy, c.ConsecutiveFail, c.ConsecutiveOK, changed =
					streakStep(c.Healthy, c.ConsecutiveFail, c.ConsecutiveOK, ok,
						clampThreshold(r.cfg.Healthcheck.FailThreshold),
						clampThreshold(r.cfg.Healthcheck.RecoverThreshold))
				if changed && c.Healthy {
					r.addEventLocked(Event{Type: EventTCPUp, NodeID: c.NodeID, IP: c.IP})
					log.Printf("candidate %s is now healthy (tcp)", c.NodeID)
				} else if changed {
					r.addEventLocked(Event{
						Type: EventTCPDown, NodeID: c.NodeID, IP: c.IP,
						Detail: fmt.Sprintf("%d consecutive probe failures", c.ConsecutiveFail),
					})
					log.Printf("candidate %s marked unhealthy (tcp)", c.NodeID)
				}
				c.TCPHist = pushRing(c.TCPHist, TCPPoint{At: time.Now(), OK: ok}, tcpHistCap)
				r.mu.Unlock()
			}(c)
		}
		wg.Wait()
	}
}

func tcpProbe(ip string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (r *Registry) expiryLoop() {
	ticker := time.NewTicker(r.cfg.HeartbeatTTL / 2)
	defer ticker.Stop()
	for range ticker.C {
		// V7.9.6: sweepExpired объединяет heartbeat-expiry и рипер
		// неактивных (prune по prune_unhealthy_min) — см. prune.go.
		r.sweepExpired(time.Now())
	}
}

func (r *Registry) selectionLoop() {
	ticker := time.NewTicker(r.cfg.SelectionInterval)
	defer ticker.Stop()
	for range ticker.C {
		// V7.8: сначала зачистка доменов, выведенных из managed-списка
		// (только тех, кем реально управляли — чужие записи зоны не трогаем).
		r.sweepOrphans()
		changes := r.evaluateAssignments(time.Now())
		for _, ch := range changes {
			if ch.ToID == "" {
				continue
			}
			log.Printf("domain %s: master -> %s (%s)", ch.Domain, ch.ToID, ch.ToIP)
			r.applyDNSTarget(ch.Domain, ch.ToID, ch.ToIP)
		}
	}
}

// domainChange — изменение мастера домена за проход evaluateAssignments.
type domainChange struct {
	Domain string
	FromID string // "" — мастера не было
	ToID   string // "" — снять нельзя (здоровых нет: записи остаются)
	ToIP   string
}

// evaluateAssignments — ядро V7: единая очередь fully-healthy + PER-DOMAIN
// мастера. Вынесено из цикла ради тестов: DNS не трогает, только состояние.
// Возвращает домены, чей мастер изменился в этом проходе (их пишет DNS-цикл).
//
// Модель:
//   - очередь по непрерывному здоровью — одна на пул (QueuedAt);
//   - каждый managed-домен имеет СВОЕГО мастера из здоровых: пока держатель
//     fully healthy — домен не трогаем (стабильность >> симметрия нагрузки,
//     переключение — это DNS-запись и микропотеря клиентов);
//   - нод < доменов: мастера дотягивают «сиротские» домены (pass 1 раздаёт
//     наименее загруженным);
//   - появилась здоровая нода БЕЗ доменов, а у кого-то их >1 — один сирота
//     мигрирует к ней (pass 2, fill-empty). Балансировки 3/1 → 2/2 НЕТ:
//     лишний DNS-черн не оправдан.
//   - здоровых нет вообще: назначения НЕ снимаются (записи остаются на
//     последних мастерах — как в V6 с активной нодой).
func (r *Registry) evaluateAssignments(now time.Time) []domainChange {
	r.mu.Lock()
	defer r.mu.Unlock()

	// --- 1. очередь по непрерывному здоровью (как в V6) ---
	list := make([]*Candidate, 0, len(r.state.Candidates))
	joined := make([]*Candidate, 0, len(r.state.Candidates))
	for _, c := range r.state.Candidates {
		full := c.IsFullyHealthy(r.cfg.ReportFreshnessTTL)
		switch {
		case full && !c.FullyHealthy:
			c.FullyHealthy = true
			c.QueuedAt = now
			c.UnhealthySince = time.Time{}   // V7.9.6: вернулась в очередь — часы рипера обнуляются
			r.clearTombstonesOnJoinLocked(c) // V7.9.7: серия prune оборвалась — карантин забываем
			joined = append(joined, c)
			log.Printf("candidate %s entered healthy queue (position=%s)", c.NodeID, now.Format(time.RFC3339))
		case !full && c.FullyHealthy:
			c.FullyHealthy = false
			c.QueuedAt = time.Time{}
			c.UnhealthySince = now // V7.9.6: старт окна непрерывного нездоровья
			r.addEventLocked(Event{
				Type: EventQueueLeft, NodeID: c.NodeID, IP: c.IP,
				Detail: c.unhealthyReason(r.cfg.ReportFreshnessTTL),
			})
			log.Printf("candidate %s left healthy queue (unhealthy) — position reset", c.NodeID)
		}
		if !full && c.UnhealthySince.IsZero() {
			// V7.9.6 lazy-arm: нода была нездорова на момент апгрейда (поля в
			// state не существовало) или регистрируется заранее больной —
			// окно рипера стартует с ближайшего evaluate.
			c.UnhealthySince = now
		}
		if full {
			list = append(list, c)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if !a.QueuedAt.Equal(b.QueuedAt) {
			return a.QueuedAt.Before(b.QueuedAt)
		}
		if !a.RegisteredAt.Equal(b.RegisteredAt) {
			return a.RegisteredAt.Before(b.RegisteredAt)
		}
		return a.NodeID < b.NodeID
	})
	for _, c := range joined {
		pos := 0
		for i, x := range list {
			if x == c {
				pos = i + 1
				break
			}
		}
		r.addEventLocked(Event{
			Type: EventQueueJoined, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("queue position %d of %d", pos, len(list)),
		})
	}

	// --- 2. эффективный список доменов (hot-edit из панели — под cfgMu) ---
	r.cfgMu.RLock()
	rawDomains := append([]string(nil), r.cfg.Cloudflare.Domains...)
	r.cfgMu.RUnlock()
	seen := make(map[string]bool, len(rawDomains))
	domains := make([]string, 0, len(rawDomains))
	for _, d := range rawDomains {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		domains = append(domains, d)
	}
	sort.Strings(domains) // детерминированный обход: воспроизводимые раскладки

	if r.state.Assignments == nil {
		r.state.Assignments = make(map[string]string)
	}
	if r.state.AssignmentsSince == nil {
		r.state.AssignmentsSince = make(map[string]time.Time)
	}
	if r.ttlOverdue == nil {
		r.ttlOverdue = make(map[string]bool)
	}
	for d := range r.state.Assignments {
		if !seen[d] {
			// домен вычеркнули из конфига — назначение silently сгорает
			// (DNS-запись не трогаем: именем может завладеть кто-то другой)
			delete(r.state.Assignments, d)
			delete(r.state.AssignmentsSince, d)
			delete(r.ttlOverdue, d)
		}
	}

	healthyByID := make(map[string]*Candidate, len(list))
	for _, c := range list {
		healthyByID[c.NodeID] = c
	}
	holds := make(map[string][]string, len(r.state.Assignments)) // nodeID → его домены
	for d, id := range r.state.Assignments {
		holds[id] = append(holds[id], d)
	}

	changes := make([]domainChange, 0, len(domains))

	// наименее загруженный из очереди; при равенстве — раньше в очереди.
	// exclude (может быть "") — держатель, которого нельзя выбирать (TTL).
	pickLeastLoaded := func(exclude string) *Candidate {
		var best *Candidate
		for _, c := range list {
			if c.NodeID == exclude {
				continue
			}
			if best == nil || len(holds[c.NodeID]) < len(holds[best.NodeID]) {
				best = c
			}
		}
		return best
	}

	// --- 3. pass 0 (V7.9.3): принудительная ротация по TTL мастерства ---
	// Здоровый держатель, у которого истёк лимит, сдаёт домен наименее
	// загруженной ноде очереди (кроме себя). Замены нет — домен остаётся на
	// нём: OVERDUE лог один раз на эпизод, панель покажет «TTL истёк».
	// V7.9.5: сдавший домен уходит в КОНЕЦ очереди (QueuedAt=now + хвост
	// list). Без этого pickLeastLoaded при равной загрузке всегда брал
	// раннего в очереди — домен ходил между двумя старшими нодами, даже
	// когда здоровых больше.
	if ttl := r.masterTTL(); ttl > 0 && len(list) > 0 {
		for _, d := range domains {
			holderID := r.state.Assignments[d]
			if holderID == "" || healthyByID[holderID] == nil {
				delete(r.ttlOverdue, d)
				continue // мёртвых раздаст pass 1
			}
			since := r.state.AssignmentsSince[d]
			if since.IsZero() {
				// назначение досталось от версии до V7.9.3 — отсчёт отныне
				r.state.AssignmentsSince[d] = now
				continue
			}
			age := now.Sub(since)
			if age < ttl {
				continue
			}
			target := pickLeastLoaded(holderID)
			if target == nil {
				if !r.ttlOverdue[d] {
					r.ttlOverdue[d] = true
					log.Printf("domain %s: master TTL %s exceeded (%s old) but no healthy replacement — keeping %q",
						d, ttl, age.Round(time.Second), holderID)
				}
				continue
			}
			reason := fmt.Sprintf("master TTL %s expired (was %s) — forced rotation", ttl, age.Round(time.Second))
			holderIP := r.state.Candidates[holderID].IP
			r.addEventLocked(Event{Type: EventMasterLost, NodeID: holderID, IP: holderIP, Domain: d, Detail: reason})
			holds[holderID] = dropDomain(holds[holderID], d)
			holds[target.NodeID] = append(holds[target.NodeID], d)
			r.state.Assignments[d] = target.NodeID
			r.state.AssignmentsSince[d] = now
			delete(r.ttlOverdue, d)
			r.state.Counters.MasterSwitches++
			r.state.Counters.MasterTTLRotations++
			// V7.9.5: истёкший держатель — в конец очереди (и персистентный
			// QueuedAt, и живой порядок list для остатка этого тика).
			if holder := r.state.Candidates[holderID]; holder != nil {
				holder.QueuedAt = now
			}
			for i, x := range list {
				if x.NodeID == holderID {
					copy(list[i:], list[i+1:])
					list[len(list)-1] = x
					break
				}
			}
			pos := 0
			for i, x := range list {
				if x == target {
					pos = i + 1
					break
				}
			}
			r.addEventLocked(Event{
				Type: EventMasterElected, NodeID: target.NodeID, IP: target.IP, Domain: d,
				Detail: fmt.Sprintf("forced rotation (TTL %s), queue #%d, was %q", ttl, pos, holderID),
			})
			changes = append(changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: master %q -> %q (%s)", d, holderID, target.NodeID, reason)
		}
	}

	// --- 4. pass 1: переназначение мёртвых/отсутствующих мастеров ---
	if len(list) > 0 {
		for _, d := range domains {
			holderID := r.state.Assignments[d]
			if holderID != "" && healthyByID[holderID] != nil {
				continue // держатель жив — домен не трогаем
			}
			var reason, holderIP string
			switch holder := r.state.Candidates[holderID]; {
			case holderID == "":
				reason = "domain had no master"
			case holder == nil:
				reason = "assignee removed from pool"
			default:
				reason = holder.unhealthyReason(r.cfg.ReportFreshnessTTL)
				holderIP = holder.IP
			}
			if holderID != "" {
				r.addEventLocked(Event{Type: EventMasterLost, NodeID: holderID, IP: holderIP, Domain: d, Detail: reason})
				holds[holderID] = dropDomain(holds[holderID], d)
			}
			target := pickLeastLoaded("")
			holds[target.NodeID] = append(holds[target.NodeID], d)
			r.state.Assignments[d] = target.NodeID
			r.state.AssignmentsSince[d] = now
			delete(r.ttlOverdue, d)
			r.state.Counters.MasterSwitches++
			pos := 0
			for i, x := range list {
				if x == target {
					pos = i + 1
					break
				}
			}
			r.addEventLocked(Event{
				Type: EventMasterElected, NodeID: target.NodeID, IP: target.IP, Domain: d,
				Detail: fmt.Sprintf("queue #%d (was %q: %s)", pos, holderID, reason),
			})
			changes = append(changes, domainChange{Domain: d, FromID: holderID, ToID: target.NodeID, ToIP: target.IP})
			log.Printf("domain %s: master %q -> %q (%s)", d, holderID, target.NodeID, reason)
		}

		// --- 4. pass 2: fill-empty — сироты перетекают к нодам без доменов ---
		for {
			var idle *Candidate
			for _, c := range list {
				if len(holds[c.NodeID]) == 0 {
					idle = c
					break
				}
			}
			if idle == nil {
				break
			}
			var donor *Candidate
			for _, c := range list {
				if len(holds[c.NodeID]) > 1 && (donor == nil || len(holds[c.NodeID]) > len(holds[donor.NodeID])) {
					donor = c
				}
			}
			if donor == nil {
				break
			}
			sort.Strings(holds[donor.NodeID])
			d := holds[donor.NodeID][0] // лексикографически первый у самого нагруженного
			holds[donor.NodeID] = holds[donor.NodeID][1:]
			holds[idle.NodeID] = append(holds[idle.NodeID], d)
			r.state.Assignments[d] = idle.NodeID
			r.state.AssignmentsSince[d] = now
			delete(r.ttlOverdue, d)
			r.state.Counters.MasterSwitches++
			r.addEventLocked(Event{Type: EventMasterLost, NodeID: donor.NodeID, IP: donor.IP, Domain: d,
				Detail: "rebalance: domain moved to a node with zero domains"})
			r.addEventLocked(Event{Type: EventMasterElected, NodeID: idle.NodeID, IP: idle.IP, Domain: d,
				Detail: fmt.Sprintf("rebalance from %q", donor.NodeID)})
			changes = append(changes, domainChange{Domain: d, FromID: donor.NodeID, ToID: idle.NodeID, ToIP: idle.IP})
			log.Printf("domain %s: rebalanced %q -> %q (idle node)", d, donor.NodeID, idle.NodeID)
		}
	}

	// --- 5. stint reconcile: мастерство = держишь ≥1 домен, будучи здоровым ---
	for _, c := range r.state.Candidates {
		holdsDomains := len(holds[c.NodeID]) > 0
		healthyNow := healthyByID[c.NodeID] != nil
		switch {
		case holdsDomains && healthyNow && c.MasterSince.IsZero():
			c.MasterSince = now
			c.MasterStints++
		case (!holdsDomains || !healthyNow) && !c.MasterSince.IsZero():
			closeMasterStintLocked(c, now)
			if holdsDomains && !healthyNow && len(list) == 0 {
				// держатель нездоров, а заменить некем (пустая очередь): записи
				// остаются на нём, но мастерство фиксируем как потерянное
				r.addEventLocked(Event{
					Type: EventMasterLost, NodeID: c.NodeID, IP: c.IP,
					Detail: "unhealthy, no healthy replacement — A-records left on it (" + strings.Join(holds[c.NodeID], ", ") + ")",
				})
			}
		}
	}

	if len(changes) > 0 || len(joined) > 0 {
		r.persistStateLocked()
	}
	return changes
}

// dropDomain убирает домен из списка (лениво: без сохранения порядка).
func dropDomain(list []string, d string) []string {
	for i, x := range list {
		if x == d {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

func (r *Registry) persistStateLocked() {
	data, err := json.MarshalIndent(r.state, "", "  ")
	if err != nil {
		log.Printf("marshal state error: %v", err)
		return
	}
	if err := os.WriteFile(r.cfg.State.File, data, 0644); err != nil {
		log.Printf("write state error: %v — проверьте владельца каталога: chown -R sharedd-registry:sharedd-registry %s",
			err, filepath.Dir(r.cfg.State.File))
		return
	}
	r.eventsDirty = false
}

func (r *Registry) loadState() {
	data, err := os.ReadFile(r.cfg.State.File)
	if err != nil {
		log.Printf("no existing state file, starting fresh")
		return
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("failed to parse state file: %v", err)
		return
	}
	if st.Candidates == nil {
		st.Candidates = make(map[string]*Candidate)
	}
	if st.Assignments == nil {
		st.Assignments = make(map[string]string)
	}
	if st.AssignmentsSince == nil {
		st.AssignmentsSince = make(map[string]time.Time)
	}
	if st.Terminated == nil {
		st.Terminated = make(map[string]*TerminatedRecord)
	}
	// V7.9.4-миграция: metrics-защёлки (MetricsHealthy) в старых state нет —
	// после апгрейда нельзя ронять всё здоровое: переносим текущее MetricsOK
	// (вердикт последнего отчёта до выключения) в защёлку. Транзиентно
	// сфейлившая нода теперь должна доказать восстановление recover-порогом —
	// это и есть задуманная гистерезисная семантика.
	for _, c := range st.Candidates {
		if c.MetricsOK && !c.MetricsHealthy {
			c.MetricsHealthy = true
		}
	}
	r.state = st
	log.Printf("loaded state: %d candidates, %d domain assignments", len(st.Candidates), len(st.Assignments))
}
