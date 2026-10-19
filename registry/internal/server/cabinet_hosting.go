package server

// Hosting-connections кабинета (docs/specs/hosting-blueprints/03-cabinet-api.md
// §3, bounded stage-04): CRUD + verify + balance refresh + каталог-заглушка.
// Credentials только в зашифрованном виде (cryptoutil.Seal, master.key);
// наружу plaintext не отдаётся никогда — ConnectionJSON его не содержит.
// Balance refresh коалесится per-connection (singleflight, in-process) с
// min-интервалом 60s; провайдер-ошибки никогда не превращаются в 500.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/nodehost"
)

// balMinInterval — повторный refresh в пределах интервала возвращает
// сохранённый свежий снимок без внешнего вызова (03-cabinet-api.md §3).
const balMinInterval = 60 * time.Second

// ── провайдеры (проверка кредов + баланс) ─────────────────────────

type hostingBalanceResult struct{ Amount, Currency, Scope string }

// hostingProvider — поверхность провайдера, нужная кабинету.
type hostingProvider interface {
	Check(ctx context.Context) error // verify-проба
	Balance(ctx context.Context) (hostingBalanceResult, error)
}

// hostingNodehost — реальные вызовы nodehost API (Account/AccountBalance).
type hostingNodehost struct{ cl *nodehost.Client }

func (p hostingNodehost) Check(ctx context.Context) error {
	_, err := p.cl.Account(ctx)
	return err
}

func (p hostingNodehost) Balance(ctx context.Context) (hostingBalanceResult, error) {
	b, err := p.cl.AccountBalance(ctx)
	if err != nil {
		return hostingBalanceResult{}, err
	}
	return hostingBalanceResult{Amount: string(b.Amount), Currency: b.Currency, Scope: b.Scope}, nil
}

// hostingBeget — ограничение stage-04: cloud-JWT verify отложен, успех
// hosting Balance() считается verify. Без hosting-пары клиент отвечает
// balance_credentials_missing → 422 (задокументировано в stage-04 doc).
type hostingBeget struct{ cl *beget.BalanceClient }

func (p hostingBeget) Check(ctx context.Context) error {
	_, err := p.cl.Balance(ctx)
	return err
}

func (p hostingBeget) Balance(ctx context.Context) (hostingBalanceResult, error) {
	b, err := p.cl.Balance(ctx)
	if err != nil {
		return hostingBalanceResult{}, err
	}
	return hostingBalanceResult{Amount: b.Amount, Currency: b.Currency, Scope: b.Scope}, nil
}

// hostingProviderFor — фабрика: тесты подменяют r.hostingFactory.
func (r *Registry) hostingProviderFor(c control.HostingConnection) (hostingProvider, error) {
	if r.hostingFactory != nil {
		return r.hostingFactory(c)
	}
	plain, err := cryptoutil.Open(r.masterKey, c.CredentialsEnc)
	if err != nil {
		return nil, err
	}
	switch c.Provider {
	case control.ProviderNodehost:
		var creds struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal([]byte(plain), &creds); err != nil {
			return nil, err
		}
		cl, err := nodehost.New(creds.APIKey)
		if err != nil {
			return nil, err
		}
		return hostingNodehost{cl: cl}, nil
	case control.ProviderBeget:
		var creds struct {
			HostingAPILogin    string `json:"hosting_api_login"`
			HostingAPIPassword string `json:"hosting_api_password"`
		}
		if err := json.Unmarshal([]byte(plain), &creds); err != nil {
			return nil, err
		}
		return hostingBeget{cl: beget.NewBalanceClient(creds.HostingAPILogin, creds.HostingAPIPassword)}, nil
	}
	return nil, control.ErrValidation
}

// hostingTimeoutErr — transport/таймаут (504 provider_timeout); всё остальное
// — бизнес-отказ провайдера (422 provider_auth_failed). Коды клиентов уже
// безопасные (stage-03), текст провайдера наружу не идёт.
func hostingTimeoutErr(err error) bool {
	var ne *nodehost.Error
	if errors.As(err, &ne) {
		return ne.Code == "transport_error" || ne.Code == "request_canceled"
	}
	var be *beget.BalanceError
	if errors.As(err, &be) {
		return be.Code == "transport_error"
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// hostingSafeErrCode — безопасный код ошибки для safe_error снимка.
func hostingSafeErrCode(err error) string {
	var ne *nodehost.Error
	if errors.As(err, &ne) {
		return ne.Code
	}
	var be *beget.BalanceError
	if errors.As(err, &be) {
		return be.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider_timeout"
	}
	return "provider_error"
}

// ── credentials: валидация формы + запечатывание ──────────────────

// sealHostingCredentials — валидирует credentials-бандл по форме провайдера и
// возвращает AES-GCM шифротекст. nodehost: {api_key}; beget: {cloud_login,
// cloud_password} обязательны, hosting-пара all-or-none. Плоского текста в
// логи/ошибки не попадает: ошибка — фиксированный текст, не содержимое.
func (r *Registry) sealHostingCredentials(provider string, raw json.RawMessage) (string, error) {
	if raw == nil {
		return "", errors.New("credentials are required")
	}
	switch provider {
	case control.ProviderNodehost:
		var c struct {
			APIKey string `json:"api_key"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || strings.TrimSpace(c.APIKey) == "" {
			return "", errors.New("api_key is required for nodehost")
		}
		return r.sealJSON(map[string]string{"api_key": c.APIKey})
	case control.ProviderBeget:
		var c struct {
			CloudLogin         string `json:"cloud_login"`
			CloudPassword      string `json:"cloud_password"`
			HostingAPILogin    string `json:"hosting_api_login"`
			HostingAPIPassword string `json:"hosting_api_password"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", errors.New("bad credentials object")
		}
		if c.CloudLogin == "" || c.CloudPassword == "" {
			return "", errors.New("cloud_login and cloud_password are required for beget")
		}
		if (c.HostingAPILogin == "") != (c.HostingAPIPassword == "") {
			return "", errors.New("hosting api login and password must be set together")
		}
		return r.sealJSON(map[string]string{
			"cloud_login": c.CloudLogin, "cloud_password": c.CloudPassword,
			"hosting_api_login": c.HostingAPILogin, "hosting_api_password": c.HostingAPIPassword,
		})
	}
	return "", errors.New("unknown provider")
}

func (r *Registry) sealJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return cryptoutil.Seal(r.masterKey, string(b))
}

// ── DTO ───────────────────────────────────────────────────────────

type hostingCapabilities struct {
	Create   bool `json:"create"`
	ChangeIP bool `json:"change_ip"`
	Delete   bool `json:"delete"`
}

type hostingBalanceErrorJSON struct {
	Code string `json:"code"`
}

type hostingBalanceJSON struct {
	Amount        *string                  `json:"amount"`
	Currency      *string                  `json:"currency"`
	Scope         string                   `json:"scope"`
	Freshness     string                   `json:"freshness"`
	CheckedAt     *int64                   `json:"checked_at"`
	LastAttemptAt *int64                   `json:"last_attempt_at"`
	Error         *hostingBalanceErrorJSON `json:"error"`
}

type connectionJSON struct {
	ID                    int64               `json:"id"`
	Provider              string              `json:"provider"`
	Label                 string              `json:"label"`
	Revision              int64               `json:"revision"`
	CredentialRevision    int64               `json:"credential_revision"`
	CredentialsConfigured bool                `json:"credentials_configured"`
	VerifiedAt            *int64              `json:"verified_at"`
	Capabilities          hostingCapabilities `json:"capabilities"`
	Balance               hostingBalanceJSON  `json:"balance"`
}

func hostingCapsFor(provider string) hostingCapabilities {
	return hostingCapabilities{Create: true, ChangeIP: provider == control.ProviderNodehost, Delete: true}
}

// hostingCapsJSON — capabilities из сохранённого snapshot (после verify).
func hostingCapsJSON(c control.HostingConnection) hostingCapabilities {
	if c.CapabilitiesJSON == "" {
		return hostingCapabilities{}
	}
	var caps hostingCapabilities
	if err := json.Unmarshal([]byte(c.CapabilitiesJSON), &caps); err != nil {
		return hostingCapabilities{}
	}
	return caps
}

// balanceView — свежесть по состоянию снимка: успешное снятие позже
// последней попытки → fresh; неудачная попытка поверх прошлой удачи →
// stale (amount остаётся); удачи не было → unavailable.
func balanceView(b control.ConnectionBalance) hostingBalanceJSON {
	out := hostingBalanceJSON{Freshness: "unavailable", Scope: b.Scope}
	if b.LastAttemptAt > 0 {
		la := b.LastAttemptAt
		out.LastAttemptAt = &la
	}
	if b.SafeError != "" {
		out.Error = &hostingBalanceErrorJSON{Code: b.SafeError}
	}
	if b.CheckedAt > 0 {
		ca := b.CheckedAt
		out.CheckedAt = &ca
		if b.Amount != "" {
			a := b.Amount
			out.Amount = &a
			out.Freshness = "fresh"
			if b.LastAttemptAt > b.CheckedAt {
				out.Freshness = "stale"
			}
		}
	}
	if b.Currency != "" {
		c := b.Currency
		out.Currency = &c
	}
	return out
}

func connectionView(c control.HostingConnection, b *control.ConnectionBalance) connectionJSON {
	v := connectionJSON{
		ID:                    c.ID,
		Provider:              c.Provider,
		Label:                 c.Label,
		Revision:              c.Revision,
		CredentialRevision:    c.CredentialRevision,
		CredentialsConfigured: c.CredentialsEnc != "",
		Capabilities:          hostingCapsJSON(c),
		Balance:               hostingBalanceJSON{Freshness: "unavailable"},
	}
	if c.VerifiedAt > 0 {
		va := c.VerifiedAt
		v.VerifiedAt = &va
	}
	if b != nil {
		v.Balance = balanceView(*b)
	}
	return v
}

func (r *Registry) connectionBalanceOf(id int64) *control.ConnectionBalance {
	b, err := r.control.HostingBalance(id)
	if err != nil {
		return nil
	}
	return &b
}

// hostingControlErr — маппинг sentinel-ошибок control в HTTP контракта.
func hostingControlErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid input")
	case errors.Is(err, control.ErrRevisionConflict):
		controlAPIError(w, http.StatusConflict, "revision_conflict", "connection was modified concurrently")
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "connection is not owned by the current user")
	default:
		log.Printf("hosting control error: %v", err)
		controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
	}
}

// hostingOwned — id из пути + подключение владельца; false = ответ записан.
func (r *Registry) hostingOwned(w http.ResponseWriter, req *http.Request) (control.HostingConnection, bool) {
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "connection is not owned by the current user")
		return control.HostingConnection{}, false
	}
	c, err := r.control.HostingConnectionOwned(id, sessionFrom(req).user.ID)
	if err != nil {
		hostingControlErr(w, err)
		return control.HostingConnection{}, false
	}
	return c, true
}

// ── маршруты ──────────────────────────────────────────────────────

func (r *Registry) mountHostingConnections(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/hosting-connections", r.requireSession(r.handleHostingList))
	mux.HandleFunc("POST /api/me/hosting-connections", r.requireSession(r.requireCSRF(r.handleHostingCreate)))
	mux.HandleFunc("PATCH /api/me/hosting-connections/{id}", r.requireSession(r.requireCSRF(r.handleHostingPatch)))
	mux.HandleFunc("DELETE /api/me/hosting-connections/{id}", r.requireSession(r.requireCSRF(r.handleHostingDelete)))
	mux.HandleFunc("POST /api/me/hosting-connections/{id}/verify", r.requireSession(r.requireCSRF(r.handleHostingVerify)))
	mux.HandleFunc("POST /api/me/hosting-connections/{id}/balance/refresh", r.requireSession(r.requireCSRF(r.handleHostingBalanceRefresh)))
	mux.HandleFunc("GET /api/me/hosting-connections/{id}/catalog", r.requireSession(r.handleHostingCatalog))
}

// GET /api/me/hosting-connections
func (r *Registry) handleHostingList(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	conns, err := r.control.HostingConnectionsByUser(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]connectionJSON, 0, len(conns))
	// ponytail: per-connection balance read; JOIN если списки вырастут.
	for _, c := range conns {
		out = append(out, connectionView(c, r.connectionBalanceOf(c.ID)))
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"connections": out})
}

// POST /api/me/hosting-connections — провайдер на create НЕ вызывается.
func (r *Registry) handleHostingCreate(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	var body struct {
		Provider    string          `json:"provider"`
		Label       string          `json:"label"`
		Credentials json.RawMessage `json:"credentials"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	sealed, err := r.sealHostingCredentials(body.Provider, body.Credentials)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	c, err := r.control.CreateHostingConnection(control.HostingConnection{
		UserID:             sessionFrom(req).user.ID,
		Provider:           body.Provider,
		Label:              strings.TrimSpace(body.Label),
		CredentialsEnc:     sealed,
		CredentialRevision: 1,
	})
	if err != nil {
		hostingControlErr(w, err)
		return
	}
	writeCabinetJSON(w, http.StatusCreated, connectionView(c, nil))
}

// PATCH /api/me/hosting-connections/{id} — CAS по expected_revision;
// credentials опущены → бандл не трогаем, передан → полная замена.
func (r *Registry) handleHostingPatch(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	c, ok := r.hostingOwned(w, req)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision int64           `json:"expected_revision"`
		Label            *string         `json:"label"`
		Credentials      json.RawMessage `json:"credentials"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	var label *string
	if body.Label != nil {
		l := strings.TrimSpace(*body.Label)
		label = &l
	}
	var sealed *string
	if body.Credentials != nil {
		s, err := r.sealHostingCredentials(c.Provider, body.Credentials)
		if err != nil {
			controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
			return
		}
		sealed = &s
	}
	updated, err := r.control.PatchHostingConnection(c.ID, sessionFrom(req).user.ID, body.ExpectedRevision, label, sealed)
	if err != nil {
		hostingControlErr(w, err)
		return
	}
	writeCabinetJSON(w, http.StatusOK, connectionView(updated, r.connectionBalanceOf(c.ID)))
}

// DELETE /api/me/hosting-connections/{id} — 204; чужое → 404. Ссылающихся
// таблиц пока нет (stage-04), поэтому connection_in_use не бывает.
func (r *Registry) handleHostingDelete(w http.ResponseWriter, req *http.Request) {
	c, ok := r.hostingOwned(w, req)
	if !ok {
		return
	}
	if err := r.control.DeleteHostingConnection(c.ID, sessionFrom(req).user.ID); err != nil {
		hostingControlErr(w, err)
		return
	}
	// Завершить возможный in-flight refresh: ждущие получат снимок, новые
	// пойдут по свежему пути.
	r.balMu.Lock()
	delete(r.balBusy, c.ID)
	r.balMu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// POST /api/me/hosting-connections/{id}/verify — {code?} принимается, но
// 2FA-flow пока не используется (задокументировано). Ограничение stage-04:
// beget — успех Balance() трактуется как verified (cloud-JWT verify отложен).
func (r *Registry) handleHostingVerify(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	c, ok := r.hostingOwned(w, req)
	if !ok {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	prov, err := r.hostingProviderFor(c)
	if err != nil {
		// Креды не расшифровались/не прошли клиентскую валидацию — это
		// безопасный отказ, без деталей шифротекста.
		log.Printf("hosting verify %d: provider unavailable: %v", c.ID, err)
		controlAPIError(w, http.StatusUnprocessableEntity, "provider_auth_failed", "provider rejected the credentials")
		return
	}
	ctx, cancel := providerCtx(req)
	defer cancel()
	if err := prov.Check(ctx); err != nil {
		if hostingTimeoutErr(err) {
			controlAPIError(w, http.StatusGatewayTimeout, "provider_timeout", "provider did not answer in time")
			return
		}
		controlAPIError(w, http.StatusUnprocessableEntity, "provider_auth_failed", "provider rejected the credentials")
		return
	}
	checkedAt := time.Now().Unix()
	caps := hostingCapsFor(c.Provider)
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	// CAS по credential_revision: креды сменились, пока Check был в полёте →
	// 409, а не штамп верификации под устаревший бандл.
	if err := r.control.UpdateHostingVerified(c.ID, c.UserID, c.CredentialRevision, checkedAt, string(capsJSON)); err != nil {
		hostingControlErr(w, err)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"verified": true, "checked_at": checkedAt, "capabilities": caps,
	})
}

// ── balance refresh: singleflight + 60s min-interval ──────────────

// balCall — один внешний вызов баланса на подключение; ждущие разделяют
// готовый ответ (готовый JSON ConnectionJSON, всегда 200).
type balCall struct {
	done chan struct{}
	out  []byte
}

func (r *Registry) balClock() time.Time {
	if r.balNow != nil {
		return r.balNow()
	}
	return time.Now()
}

// POST /api/me/hosting-connections/{id}/balance/refresh — провайдер-ошибка
// это всё равно 200 со снимком (stale/unavailable + error.code); 500 никогда.
func (r *Registry) handleHostingBalanceRefresh(w http.ResponseWriter, req *http.Request) {
	c, ok := r.hostingOwned(w, req)
	if !ok {
		return
	}
	now := r.balClock()

	r.balMu.Lock()
	if r.balBusy == nil {
		r.balBusy = map[int64]*balCall{}
	}
	if call, busy := r.balBusy[c.ID]; busy {
		r.balMu.Unlock()
		<-call.done
		r.writeRawJSON(w, http.StatusOK, call.out)
		return
	}
	// Min-interval: свежий удачный снимок + недавняя попытка → без вызова.
	if b, err := r.control.HostingBalance(c.ID); err == nil &&
		b.Amount != "" && b.CheckedAt > 0 &&
		now.Unix()-b.LastAttemptAt < int64(balMinInterval.Seconds()) {
		r.balMu.Unlock()
		writeCabinetJSON(w, http.StatusOK, connectionView(c, &b))
		return
	}
	call := &balCall{done: make(chan struct{})}
	r.balBusy[c.ID] = call
	r.balMu.Unlock()

	// ponytail: release в defer — паника в провайдере не оставляет busy-запись
	// и не подвешивает ждущих на call.done; compare-and-delete не задевает
	// запись преемника (DELETE+новый лидер во время in-flight вызова).
	defer func() {
		r.balMu.Lock()
		if cur, ok := r.balBusy[c.ID]; ok && cur == call {
			delete(r.balBusy, c.ID)
		}
		r.balMu.Unlock()
		close(call.done)
	}()

	r.refreshBalance(req.Context(), call, c, now)
	r.writeRawJSON(w, http.StatusOK, call.out)
}

func (r *Registry) writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// refreshBalance — внешний вызов + сохранение снимка (CAS по credential_revision).
func (r *Registry) refreshBalance(ctx context.Context, call *balCall, c control.HostingConnection, now time.Time) {
	b := control.ConnectionBalance{
		ConnectionID: c.ID, UserID: c.UserID, CredentialRevision: c.CredentialRevision,
		LastAttemptAt: now.Unix(),
	}
	if prov, err := r.hostingProviderFor(c); err != nil {
		b.SafeError = "credentials_unavailable"
	} else {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if res, err := prov.Balance(callCtx); err != nil {
			b.SafeError = hostingSafeErrCode(err)
			// Неудачная попытка сохраняет прошлый удачный снимок
			// (fresh→stale, amount не теряется); попыток не было —
			// остаётся unavailable.
			if prev, perr := r.control.HostingBalance(c.ID); perr == nil && prev.CheckedAt > 0 {
				b.Amount, b.Currency, b.Scope, b.CheckedAt = prev.Amount, prev.Currency, prev.Scope, prev.CheckedAt
			}
		} else {
			b.Amount, b.Currency, b.Scope, b.CheckedAt, b.SafeError = res.Amount, res.Currency, res.Scope, now.Unix(), ""
		}
	}
	// Сохранение под CAS по credential_revision (странствующий in-flight
	// запрос со старыми кредами снимок не перезапишет).
	if _, err := r.control.SaveHostingBalance(b); err != nil {
		log.Printf("hosting balance %d: save: %v", c.ID, err)
	}
	r.emitBalance(call, c)
}

// emitBalance — собирает ответ из фактического состояния БД (CAS-конфликт,
// параллельный PATCH и т.п. отражаются честно).
func (r *Registry) emitBalance(call *balCall, c control.HostingConnection) {
	fresh, err := r.control.HostingConnectionOwned(c.ID, c.UserID)
	if err != nil {
		fresh = c
	}
	stored := r.connectionBalanceOf(c.ID)
	out, err := json.Marshal(connectionView(fresh, stored))
	if err != nil {
		out = []byte(`{"error":"internal"}`)
	}
	call.out = out
}

// GET /api/me/hosting-connections/{id}/catalog — реализован в
// cabinet_catalog.go (nodehost; beget → 501).
