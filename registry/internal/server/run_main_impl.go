package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sharedd/registry/internal/state"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/cloudflare/cloudflare-go"
)

// ── история точек проверок: ёмкости колец ────────────────────────

const (
	tcpHistCap    = 360 // тик probeLoop = ProbeInterval (10с по дефолту) → ~1 час
	gpHistCap     = 288 // globalping_ms (5 мин) → ~24 часа
	reportHistCap = 360 // metrics_ms (60с) → ~6 часов
)

type Registry struct {
	cfg   *resolvedRegistryConfig
	cf    cfDNSAPI     // заменяется при смене api_token через панель
	mu    sync.RWMutex // state (кандидаты, журнал, счётчики)
	state State

	// Db — вечная история (SQLite). nil = работаем без неё
	// (файл не открылся / отключено) — пул и панель не страдают.
	db *historyDB

	// cfgMu защищает "горячие" поля cfg (панель их правит на лету).
	// ПОРЯДОК ЛОКОВ: сначала mu, потом cfgMu — никогда наоборот.
	cfgMu sync.RWMutex

	startedAt   time.Time
	eventsDirty bool // журнал изменился с последнего persistStateLocked (под mu)

	// TtlOverdue — домены с истёкшим TTL мастерства, для которых УЖЕ
	// залогировано «нет здоровой замены» (in-memory anti-spam: лог раз за
	// эпизод, сбрасывается при ротации/смене держателя или рестарте процесса).
	// Пишется только из evaluateAssignments — под r.mu.
	ttlOverdue map[string]bool

	// BanLogTick — последний лог отказа /register по карантину на
	// node_id (in-memory anti-spam: старые агенты без Retry-After долбят
	// register по каждому heartbeat — в журнал не чаще раза в минуту).
	banLogTick map[string]time.Time

	// СРМД. srmdExpandTicks/srmdFoldTicks — анти-флап счётчики
	// подряд идущих тиков селекции с условием «доменов не хватает» /
	// «слишком много» (действие после srmdStableTicks подряд). srmdPending —
	// отложенные CNAME-записи свёрнутых доменов (пишет flushSRMDDNS вне
	// локов). Всё под r.mu.
	srmdExpandTicks int
	srmdFoldTicks   int
	srmdPending     []srmdDNSAction
	dnsInFlight     map[string]bool
}

// newCFClient — фабрика Cloudflare-клиента (var ради подмены в тестах).
var newCFClient = func(token string) (cfDNSAPI, error) {
	return cloudflare.NewWithAPIToken(token)
}

// Run — точка входа регистратора: загрузка конфига, поднятие state/SQLite,
// старт фоновых циклов и HTTP-сервера, корректное завершение по сигналам.
// Вызывается из тонкого main в корне модуля.
func Run() {
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
	reg.mu.Lock()
	reg.recoverDNSDesiredLocked()
	reg.persistStateLocked()
	reg.mu.Unlock()
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

	go reg.probeLoop()
	go reg.selectionLoop()
	go reg.dnsReconcileLoop()
	go reg.expiryLoop()
	go reg.eventPersistLoop()
	go reg.historyDBLoop() // ротация событий в SQLite (баны вечны)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := reg.serveHTTP(ctx); err != nil {
		log.Printf("registry HTTP stopped: %v", err)
	}
	reg.mu.Lock()
	reg.persistStateLocked()
	reg.mu.Unlock()
	if reg.db != nil {
		reg.db.Close()
	}
}

const shutdownTimeout = 10 * time.Second

func (r *Registry) httpServer() *http.Server {
	return &http.Server{
		Addr:              r.cfg.HTTP.Addr,
		Handler:           r.buildMux(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second, // Globalping verification can take 75s.
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
}

func (r *Registry) serveHTTP(ctx context.Context) error {
	srv := r.httpServer()
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}
	log.Printf("registry HTTP listening on %s", ln.Addr())
	return serveHTTPServer(ctx, srv, ln)
}

func serveHTTPServer(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			_ = srv.Close()
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		err := <-errCh
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// buildMux собирает все маршруты. Для Go-1.22+ mux все паттерны метод-квалифицированы —
// иначе "GET /" из панели конфликтует с unqualified "/register" (panic на старте).
func (r *Registry) buildMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("POST /register", r.requireNodeToken(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body registerRequest
		if err := decodeNodeJSON(w, req, &body); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if r.nodeAPISecurityEnabled() {
			if err := validateRegisterRequest(body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		} else if body.NodeID == "" || body.IP == "" {
			http.Error(w, "node_id, ip required", http.StatusBadRequest)
			return
		}
		// Терминально убитая нода; ip_ban по ТОМУ ЖЕ ip
		// не вечен — даём одну GP-перепроверку (reverify в registerWithReverify;
		// ложные глобалпинги, прокси был выключен при отладке). dead блоком
		// не является вовсе — нода возвращается после локального восстановления,
		// запись снимается здесь же. Совпадение по ip с ЧУЖОЙ ip_ban-записью —
		// тоже reverify (переустановка агента с новым id старый ip не отмывает).
		r.mu.Lock()
		rec := r.terminatedBlockingLocked(body.NodeID, body.IP)
		if rec == nil && body.IP != "" {
			rec = r.terminatedIPBanByIPLocked(body.IP)
		}
		r.liftDeadTerminatedLocked(body.NodeID, body.IP)
		r.mu.Unlock()
		if rec != nil && !reverifyOpenLocked(rec, time.Now()) {
			log.Printf("register from terminated node %s (%s) rejected: %s (reverify_failed=%t)",
				body.NodeID, body.IP, rec.Reason, rec.ReverifyFailed)
			r.writeTerminate(w, rec)
			return
		}
		if banUntil, banned := r.registerWithReverify(body, rec); banned {
			// Карантин после prune — 429 + Retry-After. Агенты +
			// уважают Retry-After и молчат до дедлайна; старые продолжат
			// долбиться по heartbeat'ам — отказ дешёвый, лог троттлится.
			retryAfter := int(time.Until(banUntil).Seconds()) + 1
			if retryAfter < 1 {
				retryAfter = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprintf(w, `{"error":"node was pruned as inactive, re-registration deferred","retry_after_sec":%d}`+"\n", retryAfter)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))

	mux.Handle("POST /heartbeat", r.requireNodeToken(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			NodeID string `json:"node_id"`
		}
		if err := decodeNodeJSON(w, req, &body); err != nil || body.NodeID == "" {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if r.nodeAPISecurityEnabled() {
			if err := validateNodeID(body.NodeID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		r.mu.Lock()
		c, ok := r.state.Candidates[body.NodeID]
		if ok {
			c.LastHeartbeat = time.Now()
			c.HeartbeatsTotal++
			r.state.Counters.Heartbeats++
		}
		// Heartbeat от убитой по ip_ban ноды — kill-сигнал (403+terminate);
		// dead-запись блока не даёт — кандидат отсутствует, обычный 410
		// «перерегистрируйся» (агент перерегистрируется после локального
		// восстановления, register снимет dead-запись). IP берём из
		// соединения: для ip_ban сменившийся ip блока не имеет — обычный
		// 410 «перерегистрируйся» (register снимет запись по смене ip).
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
			// Ноду удалили (heartbeat-expiry / prune-рипер). Без явного
			// отказа живой агент удалённой ноды слал бы heartbeat'ы в пустоту
			// вечно и никогда не вернулся бы в пул. Агент на любой статус !=200
			// пере-регистрируется (node/main.go heartbeatLoop).
			log.Printf("heartbeat from unknown node %s (pruned/expired?) — asking to re-register", body.NodeID)
			http.Error(w, "unknown node_id, re-register", http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusOK)
	})))

	mux.Handle("POST /report", r.requireNodeToken(http.HandlerFunc(r.handleHealthReport)))

	// POST /retire — агент сообщает о само-завершении по классу
	// dead (локальные проверки красные > terminate_dead_min): регистратор
	// обязан записать бан в вечную историю, даже если кандидат уже выпал
	// по heartbeat-TTL, пока нода молчала. Привязка доверия — RemoteAddr ==
	// заявленный ip (модель угроз как у открытого /register; стучаться в
	// retire от чужого имени без его маршрутизируемого адреса нельзя).
	mux.Handle("POST /retire", r.requireNodeToken(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			NodeID string `json:"node_id"`
			IP     string `json:"ip"`
			Reason string `json:"reason"`
		}
		if err := decodeNodeJSON(w, req, &body); err != nil || body.NodeID == "" || body.IP == "" {
			http.Error(w, "node_id, ip required", http.StatusBadRequest)
			return
		}
		if r.nodeAPISecurityEnabled() {
			if err := validateNodeID(body.NodeID); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := validatePublicIPv4(body.IP); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
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
			// Само-завершение ноды ВО ВРЕМЯ карантина — это выход
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
	})))

	mux.Handle("GET /config", r.requireNodeToken(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// снапшот под cfgMu: эти секции панель может править на лету
		r.cfgMu.RLock()
		gpValidity := r.cfg.GlobalpingValidityTTL
		proxyPort := r.cfg.SharedProxy.Port
		if proxyPort == 0 {
			proxyPort = 443
		}
		resp := sharedConfigResponse{
			TLSDomain: r.cfg.SharedProxy.TLSDomain,
			ProxyPort: proxyPort,
			Users:     maps.Clone(r.cfg.SharedProxy.Users),
			Intervals: nodeIntervals{
				HeartbeatMs:  r.cfg.NodeDefaults.HeartbeatMs,
				GlobalpingMs: r.cfg.NodeDefaults.GlobalpingMs,
				MetricsMs:    r.cfg.NodeDefaults.MetricsMs,
				SyncMs:       r.cfg.NodeDefaults.SyncMs,
			},
		}
		r.cfgMu.RUnlock()
		nodeID := req.Header.Get("X-ShareDD-Node-ID")
		if nodeID != "" {
			now := time.Now()
			r.mu.Lock()
			if c := r.state.Candidates[nodeID]; c != nil && globalpingStale(c, now, gpValidity) && now.Sub(c.LastGlobalpingRequestAt) >= time.Minute {
				resp.ForceGlobalping = true
				c.LastGlobalpingRequestAt = now
			}
			r.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(resp)
	})))

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
		_ = json.NewEncoder(w).Encode(r.state)
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintln(w, "ok")
	})

	r.mountPanel(mux)
	r.mountStats(mux)     // публичная статистика нод (/statistics/...)
	r.mountDashboard(mux) // публичный дашборд блокировок (/dashboard/...)
	r.mountLinks(mux)     // публичная страница прокси-ссылок (/links)
	mountAssets(mux)

	return mux
}

// writeTerminate — kill-сигнал агенту: 403 + флаг terminate.
// Агент пишет Message в лог дословно, кладёт tombstone и останавливает
// службу (node/terminate.go); повторных регистраций быть не должно.
func (r *Registry) writeTerminate(w http.ResponseWriter, rec *TerminatedRecord) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"terminate": true,
		"reason":    rec.Reason,
		"message":   rec.Message,
	})
}

// register — /register для ноды. Возвращает (banUntil, true), если нода под
// карантином (вычищена рипером и BannedUntil ещё не истёк) — хендлер
// отвечает 429 + Retry-After, в state ничего не пишется (событие тоже не
// плодим: отказов много, лента одна — подробности уже есть в node_pruned).
func (r *Registry) register(body registerRequest) (time.Time, bool) {
	return r.registerWithReverify(body, nil)
}

// RegisterWithReverify — register + reverify-трек rec не-nil,
// когда нода регистрируется со СТАРОГО gp-забаненного ip (своего или чужого
// по наследству IP). Терминальная запись снимается, кандидат уходит в
// карантин с одной решающей попыткой (applyReverifyLocked).
func (r *Registry) registerWithReverify(body registerRequest, reverify *TerminatedRecord) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()

	// Ip_ban-запись снимается регистрацией с НОВОГО ip — оператор
	// выполнил инструкцию «запустите службу заново после его смены». Бан в
	// истории БД при этом остаётся (он был «без восстановления»).
	r.terminateLiftIfIPChangedLocked(body.NodeID, body.IP, now)

	if existing, ok := r.state.Candidates[body.NodeID]; ok {
		detail := "re-registered"
		if existing.IP != body.IP {
			// Смена ip В КАРАНТИНЕ — старый ip фиксируем как
			// блокировку (bans row + stale-запись), нода живёт на новом.
			if existing.Quarantine != nil && existing.Quarantine.Attempts > 0 {
				r.quarantineIPChangeLocked(existing, body.IP, now)
			} else if existing.Quarantine != nil {
				existing.Quarantine = nil
				existing.LastGlobalpingAt = time.Time{}
			}
			detail = fmt.Sprintf("re-registered, ip changed %s -> %s", existing.IP, body.IP)
		}
		if body.NodeType != "" && body.NodeType != existing.NodeType {
			detail += fmt.Sprintf(", type %s -> %s", existing.NodeType, body.NodeType)
		}
		existing.IP = body.IP
		existing.Generation++
		if body.NodeType != "" {
			existing.NodeType = body.NodeType
		}
		existing.LastHeartbeat = now
		if reverify != nil { // (крайний случай: кандидат ещё жив)
			r.applyReverifyLocked(existing, reverify, now)
		}
		r.state.Counters.Registrations++
		r.addEventLocked(Event{Type: EventNodeRegistered, NodeID: body.NodeID, IP: body.IP, Detail: detail})
		r.persistStateLocked()
		log.Printf("candidate re-registered: %s (%s)", body.NodeID, body.IP)
		return time.Time{}, false
	}

	// Карантин после prune. Нода (либо предыдущая регистрация с тем
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
		Generation:    1,
		// С нуля защёлка закрыта — в очередь здоровых войдёт после
		// recover_threshold подряд удачных отчётов (~2 × metrics_ms).
		MetricsHealthy: false,
		NodeType:       body.NodeType,
	}
	if reverify != nil { // тот же забаненный ip — одна попытка
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
			wg.Add(1)
			go func(c *Candidate) {
				defer wg.Done()
				r.mu.RLock()
				ip, port, generation := c.IP, c.Port, c.Generation
				r.mu.RUnlock()
				if port == 0 {
					return
				}
				ok := tcpProbe(ip, port, r.cfg.ProbeTimeout)
				r.mu.Lock()
				if current := r.state.Candidates[c.NodeID]; current != c || c.Generation != generation || c.IP != ip {
					r.mu.Unlock()
					return
				}
				// Общая анти-флап защёлка (streakStep) — та же
				// машина, что и у metrics-отчётов; события/тексты как раньше.
				r.cfgMu.RLock()
				failThreshold := clampThreshold(r.cfg.Healthcheck.FailThreshold)
				recoverThreshold := clampThreshold(r.cfg.Healthcheck.RecoverThreshold)
				r.cfgMu.RUnlock()
				var changed bool
				c.Healthy, c.ConsecutiveFail, c.ConsecutiveOK, changed =
					streakStep(c.Healthy, c.ConsecutiveFail, c.ConsecutiveOK, ok,
						failThreshold, recoverThreshold)
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
				c.TCPHist = state.PushRing(c.TCPHist, TCPPoint{At: time.Now(), OK: ok}, tcpHistCap)
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
	_ = conn.Close()
	return true
}

func (r *Registry) expiryLoop() {
	ticker := time.NewTicker(r.cfg.HeartbeatTTL / 2)
	defer ticker.Stop()
	for range ticker.C {
		r.sweepGlobalpingFreshness(time.Now())
		// SweepExpired объединяет heartbeat-expiry и рипер
		// неактивных (prune по prune_unhealthy_min) — см. prune.go.
		r.sweepExpired(time.Now())
	}
}

func (r *Registry) selectionLoop() {
	ticker := time.NewTicker(r.cfg.SelectionInterval)
	defer ticker.Stop()
	for range ticker.C {
		// Сначала зачистка доменов, выведенных из managed-списка
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
		// CNAME-записи доменов, свёрнутых СРМД в этом (или прошлых
		// со сбоя) тиках — вне локов, с ретраем при ошибке Cloudflare.
		r.flushSRMDDNS()
	}
}

// domainChange — изменение мастера домена за проход evaluateAssignments.
type domainChange struct {
	Domain string
	FromID string // "" — мастера не было
	ToID   string // "" — снять нельзя (здоровых нет: записи остаются)
	ToIP   string
}

// EvaluateAssignments — ядро единая очередь fully-healthy + PER-DOMAIN
// мастера. Вынесено из цикла ради тестов: DNS не трогает, только состояние.
// Возвращает домены, чей мастер изменился в этом проходе (их пишет DNS-цикл).
//
// Модель:
// - очередь по непрерывному здоровью — одна на пул (QueuedAt);
// - каждый managed-домен имеет СВОЕГО мастера из здоровых: пока держатель
// fully healthy — домен не трогаем (стабильность >> симметрия нагрузки,
// переключение — это DNS-запись и микропотеря клиентов);
// - нод < доменов: мастера дотягивают «сиротские» домены (pass 1 раздаёт
// наименее загруженным);
// - появилась здоровая нода БЕЗ доменов, а у кого-то их >1 — один сирота
// мигрирует к ней (pass 2, fill-empty). Балансировки 3/1 → 2/2 НЕТ:
// лишний DNS-черн не оправдан.
// - здоровых нет вообще: назначения НЕ снимаются (записи остаются на
// последних мастерах — как в с активной нодой).
func (r *Registry) evaluateAssignments(now time.Time) []domainChange {
	r.mu.Lock()
	defer r.mu.Unlock()

	// --- 1. очередь по непрерывному здоровью ---
	list := make([]*Candidate, 0, len(r.state.Candidates))
	joined := make([]*Candidate, 0, len(r.state.Candidates))
	for _, c := range r.state.Candidates {
		full := c.IsFullyHealthy(r.cfg.ReportFreshnessTTL)
		switch {
		case full && !c.FullyHealthy:
			c.FullyHealthy = true
			c.QueuedAt = now
			c.UnhealthySince = time.Time{}   // вернулась в очередь — часы рипера обнуляются
			r.clearTombstonesOnJoinLocked(c) // серия prune оборвалась — карантин забываем
			joined = append(joined, c)
			log.Printf("candidate %s entered healthy queue (position=%s)", c.NodeID, now.Format(time.RFC3339))
		case !full && c.FullyHealthy:
			c.FullyHealthy = false
			c.QueuedAt = time.Time{}
			c.UnhealthySince = now // старт окна непрерывного нездоровья
			r.addEventLocked(Event{
				Type: EventQueueLeft, NodeID: c.NodeID, IP: c.IP,
				Detail: c.UnhealthyReason(r.cfg.ReportFreshnessTTL),
			})
			log.Printf("candidate %s left healthy queue (unhealthy) — position reset", c.NodeID)
		}
		if !full && c.UnhealthySince.IsZero() {
			// lazy-arm: нода была нездорова на момент апгрейда или регистрируется
			// заранее больной — окно рипера стартует с ближайшего evaluate.
			c.UnhealthySince = now
		}
		if full {
			list = append(list, c)
		}
	}
	sortQueue(list)
	for _, c := range joined {
		r.addEventLocked(Event{
			Type: EventQueueJoined, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("queue position %d of %d", positionInQueue(list, c), len(list)),
		})
	}

	// --- 1.5. СРМД: масштабирование числа доменов под очередь ---
	srmdChanged := r.srmdRebalanceLocked(now, list)

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
		if _, folded := r.state.SRMD.CNames[d]; folded {
			continue // СРМД: домен свёрнут в CNAME — в ротации мастеров не участвует
		}
		domains = append(domains, d)
	}
	sort.Strings(domains)

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
			delete(r.state.Assignments, d)
			delete(r.state.AssignmentsSince, d)
			delete(r.ttlOverdue, d)
		}
	}

	healthyByID := make(map[string]*Candidate, len(list))
	for _, c := range list {
		healthyByID[c.NodeID] = c
	}
	holds := make(map[string][]string, len(r.state.Assignments))
	for d, id := range r.state.Assignments {
		holds[id] = append(holds[id], d)
	}

	view := &selectionView{
		queue:            list,
		healthy:          healthyByID,
		all:              r.state.Candidates,
		holds:            holds,
		Assignments:      r.state.Assignments,
		AssignmentsSince: r.state.AssignmentsSince,
		TTLOverdue:       r.ttlOverdue,
	}
	var sink selectionSink

	// --- 3–5. проходы селекции (чистые функции над view) ---
	rotateByTTL(view, domains, r.masterTTL(), now, &sink)
	reassignDead(view, domains, r.cfg.ReportFreshnessTTL, now, &sink)
	fillEmpty(view, now, &sink)
	reconcileStints(r.state.Candidates, healthyByID, holds, now, &sink)

	// сброс побочных эффектов проходов под локом
	r.state.Counters.MasterSwitches += sink.switches
	r.state.Counters.MasterTTLRotations += sink.ttlRotations
	changes := sink.changes
	for _, ev := range sink.events {
		r.addEventLocked(ev)
	}

	if len(changes) > 0 || len(joined) > 0 || srmdChanged {
		for _, ch := range changes {
			if ch.ToID != "" && ch.ToIP != "" {
				r.enqueueDNSDesiredLocked(ch.Domain, "A", ch.ToIP, ch.ToID)
			}
		}
		r.persistStateLocked()
	}
	return changes
}

// positionInQueue — 1-based позиция кандидата в очереди (0 — нет в очереди).
func positionInQueue(list []*Candidate, c *Candidate) int {
	for i, x := range list {
		if x == c {
			return i + 1
		}
	}
	return 0
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
	if err := state.Save(r.cfg.State.File, &r.state); err != nil {
		log.Printf("write state error: %v — проверьте владельца каталога: chown -R sharedd-registry:sharedd-registry %s",
			err, filepath.Dir(r.cfg.State.File))
		return
	}
	r.eventsDirty = false
}

func (r *Registry) loadState() {
	st, err := state.Load(r.cfg.State.File)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			log.Printf("no existing state file, starting fresh")
			return
		}
		log.Printf("failed to load state file: %v", err)
		return
	}
	r.state = *st
	log.Printf("loaded state: %d candidates, %d domain assignments", len(st.Candidates), len(st.Assignments))
}
