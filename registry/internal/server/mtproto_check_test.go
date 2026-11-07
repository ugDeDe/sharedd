package server

import (
	"context"
	"sync/atomic"
	"testing"

	"sharedd/registry/internal/mtprotocheck"
)

// stubMTProto — подменённый сервис проверки с фиксированным статусом.
func stubMTProto(t *testing.T, r *Registry, status string, calls *atomic.Int32) {
	t.Helper()
	r.cfg.MTProtoCheck.Token = "rf_test"
	r.cfg.SharedProxy.TLSDomain = "front.example.com"
	r.cfg.SharedProxy.Users = map[string]string{"user1": "00112233445566778899aabbccddeeff"}
	old := mtprotoCheckFunc
	t.Cleanup(func() { mtprotoCheckFunc = old })
	mtprotoCheckFunc = func(_ context.Context, _, token, ip string, port int, secret string) (*mtprotocheck.Result, error) {
		calls.Add(1)
		if token != "rf_test" || port != 443 || secret != "ee00112233445566778899aabbccddeeff"+"66726f6e742e6578616d706c652e636f6d" {
			t.Errorf("unexpected check request: token=%q ip=%s port=%d secret=%s", token, ip, port, secret)
		}
		return &mtprotocheck.Result{Status: status}, nil
	}
}

// MT_DEAD — не блокировка (mtdd всегда так виден сервису): без карантина.
func TestMTProtoMTDeadIsNotBlock(t *testing.T) {
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	var calls atomic.Int32
	stubMTProto(t, r, mtprotocheck.StatusMTDead, &calls)
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	makeGreen(t, r, "node-one")
	sendReport(t, r, "node-one", true, true)
	if c := r.state.Candidates["node-one"]; c == nil || c.Quarantine != nil {
		t.Fatal("MT_DEAD must not count as a block")
	}
}

// Globalping провалена, MTProto OK — без карантина, нода остаётся доступной.
func TestMTProtoOKKeepsNodeOutOfQuarantine(t *testing.T) {
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	var calls atomic.Int32
	stubMTProto(t, r, mtprotocheck.StatusOK, &calls)
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	makeGreen(t, r, "node-one")
	sendReport(t, r, "node-one", true, true)
	c := r.state.Candidates["node-one"]
	if c == nil || c.Quarantine != nil || !c.GlobalpingOK {
		t.Fatalf("MTProto OK must keep the node out of quarantine: %+v", c)
	}
	if calls.Load() != 1 || c.MTProtoStatus != mtprotocheck.StatusOK || !c.MTProtoRescued {
		t.Fatalf("MTProto result must be recorded: calls=%d status=%q rescued=%v", calls.Load(), c.MTProtoStatus, c.MTProtoRescued)
	}
	rescued := 0
	for _, ev := range r.state.Events {
		if ev.Type == EventMTProtoRescued {
			rescued++
		}
	}
	sendReport(t, r, "node-one", true, true)
	for _, ev := range r.state.Events {
		if ev.Type == EventMTProtoRescued {
			rescued--
		}
	}
	if rescued != 0 {
		t.Fatal("mtproto_rescued is journaled once per streak")
	}
}

// Обе отрицательны — карантин, как раньше.
func TestBothNegativeEnterQuarantine(t *testing.T) {
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	var calls atomic.Int32
	stubMTProto(t, r, mtprotocheck.StatusRKNBlock, &calls)
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	makeGreen(t, r, "node-one")
	sendReport(t, r, "node-one", true, true)
	c := r.state.Candidates["node-one"]
	if c == nil || c.Quarantine == nil || c.MTProtoStatus != mtprotocheck.StatusRKNBlock || c.MTProtoRescued {
		t.Fatalf("both negative must quarantine: %+v", c)
	}
}

// Неоднозначный ответ MTProto — решает Globalping.
func TestMTProtoUnknownLeavesGlobalpingVerdict(t *testing.T) {
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	var calls atomic.Int32
	stubMTProto(t, r, mtprotocheck.StatusUnknown, &calls)
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	makeGreen(t, r, "node-one")
	sendReport(t, r, "node-one", true, true)
	if c := r.state.Candidates["node-one"]; c == nil || c.Quarantine == nil {
		t.Fatal("UNKNOWN must not rescue the node")
	}
}

// Без токена проверка не вызывается.
func TestMTProtoDisabledWithoutToken(t *testing.T) {
	mock := newGPMock(t)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	var calls atomic.Int32
	stubMTProto(t, r, mtprotocheck.StatusOK, &calls)
	r.cfg.MTProtoCheck.Token = ""
	r.register(registerRequest{NodeID: "node-one", IP: "1.1.1.1"})
	sendReport(t, r, "node-one", true, true)
	if calls.Load() != 0 {
		t.Fatal("no token — no MTProto calls")
	}
}

func TestMTProtoVerdict(t *testing.T) {
	for status, want := range map[string][2]bool{
		"OK": {true, true}, "DATA_CUT": {true, true}, "RKN_BLOCK": {false, true},
		"MT_DEAD": {true, true}, "DOWN": {true, true}, "UNKNOWN": {false, false}, "": {false, false},
	} {
		ok, conclusive := (&mtprotocheck.Result{Status: status}).Verdict()
		if ok != want[0] || conclusive != want[1] {
			t.Fatalf("%q: ok=%v conclusive=%v", status, ok, conclusive)
		}
	}
}

func TestMTProtoCheckPanelEditor(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	rec, _ := putConfig(t, r, `{"mtproto_check":{"enabled":true,"token":" rf_abc ","api_base":"https://api.example.org/"}}`, "")
	if rec.Code != 200 {
		t.Fatalf("PUT mtproto_check: %d %s", rec.Code, rec.Body.String())
	}
	if r.cfg.MTProtoCheck.Token != "rf_abc" || r.cfg.MTProtoCheck.APIBase != "https://api.example.org" ||
		r.cfg.MTProtoCheck.Enabled == nil || !*r.cfg.MTProtoCheck.Enabled {
		t.Fatalf("mtproto_check not applied: %+v", r.cfg.MTProtoCheck)
	}
	for _, body := range []string{
		`{"mtproto_check":{"enabled":true,"token":""}}`,
		`{"mtproto_check":{"enabled":false,"token":"a b"}}`,
		`{"mtproto_check":{"enabled":false,"token":"","api_base":"ftp://x"}}`,
	} {
		if rec, _ := putConfig(t, r, body, ""); rec.Code != 400 {
			t.Fatalf("invalid %s accepted: %d", body, rec.Code)
		}
	}
}
