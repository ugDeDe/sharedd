package main

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// V7.9.11: терминальные классы — GP-карантин («всё зелёное, кроме GP» →
// N попыток → бан по IP навсегда), dead-класс (TCP+метрики молчат >10 мин),
// доставка kill-сигнала, снятие ip_ban при смене IP, /retire.

// gpMock — фейк Globalping API с переключаемым ratio (0.25 = fail, 1.0 = ok).
type gpMock struct {
	*httptest.Server
	bad *bool
}

func newGPMock(t *testing.T) *gpMock {
	bad := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// bad: 1/4 пробы прошло → ratio 0.25 < 0.5 → verified fail;
		// good: 4/4 → ratio 1.0 → verified ok.
		mk := func(ok bool) map[string]any {
			s, c := "finished", 200
			if !ok {
				s, c = "failed", 0
			}
			return map[string]any{"result": map[string]any{"status": s, "statusCode": c}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "m-test",
			"status": "finished",
			"results": []map[string]any{
				mk(true), mk(!bad), mk(!bad), mk(!bad),
			},
		})
	}))
	t.Cleanup(srv.Close)
	return &gpMock{Server: srv, bad: &bad}
}

func (m *gpMock) setBad(v bool) { *m.bad = v }

// sendReport — прогон отчёта через боевой хендлер; вердикт метрик + опц. GP.
func sendReport(t *testing.T, r *Registry, id string, metricsOK bool, withGP bool) *httptest.ResponseRecorder {
	t.Helper()
	payload := HealthReportPayload{
		NodeID: id, IP: r.state.Candidates[id].IP, Port: 443,
		MetricsOK: metricsOK, CheckedAt: time.Now(),
	}
	if withGP {
		payload.GlobalpingOK = true
		payload.GlobalpingMeasurementID = "m-test"
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/report", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	r.handleHealthReport(rec, req)
	return rec
}

// makeGreen — довести ноду до «всё зелёное кроме GP»: metrics-защёлку
// recover-порогом отчётов, TCP-защёлку ставим напрямую (probeLoop в тестах
// не ходит — сетевой сокет фиктивный).
func makeGreen(t *testing.T, r *Registry, id string) {
	t.Helper()
	sendReport(t, r, id, true, false)
	sendReport(t, r, id, true, false)
	r.mu.Lock()
	c := r.state.Candidates[id]
	if !c.MetricsHealthy {
		r.mu.Unlock()
		t.Fatalf("%s: metrics latch must hold after %d consecutive ok reports", id, r.cfg.Healthcheck.RecoverThreshold)
	}
	c.Healthy = true
	c.GlobalpingOK = true
	r.mu.Unlock()
}

func newTerminateTestRegistry(t *testing.T) *Registry {
	r := newTestRegistry(t)
	r.cfg.QuarantineAttempts = 3
	r.db = openHistoryDB(filepath.Join(t.TempDir(), "hist.db"))
	if r.db == nil {
		t.Fatal("test history db must open")
	}
	t.Cleanup(r.db.Close)
	return r
}

// giveMasterTime — нода успела поработать мастером (закрытый stint):
// V7.9.14 — только тогда её бан попадает в историю банов (дашборд/панель).
func giveMasterTime(t *testing.T, r *Registry, id string) {
	t.Helper()
	r.mu.Lock()
	c := r.state.Candidates[id]
	c.MasterStints = 1
	c.MasterSeconds = 600
	r.mu.Unlock()
}

func TestGPQuarantineFullLifecycle(t *testing.T) {
	mock := newGPMock(t)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL

	r.register(registerRequest{NodeID: "node-q", IP: "1.1.1.1"})
	makeGreen(t, r, "node-q")
	giveMasterTime(t, r, "node-q") // V7.9.14: бан без мастер-тайма в статистику не пишется

	// 1-я неудачная верификация → карантин (attempt 1)
	mock.setBad(true)
	sendReport(t, r, "node-q", true, true)
	c := r.state.Candidates["node-q"]
	if c.Quarantine == nil || c.Quarantine.Attempts != 1 {
		t.Fatalf("node must enter quarantine at attempt 1, got %+v", c.Quarantine)
	}
	if c.GlobalpingOK {
		t.Fatal("verified fail must flip GlobalpingOK")
	}

	// 2-я → attempt 2
	sendReport(t, r, "node-q", true, true)
	if got := r.state.Candidates["node-q"].Quarantine.Attempts; got != 2 {
		t.Fatalf("second failed attempt must be counted, got %d", got)
	}

	// Верифицированное восстановление → карантин снят, бан НЕ засчитан
	mock.setBad(false)
	sendReport(t, r, "node-q", true, true)
	c = r.state.Candidates["node-q"]
	if c.Quarantine != nil {
		t.Fatalf("verified ok must lift the quarantine, got %+v", c.Quarantine)
	}
	if !c.GlobalpingOK {
		t.Fatal("recovered node must be GlobalpingOK again")
	}
	bans, err := r.db.bansSince(time.Now().Add(-time.Hour), "")
	if err != nil || len(bans) != 0 {
		t.Fatalf("recovered node must NOT be banned, bans=%v err=%v", bans, err)
	}
	var sawRecovered bool
	for _, ev := range r.state.Events {
		if ev.Type == EventQuarantineRecovered {
			sawRecovered = true
		}
	}
	if !sawRecovered {
		t.Fatal("quarantine_recovered event missing")
	}

	// Полная серия неудач: 1 (вход) + 2 → attempt 3 → терминальный бан по IP
	mock.setBad(true)
	sendReport(t, r, "node-q", true, true)
	sendReport(t, r, "node-q", true, true)
	if c := r.state.Candidates["node-q"]; c == nil {
		t.Fatal("node must still be alive at attempt 2 of 3")
	}
	sendReport(t, r, "node-q", true, true)
	if _, ok := r.state.Candidates["node-q"]; ok {
		t.Fatal("attempt 3/3 must terminate the node")
	}
	rec := r.state.Terminated["node-q"]
	if rec == nil || rec.Reason != BanReasonIPBan || rec.Message != MsgIPBan {
		t.Fatalf("terminated record mismatch: %+v", rec)
	}
	if rec.Message != "Бан по ip, запустите службу заново после его смены" {
		t.Fatalf("message must be the exact TZ line, got %q", rec.Message)
	}
	bans, _ = r.db.bansSince(time.Now().Add(-time.Hour), BanReasonIPBan)
	if len(bans) != 1 || bans[0].NodeID != "node-q" || bans[0].LifetimeSec < 0 {
		t.Fatalf("ip_ban row missing/invalid in history: %+v", bans)
	}
	if r.state.Counters.NodesTerminated != 1 {
		t.Fatalf("counter mismatch: %d", r.state.Counters.NodesTerminated)
	}
}

// Kill-доставка: dead запрещает перерегистрацию навсегда (403+terminate);
// ip_ban по ТОМУ ЖЕ ip даёт одну GP-перепроверку (reverify-карантин,
// V7.9.12), с НОВОГО ip блок снимается сразу.
func TestTerminatedKillDeliveryAndIPLift(t *testing.T) {
	r := newTerminateTestRegistry(t)
	now := time.Now()
	r.mu.Lock()
	r.state.Terminated["node-dead"] = &TerminatedRecord{
		NodeID: "node-dead", IP: "1.1.1.1", Reason: BanReasonDead, Message: MsgDead, At: now,
	}
	r.state.Terminated["node-gp"] = &TerminatedRecord{
		NodeID: "node-gp", IP: "3.3.3.3", Reason: BanReasonIPBan, Message: MsgIPBan, At: now,
	}
	r.mu.Unlock()

	mux := r.buildMux()
	post := func(path, body, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		if remote != "" {
			req.RemoteAddr = net.JoinHostPort(remote, "4599")
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// dead + тот же ip → 403 terminate (бессрочно)
	rec := post("/register", `{"node_id":"node-dead","ip":"1.1.1.1"}`, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("dead-terminated register must be 403, got %d", rec.Code)
	}
	var p struct {
		Terminate bool   `json:"terminate"`
		Reason    string `json:"reason"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || !p.Terminate || p.Reason != BanReasonDead {
		t.Fatalf("bad terminate payload: %s", rec.Body.String())
	}
	// dead + heartbeat → тоже kill
	if rec := post("/heartbeat", `{"node_id":"node-dead"}`, "1.1.1.1"); rec.Code != http.StatusForbidden {
		t.Fatalf("terminated heartbeat must be 403, got %d", rec.Code)
	}

	// ip_ban + ТОТ ЖЕ ip → V7.9.12: не kill, а reverify-карантин с одной
	// решающей попыткой (attempts = max-1)
	rec = post("/register", `{"node_id":"node-gp","ip":"3.3.3.3"}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("same-ip re-register after gp ban must be accepted for re-verify, got %d (%s)", rec.Code, rec.Body.String())
	}
	c := r.state.Candidates["node-gp"]
	if c == nil {
		t.Fatal("re-registered candidate must exist")
	}
	if c.Quarantine == nil || !c.Quarantine.Reverify || c.Quarantine.Attempts != 2 {
		t.Fatalf("re-verify quarantine mismatch (want attempts=2/3 reverify): %+v", c.Quarantine)
	}
	if _, ok := r.state.Terminated["node-gp"]; ok {
		t.Fatal("terminated record must be consumed by the re-verify registration")
	}
	// heartbeat от такой ноды — обычный, kill не приходит (запись снята)
	if rec := post("/heartbeat", `{"node_id":"node-gp"}`, "3.3.3.3"); rec.Code == http.StatusForbidden {
		t.Fatal("heartbeat during re-verify must NOT be killed")
	}

	// ip_ban + ДРУГОЙ ip → блок снят, регистрация без карантина (как раньше)
	r.mu.Lock()
	r.state.Terminated["node-gp2"] = &TerminatedRecord{
		NodeID: "node-gp2", IP: "4.4.4.4", Reason: BanReasonIPBan, Message: MsgIPBan, At: now,
	}
	r.mu.Unlock()
	if rec := post("/register", `{"node_id":"node-gp2","ip":"2.2.2.2"}`, ""); rec.Code != http.StatusOK {
		t.Fatalf("register from the NEW ip must pass (ban lifted), got %d", rec.Code)
	}
	r.mu.RLock()
	_, stillThere := r.state.Terminated["node-gp2"]
	c2 := r.state.Candidates["node-gp2"]
	r.mu.RUnlock()
	if stillThere {
		t.Fatal("termination record must be lifted after the ip change")
	}
	if c2 == nil || c2.Quarantine != nil {
		t.Fatalf("register from new ip must be quarantine-free, got %+v", c2)
	}
}

// Вход в карантин — без предусловий (V7.9.12): любая нода, не прошедшая
// globalping, — в карантин, даже красная по tcp/metrics.
func TestQuarantineEntryWithoutGreen(t *testing.T) {
	mock := newGPMock(t)
	mock.setBad(true)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL

	r.register(registerRequest{NodeID: "node-red", IP: "8.8.8.8"})
	// нода красная по tcp (default false) и metrics-защёлке (закрыта с нуля)
	sendReport(t, r, "node-red", true, true)
	c := r.state.Candidates["node-red"]
	if c.Quarantine == nil || c.Quarantine.Attempts != 1 {
		t.Fatalf("red node must enter quarantine on verified gp fail, got %+v", c.Quarantine)
	}
	if c.Healthy || c.MetricsHealthy {
		t.Fatal("test node must be NOT green — entry precondition was dropped in V7.9.12")
	}
}

// Выход из карантина любым путём, кроме восстановления, — терминальный
// ip_ban (V7.9.12 #3): expiry по heartbeat-TTL и dead-окно во время
// карантина завершают ноду как ip_ban, а не node_expired/dead. В историю
// банов при этом попадает только нода со временем мастерства (V7.9.14).
func TestQuarantineDropoutCountsAsBan(t *testing.T) {
	r := newTerminateTestRegistry(t)
	r.cfg.TerminateDeadTTL = 10 * time.Minute
	now := time.Now()
	r.mu.Lock()
	// отвалится по TTL
	r.state.Candidates["node-exp"] = &Candidate{
		NodeID: "node-exp", IP: "10.0.0.1", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now.Add(-2 * r.cfg.HeartbeatTTL),
		Healthy:       true, MetricsHealthy: true,
		MasterStints: 1, MasterSeconds: 600, // V7.9.14: её бан — в историю
		Quarantine: &QuarantineState{EnteredAt: now.Add(-20 * time.Minute), Attempts: 1},
	}
	// дозреет по dead-окну (говорящая, красная по обеим ногам); мастером
	// не была — бан завершает ноду, но в историю НЕ пишется (V7.9.14)
	r.state.Candidates["node-dd"] = &Candidate{
		NodeID: "node-dd", IP: "10.0.0.2", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now, Healthy: false, MetricsHealthy: false,
		LastReportAt:  now.Add(-time.Hour),
		DeadBothSince: now.Add(-11 * time.Minute),
		Quarantine:    &QuarantineState{EnteredAt: now.Add(-30 * time.Minute), Attempts: 2},
	}
	r.mu.Unlock()

	r.sweepExpired(now)
	for _, id := range []string{"node-exp", "node-dd"} {
		if _, ok := r.state.Candidates[id]; ok {
			t.Fatalf("%s must be terminated", id)
		}
		rec := r.state.Terminated[id]
		if rec == nil || rec.Reason != BanReasonIPBan {
			t.Fatalf("%s dropout from quarantine must be an ip_ban, got %+v", id, rec)
		}
	}
	bans, _ := r.db.bansSince(now.Add(-2*time.Hour), BanReasonIPBan)
	if len(bans) != 1 || bans[0].NodeID != "node-exp" {
		t.Fatalf("only the master-time dropout must reach ban history, got %+v", bans)
	}
	if dead, _ := r.db.bansSince(now.Add(-2*time.Hour), BanReasonDead); len(dead) != 0 {
		t.Fatalf("quarantined dead must NOT be counted as dead reason: %+v", dead)
	}
	for _, ev := range r.state.Events {
		if ev.Type == EventNodeExpired && ev.NodeID == "node-exp" {
			t.Fatal("quarantine dropout must not be a plain node_expired event")
		}
	}
}

// Reverify-цикл (V7.9.12 #5): старый забаненный ip переподключается → одна
// решающая GP-проверка: fail возвращает бан (терминальная запись прежняя,
// НО без новой строки в истории — бан адреса учтён при первом бане,
// V7.9.14), последующий ok снимает бан окончательно (ban_lifted).
func TestReverifyLifecycle(t *testing.T) {
	mock := newGPMock(t)
	r := newTerminateTestRegistry(t)
	r.cfg.Globalping.APIBase = mock.URL
	mux := r.buildMux()

	r.register(registerRequest{NodeID: "node-w", IP: "1.1.1.1"})
	makeGreen(t, r, "node-w")
	giveMasterTime(t, r, "node-w") // первый бан node-w — в историю (V7.9.14)
	mock.setBad(true)
	for i := 0; i < 3; i++ {
		sendReport(t, r, "node-w", true, true)
	}
	if r.state.Terminated["node-w"] == nil {
		t.Fatal("node must be banned after 3 failed attempts")
	}

	// старый ip, тот же id → reverify
	reg := func(id, ip string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(
			`{"node_id":"`+id+`","ip":"`+ip+`"}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := reg("node-w", "1.1.1.1"); rec.Code != http.StatusOK {
		t.Fatalf("same-ip re-register must be accepted, got %d", rec.Code)
	}
	q := r.state.Candidates["node-w"].Quarantine
	if q == nil || !q.Reverify || q.Attempts != 2 {
		t.Fatalf("re-verify quarantine mismatch: %+v", q)
	}

	// провалила единственную попытку → снова бан, и шанс исчерпан
	// (ReverifyFailed): следующий same-ip register → 403 kill, а не вторая
	// перепроверка (иначе цикл «410 → register → карантин → бан» вечен).
	sendReport(t, r, "node-w", true, true)
	rec2 := r.state.Terminated["node-w"]
	if rec2 == nil {
		t.Fatal("failed re-verify must re-ban the node")
	}
	if !rec2.ReverifyFailed {
		t.Fatal("failed re-verify must mark the record ReverifyFailed")
	}
	bans, _ := r.db.bansSince(time.Now().Add(-time.Hour), BanReasonIPBan)
	if len(bans) != 1 { // V7.9.14: ре-бан по итогам reverify НЕ кладёт вторую строку — бан адреса уже учтён
		t.Fatalf("failed re-verify must NOT add a second ip_ban row, got %+v", bans)
	}
	if rec := reg("node-w", "1.1.1.1"); rec.Code != http.StatusForbidden {
		t.Fatalf("same-ip register after FAILED re-verify must be killed (403), got %d", rec.Code)
	}
	// …но не навсегда: после reverifyCooldown шанс снова открыт
	r.mu.Lock()
	r.state.Terminated["node-w"].At = time.Now().Add(-(reverifyCooldown + time.Minute))
	r.mu.Unlock()
	if rec := reg("node-w", "1.1.1.1"); rec.Code != http.StatusOK {
		t.Fatalf("re-verify must reopen after the cooldown, got %d", rec.Code)
	}
	if q := r.state.Candidates["node-w"].Quarantine; q == nil || !q.Reverify {
		t.Fatalf("post-cooldown re-verify mismatch: %+v", q)
	}
	// но с НОВОГО ip — свободен и живёт дальше; V7.9.13: выход ИЗ
	// reverify-карантина со сменённым ip = блокировка брошенного старого ip
	// (stale-запись), сама нода на новом адресе чиста.
	if rec := reg("node-w", "1.1.1.2"); rec.Code != http.StatusOK {
		t.Fatalf("new-ip register must pass, got %d", rec.Code)
	}
	cw := r.state.Candidates["node-w"]
	if cw == nil || cw.IP != "1.1.1.2" || cw.Quarantine != nil {
		t.Fatalf("node must live on the new ip quarantine-free, got %+v", cw)
	}
	if stale := r.state.Terminated["node-w"]; stale == nil || !stale.StaleIP || stale.IP != "1.1.1.1" {
		t.Fatalf("abandoned old ip must be recorded as stale ip_ban: %+v", stale)
	}

	// отдельная нода: reverify ПРОШЁЛ (GP позеленел) → полное восстановление
	r.register(registerRequest{NodeID: "node-v", IP: "5.5.5.5"})
	makeGreen(t, r, "node-v")
	giveMasterTime(t, r, "node-v") // её первый бан — тоже в историю (V7.9.14)
	mock.setBad(true)
	for i := 0; i < 3; i++ {
		sendReport(t, r, "node-v", true, true)
	}
	if r.state.Terminated["node-v"] == nil {
		t.Fatal("node-v must be banned")
	}
	if rec := reg("node-v", "5.5.5.5"); rec.Code != http.StatusOK {
		t.Fatalf("node-v same-ip re-register must open re-verify, got %d", rec.Code)
	}
	mock.setBad(false)
	sendReport(t, r, "node-v", true, true)
	c := r.state.Candidates["node-v"]
	if c == nil || c.Quarantine != nil || !c.GlobalpingOK {
		t.Fatalf("passed re-verify must fully restore the node, got %+v", c)
	}
	var lifted bool
	for _, ev := range r.state.Events {
		if ev.Type == EventBanLifted && ev.NodeID == "node-v" {
			lifted = true
		}
	}
	if !lifted {
		t.Fatal("ban_lifted event expected after passed re-verify")
	}
	bans, _ = r.db.bansSince(time.Now().Add(-time.Hour), BanReasonIPBan)
	if len(bans) != 2 { // node-w (первый бан) + node-v; V7.9.14: ре-бан, конверсия ip без мастерства и восстановление строк не плодят
		t.Fatalf("only first-time bans of master-time nodes must stay in history, got %+v", bans)
	}

	// переустановка агента: новый id, старый забаненный ip → тоже reverify
	r.mu.Lock()
	r.state.Terminated["node-old"] = &TerminatedRecord{
		NodeID: "node-old", IP: "9.9.9.9", Reason: BanReasonIPBan, Message: MsgIPBan, At: time.Now(),
	}
	// а этот уже исчерпал перепроверку — чужой наследник получает kill
	r.state.Terminated["node-old2"] = &TerminatedRecord{
		NodeID: "node-old2", IP: "6.6.6.6", Reason: BanReasonIPBan, Message: MsgIPBan, At: time.Now(),
		ReverifyFailed: true,
	}
	r.mu.Unlock()
	if rec := reg("node-fresh", "9.9.9.9"); rec.Code != http.StatusOK {
		t.Fatalf("new id with old banned ip must enter re-verify, got %d", rec.Code)
	}
	cf := r.state.Candidates["node-fresh"]
	if cf == nil || cf.Quarantine == nil || !cf.Quarantine.Reverify {
		t.Fatalf("ip-inherited re-verify mismatch: %+v", cf)
	}
	if _, ok := r.state.Terminated["node-old"]; ok {
		t.Fatal("old-id record must be consumed by the ip-inherited re-verify")
	}
	if rec := reg("node-fresh2", "6.6.6.6"); rec.Code != http.StatusForbidden {
		t.Fatalf("ip-inherited register with exhausted re-verify must be killed, got %d", rec.Code)
	}
}

// dead-класс на свипере: непрерывно красные TCP+метрики дольше окна →
// терминальное завершение; озеленение любой ноги окно обнуляет.
func TestDeadClassSweep(t *testing.T) {
	r := newTerminateTestRegistry(t)
	r.cfg.TerminateDeadTTL = 10 * time.Minute
	r.cfg.HeartbeatTTL = time.Hour // говорящая нода: не даём heartbeat-expiry её прибрать

	now := time.Now()
	r.mu.Lock()
	r.state.Candidates["node-d"] = &Candidate{
		NodeID: "node-d", IP: "3.3.3.3", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now, // говорящая, но красная по обеим ногам
		Healthy:       false, MetricsHealthy: false, LastReportAt: now.Add(-time.Hour),
	}
	r.mu.Unlock()

	r.sweepExpired(now)
	c := r.state.Candidates["node-d"]
	if c == nil || c.DeadBothSince.IsZero() {
		t.Fatal("dead-both window must be armed on first red sweep")
	}

	// озеленение TCP → окно обнуляется
	r.mu.Lock()
	c.Healthy = true
	r.mu.Unlock()
	r.sweepExpired(now.Add(time.Minute))
	if !r.state.Candidates["node-d"].DeadBothSince.IsZero() {
		t.Fatal("greening any leg must reset the dead-both window")
	}

	// снова красная, окно дозревает → терминальный dead
	r.mu.Lock()
	c.Healthy = false
	r.mu.Unlock()
	r.sweepExpired(now.Add(2 * time.Minute)) // arm
	r.sweepExpired(now.Add(2*time.Minute + 11*time.Minute))
	if _, ok := r.state.Candidates["node-d"]; ok {
		t.Fatal("node red on tcp+metrics beyond the window must be terminated")
	}
	rec := r.state.Terminated["node-d"]
	if rec == nil || rec.Reason != BanReasonDead || rec.Message != MsgDead {
		t.Fatalf("dead termination record mismatch: %+v", rec)
	}
	if rec.Message != "Регистратор не достучался до порта и/или не получил метрики" {
		t.Fatalf("message must be the exact TZ line, got %q", rec.Message)
	}
}

// Карантинную ноду рипер НЕ трогает (её судьба — счётчик GP-попыток).
func TestQuarantinedExemptFromPrune(t *testing.T) {
	r := newTestRegistry(t) // без БД: не должна потребоваться (terminate не вызывается)
	r.cfg.PruneUnhealthyTTL = time.Minute
	now := time.Now()
	r.mu.Lock()
	r.state.Candidates["node-q"] = &Candidate{
		NodeID: "node-q", IP: "4.4.4.4", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now, FullyHealthy: false, UnhealthySince: now.Add(-10 * time.Minute),
		Quarantine: &QuarantineState{EnteredAt: now.Add(-5 * time.Minute), Attempts: 1},
	}
	r.mu.Unlock()
	r.sweepExpired(now)
	if _, ok := r.state.Candidates["node-q"]; !ok {
		t.Fatal("quarantined node must be exempt from the prune reaper")
	}
}

// /retire: терминальная запись ставится и для говорящей ноды, и для уже
// выпавшей; в историю банов — только нода со временем мастерства
// (V7.9.14: у выпавшей кандидата нет, мастер-тайм неизвестен → не пишем);
// чужой RemoteAddr отклоняется.
func TestRetireEndpoint(t *testing.T) {
	r := newTerminateTestRegistry(t)
	now := time.Now()
	r.mu.Lock()
	r.state.Candidates["node-r"] = &Candidate{
		NodeID: "node-r", IP: "6.6.6.6", RegisteredAt: now.Add(-30 * time.Minute),
		LastHeartbeat: now,
		MasterStints:  1, MasterSeconds: 300, // V7.9.14: бан со мастер-таймом — в историю
	}
	r.mu.Unlock()
	mux := r.buildMux()

	post := func(body, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/retire", strings.NewReader(body))
		req.RemoteAddr = net.JoinHostPort(remote, "4599")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// RemoteAddr ≠ заявленный ip → отказ
	if rec := post(`{"node_id":"node-r","ip":"6.6.6.6","reason":"dead"}`, "9.9.9.9"); rec.Code != http.StatusForbidden {
		t.Fatalf("ip mismatch must be refused, got %d", rec.Code)
	}
	if _, ok := r.state.Candidates["node-r"]; !ok {
		t.Fatal("refused retire must not touch the candidate")
	}

	// говорящая нода → терминальная запись + удаление
	if rec := post(`{"node_id":"node-r","ip":"6.6.6.6","reason":"dead"}`, "6.6.6.6"); rec.Code != http.StatusOK {
		t.Fatalf("retire must accept, got %d", rec.Code)
	}
	if _, ok := r.state.Candidates["node-r"]; ok {
		t.Fatal("retired candidate must be removed")
	}
	if tr := r.state.Terminated["node-r"]; tr == nil || tr.Reason != BanReasonDead {
		t.Fatalf("termination record missing: %+v", tr)
	}
	bans, _ := r.db.bansSince(now.Add(-time.Hour), BanReasonDead)
	if len(bans) != 1 || bans[0].LifetimeSec != int64((30*time.Minute).Seconds()) {
		t.Fatalf("dead ban row mismatch: %+v", bans)
	}

	// уже выпавшая нода (агент молчал и протух) → терминальная запись
	// всё равно фиксируется, но строки в истории НЕТ (V7.9.14: кандидата
	// нет — время мастерства неизвестно)
	if rec := post(`{"node_id":"node-gone","ip":"7.7.7.7","reason":"dead"}`, "7.7.7.7"); rec.Code != http.StatusOK {
		t.Fatalf("retire for expired node must accept, got %d", rec.Code)
	}
	if tr := r.state.Terminated["node-gone"]; tr == nil {
		t.Fatal("retired-after-expiry node must be terminated-recorded")
	}
	bans, _ = r.db.bansSince(now.Add(-time.Hour), BanReasonDead)
	if len(bans) != 1 {
		t.Fatalf("retire with unknown master time must NOT add a ban row, got %+v", bans)
	}
}

// V7.9.13 #5: нода вышла из карантина со СМЕНЁННЫМ ip — старый ip
// фиксируется как блокировка (строка bans ip_ban + stale-запись), сама
// нода живёт и работает на новом ip (карантин снят, блок не снимается
// «простым лифтом», с старого ip — только reverify-трек).
func TestQuarantineIPChangeCountsAsBan(t *testing.T) {
	r := newTerminateTestRegistry(t)
	now := time.Now()
	r.mu.Lock()
	r.state.Candidates["node-mv"] = &Candidate{
		NodeID: "node-mv", IP: "10.0.0.5", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now,
		MasterStints:  1, MasterSeconds: 600, // V7.9.14: бан её старого ip — в историю
		Quarantine: &QuarantineState{EnteredAt: now.Add(-10 * time.Minute), Attempts: 1},
	}
	r.mu.Unlock()

	// перерегистрация с НОВОГО ip
	if _, banned := r.register(registerRequest{NodeID: "node-mv", IP: "10.0.0.6"}); banned {
		t.Fatal("register with new ip must not be banned")
	}
	c := r.state.Candidates["node-mv"]
	if c == nil || c.IP != "10.0.0.6" {
		t.Fatalf("candidate must live on the new ip, got %+v", c)
	}
	if c.Quarantine != nil {
		t.Fatalf("quarantine must be dropped after the ip change: %+v", c.Quarantine)
	}
	// старый ip — в истории как бан (в статистику «без восстановления»)
	bans, _ := r.db.bansSince(now.Add(-2*time.Hour), BanReasonIPBan)
	if len(bans) != 1 || bans[0].IP != "10.0.0.5" || bans[0].LifetimeSec != int64(time.Hour.Seconds()) {
		t.Fatalf("quarantine ip change must record an ip_ban row for the OLD ip: %+v", bans)
	}
	rec := r.state.Terminated["node-mv"]
	if rec == nil || rec.IP != "10.0.0.5" || !rec.StaleIP {
		t.Fatalf("stale terminated record for the old ip must exist: %+v", rec)
	}
	var sawBlocked bool
	for _, ev := range r.state.Events {
		if ev.Type == EventIPBlocked && ev.IP == "10.0.0.5" {
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Fatal("ip_blocked event expected")
	}

	// обычная перерегистрация с очередного нового ip НЕ снимает stale-блок
	if _, banned := r.register(registerRequest{NodeID: "node-mv", IP: "10.0.0.7"}); banned {
		t.Fatal("register with yet another ip must not be banned")
	}
	if r.state.Terminated["node-mv"] == nil {
		t.Fatal("stale record must NOT be lifted by a plain ip change")
	}

	// а кто-то со СТАРОГО ip (даже с новым id) — reverify-трек V7.9.12
	mux := r.buildMux()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{"node_id":"node-stranger","ip":"10.0.0.5"}`))
	resp := httptest.NewRecorder()
	mux.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("old-ip stranger must enter re-verify, got %d", resp.Code)
	}
	cs := r.state.Candidates["node-stranger"]
	if cs == nil || cs.Quarantine == nil || !cs.Quarantine.Reverify {
		t.Fatalf("stranger from the old ip must be in re-verify quarantine: %+v", cs)
	}
	if r.state.Terminated["node-mv"] != nil {
		t.Fatal("re-verify must consume the stale record")
	}

	// V7.9.14: та же конверсия ip, но нода мастером НЕ была — stale-запись
	// и событие как обычно, а строки в истории банов нет.
	r.mu.Lock()
	r.state.Candidates["node-mv2"] = &Candidate{
		NodeID: "node-mv2", IP: "10.0.0.9", RegisteredAt: now.Add(-time.Hour),
		LastHeartbeat: now,
		Quarantine:    &QuarantineState{EnteredAt: now.Add(-5 * time.Minute), Attempts: 1},
	}
	r.mu.Unlock()
	if _, banned := r.register(registerRequest{NodeID: "node-mv2", IP: "10.0.0.10"}); banned {
		t.Fatal("node-mv2 new-ip register must not be banned")
	}
	if rec := r.state.Terminated["node-mv2"]; rec == nil || !rec.StaleIP || rec.IP != "10.0.0.9" {
		t.Fatalf("node-mv2 old ip must still get the stale record: %+v", rec)
	}
	bans, _ = r.db.bansSince(now.Add(-2*time.Hour), BanReasonIPBan)
	if len(bans) != 1 {
		t.Fatalf("ip change without master time must NOT add a ban row, got %+v", bans)
	}
}

// V7.9.14: в историю банов (дашборд/панель) попадают только баны нод,
// успевших поработать мастером; завершение, блоки, счётчики и события от
// наличия мастер-тайма не зависят.
func TestBanStatsRequireMasterTime(t *testing.T) {
	r := newTerminateTestRegistry(t)
	now := time.Now()

	// без мастер-тайма: terminate есть, строки в bans нет
	r.mu.Lock()
	r.terminateNodeLocked(&Candidate{
		NodeID: "node-plain", IP: "10.1.0.1", RegisteredAt: now.Add(-time.Hour),
	}, now, BanReasonIPBan, "")
	r.mu.Unlock()
	if r.state.Terminated["node-plain"] == nil {
		t.Fatal("termination record must be created regardless of master time")
	}
	if r.state.Counters.NodesTerminated != 1 {
		t.Fatalf("counter mismatch: %d", r.state.Counters.NodesTerminated)
	}
	var sawTerm bool
	for _, ev := range r.state.Events {
		if ev.Type == EventNodeTerminated && ev.NodeID == "node-plain" {
			sawTerm = true
		}
	}
	if !sawTerm {
		t.Fatal("node_terminated event must fire regardless of master time")
	}
	bans, _ := r.db.bansSince(now.Add(-time.Hour), "")
	if len(bans) != 0 {
		t.Fatalf("ban without master time must NOT reach history: %+v", bans)
	}

	// с мастер-таймом (закрытые stint'ы): строка есть, lifetime реальный
	r.mu.Lock()
	r.terminateNodeLocked(&Candidate{
		NodeID: "node-master", IP: "10.1.0.2", RegisteredAt: now.Add(-2 * time.Hour),
		MasterStints: 2, MasterSeconds: 3600,
	}, now, BanReasonDead, "")
	r.mu.Unlock()
	bans, _ = r.db.bansSince(now.Add(-time.Hour), "")
	if len(bans) != 1 || bans[0].NodeID != "node-master" || bans[0].Reason != BanReasonDead ||
		bans[0].LifetimeSec != int64((2*time.Hour).Seconds()) {
		t.Fatalf("master-time ban must be recorded: %+v", bans)
	}

	// открытый stint (мастер прямо сейчас) — тоже мастер-тайм
	r.mu.Lock()
	r.terminateNodeLocked(&Candidate{
		NodeID: "node-live", IP: "10.1.0.4", RegisteredAt: now.Add(-time.Hour),
		MasterStints: 1, MasterSince: now.Add(-5 * time.Minute),
	}, now, BanReasonIPBan, "")
	r.mu.Unlock()
	bans, _ = r.db.bansSince(now.Add(-time.Hour), "")
	if len(bans) != 2 {
		t.Fatalf("open-stint master ban must be recorded: %+v", bans)
	}

	// retire по выпавшей ноде: мастер-тайм неизвестен → строки нет,
	// терминальная запись есть
	r.mu.Lock()
	r.terminateRetiredLocked("node-gone2", "10.1.0.3", now, BanReasonDead)
	r.mu.Unlock()
	if r.state.Terminated["node-gone2"] == nil {
		t.Fatal("retired node must still get the termination record")
	}
	bans, _ = r.db.bansSince(now.Add(-time.Hour), "")
	if len(bans) != 2 {
		t.Fatalf("retire with unknown master time must NOT add a ban row: %+v", bans)
	}
}
