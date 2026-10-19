package server

// Тесты hosting-connections кабинета (stage-04): CRUD-lifecycle, CSRF,
// валидация, verify-маппинг, баланс (fresh/stale/unavailable, min-interval,
// коалесинг), отсутствие утечки credentials в JSON. Реальных провайдеров нет —
// фабрика подменена моком-счётчиком (как fakeBeget).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

// fakeHosting — мок hosting-провайдера: счётчики внешних вызовов, управляемые
// ошибки, блокировка Balance для теста коалесинга.
type fakeHosting struct {
	mu         sync.Mutex
	checks     int
	balances   int
	checkErr   error
	balanceErr error
	balResult  hostingBalanceResult
	// panicBalance — Balance паникует (R-2/R-3: singleflight не wedge).
	panicBalance bool
	// catalog/estimate (cabinet_catalog.go): управляемые данные и ошибки.
	catalogData hostingCatalogData
	catalogErr  error
	estResult   *nodehost.Estimate
	estErr      error
	// blockBalance — Balance держится до закрытия канала (коалесинг).
	blockBalance chan struct{}
	// balanceStarted закрывается на входе в первый Balance.
	balanceStarted chan struct{}
	startedOnce    sync.Once
	// blockCheck — Check держится до закрытия канала (R-1: verify/PATCH-гонка).
	blockCheck   chan struct{}
	checkStarted chan struct{}
	checkOnce    sync.Once
}

func (f *fakeHosting) Check(context.Context) error {
	f.mu.Lock()
	f.checks++
	err := f.checkErr
	f.mu.Unlock()
	if f.checkStarted != nil {
		f.checkOnce.Do(func() { close(f.checkStarted) })
	}
	if f.blockCheck != nil {
		<-f.blockCheck
	}
	return err
}

func (f *fakeHosting) Balance(ctx context.Context) (hostingBalanceResult, error) {
	f.mu.Lock()
	f.balances++
	boom := f.panicBalance
	f.mu.Unlock()
	if boom {
		panic("fake: provider adapter exploded")
	}
	if f.balanceStarted != nil {
		f.startedOnce.Do(func() { close(f.balanceStarted) })
	}
	if f.blockBalance != nil {
		select {
		case <-f.blockBalance:
		case <-ctx.Done():
			return hostingBalanceResult{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.balanceErr != nil {
		return hostingBalanceResult{}, f.balanceErr
	}
	return f.balResult, nil
}

func (f *fakeHosting) balanceCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.balances
}

// catalogData/catalogErr/estResult/estErr — поверхность hostingCataloger
// (cabinet_catalog.go); nil err/nil result — значения по умолчанию для тестов,
// которые каталог не трогают.
func (f *fakeHosting) Catalog(_ context.Context, _ string) (hostingCatalogData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.catalogData, f.catalogErr
}

func (f *fakeHosting) Estimate(_ context.Context, _ nodehost.EstimateRequest) (*nodehost.Estimate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.estResult, f.estErr
}

// newHostingRegistry — кабинет + подменённая фабрика провайдера.
func newHostingRegistry(t *testing.T) (*Registry, *fakeHosting) {
	t.Helper()
	r, _ := newCabinetRegistry(t)
	fake := &fakeHosting{balResult: hostingBalanceResult{Amount: "42.50", Currency: "USD", Scope: "nodehost_api"}}
	r.hostingFactory = func(control.HostingConnection) (hostingProvider, error) { return fake, nil }
	return r, fake
}

func hostingSession(t *testing.T, r *Registry, name string) (int64, map[string]string, string) {
	t.Helper()
	uid := cabinetUser(t, r, name, 10)
	sess, csrf := cabinetLogin(t, r, name, "password-123")
	return uid, cookieMap(sess, csrf), csrf
}

func hostingCreate(t *testing.T, r *Registry, cookies map[string]string, csrf, body string) *httptestResponse {
	t.Helper()
	return wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/hosting-connections", cookies, csrf, body))
}

// httptestResponse — тонкая обёртка для читаемых ассертов.
type httptestResponse struct {
	Code int
	Body string
}

func wrapRec(rr *httptest.ResponseRecorder) *httptestResponse {
	return &httptestResponse{Code: rr.Code, Body: rr.Body.String()}
}

type connJSON struct {
	ID                    int64  `json:"id"`
	Provider              string `json:"provider"`
	Label                 string `json:"label"`
	Revision              int64  `json:"revision"`
	CredentialRevision    int64  `json:"credential_revision"`
	CredentialsConfigured bool   `json:"credentials_configured"`
	VerifiedAt            *int64 `json:"verified_at"`
	Capabilities          struct {
		Create   bool `json:"create"`
		ChangeIP bool `json:"change_ip"`
		Delete   bool `json:"delete"`
	} `json:"capabilities"`
	Balance struct {
		Amount        *string `json:"amount"`
		Currency      *string `json:"currency"`
		Scope         string  `json:"scope"`
		Freshness     string  `json:"freshness"`
		CheckedAt     *int64  `json:"checked_at"`
		LastAttemptAt *int64  `json:"last_attempt_at"`
		Error         *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"balance"`
}

func decodeConn(t *testing.T, body string) connJSON {
	t.Helper()
	var c connJSON
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatalf("decode connection: %v (%s)", err, body)
	}
	return c
}

func decodeConns(t *testing.T, body string) []connJSON {
	t.Helper()
	var out struct {
		Connections []connJSON `json:"connections"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode list: %v (%s)", err, body)
	}
	return out.Connections
}

func TestHostingCRUDLifecycle(t *testing.T) {
	r, _ := newHostingRegistry(t)
	uidA, sessA, csrfA := hostingSession(t, r, "alice")
	_, sessB, csrfB := hostingSession(t, r, "bob")

	// Create: 201, ревизии 1/1, провайдер не вызывается, verified_at null.
	rec := hostingCreate(t, r, sessA, csrfA,
		`{"provider":"nodehost","label":"main","credentials":{"api_key":"key-1"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	c := decodeConn(t, rec.Body)
	if c.ID == 0 || c.Revision != 1 || c.CredentialRevision != 1 || !c.CredentialsConfigured || c.VerifiedAt != nil {
		t.Fatalf("create result: %+v", c)
	}
	if c.Capabilities != (struct {
		Create   bool `json:"create"`
		ChangeIP bool `json:"change_ip"`
		Delete   bool `json:"delete"`
	}{}) {
		t.Fatalf("capabilities must be empty before verify: %+v", c.Capabilities)
	}

	// List.
	if list := decodeConns(t, wrapRec(cabinetCall(t, r, http.MethodGet, "/api/me/hosting-connections", sessA, "", "")).Body); len(list) != 1 || list[0].ID != c.ID {
		t.Fatalf("list: %+v", list)
	}

	// PATCH label only: revision 2, credential_revision 1.
	rec = wrapRec(cabinetCall(t, r, http.MethodPatch, fmt.Sprintf("/api/me/hosting-connections/%d", c.ID),
		sessA, csrfA, `{"expected_revision":1,"label":"renamed"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch label: %d %s", rec.Code, rec.Body)
	}
	if got := decodeConn(t, rec.Body); got.Label != "renamed" || got.Revision != 2 || got.CredentialRevision != 1 {
		t.Fatalf("patch label result: %+v", got)
	}

	// PATCH credentials: credential_revision 2, revision 3.
	rec = wrapRec(cabinetCall(t, r, http.MethodPatch, fmt.Sprintf("/api/me/hosting-connections/%d", c.ID),
		sessA, csrfA, `{"expected_revision":2,"credentials":{"api_key":"key-2"}}`))
	if rec.Code != http.StatusOK || decodeConn(t, rec.Body).CredentialRevision != 2 {
		t.Fatalf("patch creds: %d %s", rec.Code, rec.Body)
	}

	// Revision conflict.
	rec = wrapRec(cabinetCall(t, r, http.MethodPatch, fmt.Sprintf("/api/me/hosting-connections/%d", c.ID),
		sessA, csrfA, `{"expected_revision":1,"label":"x"}`))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body, "revision_conflict") {
		t.Fatalf("stale revision: %d %s", rec.Code, rec.Body)
	}

	// Foreign id: чужой PATCH/DELETE → 404.
	foreign := fmt.Sprintf("/api/me/hosting-connections/%d", c.ID)
	rec = wrapRec(cabinetCall(t, r, http.MethodPatch, foreign, sessB, csrfB, `{"expected_revision":3,"label":"hijack"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign patch: %d %s", rec.Code, rec.Body)
	}
	rec = wrapRec(cabinetCall(t, r, http.MethodDelete, foreign, sessB, csrfB, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete: %d %s", rec.Code, rec.Body)
	}

	// Delete свой → 204, повторный → 404.
	if rec = wrapRec(cabinetCall(t, r, http.MethodDelete, foreign, sessA, csrfA, "")); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if rec = wrapRec(cabinetCall(t, r, http.MethodDelete, foreign, sessA, csrfA, "")); rec.Code != http.StatusNotFound {
		t.Fatalf("double delete: %d %s", rec.Code, rec.Body)
	}
	if uidA == 0 {
		t.Fatal("unused")
	}
}

func TestHostingCreateRequiresCSRF(t *testing.T) {
	r, _ := newHostingRegistry(t)
	_, sess, _ := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, "", `{"provider":"nodehost","label":"x","credentials":{"api_key":"k"}}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body, "csrf_invalid") {
		t.Fatalf("csrf: %d %s", rec.Code, rec.Body)
	}
}

func TestHostingCreateValidation(t *testing.T) {
	r, _ := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	long := strings.Repeat("а", 101)
	cases := []struct {
		name, body string
	}{
		{"bad provider", `{"provider":"aws","label":"x","credentials":{"api_key":"k"}}`},
		{"empty label", `{"provider":"nodehost","label":"","credentials":{"api_key":"k"}}`},
		{"long label", fmt.Sprintf(`{"provider":"nodehost","label":%q,"credentials":{"api_key":"k"}}`, long)},
		{"nodehost no key", `{"provider":"nodehost","label":"x","credentials":{}}`},
		{"nodehost no credentials", `{"provider":"nodehost","label":"x"}`},
		{"beget no cloud pair", `{"provider":"beget","label":"x","credentials":{"cloud_login":"l"}}`},
		{"beget hosting half", `{"provider":"beget","label":"x","credentials":{"cloud_login":"l","cloud_password":"p","hosting_api_login":"h"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := hostingCreate(t, r, sess, csrf, tc.body)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body, "validation_failed") {
				t.Fatalf("%s: %d %s", tc.name, rec.Code, rec.Body)
			}
		})
	}
	// Корректный beget (hosting-пара полная) проходит.
	rec := hostingCreate(t, r, sess, csrf,
		`{"provider":"beget","label":"cloud","credentials":{"cloud_login":"l","cloud_password":"p","hosting_api_login":"h","hosting_api_password":"hp"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid beget: %d %s", rec.Code, rec.Body)
	}
}

func TestHostingVerify(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")

	mkConn := func(provider string) int64 {
		rec := hostingCreate(t, r, sess, csrf,
			fmt.Sprintf(`{"provider":%q,"label":"c","credentials":{"api_key":"k","cloud_login":"l","cloud_password":"p"}}`, provider))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", provider, rec.Code, rec.Body)
		}
		return decodeConn(t, rec.Body).ID
	}
	patchErr := func(err error) {
		fake.mu.Lock()
		fake.checkErr = err
		fake.mu.Unlock()
	}

	// nodehost: успех → 200, capabilities {create,change_ip,delete}.
	id := mkConn(control.ProviderNodehost)
	rec := wrapRec(cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{"code":"123456"}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"verified":true`) {
		t.Fatalf("verify ok: %d %s", rec.Code, rec.Body)
	}
	c := decodeConns(t, wrapRec(cabinetCall(t, r, http.MethodGet, "/api/me/hosting-connections", sess, "", "")).Body)[0]
	if c.VerifiedAt == nil || !c.Capabilities.Create || !c.Capabilities.ChangeIP || !c.Capabilities.Delete {
		t.Fatalf("verified state: %+v", c)
	}

	// Бизнес-отказ → 422 provider_auth_failed.
	patchErr(&nodehost.Error{Code: "unauthorized"})
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body, "provider_auth_failed") {
		t.Fatalf("verify auth fail: %d %s", rec.Code, rec.Body)
	}

	// Transport → 504 provider_timeout.
	patchErr(&nodehost.Error{Code: "transport_error"})
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	if rec.Code != http.StatusGatewayTimeout || !strings.Contains(rec.Body, "provider_timeout") {
		t.Fatalf("verify timeout: %d %s", rec.Code, rec.Body)
	}

	// beget: change_ip=false в capabilities.
	id = mkConn(control.ProviderBeget)
	patchErr(nil)
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"change_ip":false`) {
		t.Fatalf("verify beget: %d %s", rec.Code, rec.Body)
	}

	// beget BalanceError (бизнес) → 422.
	patchErr(&beget.BalanceError{Code: "error_code"})
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("verify beget fail: %d %s", rec.Code, rec.Body)
	}
	if fake.checks < 4 {
		t.Fatalf("provider Check calls: %d", fake.checks)
	}
}

// balanceClock — инъекция часов (unix) для min-interval тестов.
func balanceClock(r *Registry, at *atomic.Int64) {
	r.balNow = func() time.Time { return time.Unix(at.Load(), 0) }
}

func hostingRefresh(t *testing.T, r *Registry, sess map[string]string, csrf string, id int64) *httptestResponse {
	t.Helper()
	return wrapRec(cabinetCall(t, r, http.MethodPost,
		fmt.Sprintf("/api/me/hosting-connections/%d/balance/refresh", id), sess, csrf, `{}`))
}

func TestHostingBalanceLifecycle(t *testing.T) {
	r, fake := newHostingRegistry(t)
	var at atomic.Int64
	at.Store(1000)
	balanceClock(r, &at)
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf, `{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`)
	id := decodeConn(t, rec.Body).ID

	// Первая неудача → unavailable + error.code, 200 (не 500).
	fake.mu.Lock()
	fake.balanceErr = &nodehost.Error{Code: "provider_error"}
	fake.mu.Unlock()
	rec = hostingRefresh(t, r, sess, csrf, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("first failure: %d %s", rec.Code, rec.Body)
	}
	c := decodeConn(t, rec.Body)
	if c.Balance.Freshness != "unavailable" || c.Balance.Amount != nil || c.Balance.Error == nil || c.Balance.Error.Code != "provider_error" {
		t.Fatalf("unavailable snapshot: %+v", c.Balance)
	}

	// Успех после интервала → fresh.
	fake.mu.Lock()
	fake.balanceErr = nil
	fake.mu.Unlock()
	at.Store(1061)
	c = decodeConn(t, hostingRefresh(t, r, sess, csrf, id).Body)
	if c.Balance.Freshness != "fresh" || c.Balance.Amount == nil || *c.Balance.Amount != "42.50" || c.Balance.Error != nil {
		t.Fatalf("fresh snapshot: %+v", c.Balance)
	}

	// Неудача после интервала поверх удачного снимка → stale, amount сохранён.
	fake.mu.Lock()
	fake.balanceErr = &nodehost.Error{Code: "insufficient_funds"}
	fake.mu.Unlock()
	at.Store(2122)
	c = decodeConn(t, hostingRefresh(t, r, sess, csrf, id).Body)
	if c.Balance.Freshness != "stale" || c.Balance.Amount == nil || *c.Balance.Amount != "42.50" ||
		c.Balance.Error == nil || c.Balance.Error.Code != "insufficient_funds" {
		t.Fatalf("stale snapshot: %+v", c.Balance)
	}
	if n := fake.balanceCalls(); n != 3 {
		t.Fatalf("external calls: %d", n)
	}

	// Min-interval: свежий снимок + недавняя попытка → без внешнего вызова.
	fake.mu.Lock()
	fake.balanceErr = nil
	fake.mu.Unlock()
	at.Store(2130)
	c = decodeConn(t, hostingRefresh(t, r, sess, csrf, id).Body)
	if c.Balance.Freshness != "stale" || fake.balanceCalls() != 3 {
		t.Fatalf("min-interval must reuse snapshot: calls=%d bal=%+v", fake.balanceCalls(), c.Balance)
	}
}

func TestHostingBalanceCoalescing(t *testing.T) {
	r, fake := newHostingRegistry(t)
	fake.blockBalance = make(chan struct{})
	fake.balanceStarted = make(chan struct{})
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf, `{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`)
	id := decodeConn(t, rec.Body).ID

	const n = 5
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = hostingRefresh(t, r, sess, csrf, id).Code
		}(i)
	}
	<-fake.balanceStarted
	// Окно, чтобы остальные запросы встали в ожидание coalesced-вызова.
	time.Sleep(50 * time.Millisecond)
	close(fake.blockBalance)
	wg.Wait()
	for i, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("request %d: %d", i, code)
		}
	}
	if calls := fake.balanceCalls(); calls != 1 {
		t.Fatalf("external calls: want 1, got %d", calls)
	}
}

// R-1: ротация кредов между provider Check и persist инвалидирует verify —
// 409 revision_conflict, verified_at/caps под старый бандл не штампуются.
func TestHostingVerifyCredentialRevisionConflict(t *testing.T) {
	r, fake := newHostingRegistry(t)
	fake.blockCheck = make(chan struct{})
	fake.checkStarted = make(chan struct{})
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf, `{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`)
	id := decodeConn(t, rec.Body).ID

	var verifyRec *httptestResponse
	verifyDone := make(chan struct{})
	go func() {
		defer close(verifyDone)
		verifyRec = wrapRec(cabinetCall(t, r, http.MethodPost,
			fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	}()
	<-fake.checkStarted
	// PATCH с новыми кредами, пока verify в полёте: credential_revision 1→2.
	patch := wrapRec(cabinetCall(t, r, http.MethodPatch,
		fmt.Sprintf("/api/me/hosting-connections/%d", id), sess, csrf,
		`{"expected_revision":1,"credentials":{"api_key":"k2"}}`))
	if patch.Code != http.StatusOK {
		t.Fatalf("patch mid-verify: %d %s", patch.Code, patch.Body)
	}
	close(fake.blockCheck)
	<-verifyDone

	if verifyRec.Code != http.StatusConflict || !strings.Contains(verifyRec.Body, "revision_conflict") {
		t.Fatalf("mid-verify rotation: %d %s", verifyRec.Code, verifyRec.Body)
	}
	c := decodeConns(t, wrapRec(cabinetCall(t, r, http.MethodGet, "/api/me/hosting-connections", sess, "", "")).Body)[0]
	if c.CredentialRevision != 2 || c.VerifiedAt != nil || c.Capabilities.Create || c.Capabilities.ChangeIP || c.Capabilities.Delete {
		t.Fatalf("state after conflict: %+v", c)
	}

	// Повторный verify по актуальным кредам — обычный успех 200.
	rec = wrapRec(cabinetCall(t, r, http.MethodPost,
		fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), sess, csrf, `{}`))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body, `"verified":true`) {
		t.Fatalf("re-verify: %d %s", rec.Code, rec.Body)
	}
}

// R-2/R-3: паника в провайдере не оставляет busy-запись и не wedжит
// singleflight — следующий refresh жив.
func TestHostingBalanceRefreshPanicSafety(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf, `{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`)
	id := decodeConn(t, rec.Body).ID

	fake.mu.Lock()
	fake.panicBalance = true
	fake.mu.Unlock()
	// В реальном сервере панику гасит net/http per-connection; здесь так же.
	func() {
		defer func() { _ = recover() }()
		hostingRefresh(t, r, sess, csrf, id)
	}()

	r.balMu.Lock()
	busy := len(r.balBusy)
	r.balMu.Unlock()
	if busy != 0 {
		t.Fatalf("balBusy leaked after panic: %d entries", busy)
	}

	fake.mu.Lock()
	fake.panicBalance = false
	fake.mu.Unlock()
	out := hostingRefresh(t, r, sess, csrf, id)
	if out.Code != http.StatusOK {
		t.Fatalf("refresh after panic: %d %s", out.Code, out.Body)
	}
	if c := decodeConn(t, out.Body); c.Balance.Freshness != "fresh" {
		t.Fatalf("refresh after panic: %+v", c.Balance)
	}
}

func TestHostingCredentialsNeverLeak(t *testing.T) {
	r, _ := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	const secret = "TOPSECRET-KEY-42"
	rec := hostingCreate(t, r, sess, csrf, fmt.Sprintf(`{"provider":"nodehost","label":"c","credentials":{"api_key":%q}}`, secret))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body, secret) {
		t.Fatalf("credentials leaked in create response: %s", rec.Body)
	}
	id := decodeConn(t, rec.Body).ID
	for _, call := range []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/api/me/hosting-connections", ""},
		{http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/balance/refresh", id), "{}"},
		{http.MethodPost, fmt.Sprintf("/api/me/hosting-connections/%d/verify", id), "{}"},
	} {
		out := wrapRec(cabinetCall(t, r, call.method, call.path, sess, csrf, call.body))
		if strings.Contains(out.Body, secret) {
			t.Fatalf("credentials leaked in %s %s: %s", call.method, call.path, out.Body)
		}
	}
}

// bareHosting — провайдер без каталога (как hostingBeget):catalog/estimate → 501.
type bareHosting struct{}

func (bareHosting) Check(context.Context) error { return nil }
func (bareHosting) Balance(context.Context) (hostingBalanceResult, error) {
	return hostingBalanceResult{Amount: "1", Currency: "USD", Scope: "x"}, nil
}

// hostingCatalogFixture — живой-shape каталог провайдера (2026-10-17).
func hostingCatalogFixture(withNetwork bool) hostingCatalogData {
	d := hostingCatalogData{
		Locations: []nodehost.Location{{Code: "DE", Name: "Германия"}},
		OS:        []nodehost.OSTemplate{{ID: 7, Name: "Debian 12"}},
		CPU:       []string{"7950X"},
		Disks:     []nodehost.DiskType{{ID: 1, Name: "Storage1"}},
		Limits: &nodehost.Limits{
			Types: map[string]bool{"VPS": true},
			Limits: struct {
				CPU  nodehost.Range `json:"cpu"`
				RAM  nodehost.Range `json:"ram"`
				Disk nodehost.Range `json:"disk"`
			}{CPU: nodehost.Range{Min: 1, Max: 24}, RAM: nodehost.Range{Min: 1, Max: 64}, Disk: nodehost.Range{Min: 10, Max: 1000}},
		},
	}
	if withNetwork {
		d.Network = &nodehost.NetworkOptions{Speeds: []string{"200 Mbps"}, HasAvailableNodes: true}
	}
	return d
}

func TestHostingCatalogNodehost(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	_, sessB, _ := hostingSession(t, r, "bob")
	fake.catalogData = hostingCatalogFixture(true)
	id := decodeConn(t, hostingCreate(t, r, sess, csrf,
		`{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`).Body).ID

	rec := wrapRec(cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/hosting-connections/%d/catalog", id), sess, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Provider  string `json:"provider"`
		CheckedAt int64  `json:"checked_at"`
		Locations []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"locations"`
		OS []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"os"`
		CPUModels []string `json:"cpu_models"`
		DiskTypes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"disk_types"`
		Limits struct {
			Cores struct {
				Min, Max int
			} `json:"cores"`
			RamGB struct {
				Min, Max int
			} `json:"ram_gb"`
			DiskGB struct {
				Min, Max int
			} `json:"disk_gb"`
			Types map[string]bool `json:"types"`
		} `json:"limits"`
		Network *struct {
			Speeds    []string `json:"speeds"`
			Available bool     `json:"available"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &out); err != nil {
		t.Fatalf("decode catalog: %v (%s)", err, rec.Body)
	}
	if out.Provider != "nodehost" || out.CheckedAt == 0 {
		t.Fatalf("catalog envelope: %+v", out)
	}
	if len(out.Locations) != 1 || out.Locations[0].Code != "DE" || out.Locations[0].Name != "Германия" {
		t.Fatalf("locations: %+v", out.Locations)
	}
	// id = имя ОС (оно уходит в CreateRequest.os).
	if len(out.OS) != 1 || out.OS[0].ID != "Debian 12" || out.OS[0].Name != "Debian 12" {
		t.Fatalf("os: %+v", out.OS)
	}
	if len(out.CPUModels) != 1 || out.CPUModels[0] != "7950X" {
		t.Fatalf("cpu: %+v", out.CPUModels)
	}
	if len(out.DiskTypes) != 1 || out.DiskTypes[0].ID != "Storage1" {
		t.Fatalf("disk types: %+v", out.DiskTypes)
	}
	if out.Limits.Cores.Min != 1 || out.Limits.Cores.Max != 24 ||
		out.Limits.RamGB.Max != 64 || out.Limits.DiskGB.Max != 1000 || !out.Limits.Types["VPS"] {
		t.Fatalf("limits: %+v", out.Limits)
	}
	if out.Network == nil || !out.Network.Available || len(out.Network.Speeds) != 1 {
		t.Fatalf("network: %+v", out.Network)
	}
	// Чужой каталог — 404.
	rec = wrapRec(cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/hosting-connections/%d/catalog", id), sessB, "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign catalog: %d %s", rec.Code, rec.Body)
	}
	// Мусорный id — 404.
	rec = wrapRec(cabinetCall(t, r, http.MethodGet, "/api/me/hosting-connections/abc/catalog", sess, "", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bad id catalog: %d %s", rec.Code, rec.Body)
	}
}

func TestHostingCatalogNetworkDegrades(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	fake.catalogData = hostingCatalogFixture(false)
	id := decodeConn(t, hostingCreate(t, r, sess, csrf,
		`{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`).Body).ID
	rec := wrapRec(cabinetCall(t, r, http.MethodGet,
		fmt.Sprintf("/api/me/hosting-connections/%d/catalog?location=DE", id), sess, "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("catalog: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body, `"network"`) {
		t.Fatalf("network must be omitted when unavailable: %s", rec.Body)
	}
}

func TestHostingCatalogBeget501(t *testing.T) {
	r, _ := newHostingRegistry(t)
	// Фабрика возвращает провайдер без hostingCataloger (как beget).
	r.hostingFactory = func(control.HostingConnection) (hostingProvider, error) { return bareHosting{}, nil }
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf,
		`{"provider":"beget","label":"b","credentials":{"cloud_login":"l","cloud_password":"p"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create beget: %d %s", rec.Code, rec.Body)
	}
	id := decodeConn(t, rec.Body).ID
	out := wrapRec(cabinetCall(t, r, http.MethodGet, fmt.Sprintf("/api/me/hosting-connections/%d/catalog", id), sess, "", ""))
	if out.Code != http.StatusNotImplemented || !strings.Contains(out.Body, "not_implemented") {
		t.Fatalf("beget catalog: %d %s", out.Code, out.Body)
	}
}

func TestHostingCatalogProviderErrors(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	id := decodeConn(t, hostingCreate(t, r, sess, csrf,
		`{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`).Body).ID
	for _, tc := range []struct {
		err  error
		code int
	}{
		{&nodehost.Error{Code: "request_canceled"}, http.StatusGatewayTimeout},
		{&nodehost.Error{Code: "provider_error"}, http.StatusBadGateway},
	} {
		fake.catalogErr = tc.err
		rec := wrapRec(cabinetCall(t, r, http.MethodGet,
			fmt.Sprintf("/api/me/hosting-connections/%d/catalog", id), sess, "", ""))
		if rec.Code != tc.code {
			t.Fatalf("catalog %v: %d %s", tc.err, rec.Code, rec.Body)
		}
	}
}

func TestCloudEstimate(t *testing.T) {
	r, fake := newHostingRegistry(t)
	_, sess, csrf := hostingSession(t, r, "alice")
	_, sessB, _ := hostingSession(t, r, "bob")
	fake.estResult = &nodehost.Estimate{
		MonthlyPrice: "2.12", PeriodPrice: "2.12", PeriodDays: 30, Currency: "USD",
	}
	id := decodeConn(t, hostingCreate(t, r, sess, csrf,
		`{"provider":"nodehost","label":"c","credentials":{"api_key":"k"}}`).Body).ID
	body := fmt.Sprintf(`{"connection_id":%d,"configuration":{"cores":2,"ram":2,"disk":20,"period":"1m","location":"DE"}}`, id)

	rec := wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/cloud-resources/estimate", sess, "", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("estimate: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Amount     string `json:"amount"`
		Currency   string `json:"currency"`
		Period     string `json:"period"`
		PeriodDays int    `json:"period_days"`
		CheckedAt  int64  `json:"checked_at"`
		Scope      string `json:"scope"`
	}
	if err := json.Unmarshal([]byte(rec.Body), &out); err != nil {
		t.Fatalf("decode estimate: %v (%s)", err, rec.Body)
	}
	if out.Amount != "2.12" || out.Currency != "USD" || out.Period != "1m" ||
		out.PeriodDays != 30 || out.CheckedAt == 0 || out.Scope != "nodehost_api_pricing" {
		t.Fatalf("estimate: %+v", out)
	}

	// Валидация: cores/ram/disk положительные, период из whitelist → 400.
	for _, bad := range []string{
		`{"connection_id":%d,"configuration":{"cores":0,"ram":2,"disk":20}}`,
		`{"connection_id":%d,"configuration":{"cores":2,"ram":-1,"disk":20}}`,
		`{"connection_id":%d,"configuration":{"cores":2,"ram":2,"disk":0}}`,
		`{"connection_id":%d,"configuration":{"cores":2,"ram":2,"disk":20,"period":"2w"}}`,
		`{"configuration":{"cores":2,"ram":2,"disk":20}}`,
	} {
		rec := wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/cloud-resources/estimate", sess, "",
			fmt.Sprintf(bad, id)))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body, "validation_failed") {
			t.Fatalf("estimate validation %s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	// Чужое подключение — 404.
	foreign := fmt.Sprintf(`{"connection_id":%d,"configuration":{"cores":2,"ram":2,"disk":20}}`, id)
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/cloud-resources/estimate", sessB, "", foreign))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign estimate: %d %s", rec.Code, rec.Body)
	}
	// Таймаут провайдера — 504.
	fake.estErr = &nodehost.Error{Code: "request_canceled"}
	rec = wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/cloud-resources/estimate", sess, "", body))
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("estimate timeout: %d %s", rec.Code, rec.Body)
	}
}

func TestCloudEstimateBeget501(t *testing.T) {
	r, _ := newHostingRegistry(t)
	r.hostingFactory = func(control.HostingConnection) (hostingProvider, error) { return bareHosting{}, nil }
	_, sess, csrf := hostingSession(t, r, "alice")
	rec := hostingCreate(t, r, sess, csrf,
		`{"provider":"beget","label":"b","credentials":{"cloud_login":"l","cloud_password":"p"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create beget: %d %s", rec.Code, rec.Body)
	}
	id := decodeConn(t, rec.Body).ID
	out := wrapRec(cabinetCall(t, r, http.MethodPost, "/api/me/cloud-resources/estimate", sess, "",
		fmt.Sprintf(`{"connection_id":%d,"configuration":{"cores":2,"ram":2,"disk":20}}`, id)))
	if out.Code != http.StatusNotImplemented || !strings.Contains(out.Body, "not_implemented") {
		t.Fatalf("beget estimate: %d %s", out.Code, out.Body)
	}
}
