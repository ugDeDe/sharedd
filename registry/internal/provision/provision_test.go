package provision

// Тесты машины состояний (beget-provisioning.md §7): идемпотентность,
// гонка idem_key, timeout→unknown→reconcile, Remove-backoff, лимиты/cooldown,
// профиль vars + идемпотентность установки (на моках executor). Реальные
// платные API не вызываются — только мок-транспорт.

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/sshx"
)

// okAuth — валидные SSH-креды (ключ-заглушка; реальный SSH не вызывается).
func okAuth() sshx.Auth { return sshx.Auth{Type: "key", Key: "dummy-key"} }

type mockBeget struct {
	mu           sync.Mutex
	createCalls  int
	getListCalls int
	removeCalls  int
	regions      []beget.RegionInfo
	createFn     func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error)
	list         []beget.VpsInfo
	removeErr    error
	// regionsEntered/regionsRelease — детерминированное окно pending
	// (валидация каталога идёт до CAS pending→sent и платного вызова).
	regionsEntered chan struct{}
	regionsRelease chan struct{}
	regionsOnce    sync.Once
	// infoStatus/infoIP — статус/адрес VPS из GetInfo (пустой статус = готов).
	infoStatus string
	infoIP     string
}

func (m *mockBeget) GetRegions(context.Context) ([]beget.RegionInfo, error) {
	if m.regionsEntered != nil {
		m.regionsOnce.Do(func() { close(m.regionsEntered) })
	}
	if m.regionsRelease != nil {
		<-m.regionsRelease
	}
	if m.regions == nil {
		return []beget.RegionInfo{{ID: "ru-1", Available: true}}, nil
	}
	return m.regions, nil
}

func (m *mockBeget) CreateVps(ctx context.Context, req beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
	m.mu.Lock()
	m.createCalls++
	fn := m.createFn
	m.mu.Unlock()
	if fn != nil {
		return fn(ctx, req)
	}
	return &beget.CreateVpsResponse{Vps: &beget.VpsInfo{ID: "vps-1", Hostname: req.Hostname}}, nil
}

func (m *mockBeget) GetList(context.Context) (*beget.GetListResponse, error) {
	m.mu.Lock()
	m.getListCalls++
	m.mu.Unlock()
	return &beget.GetListResponse{Vps: m.list}, nil
}

func (m *mockBeget) GetInfo(_ context.Context, id string) (*beget.GetInfoResponse, error) {
	ip := m.infoIP
	if ip == "" {
		ip = "203.0.113.9"
	}
	return &beget.GetInfoResponse{Vps: &beget.VpsInfo{ID: id, Status: m.infoStatus, IPAddress: ip}}, nil
}

func (m *mockBeget) Remove(context.Context, string) error {
	m.mu.Lock()
	m.removeCalls++
	err := m.removeErr
	m.mu.Unlock()
	return err
}

func (m *mockBeget) calls() (create, list, remove int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.createCalls, m.getListCalls, m.removeCalls
}

func newStore(t *testing.T) *control.Store {
	t.Helper()
	s, err := control.Open(filepath.Join(t.TempDir(), "control.db"), 100000)
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func req(idem string) CreateRequest {
	return CreateRequest{UserID: 1, NodeID: "node-a1b2c", IdemKey: idem, Region: "ru-1", ConfigID: "cfg-1", MaxNodes: 100}
}

// 1. повторный запрос с тем же idem_key → та же операция, второй заказ не уходит.
func TestCreateIdempotentReplay(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	p := New(s, m, Config{})

	op1, err := p.Create(context.Background(), req("k1"))
	if err != nil || op1.State != control.OpConfirmed {
		t.Fatalf("create1: %+v err=%v", op1, err)
	}
	op2, err := p.Create(context.Background(), req("k1"))
	if err != nil {
		t.Fatalf("create2: %v", err)
	}
	if op2.ID != op1.ID || op2.State != control.OpConfirmed {
		t.Fatalf("replay must return same op: %+v vs %+v", op1, op2)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", c)
	}
}

// 2. параллельные запросы одного idem_key → один вызов CreateVps (мок-счётчик).
func TestCreateConcurrentOnePaidCall(t *testing.T) {
	s := newStore(t)
	release := make(chan struct{})
	m := &mockBeget{createFn: func(ctx context.Context, r beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		<-release // держим победителя внутри платного вызова
		return &beget.CreateVpsResponse{Vps: &beget.VpsInfo{ID: "vps-1", Hostname: r.Hostname}}, nil
	}}
	p := New(s, m, Config{})

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = p.Create(context.Background(), req("race"))
		}(i)
	}
	close(release)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want exactly 1", c)
	}
}

// 2b. H3: пока заказ пользователя не завершён (pending), новый заказ с
// ДРУГИМ idem_key отклоняется — повторный платный вызов не уходит.
func TestCreateDistinctKeyBlockedWhilePending(t *testing.T) {
	s := newStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m := &mockBeget{createFn: func(ctx context.Context, r beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		close(entered)
		<-release
		return &beget.CreateVpsResponse{Vps: &beget.VpsInfo{ID: "vps-1", Hostname: r.Hostname}}, nil
	}}
	p := New(s, m, Config{MaxCreatesPerDay: 100})

	done := make(chan error, 1)
	go func() {
		_, err := p.Create(context.Background(), req("winner"))
		done <- err
	}()
	<-entered // победитель в pending внутри платного вызова

	loser := req("loser")
	loser.NodeID = "node-b2c3d"
	if _, err := p.Create(context.Background(), loser); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second distinct key while pending: err = %v, want ErrInProgress", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("winner: %v", err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want exactly 1", c)
	}
}

// 2b-R1. Отмена во время платного вызова не освобождает слот и не пускает
// второй заказ: state=sent в полёте, cancel проигрывает CAS, второй ключ
// заблокирован H3 — ровно один CreateVps.
func TestCreateCancelDuringInFlightDoesNotFreeSlot(t *testing.T) {
	s := newStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m := &mockBeget{createFn: func(ctx context.Context, r beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		close(entered)
		<-release
		return &beget.CreateVpsResponse{Vps: &beget.VpsInfo{ID: "vps-1", Hostname: r.Hostname}}, nil
	}}
	p := New(s, m, Config{MaxCreatesPerDay: 100})

	done := make(chan error, 1)
	go func() {
		_, err := p.Create(context.Background(), req("race-cancel"))
		done <- err
	}()
	<-entered // state=sent, платный вызов в полёте

	op, err := s.OperationByIdemKey("race-cancel")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if op.State != control.OpSent {
		t.Fatalf("state = %q, want sent (CAS перед платным вызовом)", op.State)
	}
	if ok, err := s.CancelPendingOperation(op.ID); err != nil || ok {
		t.Fatalf("cancel in flight: ok=%v err=%v, want refused", ok, err)
	}
	second := req("second-key")
	second.NodeID = "node-b2c3d"
	if _, err := p.Create(context.Background(), second); !errors.Is(err, ErrInProgress) {
		t.Fatalf("second create err = %v, want ErrInProgress", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("winner: %v", err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want exactly 1", c)
	}
}

// 2b-R1b. Отмена в окне ДО платного вызова (pending, идёт валидация каталога)
// проходит: провайдер не вызывается, слот освобождается для нового ключа.
func TestCreateCancelBeforeSendSucceeds(t *testing.T) {
	s := newStore(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	m := &mockBeget{regionsEntered: entered, regionsRelease: release}
	p := New(s, m, Config{MaxCreatesPerDay: 100})

	done := make(chan error, 1)
	go func() {
		_, err := p.Create(context.Background(), req("pre-send"))
		done <- err
	}()
	<-entered // state=pending (каталог ещё грузится, платный вызов не ушёл)

	op, err := s.OperationByIdemKey("pre-send")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if op.State != control.OpPending {
		t.Fatalf("state = %q, want pending", op.State)
	}
	if ok, err := s.CancelPendingOperation(op.ID); err != nil || !ok {
		t.Fatalf("cancel pending: ok=%v err=%v, want success", ok, err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("cancelled create must not error: %v", err)
	}
	if c, _, _ := m.calls(); c != 0 {
		t.Fatalf("CreateVps calls = %d, want 0 (cancel before send)", c)
	}
	if _, err := p.Create(context.Background(), req("after-cancel")); err != nil {
		t.Fatalf("post-cancel create: %v", err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", c)
	}
}

// Confirmed VPS orders are not pool admissions, even when max_nodes is one.
func TestCreateQuotaCountsProvisionedVps(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	p := New(s, m, Config{MaxCreatesPerDay: 100})

	r1 := req("quota-1")
	r1.MaxNodes = 1
	if op, err := p.Create(context.Background(), r1); err != nil || op.State != control.OpConfirmed {
		t.Fatalf("first create: %+v err=%v", op, err)
	}
	r2 := req("quota-2")
	r2.MaxNodes = 1
	r2.NodeID = "node-b2c3d" // другой заказ = другой node_id (как в проде из idem_key)
	if _, err := p.Create(context.Background(), r2); err != nil {
		t.Fatalf("second create before registration: %v", err)
	}
	if c, _, _ := m.calls(); c != 2 {
		t.Fatalf("CreateVps calls = %d, want 2 (quota applies only to pool admission)", c)
	}
}

// 2d. H4: чужой idem_key не отдаём и не переиспользуем.
func TestCreateCrossUserIdemConflict(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	p := New(s, m, Config{MaxCreatesPerDay: 100})
	if _, err := p.Create(context.Background(), req("shared-key")); err != nil {
		t.Fatalf("owner create: %v", err)
	}
	other := req("shared-key")
	other.UserID = 2
	if _, err := p.Create(context.Background(), other); !errors.Is(err, ErrIdemConflict) {
		t.Fatalf("cross-user replay err = %v, want ErrIdemConflict", err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", c)
	}
}

// 3. таймаут CreateVps → state=unknown; GetList подтверждает; автоповтор не отправлен.
func TestCreateTimeoutThenReconcile(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{
		createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
			return nil, context.DeadlineExceeded
		},
	}
	p := New(s, m, Config{})

	op, err := p.Create(context.Background(), req("t1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if op.State != control.OpUnknown {
		t.Fatalf("state = %q, want unknown", op.State)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1 (no retry)", c)
	}
	// сверка: провайдер вернул наш сервер по hostname
	m.list = []beget.VpsInfo{{ID: "vps-9", Hostname: hostnameFor("node-a1b2c")}}
	rec, err := p.Reconcile(context.Background(), "t1")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.State != control.OpConfirmed || rec.ProviderVPSID != "vps-9" {
		t.Fatalf("reconciled = %+v", rec)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls after reconcile = %d, want still 1", c)
	}
}

// 3b. сверка не нашла сервер → остаётся unknown, новый заказ не уходит.
func TestReconcileUnknownStaysUnknown(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	p := New(s, m, Config{})
	if _, err := p.Create(context.Background(), req("t2")); err != nil {
		t.Fatalf("create: %v", err)
	}
	m.list = nil
	rec, err := p.Reconcile(context.Background(), "t2")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.State != control.OpUnknown {
		t.Fatalf("state = %q, want unknown", rec.State)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", c)
	}
}

// 4. ошибка Remove → vps_id сохраняется, state=deleting с backoff.
func TestRemoveFailureKeepsVPSID(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{removeErr: errors.New("provider busy")}
	p := New(s, m, Config{RemoveBackoff: time.Hour})

	op, err := p.Create(context.Background(), req("r1"))
	if err != nil || op.State != control.OpConfirmed {
		t.Fatalf("create: %+v err=%v", op, err)
	}
	after, err := p.Remove(context.Background(), op.ID)
	if err == nil {
		t.Fatal("remove must surface provider error")
	}
	if after.State != control.OpDeleting || after.ProviderVPSID != "vps-1" {
		t.Fatalf("after failed remove: %+v (vps id must be kept)", after)
	}
	// backoff: повтор слишком рано → без нового вызова
	if _, err := p.Remove(context.Background(), op.ID); !errors.Is(err, ErrBackoff) {
		t.Fatalf("second remove err = %v, want ErrBackoff", err)
	}
	if _, _, r := m.calls(); r != 1 {
		t.Fatalf("Remove calls = %d, want 1", r)
	}
}

// 4b. успешный Remove → deleted.
func TestRemoveSuccess(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	p := New(s, m, Config{})
	op, _ := p.Create(context.Background(), req("r2"))
	after, err := p.Remove(context.Background(), op.ID)
	if err != nil || after.State != control.OpDeleted || after.ProviderVPSID != "vps-1" {
		t.Fatalf("after remove: %+v err=%v", after, err)
	}
	cr, err := s.CloudResource("vps-1")
	if err != nil || cr.State != "deleted" {
		t.Fatalf("cloud resource = %+v err=%v", cr, err)
	}
}

// 6. лимит max_creates_per_day и cooldown персистятся.
func TestLimitsPersistAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := control.Open(path, 100000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	m := &mockBeget{}
	p := New(s, m, Config{MaxCreatesPerDay: 1, CooldownErrors: 1, Cooldown: time.Hour})
	if _, err := p.Create(context.Background(), req("a")); err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := p.Create(context.Background(), req("b")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	s.Close()

	// рестарт: лимит переживает reopen
	s2, err := control.Open(path, 100000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(s2.Close)
	p2 := New(s2, &mockBeget{}, Config{MaxCreatesPerDay: 1, CooldownErrors: 1, Cooldown: time.Hour})
	if _, err := p2.Create(context.Background(), req("c")); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("after reopen err = %v, want ErrRateLimited", err)
	}
}

func TestCooldownPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, _ := control.Open(path, 100000)
	m := &mockBeget{createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	p := New(s, m, Config{MaxCreatesPerDay: 10, CooldownErrors: 1, Cooldown: time.Hour})
	if _, err := p.Create(context.Background(), req("err1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := p.Create(context.Background(), req("err2")); !errors.Is(err, ErrCooldown) {
		t.Fatalf("err = %v, want ErrCooldown", err)
	}
	s.Close()
	s2, _ := control.Open(path, 100000)
	t.Cleanup(s2.Close)
	p2 := New(s2, &mockBeget{}, Config{MaxCreatesPerDay: 10, CooldownErrors: 1, Cooldown: time.Hour})
	if _, err := p2.Create(context.Background(), req("err3")); !errors.Is(err, ErrCooldown) {
		t.Fatalf("after reopen err = %v, want ErrCooldown", err)
	}
}

// 7. профиль: невалидные vars отклонены; шаги выполняются по SSH; новых
// заказов не появляется.
func TestApplyProfileVarsAndIdempotency(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})

	prof, err := s.CreateProfile("sharedd-node", "standard node", 0)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	schema := `{"vars":[{"name":"authkey","secret":true},{"name":"api_addr"}]}`
	steps := "# Шаг\ncommand --authkey {{.authkey}} --addr {{.api_addr}}\n"
	if _, err := s.AddProfileVersion(prof.ID, steps, schema); err != nil {
		t.Fatalf("add version: %v", err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	target := sshx.Target{Host: "203.0.113.9", Port: 22, User: "root", Auth: okAuth()}

	// §6: нелегальное имя переменной отклоняется ПЕРЕД запуском.
	if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{"bad-name": "evil"}); !errors.Is(err, control.ErrValidation) {
		t.Fatalf("illegal var err = %v, want validation", err)
	}
	if exec.ran() != 0 {
		t.Fatal("no step may run when vars are rejected")
	}

	if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{"authkey": "sda_x", "api_addr": "127.0.0.1:8081"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if exec.ran() != 1 {
		t.Fatalf("step runs = %d, want 1", exec.ran())
	}
	cmd := exec.allCommands()
	if !strings.Contains(cmd, "'sda_x'") || !strings.Contains(cmd, "'127.0.0.1:8081'") {
		t.Fatalf("vars must be shell-quoted into the command: %q", cmd)
	}
	if c, _, _ := m.calls(); c != 0 {
		t.Fatalf("ApplyProfile must not trigger purchases; CreateVps calls = %d", c)
	}
}

// §3 — первый упавший шаг прерывает профиль; следующие не выполняются.
func TestApplyProfileAbortsOnFirstFailure(t *testing.T) {
	s := newStore(t)
	exec := &mockExec{failOn: 2}
	p := New(s, &mockBeget{}, Config{Exec: exec})
	prof, err := s.CreateProfile("abort-prof", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	steps := "# one\necho 1\n# two\necho 2\n# three\necho 3\n"
	if _, err := s.AddProfileVersion(prof.ID, steps, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatal(err)
	}
	target := sshx.Target{Host: "203.0.113.9", User: "root", Auth: okAuth()}
	_, err = p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{})
	if err == nil {
		t.Fatal("failing step must abort the profile")
	}
	if exec.ran() != 2 {
		t.Fatalf("steps run = %d, want 2 (abort on second)", exec.ran())
	}
}

func TestInstallProfileSSHFailureFinalizes(t *testing.T) {
	for _, cause := range []error{sshx.ErrTransportLost, context.Canceled, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			s := newStore(t)
			exec := &mockExec{runErr: cause}
			p := New(s, &mockBeget{}, Config{Exec: exec})
			prof, err := s.CreateProfile("ssh-failure", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddProfileVersion(prof.ID, "# first\ntrue\n# never\nfalse\n", ""); err != nil {
				t.Fatal(err)
			}
			op, _, err := s.CreateOperation(control.Operation{
				UserID: 1, NodeID: "ssh-a1b2c", Kind: control.OpKindCreate,
				State: control.OpConfirmed, IdemKey: "ssh-failure", ProviderVPSID: "existing-vps",
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = p.InstallProfile(context.Background(), InstallRequest{
				OperationID: op.ID, ProfileID: prof.ID, Host: "203.0.113.9", Auth: okAuth(),
			})
			if !errors.Is(err, cause) || exec.ran() != 1 {
				t.Fatalf("err=%v runs=%d; want cause and no subsequent step", err, exec.ran())
			}
			saved, err := s.Operation(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.State != control.OpFailed || saved.ProviderVPSID != "existing-vps" || !strings.Contains(saved.LastError, cause.Error()) {
				t.Fatalf("failure was not persisted safely: %+v", saved)
			}
			steps, err := s.OperationSteps(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(steps) != 1 || steps[0].Status != control.StepError || steps[0].FinishedAt == 0 {
				t.Fatalf("step was not finalized: %+v", steps)
			}
		})
	}
}

// mockExec — исполнитель шагов по SSH: помнит прогнанные команды, отдаёт
// заданные результаты; реальной сети нет.
type mockExec struct {
	mu       sync.Mutex
	calls    int
	commands []string
	// failOn — номер команды (1-based), на которой вернуть ненулевой код.
	failOn int
	// runErr — вернуть транспортную ошибку.
	runErr   error
	failCode int
}

func (e *mockExec) WaitSSH(context.Context, sshx.Target) error { return nil }

func (e *mockExec) Run(_ context.Context, _ sshx.Target, command, stdin string, _ time.Duration) (sshx.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.commands = append(e.commands, command)
	if stdin != "" {
		e.commands = append(e.commands, stdin)
	}
	if e.runErr != nil {
		return sshx.Result{Stderr: e.runErr.Error()}, e.runErr
	}
	if e.failOn > 0 && e.calls == e.failOn {
		code := e.failCode
		if code == 0 {
			code = 1
		}
		return sshx.Result{Stderr: "boom", Code: code}, nil
	}
	return sshx.Result{Stdout: "ok"}, nil
}

func (e *mockExec) ran() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *mockExec) allCommands() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.commands, "\n")
}

// addendum §5.5/§7.1 — нет SSH-кредов: операция failed БЕЗ платного заказа.
func TestProvisionNoSSHCredsFailsWithoutOrder(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})
	op, err := p.Provision(context.Background(), req("gate"), 1, "root", sshx.Auth{}, map[string]any{})
	if !errors.Is(err, ErrSSHUnavailable) {
		t.Fatalf("err = %v, want ErrSSHUnavailable", err)
	}
	if op.State != control.OpFailed {
		t.Fatalf("op state = %q, want failed", op.State)
	}
	if c, _, _ := m.calls(); c != 0 {
		t.Fatalf("CreateVps calls = %d, want 0 (no paid order without creds)", c)
	}
}

// addendum §5.5 — полный поток: один CreateVps, шаги ровно один раз,
// операция installing→done.
func TestProvisionAppliesProfileOnce(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})
	prof, err := s.CreateProfile("flow-prof", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(prof.ID, "# s\necho hi\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatal(err)
	}

	op, err := p.Provision(context.Background(), req("flow"), prof.ID, "root", okAuth(), map[string]any{})
	if err != nil || op.State != control.OpDone {
		t.Fatalf("provision = %+v err=%v, want done", op, err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want exactly 1", c)
	}
	if exec.ran() != 1 {
		t.Fatalf("step runs = %d, want exactly 1", exec.ran())
	}
}

// addendum §5.5 — таймаут CreateVps → unknown; установка не запускается,
// повторный заказ не уходит.
func TestProvisionTimeoutUnknownNoInstall(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})
	op, err := p.Provision(context.Background(), req("timeout"), 1, "root", okAuth(), map[string]any{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if op.State != control.OpUnknown {
		t.Fatalf("state = %q, want unknown", op.State)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1 (no retry)", c)
	}
	if exec.ran() != 0 {
		t.Fatalf("apply must not run on unknown, runs=%d", exec.ran())
	}
}

// §7.1 — пустой (не собранный из шагов) профиль отклоняется до запуска шагов.
func TestApplyProfileEmptyStepsRejected(t *testing.T) {
	s := newStore(t)
	exec := &mockExec{}
	p := New(s, &mockBeget{}, Config{Exec: exec})
	prof, err := s.CreateProfile("empty-prof", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	// только заголовок без команды → шагов нет.
	if _, err := s.AddProfileVersion(prof.ID, "# только заголовок\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatal(err)
	}
	target := sshx.Target{Host: "203.0.113.9", User: "root", Auth: okAuth()}
	if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{}); err == nil {
		t.Fatal("empty profile must be rejected")
	}
	if exec.ran() != 0 {
		t.Fatalf("no step may run, runs=%d", exec.ran())
	}
}

// §7.2 — retry-install переиспользует provider_vps_id: НОЛЬ новых CreateVps.
func TestRetryInstallZeroCreateVps(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})
	prof, err := s.CreateProfile("retry-prof", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(prof.ID, "# s\necho hi\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatal(err)
	}

	// Первый заказ — единственный платный вызов.
	op, err := p.Create(context.Background(), req("retry-key"))
	if err != nil || op.State != control.OpConfirmed {
		t.Fatalf("create: %+v err=%v", op, err)
	}
	// Имитируем провал установки — состояние failed с сохранённым vps id.
	if err := s.UpdateOperationState(op.ID, control.OpFailed, op.ProviderVPSID, "boom"); err != nil {
		t.Fatal(err)
	}
	// Два retry: первый → done, второй отказан (already_done); оба БЕЗ CreateVps.
	r1, err := p.RetryInstall(context.Background(), op.ID, prof.ID, "root", okAuth(), map[string]any{})
	if err != nil || r1.State != control.OpDone {
		t.Fatalf("retry1: %+v err=%v", r1, err)
	}
	if _, err := p.RetryInstall(context.Background(), op.ID, prof.ID, "root", okAuth(), map[string]any{}); !errors.Is(err, ErrAlreadyDone) {
		t.Fatalf("retry2 err = %v, want ErrAlreadyDone", err)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1 (no paid call on retry)", c)
	}
}

// §7.2 — отказы retry-install: нет VPS id / активная операция.
func TestRetryInstallRefusals(t *testing.T) {
	s := newStore(t)
	p := New(s, &mockBeget{}, Config{Exec: &mockExec{}})
	op, _, err := s.CreateOperation(control.Operation{
		UserID: 1, NodeID: "node-a1b2c", Kind: control.OpKindCreate,
		State: control.OpFailed, IdemKey: "no-vps",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.RetryInstall(context.Background(), op.ID, 1, "root", okAuth(), nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("no vps id err = %v, want ErrValidation", err)
	}
	act, _, _ := s.CreateOperation(control.Operation{
		UserID: 1, NodeID: "node-b2c3d", Kind: control.OpKindCreate,
		State: control.OpInstalling, IdemKey: "active", ProviderVPSID: "vps-1",
	})
	if _, err := p.RetryInstall(context.Background(), act.ID, 1, "root", okAuth(), nil); !errors.Is(err, ErrInProgress) {
		t.Fatalf("active err = %v, want ErrInProgress", err)
	}
}

// §7.2 — retry-install повторяет профиль, сохранённый при создании, а не
// переданный в теле retry (там profile_id=0 по умолчанию).
func TestRetryInstallUsesStoredProfile(t *testing.T) {
	s := newStore(t)
	exec := &mockExec{}
	p := New(s, &mockBeget{}, Config{Exec: exec})
	addPublishedProfile(t, s, "prof-a", "# a\necho PROFILE_A\n")
	profB := addPublishedProfile(t, s, "prof-b", "# b\necho PROFILE_B\n")
	profA, _ := s.ProfileByName("prof-a")

	reqA := req("stored-prof")
	reqA.ProfileID = profA.ID
	op, err := p.Create(context.Background(), reqA)
	if err != nil || op.State != control.OpConfirmed {
		t.Fatalf("create: %+v err=%v", op, err)
	}
	if op.ProfileID != profA.ID {
		t.Fatalf("stored profile = %d, want %d", op.ProfileID, profA.ID)
	}
	if err := s.UpdateOperationState(op.ID, control.OpFailed, op.ProviderVPSID, "boom"); err != nil {
		t.Fatal(err)
	}
	// retry передаёт другой профиль B (и/или 0 от UI) → должен остаться A.
	if _, err := p.RetryInstall(context.Background(), op.ID, profB, "root", okAuth(), map[string]any{}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !strings.Contains(exec.allCommands(), "PROFILE_A") {
		t.Fatalf("retry ran wrong profile: %q", exec.allCommands())
	}
}

// §7.2 — legacy-операция без сохранённого profile_id (0) падает на переданный
// профиль без ошибки (обратная совместимость сохранённых журналов).
func TestRetryInstallLegacyFallsBack(t *testing.T) {
	s := newStore(t)
	exec := &mockExec{}
	p := New(s, &mockBeget{}, Config{Exec: exec})
	prof := addPublishedProfile(t, s, "legacy-prof", "# l\necho LEGACY\n")
	op, _, err := s.CreateOperation(control.Operation{
		UserID: 1, NodeID: "node-a1b2c", Kind: control.OpKindCreate,
		State: control.OpFailed, IdemKey: "legacy", ProviderVPSID: "vps-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if op.ProfileID != 0 {
		t.Fatalf("legacy op profile = %d, want 0", op.ProfileID)
	}
	if _, err := p.RetryInstall(context.Background(), op.ID, prof, "root", okAuth(), map[string]any{}); err != nil {
		t.Fatalf("legacy retry: %v", err)
	}
	if !strings.Contains(exec.allCommands(), "LEGACY") {
		t.Fatalf("legacy retry ran wrong profile: %q", exec.allCommands())
	}
}

// addPublishedProfile — опубликованный профиль с заданными шагами; возвращает id.
func addPublishedProfile(t *testing.T, s *control.Store, name, steps string) int64 {
	t.Helper()
	p, err := s.CreateProfile(name, "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(p.ID, steps, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(p.ID); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

// §7.3 — таймаут готовности → waiting_vps; место занято, второго заказа нет.
func TestProvisionReadyTimeoutWaitingVps(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{infoStatus: "creating"}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec, VpsReadyTimeout: 5 * time.Millisecond, VpsPollInterval: time.Millisecond})

	op, err := p.Provision(context.Background(), req("slow"), 1, "root", okAuth(), map[string]any{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if op.State != control.OpWaitingVps {
		t.Fatalf("state = %q, want waiting_vps", op.State)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want 1", c)
	}
	if exec.ran() != 0 {
		t.Fatalf("apply must not run before RUNNING, runs=%d", exec.ran())
	}
	// повтор того же idem_key — та же операция, нового заказа нет.
	op2, err := p.Provision(context.Background(), req("slow"), 1, "root", okAuth(), map[string]any{})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if op2.ID != op.ID {
		t.Fatalf("replay returned new op: %+v vs %+v", op2, op)
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("CreateVps calls = %d, want still 1", c)
	}
}

// §6 — значения секретных переменных маскируются в выводе прогона.
func TestMaskSecretValues(t *testing.T) {
	schema := `{"vars":[{"name":"authkey","secret":true},{"name":"api_addr"}]}`
	vars := map[string]any{"authkey": "sda-TOPSECRET", "api_addr": "127.0.0.1:8081"}
	got := MaskSecrets("cmd authkey=sda-TOPSECRET ok", secretValues(schema, vars))
	if strings.Contains(got, "sda-TOPSECRET") || !strings.Contains(got, "***") {
		t.Fatalf("secret not masked: %q", got)
	}
	if strings.Contains(MaskSecrets("addr 127.0.0.1:8081", secretValues(schema, vars)), "***") {
		t.Fatal("non-secret value must not be masked")
	}
}

// newBoundStore — store с привязанной к пользователю нодой (слот занят) для
// проверок освобождения/удержания места (MED-5).
func newBoundStore(t *testing.T) (*control.Store, control.User) {
	t.Helper()
	s := newStore(t)
	u, err := s.CreateUser("alice", "password-123", "user", 5)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.AuthorizeNode("node-a1b2c", u); err != nil {
		t.Fatalf("authorize node: %v", err)
	}
	return s, u
}

// MED-5 — отказ ДО платного вызова (недоступный регион) освобождает слот.
func TestCreatePreSendFailureReleasesSlot(t *testing.T) {
	s, u := newBoundStore(t)
	used0, err := s.UsedSlots(u.ID)
	if err != nil || used0 != 1 {
		t.Fatalf("pre-create used = %d err=%v, want 1", used0, err)
	}
	// каталог не содержит запрошенный регион → failed до CreateVps.
	m := &mockBeget{regions: []beget.RegionInfo{{ID: "other", Available: true}}}
	p := New(s, m, Config{})
	r := req("presend")
	r.UserID = u.ID
	op, err := p.Create(context.Background(), r)
	if err == nil {
		t.Fatal("invalid region must fail")
	}
	if op.State != control.OpFailed {
		t.Fatalf("state = %q, want failed", op.State)
	}
	if c, _, _ := m.calls(); c != 0 {
		t.Fatalf("CreateVps calls = %d, want 0 (pre-send failure)", c)
	}
	used1, _ := s.UsedSlots(u.ID)
	if used1 != 0 {
		t.Fatalf("slot must be released on pre-send failure, used = %d", used1)
	}
}

// MED-5 — отказ ПОСЛЕ отправки (unknown) слот НЕ освобождает (fails safe).
func TestCreatePostSendFailureKeepsSlot(t *testing.T) {
	s, u := newBoundStore(t)
	m := &mockBeget{createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		return nil, context.DeadlineExceeded
	}}
	p := New(s, m, Config{})
	r := req("postsend")
	r.UserID = u.ID
	op, err := p.Create(context.Background(), r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if op.State != control.OpUnknown {
		t.Fatalf("state = %q, want unknown", op.State)
	}
	used, _ := s.UsedSlots(u.ID)
	if used != 1 {
		t.Fatalf("slot must stay after sent, used = %d want 1", used)
	}
}

// MED-6 — last_error не несёт сырое тело ответа провайдера и request-id.
func TestProviderErrorNotLeakedIntoLastError(t *testing.T) {
	s := newStore(t)
	apiErr := &beget.APIError{HTTPStatus: 502, Body: "SECRET-PROVIDER-BODY", RequestID: "req-SECRET-123"}
	m := &mockBeget{createFn: func(context.Context, beget.CreateVpsRequest) (*beget.CreateVpsResponse, error) {
		return nil, apiErr
	}}
	p := New(s, m, Config{})
	op, _ := p.Create(context.Background(), req("leak"))
	for _, bad := range []string{"SECRET-PROVIDER-BODY", "req-SECRET-123"} {
		if strings.Contains(op.LastError, bad) {
			t.Fatalf("last_error leaks provider internals: %q", op.LastError)
		}
	}
	stored, err := s.OperationByIdemKey("leak")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if strings.Contains(stored.LastError, "SECRET-PROVIDER-BODY") || strings.Contains(stored.LastError, "req-SECRET-123") {
		t.Fatalf("persisted last_error leaks provider internals: %q", stored.LastError)
	}
}

// ── H1/H3: фоновый бюджет и fail-fast исполнение шагов ─────────────

// H1: бюджет воркера задаётся vps_ready_timeout + N×install_timeout (+запас),
// а не 30-секундным запросным ctx.
func TestProvisionBudgetUsesConfiguredTimeouts(t *testing.T) {
	p := New(nil, nil, Config{VpsReadyTimeout: 10 * time.Minute, StepTimeout: 20 * time.Minute})
	got := p.ProvisionBudget(3)
	want := 10*time.Minute + 4*20*time.Minute + 5*time.Minute
	if got != want {
		t.Fatalf("ProvisionBudget(3) = %s, want %s", got, want)
	}
	if p.ProvisionBudget(0) <= 10*time.Minute {
		t.Fatalf("budget must exceed the ready timeout: %s", p.ProvisionBudget(0))
	}
}

// recordingExec фиксирует (command, stdin) каждой прогонки шага.
type recordingExec struct {
	mu   sync.Mutex
	cmds []string
	ins  []string
}

func (e *recordingExec) WaitSSH(context.Context, sshx.Target) error { return nil }

func (e *recordingExec) Run(_ context.Context, _ sshx.Target, command, stdin string, _ time.Duration) (sshx.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cmds = append(e.cmds, command)
	e.ins = append(e.ins, stdin)
	return sshx.Result{Stdout: "ok"}, nil
}

// H3: тело шага исполняется `bash -eo pipefail -s` (команда на stdin), а не
// одной login-shell-строкой с кодом последней команды.
func TestRunStepsWrapsBashPipefail(t *testing.T) {
	s := newStore(t)
	exec := &recordingExec{}
	p := New(s, &mockBeget{}, Config{Exec: exec})
	prof, err := s.CreateProfile("wrap", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(prof.ID, "# s\necho {{ v }}\n", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PublishProfile(prof.ID); err != nil {
		t.Fatal(err)
	}
	target := sshx.Target{Host: "203.0.113.9", User: "root", Auth: okAuth()}
	if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{"v": "x"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(exec.cmds) != 1 || exec.cmds[0] != "bash -eo pipefail -s" {
		t.Fatalf("step command = %v, want 'bash -eo pipefail -s'", exec.cmds)
	}
	if len(exec.ins) != 1 || !strings.Contains(exec.ins[0], "echo 'x'") {
		t.Fatalf("step stdin = %v, want rendered command", exec.ins)
	}
}

// localBashExec исполняет шаг настоящим bash (команда — `bash -eo pipefail -s`,
// тело — на stdin). Только для тестов fail-fast семантики; сети нет.
type localBashExec struct{}

func (localBashExec) WaitSSH(context.Context, sshx.Target) error { return nil }

func (localBashExec) Run(ctx context.Context, _ sshx.Target, command, stdin string, _ time.Duration) (sshx.Result, error) {
	c := exec.CommandContext(ctx, "bash", "-c", command)
	c.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	res := sshx.Result{Stdout: out.String(), Stderr: errb.String()}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Code = ee.ExitCode()
		return res, nil
	}
	return res, err
}

// H3: многострочный шаг с ошибкой в середине и пайп в bash без вывода —
// оба обязаны провалить шаг (раньше код возврата = код последней команды → 0).
func TestApplyProfileFailFastPipeAndMultiline(t *testing.T) {
	cases := map[string]string{
		"multiline-false-middle": "# s\nfalse\ntrue\n",
		"empty-pipe-into-bash":   "# s\nfalse | bash\n",
		// no-op install: «установка» прошла, но verify-шаг (агент не поднялся)
		// обязан провалить профиль, а не дать ложный done.
		"verify-step-fails": "# install\ntrue\n# verify\nfalse\n",
	}
	for name, steps := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			p := New(s, &mockBeget{}, Config{Exec: localBashExec{}})
			prof, err := s.CreateProfile(name, "d", 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.AddProfileVersion(prof.ID, steps, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PublishProfile(prof.ID); err != nil {
				t.Fatal(err)
			}
			target := sshx.Target{Host: "203.0.113.9", User: "root", Auth: okAuth()}
			if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{}); err == nil {
				t.Fatalf("step %q must fail under bash -e -o pipefail", steps)
			}
		})
	}
	// контроль: успешный шаг проходит.
	s := newStore(t)
	p := New(s, &mockBeget{}, Config{Exec: localBashExec{}})
	prof, _ := s.CreateProfile("ok", "d", 0)
	_, _ = s.AddProfileVersion(prof.ID, "# s\ntrue\necho ok\n", "")
	_, _ = s.PublishProfile(prof.ID)
	target := sshx.Target{Host: "203.0.113.9", User: "root", Auth: okAuth()}
	if _, err := p.ApplyProfile(context.Background(), prof.ID, target, map[string]any{}); err != nil {
		t.Fatalf("successful step must pass: %v", err)
	}
}

// L9: секрет маскируется ДО обрезки — длинный секрет не оставляет префикс.
func TestMaskSecretsBeforeTruncation(t *testing.T) {
	secret := strings.Repeat("S", 5000) // длиннее лимита обрезки (2000)
	output := "prefix " + secret + " suffix"
	masked := truncateOutput(MaskSecrets(output, []string{secret}))
	if strings.Contains(masked, "SSSS") {
		t.Fatalf("truncated secret leaked: %q", masked)
	}
	if !strings.Contains(masked, "***") {
		t.Fatalf("secret must be masked: %q", masked)
	}
}

// H1: InstallOnConfirmed (точка входа фонового воркера) доводит подтверждённую
// операцию installing→done на мок-исполнителе и НЕ делает новых платных вызовов.
func TestInstallOnConfirmedTransitions(t *testing.T) {
	s := newStore(t)
	m := &mockBeget{}
	exec := &mockExec{}
	p := New(s, m, Config{Exec: exec})
	prof, err := s.CreateProfile("worker-prof", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddProfileVersion(prof.ID, "# s\necho install\n", ""); err != nil {
		t.Fatal(err)
	}
	op, err := p.Create(context.Background(), req("worker-inst"))
	if err != nil {
		t.Fatal(err)
	}
	if op.State != control.OpConfirmed {
		t.Fatalf("precondition: op state = %s, want confirmed", op.State)
	}
	got, err := p.InstallOnConfirmed(context.Background(), op.ID, prof.ID, "root", okAuth(), map[string]any{})
	if err != nil {
		t.Fatalf("InstallOnConfirmed: %v", err)
	}
	if got.State != control.OpDone {
		t.Fatalf("worker install state = %s, want done", got.State)
	}
	if exec.ran() != 1 {
		t.Fatalf("steps run = %d, want 1", exec.ran())
	}
	if c, _, _ := m.calls(); c != 1 {
		t.Fatalf("worker must not issue paid calls, CreateVps = %d, want 1 (the create)", c)
	}
	// persisted state reflects done for UI polling.
	stored, err := s.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != control.OpDone {
		t.Fatalf("persisted state = %s, want done", stored.State)
	}
}
