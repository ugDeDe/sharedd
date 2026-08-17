package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Терминальное завершение ноды — tombstone, boot-перевалидация,
// разбор kill-ответа регистратора, retire-уведомление при локальном dead.

// stubExit подменяет «останов службы» на запись сообщения; возвращает
// канал, в который упадёт текст при попытке завершения.
func stubExit(t *testing.T) chan string {
	t.Helper()
	ch := make(chan string, 4)
	old := dieSelfFn
	dieSelfFn = func(msg string) { ch <- msg }
	t.Cleanup(func() { dieSelfFn = old })
	return ch
}

func useTempTombstone(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "terminated.json")
	old := tombstonePath
	tombstonePath = p
	t.Cleanup(func() { tombstonePath = old })
	return p
}

func TestParseTerminateBody(t *testing.T) {
	te, ok := parseTerminateBody([]byte(`{"terminate":true,"reason":"ip_ban","message":"Бан по ip"}`))
	if !ok || te.Reason != "ip_ban" || te.Message == "" {
		t.Fatalf("valid terminate body rejected: %+v, ok=%v", te, ok)
	}
	for _, bad := range []string{
		`{"terminate":false,"reason":"dead"}`,
		`{"terminate":true}`,
		`not json at all`,
		`{}`,
	} {
		if _, ok := parseTerminateBody([]byte(bad)); ok {
			t.Fatalf("non-terminate body parsed as terminate: %s", bad)
		}
	}
}

func TestTombstoneRoundTrip(t *testing.T) {
	p := useTempTombstone(t)
	if readTombstone() != nil {
		t.Fatal("missing file must read as nil")
	}
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	tm := readTombstone()
	if tm == nil || tm.Reason != reasonIPBan || tm.IP != "5.6.7.8" || tm.At.IsZero() {
		t.Fatalf("tombstone does not round-trip: %+v", tm)
	}
	clearTombstone()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("clearTombstone must remove the file")
	}
	if readTombstone() != nil {
		t.Fatal("after clear, tombstone must be nil")
	}
}

// boot-кейсы ip_ban с НЕИЗМЕННЫМ ip: нода просит у регистратора
// GP-перепроверку. Регистратор решил — принял в reverify (200) или убил
// (403); недоступный регистратор = перепроверка не состоялась → умираем.
func bootReverifyEnv(t *testing.T, regHandler http.HandlerFunc) (*NodeConfig, *http.Client) {
	t.Helper()
	isrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("5.6.7.8")) // текущий публичный ip == забаненному
	}))
	t.Cleanup(isrv.Close)
	oldSvc := ipEchoServices
	ipEchoServices = []string{isrv.URL}
	t.Cleanup(func() { ipEchoServices = oldSvc })
	cfg := &NodeConfig{}
	if regHandler != nil {
		rsrv := httptest.NewServer(regHandler)
		t.Cleanup(rsrv.Close)
		cfg.Registry.URL = rsrv.URL
	} else {
		cfg.Registry.URL = "http://127.0.0.1:1" // ничего не слушает
	}
	nodeID = "node-bootrv01"
	return cfg, &http.Client{Timeout: 2 * time.Second}
}

// регистратор ответил 200 (открыл reverify) → tombstone стёрт, живём.
func TestBootCheckIPBanSameIPReverifyAccepted(t *testing.T) {
	p := useTempTombstone(t)
	died := stubExit(t)
	cfg, client := bootReverifyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/register" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(cfg, newIPResolver(), client)
	select {
	case msg := <-died:
		t.Fatalf("node must NOT die when re-verify is opened, died with %q", msg)
	default:
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("tombstone must be cleared once re-verify is opened")
	}
}

// регистратор ответил 403 terminate (перепроверка запрещена/провалена) →
// смерть с дословным сообщением, tombstone остаётся.
func TestBootCheckIPBanSameIPReverifyDenied(t *testing.T) {
	useTempTombstone(t)
	died := stubExit(t)
	cfg, client := bootReverifyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"terminate":true,"reason":"ip_ban","message":"` + msgIPBan + `"}`))
	})
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(cfg, newIPResolver(), client)
	select {
	case msg := <-died:
		if msg != msgIPBan {
			t.Fatalf("wrong die message: %q (want exact TZ line)", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("403 on re-verify must terminate the node")
	}
	if tm := readTombstone(); tm == nil {
		t.Fatal("tombstone must stay in place when re-verify is denied")
	}
}

// регистратор недоступен — перепроверка не состоялась → смерть (как).
func TestBootCheckIPBanSameIPRegistryDown(t *testing.T) {
	useTempTombstone(t)
	died := stubExit(t)
	cfg, client := bootReverifyEnv(t, nil)
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(cfg, newIPResolver(), client)
	select {
	case msg := <-died:
		if msg != msgIPBan {
			t.Fatalf("wrong die message: %q", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("unreachable registry must NOT resurrect the node")
	}
}

// ip_ban + IP сменился → tombstone стирается, нода живёт дальше.
func TestBootCheckIPBanLiftedOnIPChange(t *testing.T) {
	p := useTempTombstone(t)
	died := stubExit(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("9.10.11.12"))
	}))
	t.Cleanup(srv.Close)
	oldSvc := ipEchoServices
	ipEchoServices = []string{srv.URL}
	t.Cleanup(func() { ipEchoServices = oldSvc })

	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(nil, newIPResolver(), nil) // регистратор не нужен: ip сменился
	select {
	case msg := <-died:
		t.Fatalf("node must NOT die after ip change, died with %q", msg)
	default:
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("tombstone must be cleared after the ip change")
	}
}

// dead — нода мертва навсегда: boot не перевалидирует, умирает сразу,
// даже если локально снова зелёно (воскрешение — только ручное).
func TestBootCheckDeadDiesImmediately(t *testing.T) {
	p := useTempTombstone(t)
	died := stubExit(t)

	writeTombstone(termination{Reason: reasonDead, IP: "5.6.7.8"})
	checkTerminationTombstone(nil, newIPResolver(), nil) // dead никогда не спрашивает регистратор
	select {
	case msg := <-died:
		if msg != msgDead {
			t.Fatalf("wrong die message: %q (want exact TZ line)", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("dead tombstone must terminate the node at once")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("tombstone must stay in place when we refuse to resurrect")
	}
}

// Локальный dead-килл: до смерти агент успевает сообщить /retire.
func TestSelfTerminateNotifiesRetire(t *testing.T) {
	useTempTombstone(t)
	died := stubExit(t)
	nodeID = "node-testdead01"

	type retireBody struct {
		NodeID string `json:"node_id"`
		IP     string `json:"ip"`
		Reason string `json:"reason"`
	}
	got := make(chan retireBody, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retire" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data, _ := io.ReadAll(r.Body)
		var b retireBody
		if err := json.Unmarshal(data, &b); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got <- b
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	cfg := &NodeConfig{}
	cfg.Registry.URL = srv.URL
	selfTerminate(cfg, reasonDead, msgDead, "5.6.7.8")

	select {
	case msg := <-died:
		if msg != msgDead {
			t.Fatalf("wrong die message: %q", msg)
		}
	default:
		t.Fatal("selfTerminate must end the service")
	}
	select {
	case b := <-got:
		if b.NodeID != nodeID || b.IP != "5.6.7.8" || b.Reason != reasonDead {
			t.Fatalf("/retire payload mismatch: %+v", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/retire must be posted before dying")
	}
	tm := readTombstone()
	if tm == nil || tm.Reason != reasonDead || tm.IP != "5.6.7.8" {
		t.Fatalf("tombstone missing after selfTerminate: %+v", tm)
	}
}
