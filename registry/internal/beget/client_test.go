package beget

// Деньги-критичные тесты клиента: ровно один POST на create, отсутствие
// авто-повтора неидемпотентного POST при 401, ошибка Remove не «успех»,
// секреты не утекают в текст ошибок.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func newClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{Base: srv.URL, HC: srv.Client(), Login: "user", Password: "supersecret"}, srv
}

func TestCreateVpsSingleCallNoRetryOn401(t *testing.T) {
	var calls int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "UNAUTHORIZED", "message": "bad token"}})
	})
	_, err := c.CreateVps(context.Background(), CreateVpsRequest{Region: "ru-1"})
	if err == nil {
		t.Fatal("want error")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("CreateVps HTTP calls = %d, want 1 (paid POST must never auto-retry)", n)
	}
}

func TestGetListReauthsOn401(t *testing.T) {
	var listCalls int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth" {
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
			return
		}
		n := atomic.AddInt32(&listCalls, 1)
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if n > 2 {
			t.Errorf("unexpected extra retry")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"vps": []map[string]string{{"id": "1"}}})
	})
	out, err := c.GetList(context.Background())
	if err != nil || out.TotalCount != 0 || len(out.Vps) != 1 {
		t.Fatalf("GetList = %+v err=%v", out, err)
	}
}

func TestCreateVpsSuccessSingleCall(t *testing.T) {
	var calls int32
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_ = json.NewEncoder(w).Encode(map[string]any{"vps": map[string]any{"id": "vps-1", "hostname": "sharedd-x"}})
	})
	out, err := c.CreateVps(context.Background(), CreateVpsRequest{Region: "ru-1"})
	if err != nil || out.Vps == nil || out.Vps.ID != "vps-1" {
		t.Fatalf("create = %+v err=%v", out, err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestRemoveErrorSurfaced(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "BUSY", "message": "try later"}})
	})
	err := c.Remove(context.Background(), "vps-1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "BUSY" {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthErrorDoesNotLeakPassword(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "INCORRECT_CREDENTIALS", "message": "incorrect login or password"}})
	})
	err := c.Auth(context.Background(), "")
	if err == nil {
		t.Fatal("want auth error")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("error leaks password: %v", err)
	}
}

// Code уходит в тело только когда непустой; пустой — поле отсутствует.
func TestAuthCodePassthroughOnlyWhenNonEmpty(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		wantField bool
	}{
		{"empty omits code", "", false},
		{"non-empty sends code", "123456", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got map[string]any
			c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_ = json.NewEncoder(w).Encode(map[string]string{"token": "tok"})
			})
			if err := c.Auth(context.Background(), tc.code); err != nil {
				t.Fatalf("Auth: %v", err)
			}
			v, has := got["code"]
			if has != tc.wantField {
				t.Fatalf("code present=%v, want %v (body=%v)", has, tc.wantField, got)
			}
			if tc.wantField && v != tc.code {
				t.Fatalf("code = %v, want %q", v, tc.code)
			}
		})
	}
}

// Строковая форма ошибки `{"error":"CODE"}` — это код; сохраняется как
// APIError.Code (иначе кабинет не сможет смаппить причину).
func TestAuthStringErrorCodeSurvives(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "INCORRECT_CREDENTIALS"})
	})
	err := c.Auth(context.Background(), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "INCORRECT_CREDENTIALS" {
		t.Fatalf("string error must survive as APIError.Code, got %#v (%v)", apiErr, err)
	}
}

func TestAPIErrorIsValidation(t *testing.T) {
	if !(&APIError{HTTPStatus: 400}).IsValidation() {
		t.Fatal("400 must be validation")
	}
	if (&APIError{HTTPStatus: 500}).IsValidation() {
		t.Fatal("500 must not be validation")
	}
}
