package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Завершение ноды и самостоятельное возвращение в строй: разбор kill-ответа
// регистратора, retire-уведомление при локальном dead, режимы ожидания
// (смена IP после ip_ban, локальное оздоровление после dead).

// stubExitProcess — подмена «завершения процесса»: код exit пишется в канал.
// Вызывать ОДИН раз на тест до wait-стабов (последняя подмена выигрывает).
func stubExitProcess(t *testing.T) chan int {
	t.Helper()
	exited := make(chan int, 4)
	old := exitProcess
	exitProcess = func(code int) { exited <- code }
	t.Cleanup(func() { exitProcess = old })
	return exited
}

// stubIPBanWait — подмена ожидания смены IP после ip_ban: «смена» происходит
// мгновенно (возвращается newIP). Возвращает канал: waited — с каким
// забаненным IP вошли в ожидание.
func stubIPBanWait(t *testing.T, newIP string) chan string {
	t.Helper()
	waited := make(chan string, 4)

	oldWait := awaitIPChangeFn
	awaitIPChangeFn = func(banned string, _ *ipResolver) string {
		waited <- banned
		return newIP
	}
	oldOnce := ipBanOnce
	ipBanOnce = new(sync.Once)
	t.Cleanup(func() {
		awaitIPChangeFn = oldWait
		ipBanOnce = oldOnce
	})
	return waited
}

// stubDeadWait — подмена ожидания локального восстановления после dead:
// «оздоровление» происходит мгновенно. Канал: entered — факт входа в ожидание.
func stubDeadWait(t *testing.T) chan struct{} {
	t.Helper()
	entered := make(chan struct{}, 4)

	oldWait := awaitLocalRecoveryFn
	awaitLocalRecoveryFn = func(_ *NodeConfig) { entered <- struct{}{} }
	oldOnce := deadOnce
	deadOnce = new(sync.Once)
	t.Cleanup(func() {
		awaitLocalRecoveryFn = oldWait
		deadOnce = oldOnce
	})
	return entered
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

// Локальный dead-килл: до ухода в ожидание агент успевает сообщить /retire;
// после «восстановления» — самоперезапуск exit(1), служба не останавливается.
func TestSelfTerminateNotifiesRetire(t *testing.T) {
	nodeID = "node-testdead01"
	exited := stubExitProcess(t)
	entered := stubDeadWait(t)

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
	case code := <-exited:
		if code != 1 {
			t.Fatalf("after recovery want exit(1) for Restart=always, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("recovered dead must restart the agent, not stop the service")
	}
	select {
	case b := <-got:
		if b.NodeID != nodeID || b.IP != "5.6.7.8" || b.Reason != reasonDead {
			t.Fatalf("/retire payload mismatch: %+v", b)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("/retire must be posted before entering the recovery wait")
	}
	select {
	case <-entered:
	default:
		t.Fatal("dead must enter the local-recovery wait")
	}
}

// Рантайм ip_ban (kill-сигнал 403 посреди работы): служба НЕ останавливается,
// агент ждёт смену IP и после неё рестартует себя.
func TestSelfTerminateIPBanWaitsForIPChange(t *testing.T) {
	exited := stubExitProcess(t)
	waited := stubIPBanWait(t, "9.9.9.9")

	selfTerminate(nil, reasonIPBan, msgIPBan, "5.6.7.8")

	select {
	case banned := <-waited:
		if banned != "5.6.7.8" {
			t.Fatalf("waiting for the wrong IP: %q", banned)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runtime ip_ban must enter the await-IP-change mode")
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("want exit(1) for systemd Restart=always, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent must restart itself after the IP change")
	}
}

// dead не терминален: никакого останова службы и никакого ожидания смены IP —
// только ожидание локального оздоровления.
func TestSelfTerminateDeadWaitsForRecovery(t *testing.T) {
	exited := stubExitProcess(t)
	entered := stubDeadWait(t)
	ipWaited := stubIPBanWait(t, "9.9.9.9")

	selfTerminate(nil, reasonDead, msgDead, "5.6.7.8")

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("dead must enter the local-recovery wait")
	}
	select {
	case <-ipWaited:
		t.Fatal("dead must not wait for an IP change")
	default:
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("after recovery want exit(1), got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("after recovery the agent must restart itself")
	}
}

// awaitIPChange (настоящий, без стаба): пока echo-сервис отдаёт забаненный
// адрес — ждём; отдал новый — вернулись с ним.
func TestAwaitIPChangeReturnsOnNewIP(t *testing.T) {
	var mu sync.Mutex
	current := "5.6.7.8"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = w.Write([]byte(current))
	}))
	t.Cleanup(srv.Close)
	oldSvc := ipEchoServices
	ipEchoServices = []string{srv.URL}
	t.Cleanup(func() { ipEchoServices = oldSvc })

	oldPoll := ipBanPollInterval
	ipBanPollInterval = 30 * time.Millisecond
	t.Cleanup(func() { ipBanPollInterval = oldPoll })

	got := make(chan string, 1)
	go func() { got <- awaitIPChange("5.6.7.8", newIPResolver()) }()

	select {
	case ip := <-got:
		t.Fatalf("returned while the IP is still banned: %q", ip)
	case <-time.After(150 * time.Millisecond):
	}

	mu.Lock()
	current = "9.10.11.12"
	mu.Unlock()

	select {
	case ip := <-got:
		if ip != "9.10.11.12" {
			t.Fatalf("wrong new IP: %q", ip)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("awaitIPChange must return once the public IP differs from the banned one")
	}
}

// Параллельные 403 из нескольких циклов (heartbeat + globalping + metrics
// могут словить kill одновременно) → ожидание входит ровно один раз
// (ip_ban).
func TestIPBanRecoverSingleFlight(t *testing.T) {
	exited := stubExitProcess(t)
	waited := stubIPBanWait(t, "9.9.9.9")

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selfTerminate(nil, reasonIPBan, msgIPBan, "5.6.7.8")
		}()
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("no await entered")
	}
	<-exited
	wg.Wait()
	if len(waited) != 0 {
		t.Fatalf("await-IP-change entered %d extra times — must be single-flight", len(waited))
	}
}

// То же для dead: параллельные dead-верdictы входят в ожидание один раз.
func TestDeadRecoverSingleFlight(t *testing.T) {
	exited := stubExitProcess(t)
	entered := stubDeadWait(t)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selfTerminate(nil, reasonDead, msgDead, "5.6.7.8")
		}()
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("no recovery wait entered")
	}
	<-exited
	wg.Wait()
	if len(entered) != 0 {
		t.Fatalf("recovery wait entered %d extra times — must be single-flight", len(entered))
	}
}
