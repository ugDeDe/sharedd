package server

// Кабинет пользователя (docs/api-contract.md §2.1): session-cookie аутентификация
// (не panel.token), CSRF double-submit на мутирующих запросах, owner-проверки на
// каждом объекте. Регистрация создаёт role=user с max_nodes=0 (квоту выдаёт
// админ). User-режим выключен / control-БД недоступна → 503 (fail-closed, как у
// admin-API M1).

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"sharedd/registry/internal/control"
)

const (
	sessionCookieName = "sharedd_session"
	// __Host- префикс: Secure, Path=/, без Domain — сиблинг-поддомен не может
	// подбросить cookie с тем же именем (M1).
	csrfCookieName = "__Host-sharedd_csrf"
	csrfHeaderName = "X-CSRF-Token"
	sessionTTL     = 7 * 24 * time.Hour
)

// sessionCtx — сессия запроса: пользователь + сырой токен (для logout).
type sessionCtx struct {
	user  control.User
	token string
}

type ctxKey int

const sessionCtxKey ctxKey = 0

func withSession(ctx context.Context, s sessionCtx) context.Context {
	return context.WithValue(ctx, sessionCtxKey, s)
}

func sessionFrom(req *http.Request) sessionCtx {
	s, _ := req.Context().Value(sessionCtxKey).(sessionCtx)
	return s
}

// ── гварды ────────────────────────────────────────────────────────

// cabinetGuard — user-режим включён и control-БД открыта. false = ответ записан.
func (r *Registry) cabinetGuard(w http.ResponseWriter) bool {
	if !r.usersEnabled() {
		controlAPIError(w, http.StatusServiceUnavailable, "users_disabled", "user mode is disabled")
		return false
	}
	if r.control == nil {
		r.writeControlUnavailable(w)
		return false
	}
	return true
}

// requireSession — cookie-сессия обязательна; кладёт пользователя в контекст.
func (r *Registry) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if r.usersEnabled() && r.control == nil {
			log.Printf("control operation=%s %s session guard: store is nil", req.Method, req.Pattern)
		}
		if !r.cabinetGuard(w) {
			return
		}
		c, err := req.Cookie(sessionCookieName)
		if err != nil || c.Value == "" {
			r.clearAuthCookies(w)
			controlAPIError(w, http.StatusUnauthorized, "unauthorized", "no session")
			return
		}
		u, err := r.control.SessionUser(c.Value, time.Now())
		switch {
		case errors.Is(err, control.ErrNotFound), errors.Is(err, control.ErrExpired):
			r.clearAuthCookies(w)
			controlAPIError(w, http.StatusUnauthorized, "unauthorized", "session expired or unknown")
			return
		case err != nil:
			logProfileControlErr("session.lookup "+req.Method+" "+req.Pattern, err)
			r.writeControlUnavailable(w)
			return
		}
		if u.Disabled {
			r.clearAuthCookies(w)
			controlAPIError(w, http.StatusForbidden, "user_disabled", "user account is disabled")
			return
		}
		next(w, req.WithContext(withSession(req.Context(), sessionCtx{user: u, token: c.Value})))
	}
}

// requireCSRF — double-submit, привязанный к сессии: значение вычисляется
// сервером из (HttpOnly) сессионного токена, а клиент доставляет его в
// заголовке X-CSRF-Token. Подброшенная сиблинг-доменом cookie значения не
// подменит (cookie вообще не участвует в проверке). Только после requireSession.
func (r *Registry) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		s := sessionFrom(req)
		want := csrfFor(s.token)
		got := req.Header.Get(csrfHeaderName)
		if got == "" || s.token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			controlAPIError(w, http.StatusForbidden, "csrf_invalid", "csrf token mismatch")
			return
		}
		next(w, req)
	}
}

// ── cookies ───────────────────────────────────────────────────────

// issueAuthCookies ставит сессионную (HttpOnly) и CSRF (читаемую) куки.
// Secure всегда: кабинет обслуживается только по TLS. CSRF-токен детерминирован
// от сессионного токена (csrfFor) — cookie лишь доставляет его в JS.
func issueAuthCookies(w http.ResponseWriter, sessionToken string) string {
	csrf := csrfFor(sessionToken)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: sessionToken, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookieName, Value: csrf, Path: "/",
		HttpOnly: false, Secure: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
	return csrf
}

// csrfFor — CSRF-токен, производный от сессионного токена. Сессия хранится
// в HttpOnly-cookie, поэтому атакующий не может вычислить токен; при этом
// проверка не зависит от cookie-канала (анти cookie-tossing).
func csrfFor(sessionToken string) string {
	sum := sha256.Sum256([]byte("sharedd-csrf:" + sessionToken))
	return hex.EncodeToString(sum[:])
}

func (r *Registry) clearAuthCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookieName, csrfCookieName} {
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", HttpOnly: name == sessionCookieName,
			Secure: true, SameSite: http.SameSiteLaxMode, MaxAge: -1,
		})
	}
}

// ── rate limit (регистрация / логин) ──────────────────────────────

// rateLimiterMaxKeys — потолок карты лимитера (M6): при превышении старые
// ключи без свежих попаданий вычищаются, чтобы поток уникальных IP не рос
// бесконечно.
const rateLimiterMaxKeys = 4096

type ipRateLimiter struct {
	mu     chan struct{}
	limit  int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

func newIPRateLimiter(limit int, window time.Duration) *ipRateLimiter {
	return &ipRateLimiter{mu: make(chan struct{}, 1), limit: limit, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

// pruneLocked оставляет только свежие попадания ключа и при переполнении
// карты вычищает ключи без свежих попаданий.
func (l *ipRateLimiter) pruneLocked(key string, now time.Time) []time.Time {
	cutoff := now.Add(-l.window)
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
	} else {
		l.hits[key] = kept
	}
	if len(l.hits) >= rateLimiterMaxKeys {
		for k, v := range l.hits {
			if len(l.hits) < rateLimiterMaxKeys {
				break
			}
			if k == key {
				continue
			}
			fresh := false
			for _, t := range v {
				if t.After(cutoff) {
					fresh = true
					break
				}
			}
			if !fresh {
				delete(l.hits, k)
			}
		}
		// Жёсткий потолок: если все ключи свежие (флуд уникальных адресов),
		// вытесняем произвольные до лимита (M6). Per-key точность под атакой
		// деградирует осознанно, память — нет.
		for k := range l.hits {
			if len(l.hits) < rateLimiterMaxKeys {
				break
			}
			if k != key {
				delete(l.hits, k)
			}
		}
	}
	return kept
}

// blocked — превышен ли лимит ключа (без записи нового попадания).
func (l *ipRateLimiter) blocked(key string) bool {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	kept := l.pruneLocked(key, l.now())
	return len(kept) >= l.limit
}

// record — зафиксировать попадание ключа.
func (l *ipRateLimiter) record(key string) {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	now := l.now()
	l.pruneLocked(key, now)
	l.hits[key] = append(l.hits[key], now)
}

// reset — снять счётчик ключа (успешный логин).
func (l *ipRateLimiter) reset(key string) {
	l.mu <- struct{}{}
	defer func() { <-l.mu }()
	delete(l.hits, key)
}

func (l *ipRateLimiter) allow(key string) bool {
	if l.blocked(key) {
		return false
	}
	l.record(key)
	return true
}

// cabinetLimiter — ленивая инициализация (5 регистраций / 10 мин на IP).
func (r *Registry) cabinetLimiter() *ipRateLimiter {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	if r.regLimiter == nil {
		r.regLimiter = newIPRateLimiter(5, 10*time.Minute)
	}
	return r.regLimiter
}

// loginIPLimiter — 10 попыток логина / 15 мин на IP (H2).
func (r *Registry) loginIPLimiter() *ipRateLimiter {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	if r.loginLimiter == nil {
		r.loginLimiter = newIPRateLimiter(10, 15*time.Minute)
	}
	return r.loginLimiter
}

// loginFailLimiter — 5 неудач логина / 15 мин на username (H2, per-account
// lockout поверх per-IP).
func (r *Registry) loginFailLimiter() *ipRateLimiter {
	r.rateMu.Lock()
	defer r.rateMu.Unlock()
	if r.loginFailLimit == nil {
		r.loginFailLimit = newIPRateLimiter(5, 15*time.Minute)
	}
	return r.loginFailLimit
}

func clientIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// ── маршруты ──────────────────────────────────────────────────────

func (r *Registry) mountCabinet(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/register", r.handleAuthRegister)
	mux.HandleFunc("POST /api/auth/login", r.handleAuthLogin)
	mux.HandleFunc("POST /api/auth/logout", r.requireSession(r.requireCSRF(r.handleAuthLogout)))
	mux.HandleFunc("GET /api/me", r.requireSession(r.handleMe))
	mux.HandleFunc("GET /api/me/install-defaults", r.requireSession(r.handleMeInstallDefaults))
	mux.HandleFunc("GET /api/me/nodes", r.requireSession(r.handleMeNodes))
	mux.HandleFunc("GET /api/me/nodes/{node_id}/detail", r.requireSession(r.handleMeNodeDetail))
	mux.HandleFunc("GET /api/me/authkey", r.requireSession(r.handleMeAuthkey))
	mux.HandleFunc("POST /api/me/authkey/rotate", r.requireSession(r.requireCSRF(r.handleMeAuthkeyRotate)))
	mux.HandleFunc("DELETE /api/me/authkey", r.requireSession(r.requireCSRF(r.handleMeAuthkeyDelete)))
	mux.HandleFunc("GET /api/me/beget/link", r.requireSession(r.handleBegetLinkGet))
	mux.HandleFunc("PUT /api/me/beget/link", r.requireSession(r.requireCSRF(r.handleBegetLinkPut)))
	mux.HandleFunc("GET /api/me/beget/regions", r.requireSession(r.handleBegetRegions))
	mux.HandleFunc("POST /api/me/vps", r.requireSession(r.requireCSRF(r.handleMeVps)))
	mux.HandleFunc("GET /api/me/operations", r.requireSession(r.handleMeOperations))
	mux.HandleFunc("POST /api/me/operations/{id}/cancel", r.requireSession(r.requireCSRF(r.handleOperationCancel)))
	mux.HandleFunc("POST /api/me/operations/{id}/retry-install", r.requireSession(r.requireCSRF(r.handleOperationRetryInstall)))
	mux.HandleFunc("DELETE /api/me/nodes/{node_id}", r.requireSession(r.requireCSRF(r.handleDeleteNode)))
	r.mountHostingConnections(mux)
	r.mountCloudResources(mux)
	r.mountFleets(mux)
	r.mountBlueprintRuns(mux)
}

func decodeCabinetJSON(w http.ResponseWriter, req *http.Request, dst any) error {
	return decodeCabinetJSONLimit(w, req, dst, 64<<10)
}

// decodeCabinetJSONLimit decodes a JSON body with an explicit byte cap
// (graph validate/CRUD bodies may be up to 256KiB, 03 §2).
func decodeCabinetJSONLimit(w http.ResponseWriter, req *http.Request, dst any, max int64) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, max)).Decode(dst); err != nil {
		return err
	}
	return nil
}

func writeCabinetJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// handleMeInstallDefaults exposes only non-secret installation defaults.
func (r *Registry) handleMeInstallDefaults(w http.ResponseWriter, req *http.Request) {
	r.cfgMu.RLock()
	defaults := struct {
		ProxyPort             int    `json:"proxy_port"`
		ProxySNI              string `json:"proxy_sni"`
		ProxySecretConfigured bool   `json:"proxy_secret_configured"`
	}{
		ProxyPort:             r.cfg.SharedProxy.Port,
		ProxySNI:              r.cfg.SharedProxy.TLSDomain,
		ProxySecretConfigured: selectProxySecret(r.cfg.SharedProxy.Users) != "",
	}
	r.cfgMu.RUnlock()
	writeCabinetJSON(w, http.StatusOK, defaults)
}

// ── auth ──────────────────────────────────────────────────────────

func (r *Registry) handleAuthRegister(w http.ResponseWriter, req *http.Request) {
	if !r.cabinetGuard(w) {
		return
	}
	if !r.cabinetLimiter().allow(clientIP(req)) {
		controlAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many registrations from this IP")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		// Invite — одноразовый токен приглашения; открытой саморегистрации
		// больше нет (invitations-migration-contract.md §2).
		Invite string `json:"invite"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	// Валидация полей и резерв приглашения атомарны (одна транзакция):
	// провал валидации полей приглашение не расходует. Квота нового
	// пользователя — [users] default_max_nodes (0 = прежнее поведение).
	u, err := r.control.CreateUserWithInviteQuota(body.Username, body.Password, body.Invite, r.cfg.Users.DefaultMaxNodes, time.Now())
	if err != nil {
		writeControlErr(w, err)
		return
	}
	writeCabinetJSON(w, http.StatusOK, toUserJSON(control.UserWithUsage{User: u}))
}

func (r *Registry) handleAuthLogin(w http.ResponseWriter, req *http.Request) {
	if !r.cabinetGuard(w) {
		return
	}
	ip := clientIP(req)
	if !r.loginIPLimiter().allow(ip) {
		controlAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts from this IP")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	// Per-account lockout поверх per-IP (H2).
	uname := strings.ToLower(strings.TrimSpace(body.Username))
	if uname != "" && r.loginFailLimiter().blocked(uname) {
		controlAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many failed attempts for this account")
		return
	}
	u, err := r.control.Authenticate(body.Username, body.Password)
	switch {
	case errors.Is(err, control.ErrUserDisabled):
		// Униформа 401: не раскрываем существование/статус аккаунта (M2).
		log.Printf("cabinet login denied for disabled account %q from %s", body.Username, ip)
		r.loginFailLimiter().record(uname)
		controlAPIError(w, http.StatusUnauthorized, "bad_credentials", "invalid username or password")
		return
	case errors.Is(err, control.ErrBadCredentials):
		r.loginFailLimiter().record(uname)
		controlAPIError(w, http.StatusUnauthorized, "bad_credentials", "invalid username or password")
		return
	case err != nil:
		r.writeControlUnavailable(w)
		return
	}
	r.loginFailLimiter().reset(uname)
	tok, err := r.control.CreateSession(u.ID, sessionTTL)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	issueAuthCookies(w, tok)
	used, _ := r.control.UsedNodes(u.ID)
	writeCabinetJSON(w, http.StatusOK, meView(u, used))
}

func (r *Registry) handleAuthLogout(w http.ResponseWriter, req *http.Request) {
	s := sessionFrom(req)
	if err := r.control.DeleteSession(s.token); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	r.clearAuthCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// ── /api/me ───────────────────────────────────────────────────────

type meJSON struct {
	Username  string `json:"username"`
	Role      string `json:"role"`
	MaxNodes  int    `json:"max_nodes"`
	UsedNodes int    `json:"used_nodes"`
}

func meView(u control.User, used int) meJSON {
	return meJSON{Username: u.Username, Role: u.Role, MaxNodes: u.MaxNodes, UsedNodes: used}
}

func (r *Registry) handleMe(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	used, err := r.control.UsedNodes(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, meView(u, used))
}

type quarantineJSON struct {
	Attempts  int   `json:"attempts"`
	EnteredAt int64 `json:"entered_at"`
}

// nodeJSON — санитизированное представление ноды кабинета: id/ip/status и
// агрегаты; provider-ids и SSH-секреты сюда не попадают вовсе.
type nodeJSON struct {
	NodeID        string          `json:"node_id"`
	IP            string          `json:"ip,omitempty"`
	Status        string          `json:"status"`
	Quarantine    *quarantineJSON `json:"quarantine,omitempty"`
	MasterDomains []string        `json:"master_domains,omitempty"`
	Availability  float64         `json:"availability"`
	DC2OK         bool            `json:"dc2_ok"`
}

func (r *Registry) handleMeNodes(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	owned, err := r.control.NodesByUser(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]nodeJSON, 0, len(owned))
	r.mu.RLock()
	freshness := r.cfg.ReportFreshnessTTL
	for _, n := range owned {
		out = append(out, buildNodeJSON(r.state.Candidates[n.NodeID], n.NodeID, n.Deleting, freshness, r.state.Assignments))
	}
	r.mu.RUnlock()
	writeCabinetJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// buildNodeJSON — слияние control-записи с живым кандидатом. ВЫЗЫВАТЬ под r.mu.
func buildNodeJSON(c *Candidate, nodeID string, deleting bool, freshness time.Duration, assignments map[string]AssignmentSlots) nodeJSON {
	n := nodeJSON{NodeID: nodeID, Status: "offline"}
	if deleting {
		n.Status = "deleting"
	}
	if c == nil {
		return n
	}
	n.IP = c.IP
	n.DC2OK = c.MetricsOK // для dc2-агентов MetricsOK = dc2_ok (см. healthcheck.go)
	n.Availability = c.AvailabilityPct()
	if c.Quarantine != nil {
		n.Quarantine = &quarantineJSON{Attempts: c.Quarantine.Attempts, EnteredAt: c.Quarantine.EnteredAt.Unix()}
	}
	for d, slots := range assignments {
		if slices.Contains(slots, nodeID) {
			n.MasterDomains = append(n.MasterDomains, d)
		}
	}
	sort.Strings(n.MasterDomains)
	switch {
	case deleting:
		n.Status = "deleting"
	case c.Quarantine != nil:
		n.Status = "quarantined"
	case c.IsFullyHealthy(freshness):
		n.Status = "healthy"
	default:
		n.Status = "unhealthy"
	}
	return n
}

type authkeyJSON struct {
	Prefix    string `json:"prefix"`
	CreatedAt int64  `json:"created_at"`
	Active    bool   `json:"active"`
}

func (r *Registry) handleMeAuthkey(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	prefix, createdAt, active, err := r.control.LiveAuthkey(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, authkeyJSON{Prefix: prefix, CreatedAt: createdAt, Active: active})
}

// handleMeAuthkeyRotate — POST /api/me/authkey/rotate (session+CSRF): отзывает
// ВСЕ прежние ключи пользователя и выдаёт новый. Полный ключ показывается ОДИН
// раз (хранится только хеш). Один ключ покрывает все VPS пользователя.
func (r *Registry) handleMeAuthkeyRotate(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	key, err := r.control.RotateAuthkey(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]string{"authkey": key})
}

// handleMeAuthkeyDelete — DELETE /api/me/authkey (session+CSRF): отзывает все
// ключи пользователя. Живые ноды продолжают работать, новые регистрации с
// отозванным ключом — нет.
func (r *Registry) handleMeAuthkeyDelete(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	if err := r.control.RevokeAuthkeys(u.ID); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type profileJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	VarsSchema  string `json:"vars_schema,omitempty"`
}

// ── DELETE /api/me/nodes/{node_id} ────────────────────────────────

func (r *Registry) handleDeleteNode(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	nodeID := strings.TrimSpace(req.PathValue("node_id"))
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
	if err := r.control.MarkNodeDeleting(nodeID); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"node_id": nodeID, "state": "deleting"})
}

// strconv используется в beget-части (тот же файл пакета); держим импорт
// осмысленным — parseID вынесен сюда.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil
}
