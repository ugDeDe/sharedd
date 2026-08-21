package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestTombstoneMode(t *testing.T) {
	old := tombstonePath
	tombstonePath = filepath.Join(t.TempDir(), "terminated.json")
	defer func() { tombstonePath = old }()
	if err := os.WriteFile(tombstonePath, []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	writeTombstone(termination{Reason: reasonDead, IP: "1.1.1.1"})
	info, err := os.Stat(tombstonePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("tombstone mode = %o, want 0600", info.Mode().Perm())
	}
}

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

// stubIPBanWait — подмена ожидания смены IP после ip_ban: «смена» происходит
// мгновенно (возвращается newIP), exitProcess пишется в канал. Возвращает
// каналы: waited — с каким забаненным IP вошли в ожидание, exited — код exit.
func stubIPBanWait(t *testing.T, newIP string) (chan string, chan int) {
	t.Helper()
	waited := make(chan string, 4)
	exited := make(chan int, 4)

	oldWait := awaitIPChangeFn
	awaitIPChangeFn = func(banned string, _ *ipResolver) string {
		waited <- banned
		return newIP
	}
	oldExit := exitProcess
	exitProcess = func(code int) { exited <- code }
	oldOnce := ipBanOnce
	ipBanOnce = new(sync.Once)
	t.Cleanup(func() {
		awaitIPChangeFn = oldWait
		exitProcess = oldExit
		ipBanOnce = oldOnce
	})
	return waited, exited
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
// служба НЕ умирает: уходит в ожидание смены IP, после «смены» — рестарт
// (exit(1) под Restart=always), tombstone к этому моменту стёрт.
func TestBootCheckIPBanSameIPReverifyDenied(t *testing.T) {
	p := useTempTombstone(t)
	died := stubExit(t)
	waited, exited := stubIPBanWait(t, "9.9.9.9")
	cfg, client := bootReverifyEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"terminate":true,"reason":"ip_ban","message":"` + msgIPBan + `"}`))
	})
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(cfg, newIPResolver(), client)
	select {
	case msg := <-died:
		t.Fatalf("ip_ban must NOT stop the service anymore, died with %q", msg)
	default:
	}
	select {
	case banned := <-waited:
		if banned != "5.6.7.8" {
			t.Fatalf("must wait for a change of the banned IP, got %q", banned)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("denied re-verify must enter the await-IP-change mode")
	}
	select {
	case code := <-exited:
		if code != 1 {
			t.Fatalf("restart wants exit(1) for Restart=always, got %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("after the IP change the agent must restart itself")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("tombstone must be cleared before the self-restart")
	}
}

// регистратор недоступен — перепроверка не состоялась → НЕ умираем, ждём
// смену IP (как при отказе в reverify).
func TestBootCheckIPBanSameIPRegistryDown(t *testing.T) {
	useTempTombstone(t)
	died := stubExit(t)
	waited, _ := stubIPBanWait(t, "9.9.9.9")
	cfg, client := bootReverifyEnv(t, nil)
	writeTombstone(termination{Reason: reasonIPBan, IP: "5.6.7.8"})
	checkTerminationTombstone(cfg, newIPResolver(), client)
	select {
	case msg := <-died:
		t.Fatalf("unreachable registry must lead to await-IP-change, not death; died with %q", msg)
	default:
	}
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("unreachable registry must enter the await-IP-change mode")
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

// Рантайм ip_ban (kill-сигнал 403 посреди работы — ровно кейс из жалобы:
// «heartbeat rejected 410 → register → 403 ip_ban»): служба НЕ
// останавливается, tombstone пишется, агент ждёт смену IP и после неё
// рестартует себя с уже СТЁРТЫМ tombstone'ом.
func TestSelfTerminateIPBanWaitsForIPChange(t *testing.T) {
	p := useTempTombstone(t)
	died := stubExit(t)
	waited, exited := stubIPBanWait(t, "9.9.9.9")

	selfTerminate(nil, reasonIPBan, msgIPBan, "5.6.7.8")

	select {
	case msg := <-died:
		t.Fatalf("ip_ban must NOT stop the service (was the old behaviour), died with %q", msg)
	default:
	}
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
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("tombstone must be cleared before the self-restart")
	}
}

// dead остаётся терминальным: селф-рестартов и ожиданий нет, служба
// останавливается как раньше.
func TestSelfTerminateDeadStillStops(t *testing.T) {
	useTempTombstone(t)
	died := stubExit(t)
	waited, _ := stubIPBanWait(t, "9.9.9.9")

	selfTerminate(nil, reasonDead, msgDead, "5.6.7.8")

	select {
	case msg := <-died:
		if msg != msgDead {
			t.Fatalf("wrong die message: %q", msg)
		}
	default:
		t.Fatal("dead must stop the service as before")
	}
	select {
	case <-waited:
		t.Fatal("dead must not wait for an IP change")
	default:
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
		w.Write([]byte(current))
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
// могут словить kill одновременно) → ожидание входит ровно один раз.
func TestIPBanRecoverSingleFlight(t *testing.T) {
	useTempTombstone(t)
	stubExit(t)
	waited, exited := stubIPBanWait(t, "9.9.9.9")

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
