package server

// has_vps в operation JSON: кабинет не должен предлагать «повторить установку»
// для операций без созданного VPS. Флаг выводится из provider_vps_id, но сам
// provider_vps_id (провайдер-внутренний id) наружу не отдаётся.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"sharedd/registry/internal/control"
)

func operationsJSON(t *testing.T, r *Registry, sess, csrf string) []operationJSON {
	t.Helper()
	rec := cabinetCall(t, r, http.MethodGet, "/api/me/operations", cookieMap(sess, csrf), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("operations: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Operations []operationJSON `json:"operations"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	return list.Operations
}

// Подтверждённое создание: has_vps=true во всех трёх эндпоинтах; сырой
// provider_vps_id не появляется в user-facing JSON.
func TestHasVPSAfterConfirmedCreate(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"idem-hasvps","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	if !op.HasVPS {
		t.Fatalf("POST /api/me/vps after confirmed create: has_vps=false, want true")
	}
	// сырой провайдер-внутренний id не утекает.
	if strings.Contains(rec.Body.String(), "provider_vps_id") || strings.Contains(rec.Body.String(), "beget-vps-123") {
		t.Fatalf("create JSON leaked provider_vps_id: %s", rec.Body.String())
	}

	// GET /api/me/operations.
	ops := operationsJSON(t, r, sess, csrf)
	if len(ops) == 0 || !ops[0].HasVPS {
		t.Fatalf("operations list: has_vps=false, want true: %+v", ops)
	}
	rec = cabinetCall(t, r, http.MethodGet, "/api/me/operations", cookieMap(sess, csrf), "", "")
	if strings.Contains(rec.Body.String(), "provider_vps_id") {
		t.Fatalf("operations JSON leaked provider_vps_id: %s", rec.Body.String())
	}

	// POST .../retry-install после провала установки (VPS уже создан).
	if err := r.control.UpdateOperationState(op.ID, control.OpFailed, "", "install boom"); err != nil {
		t.Fatal(err)
	}
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/operations/"+strconv.FormatInt(op.ID, 10)+"/retry-install",
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("retry-install: %d %s", rec.Code, rec.Body.String())
	}
	var retried operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &retried)
	if !retried.HasVPS {
		t.Fatalf("retry-install: has_vps=false, want true")
	}
	if strings.Contains(rec.Body.String(), "provider_vps_id") || strings.Contains(rec.Body.String(), "beget-vps-123") {
		t.Fatalf("retry JSON leaked provider_vps_id: %s", rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("retry must not issue a paid call, CreateVps=%d", fake.creates())
	}
}

// Операция без созданного VPS (платный вызов упал): has_vps=false, сырой id
// не отдаётся, retry-install отклоняется как «нечего повторять».
func TestHasVPSFalseWithoutVps(t *testing.T) {
	r, fake := newCabinetRegistry(t)
	fake.createErr = errors.New("provider down")
	cabinetUser(t, r, "alice", 5)
	sess, csrf := cabinetLogin(t, r, "alice", "password-123")
	linkBeget(t, r, sess, csrf)

	rec := cabinetCall(t, r, http.MethodPost, "/api/me/vps", cookieMap(sess, csrf), csrf,
		`{"region":"ru1","idem_key":"idem-novps","confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vps: %d %s", rec.Code, rec.Body.String())
	}
	var op operationJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &op)
	if op.HasVPS {
		t.Fatalf("operation without VPS: has_vps=true, want false")
	}
	if strings.Contains(rec.Body.String(), "provider_vps_id") {
		t.Fatalf("create JSON leaked provider_vps_id: %s", rec.Body.String())
	}

	ops := operationsJSON(t, r, sess, csrf)
	if len(ops) == 0 || ops[0].HasVPS {
		t.Fatalf("operations list: has_vps=true, want false: %+v", ops)
	}

	// нечего повторять → отказ, платный вызов не делается.
	rec = cabinetCall(t, r, http.MethodPost, "/api/me/operations/"+strconv.FormatInt(op.ID, 10)+"/retry-install",
		cookieMap(sess, csrf), csrf, `{}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "no_vps") {
		t.Fatalf("retry without VPS: want 400 no_vps, got %d %s", rec.Code, rec.Body.String())
	}
	if fake.creates() != 1 {
		t.Fatalf("CreateVps=%d, want 1 (only the failed attempt)", fake.creates())
	}
}
