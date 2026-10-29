package server

// Диагностика подключения Beget-аккаунта: маппинг кода провайдера в
// machine-readable reason + конкретное сообщение, 2FA-код доходит до Auth,
// пароль не попадает в лог.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"

	"sharedd/registry/internal/beget"
)

func TestBegetAuthFailureMapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantReason string
		wantSub    string
	}{
		{"incorrect creds", &beget.APIError{HTTPStatus: 200, Code: "INCORRECT_CREDENTIALS"}, "bad_credentials", "ЛОГИН"},
		{"code", &beget.APIError{HTTPStatus: 200, Code: "CODE_REQUIRED"}, "code_required", "Код"},
		{"totp", &beget.APIError{HTTPStatus: 200, Code: "TOTP_REQUIRED"}, "code_required", "Код"},
		{"2fa", &beget.APIError{HTTPStatus: 200, Code: "NEED_2FA"}, "code_required", "Код"},
		{"verify", &beget.APIError{HTTPStatus: 200, Code: "NEED_VERIFY"}, "code_required", "Код"},
		{"rate", &beget.APIError{HTTPStatus: 429, Code: "RATE_LIMIT"}, "rate_limited", "Подождите"},
		{"captcha", &beget.APIError{HTTPStatus: 200, Code: "CAPTCHA_REQUIRED"}, "rate_limited", "Подождите"},
		{"too many", &beget.APIError{HTTPStatus: 429, Code: "TOO_MANY_REQUESTS"}, "rate_limited", "Подождите"},
		// Реальный кейс Beget: троттлинг отдаёт пустое тело (без code) — статус
		// обязан сработать раньше code-веток и дать точный текст про 30 минут.
		{"429 empty body", &beget.APIError{HTTPStatus: 429}, "rate_limited", begetRateLimitedMessage},
		{"403 empty body", &beget.APIError{HTTPStatus: 403}, "rate_limited", begetRateLimitedMessage},
		// Статус важнее кода: 403 с чужим code всё равно троттлинг.
		{"403 wins over code", &beget.APIError{HTTPStatus: 403, Code: "INCORRECT_CREDENTIALS"}, "rate_limited", "30 минут"},
		// 200 + INCORRECT_CREDENTIALS не должен быть поглощён статусным правилом.
		{"200 incorrect not swallowed", &beget.APIError{HTTPStatus: 200, Code: "INCORRECT_CREDENTIALS", Message: "incorrect login or password"}, "bad_credentials", "ЛОГИН"},
		{"unknown keeps raw code", &beget.APIError{HTTPStatus: 418, Code: "SOME_WEIRD"}, "provider_error", "SOME_WEIRD"},
		{"message-only", &beget.APIError{HTTPStatus: 500, Message: "boom"}, "provider_error", "boom"},
		{"transport", errors.New("dial tcp: i/o timeout"), "provider_error", "связаться"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, msg := begetAuthFailure(tc.err)
			if reason != tc.wantReason || !strings.Contains(msg, tc.wantSub) {
				t.Fatalf("begetAuthFailure = (%q, %q), want reason %q containing %q", reason, msg, tc.wantReason, tc.wantSub)
			}
		})
	}
}

func TestBegetAuthFailureRateLimitNotMisreadAs2FA(t *testing.T) {
	// «LIMIT» не должен попасть в ветку CODE/TOTP/2FA/VERIFY.
	reason, _ := begetAuthFailure(&beget.APIError{HTTPStatus: 429, Code: "LIMIT_EXCEEDED"})
	if reason != "rate_limited" {
		t.Fatalf("LIMIT_EXCEEDED reason = %q, want rate_limited", reason)
	}
}

// Полный путь handler'а: провайдер вернул INCORRECT_CREDENTIALS → 400 с
// машиночитаемым reason, пароль не в логе, логин замаскирован.
func TestBegetLinkAuthFailureReasonAndNoPasswordLog(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	fake.authErr = &beget.APIError{HTTPStatus: 200, Code: "INCORRECT_CREDENTIALS", Message: "incorrect login or password"}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	const password = "s3cret-password-must-not-log"
	rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"login":"alice@beget","password":%q,"code":"123456"}`, password))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != "beget_auth_failed" || got.Reason != "bad_credentials" {
		t.Fatalf("response = %+v", got)
	}
	if !strings.Contains(got.Message, "ЛОГИН") {
		t.Fatalf("message must be actionable, got %q", got.Message)
	}
	// 2FA-код дошёл до Auth.
	if fake.lastAuthCode() != "123456" {
		t.Fatalf("auth code = %q, want 123456", fake.lastAuthCode())
	}
	logged := buf.String()
	if strings.Contains(logged, password) {
		t.Fatalf("password leaked in log: %q", logged)
	}
	if strings.Contains(logged, "alice@beget") {
		t.Fatalf("login must be masked in log: %q", logged)
	}
	if !strings.Contains(logged, "INCORRECT_CREDENTIALS") {
		t.Fatalf("log must carry provider code: %q", logged)
	}
}

// Троттлинг Beget: HTTP 429 с ПУСТЫМ телом → reason=rate_limited с точным
// текстом (не provider_error), пароль не в логе, статус залогирован.
func TestBegetLinkAuthRateLimitedEmptyBody(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	// Пустое тело: Code и Message не заполнены — именно это ломало маппинг.
	fake.authErr = &beget.APIError{HTTPStatus: 429}

	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(old) })

	const password = "s3cret-password-must-not-log"
	rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		fmt.Sprintf(`{"login":"alice@beget","password":%q}`, password))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error   string `json:"error"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error != "beget_auth_failed" || got.Reason != "rate_limited" {
		t.Fatalf("response = %+v, want beget_auth_failed/rate_limited", got)
	}
	if got.Message != begetRateLimitedMessage {
		t.Fatalf("message = %q, want %q", got.Message, begetRateLimitedMessage)
	}
	logged := buf.String()
	if strings.Contains(logged, password) {
		t.Fatalf("password leaked in log: %q", logged)
	}
	if !strings.Contains(logged, "status=429") {
		t.Fatalf("log must carry status: %q", logged)
	}
}

// Ответ указывает reason=code_required, чтобы UI подсветил поле 2FA.
func TestBegetLinkAuthCodeRequiredReason(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	fake.authErr = &beget.APIError{HTTPStatus: 200, Code: "CODE_REQUIRED"}

	rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"alice@beget","password":"p","code":"123456"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Reason != "code_required" {
		t.Fatalf("reason = %q, want code_required", got.Reason)
	}
}

// Пустой code не отправляется в Auth (сервер передаёт его как "").
func TestBegetLinkAuthCodeEmptyWhenOmitted(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	rec := cabinetCall(t, r, http.MethodPut, "/api/me/beget/link", cookieMap(sess, csrf), csrf,
		`{"login":"alice@beget","password":"p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("link: %d %s", rec.Code, rec.Body.String())
	}
	if fake.lastAuthCode() != "" {
		t.Fatalf("auth code = %q, want empty", fake.lastAuthCode())
	}
}
