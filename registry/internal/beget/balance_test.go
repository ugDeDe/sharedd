package beget

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBalanceFormBodyNotQuery(t *testing.T) {
	var gotQuery string
	var gotLogin, gotPasswd, gotFormat string
	var gotContentType string
	c := NewBalanceClient("my-login", "my-pass")
	c.Base = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotContentType = r.Header.Get("Content-Type")
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		// PostForm — только тело; query не должен участвовать.
		gotLogin = r.PostFormValue("login")
		gotPasswd = r.PostFormValue("passwd")
		gotFormat = r.PostFormValue("output_format")
		_, _ = w.Write([]byte(`{"status":"success","answer":{"status":"success","result":{"user_balance":1339.57}}}`))
	}))
	t.Cleanup(srv.Close)
	c.HC = srv.Client()
	c.Base = srv.URL
	b, err := c.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if gotQuery != "" {
		t.Errorf("credentials leaked into query string: %q", gotQuery)
	}
	if gotContentType != "application/x-www-form-urlencoded" {
		t.Errorf("content type %q", gotContentType)
	}
	if gotLogin != "my-login" || gotPasswd != "my-pass" || gotFormat != "json" {
		t.Errorf("form body: login=%q passwd=%q format=%q", gotLogin, gotPasswd, gotFormat)
	}
	if b.Amount != "1339.57" || b.Currency != "" || b.Scope != "beget_hosting_unverified" {
		t.Errorf("balance %+v", b)
	}
}

func TestBalanceSuccess(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"success","result":{"user_balance":1339.57}}}`)
	b, err := c.Balance(context.Background())
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if b.Amount != "1339.57" {
		t.Errorf("amount %q", b.Amount)
	}
}

func TestBalanceStringDecimalAndPrecision(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"success","result":{"user_balance":"142.50"}}}`)
	b, err := c.Balance(context.Background())
	if err != nil || b.Amount != "142.50" {
		t.Fatalf("string decimal: %v %+v", err, b)
	}

	raw := `{"status":"success","answer":{"status":"success","result":{"user_balance":0.1234567890123456789012345678901234567890}}}`
	c2 := NewBalanceClient("l", "p")
	c2.Base = testBalanceServer(t, raw)
	b2, err := c2.Balance(context.Background())
	if err != nil {
		t.Fatalf("precision: %v", err)
	}
	want := "0.1234567890123456789012345678901234567890"
	if b2.Amount != want {
		t.Errorf("precision lost: %q", b2.Amount)
	}
}

func TestBalanceOuterError(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `{"status":"error","error_code":"ERR_LOGIN","error_text":"bad login"}`)
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "ERR_LOGIN" || be.Message != "bad login" {
		t.Fatalf("outer error: %v", err)
	}
	if strings.Contains(err.Error(), "my-login") || strings.Contains(err.Error(), "my-pass") {
		t.Errorf("credentials leaked: %v", err)
	}
}

func TestBalanceInnerErrors(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"error","errors":[{"error_code":"E_FIRST","error_text":"first"},{"error_code":"E_SECOND","error_text":"second"}]}}`)
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "E_FIRST" || be.Message != "first" {
		t.Fatalf("inner error: %v", err)
	}
}

func TestBalanceMissingUserBalance(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"success","result":{}}}`)
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "invalid_balance" {
		t.Fatalf("missing balance: %v", err)
	}

	c2 := NewBalanceClient("l", "p")
	c2.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"success","result":{"user_balance":"abc"}}}`)
	_, err = c2.Balance(context.Background())
	if !errors.As(err, &be) || be.Code != "invalid_balance" {
		t.Fatalf("non-numeric balance: %v", err)
	}

	c3 := NewBalanceClient("l", "p")
	c3.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"success","result":{"user_balance":""}}}`)
	_, err = c3.Balance(context.Background())
	if !errors.As(err, &be) || be.Code != "invalid_balance" {
		t.Fatalf("empty balance: %v", err)
	}
}

func TestBalanceTimeout(t *testing.T) {
	c := NewBalanceClient("l", "p")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	t.Cleanup(srv.Close)
	c.Base = srv.URL
	c.HC = &http.Client{Timeout: 50 * time.Millisecond}
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "transport_error" {
		t.Fatalf("timeout: %v", err)
	}
}

func TestBalanceOversized(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, strings.Repeat("x", maxBalanceBody+1))
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "response_too_large" {
		t.Fatalf("oversized: %v", err)
	}
}

func TestBalanceCredentialsMissing(t *testing.T) {
	_, err := NewBalanceClient("", "p").Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "balance_credentials_missing" {
		t.Fatalf("missing login: %v", err)
	}
	_, err = NewBalanceClient("l", "").Balance(context.Background())
	if !errors.As(err, &be) || be.Code != "balance_credentials_missing" {
		t.Fatalf("missing password: %v", err)
	}
}

func TestBalanceInvalidEnvelope(t *testing.T) {
	c := NewBalanceClient("l", "p")
	c.Base = testBalanceServer(t, `not json at all`)
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "invalid_response" {
		t.Fatalf("invalid json: %v", err)
	}
}

func TestBalanceRedirectNotFollowed(t *testing.T) {
	secondHit := false
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondHit = true
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	t.Cleanup(second.Close)
	c := NewBalanceClient("my-login", "my-pass")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", second.URL+"/api/user/getAccountInfo")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c.HC = srv.Client()
	c.Base = srv.URL
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) || be.Code != "invalid_response" {
		t.Fatalf("redirect: %v", err)
	}
	if be.HTTPStatus != 307 {
		t.Fatalf("want last response 307, got %d", be.HTTPStatus)
	}
	if secondHit {
		t.Error("redirect target must not be requested (no credential forward)")
	}
}

func TestBalanceErrorMasksCredentials(t *testing.T) {
	c := NewBalanceClient("my-login", "my-pass")
	c.Base = testBalanceServer(t, `{"status":"error","error_code":"ERR_AUTH","error_text":"user my-login not found, passwd my-pass rejected"}`)
	_, err := c.Balance(context.Background())
	var be *BalanceError
	if !errors.As(err, &be) {
		t.Fatalf("outer error: %v", err)
	}
	if strings.Contains(be.Message, "my-login") || strings.Contains(be.Message, "my-pass") {
		t.Fatalf("credentials leaked into Message: %q", be.Message)
	}
	if !strings.Contains(be.Message, "***") {
		t.Fatalf("masking not applied: %q", be.Message)
	}

	c2 := NewBalanceClient("my-login", "my-pass")
	c2.Base = testBalanceServer(t, `{"status":"success","answer":{"status":"error","errors":[{"error_code":"E_AUTH","error_text":"user my-login not found"}]}}`)
	_, err = c2.Balance(context.Background())
	if !errors.As(err, &be) || strings.Contains(be.Message, "my-login") {
		t.Fatalf("inner error masking: %v", err)
	}
}

// testBalanceServer — фиксированный ответ; HC клиента уже привязан к серверу.
func testBalanceServer(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
