package provision

// Машина состояний провижининга личного Beget (docs/specs/beget-provisioning.md
// §4). Деньги-критичные гарантии:
//   - ровно один платный CreateVps на idem_key (в т.ч. под гонкой);
//   - строка operations(state=pending) персистится ДО вызова API;
//   - таймаут/потеря ответа → state=unknown; разрешение — только сверкой GetList,
//     НИКОГДА повторным POST (автоповтор платного заказа запрещён);
//   - ошибка/здоровье/установка НЕ порождают новый заказ или пересоздание;
//   - ошибка Remove сохраняет provider_vps_id и оставляет state=deleting с backoff;
//   - лимит max_creates_per_day и cooldown персистятся (derived из operations).
//
// Установка профиля (addendum §3-4, §7): профиль — упорядоченный список
// shell-команд, выполняемых по SSH на VPS пользователя (не на регистраторе).
// Первый упавший шаг прерывает профиль. Значения переменных shell-квотируются
// при подстановке; вывод обрезается и маскирует секреты.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/sshx"
)

var (
	// ErrRateLimited — достигнут max_creates_per_day.
	ErrRateLimited = errors.New("max_creates_per_day reached")
	// ErrCooldown — после серии ошибок активен cooldown.
	ErrCooldown = errors.New("cooldown active after errors")
	// ErrBackoff — повтор Remove слишком рано (backoff ещё не истёк).
	ErrBackoff = errors.New("remove backoff active")
	// ErrValidation — неверные входные данные (до платного вызова).
	ErrValidation = errors.New("validation failed")
	// ErrQuotaExceeded — исчерпан бюджет мест пользователя (max_nodes).
	ErrQuotaExceeded = errors.New("quota exceeded")
	// ErrInProgress — у пользователя уже есть незавершённый заказ (H3).
	ErrInProgress = errors.New("create already in progress")
	// ErrIdemConflict — idem_key принадлежит другому пользователю (H4).
	ErrIdemConflict = errors.New("idem_key belongs to another user")
	// ErrSSHUnavailable — SSH-креды VPS не заданы (нет ключа/пароля):
	// установка невозможна, платный заказ НЕ уходит.
	ErrSSHUnavailable = errors.New("ssh credentials not available")
	// ErrAlreadyDone — retry-install на завершённой операции запрещён.
	ErrAlreadyDone = errors.New("operation already done")
	// ErrInstallFailed — установка профиля завершилась ошибкой.
	ErrInstallFailed = errors.New("profile installation failed")
)

// BegetAPI — минимальная поверхность провайдера, нужная провижинингу.
type BegetAPI interface {
	GetRegions(ctx context.Context) ([]beget.RegionInfo, error)
	CreateVps(ctx context.Context, req beget.CreateVpsRequest) (*beget.CreateVpsResponse, error)
	GetInfo(ctx context.Context, id string) (*beget.GetInfoResponse, error)
	GetList(ctx context.Context) (*beget.GetListResponse, error)
	Remove(ctx context.Context, id string) error
}

// Executor выполняет команды профиля на VPS по SSH. Интерфейс инъектируется
// ради детерминированных тестов; в проде — sshExecutor поверх sshx.
// stdin — тело шага (исполняется `bash -eo pipefail -s` на целевой машине).
type Executor interface {
	WaitSSH(ctx context.Context, target sshx.Target) error
	Run(ctx context.Context, target sshx.Target, command, stdin string, timeout time.Duration) (sshx.Result, error)
}

// sshExecutor — боевая реализация Executor.
type sshExecutor struct{}

// SSHExecutor — боевой исполнитель шагов (реальный SSH) как публичный
// Executor: нужен вызывающим (например, check-эндпоинту manual-серверов),
// которым исполнитель требуется без создания Provisioner.
func SSHExecutor() Executor { return sshExecutor{} }

func (sshExecutor) WaitSSH(ctx context.Context, target sshx.Target) error {
	return sshx.WaitSSH(ctx, target, 20, 30*time.Second)
}

func (sshExecutor) Run(ctx context.Context, target sshx.Target, command, stdin string, timeout time.Duration) (sshx.Result, error) {
	res, err := sshx.Run(ctx, target, command, timeout, stdin)
	if res == nil {
		return sshx.Result{}, err
	}
	return *res, err
}

// Config — лимиты, персистятся через operations (переживают рестарт).
type Config struct {
	MaxCreatesPerDay int
	CooldownErrors   int
	Cooldown         time.Duration
	RemoveBackoff    time.Duration
	// VpsReadyTimeout/VpsPollInterval — ограниченное ожидание RUNNING после
	// платного заказа (addendum §7.3). Таймаут → waiting_vps, новый заказ не
	// уходит.
	VpsReadyTimeout time.Duration
	VpsPollInterval time.Duration
	// StepTimeout — таймаут одного шага профиля (install_timeout_min).
	StepTimeout time.Duration
	// Exec — исполнитель шагов по SSH; nil → sshExecutor (реальный SSH).
	Exec Executor
}

// RunResult — итог прогона профиля.
type RunResult struct {
	Host   string `json:"host"`
	Steps  int    `json:"steps"`
	Output string `json:"output,omitempty"`
}

// Provisioner — оркестратор; store — источник истины по операциям.
type Provisioner struct {
	store *control.Store
	beget BegetAPI
	exec  Executor
	cfg   Config
	now   func() time.Time
}

// New — с безопасными дефолтами (5/сутки, cooldown 30м после 2 ошибок, backoff 5м).
func New(store *control.Store, api BegetAPI, cfg Config) *Provisioner {
	if cfg.MaxCreatesPerDay <= 0 {
		cfg.MaxCreatesPerDay = 5
	}
	if cfg.CooldownErrors <= 0 {
		cfg.CooldownErrors = 2
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Minute
	}
	if cfg.RemoveBackoff <= 0 {
		cfg.RemoveBackoff = 5 * time.Minute
	}
	if cfg.VpsReadyTimeout <= 0 {
		cfg.VpsReadyTimeout = 10 * time.Minute
	}
	if cfg.VpsPollInterval <= 0 {
		cfg.VpsPollInterval = 10 * time.Second
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 20 * time.Minute
	}
	if cfg.Exec == nil {
		cfg.Exec = sshExecutor{}
	}
	return &Provisioner{store: store, beget: api, exec: cfg.Exec, cfg: cfg, now: time.Now}
}

// CreateRequest — запрос на создание VPS.
type CreateRequest struct {
	UserID      int64
	NodeID      string
	IdemKey     string
	Region      string
	ConfigID    string
	DisplayName string
	SSHKeyIDs   []uint32
	// ProfileID — профиль установки, выбранный оператором; персистится в
	// operations.profile_id, чтобы retry-install повторил тот же профиль.
	ProfileID int64
	// MaxNodes — бюджет мест пользователя на момент запроса; заказ (как и
	// регистрация ноды) занимает место, подтверждённый VPS его расходует.
	MaxNodes int
}

// Create — единственный путь платного заказа (синхронно). Резерв и платный
// вызов — в reserve/runCreated; используется тестами и legacy-вызовом.
func (p *Provisioner) Create(ctx context.Context, req CreateRequest) (control.Operation, error) {
	op, created, err := p.reserve(req)
	if err != nil {
		return control.Operation{}, err
	}
	if !created {
		return op, nil // гонка/повтор — CreateVps не вызывается
	}
	return p.runCreated(ctx, op, req)
}

// reserve — валидация запроса + идемпотентность + атомарный резерв.
func (p *Provisioner) reserve(req CreateRequest) (control.Operation, bool, error) {
	if strings.TrimSpace(req.IdemKey) == "" {
		return control.Operation{}, false, fmt.Errorf("%w: idem_key required", ErrValidation)
	}
	if strings.TrimSpace(req.Region) == "" {
		return control.Operation{}, false, fmt.Errorf("%w: region required", ErrValidation)
	}
	if req.NodeID == "" {
		return control.Operation{}, false, fmt.Errorf("%w: node_id required", ErrValidation)
	}
	// Идемпотентность: повтор того же ключа — та же операция, новый заказ не
	// уходит. Чужой ключ не отдаём (H4).
	if op, err := p.store.OperationByIdemKey(req.IdemKey); err == nil {
		if op.UserID != req.UserID {
			return control.Operation{}, false, ErrIdemConflict
		}
		return op, false, nil
	} else if !errors.Is(err, control.ErrNotFound) {
		return control.Operation{}, false, err
	}
	// ШЛАГБАУМ + РЕЗЕРВ + ВСЕ ЛИМИТЫ в одной транзакции (C1/H1/H3).
	op, created, err := p.store.ReserveCreateOperation(control.Operation{
		UserID: req.UserID, NodeID: req.NodeID, Kind: control.OpKindCreate,
		State: control.OpPending, IdemKey: req.IdemKey, Region: req.Region, ConfigID: req.ConfigID,
		ProfileID: req.ProfileID, RequestJSON: requestJSON(req),
	}, control.CreateLimits{
		MaxNodes:         req.MaxNodes,
		MaxCreatesPerDay: p.cfg.MaxCreatesPerDay,
		CooldownErrors:   p.cfg.CooldownErrors,
		Cooldown:         p.cfg.Cooldown,
		Now:              p.now(),
	})
	if err != nil {
		return control.Operation{}, false, mapReserveErr(err)
	}
	if !created && op.UserID != req.UserID { // гонка по чужому ключу (H4)
		return control.Operation{}, false, ErrIdemConflict
	}
	return op, created, nil
}

// runCreated — валидация региона, единственный платный CreateVps (CAS
// pending→sent) и запись результата. op должен быть в состоянии pending.
func (p *Provisioner) runCreated(ctx context.Context, op control.Operation, req CreateRequest) (control.Operation, error) {
	// Валидация каталога ДО платного вызова (неверный регион → failed, без повторов).
	if err := p.validateRegion(ctx, req.Region); err != nil {
		_, _ = p.store.ReleaseNode(op.NodeID)
		_ = p.store.UpdateOperationState(op.ID, control.OpFailed, "", providerErrMessage(err))
		op.State, op.LastError = control.OpFailed, providerErrMessage(err)
		return op, err
	}
	// R1: атомарный переход pending→sent ПЕРЕД платным вызовом.
	sent, err := p.store.MarkCreateSent(op.ID)
	if err != nil {
		return control.Operation{}, err
	}
	if !sent {
		if cur, gErr := p.store.Operation(op.ID); gErr == nil {
			return cur, nil
		}
		return control.Operation{}, nil
	}
	op.State = control.OpSent
	resp, err := p.beget.CreateVps(ctx, beget.CreateVpsRequest{
		Hostname:        hostnameFor(req.NodeID),
		DisplayName:     req.DisplayName,
		Region:          req.Region,
		ConfigurationID: req.ConfigID,
		SSHKeys:         req.SSHKeyIDs,
	})
	return p.settleCreate(op, resp, err)
}

// ProvisionBudget — бюджет фонового воркера для профиля из n шагов:
// vps_ready_timeout + (n+1)×install_timeout + запас. Именно эти два конфига
// задают потолок (H1); запросный 30-секундный ctx к воркеру неприменим.
func (p *Provisioner) ProvisionBudget(steps int) time.Duration {
	if steps < 0 {
		steps = 0
	}
	return p.cfg.VpsReadyTimeout + time.Duration(steps+1)*p.cfg.StepTimeout + 5*time.Minute
}

// InstallOnConfirmed — ожидание RUNNING (ограничено VpsReadyTimeout) и прогон
// профиля (таймаут шага — StepTimeout). Используется фоновым воркером для
// начального провижининга и для retry-install; нового CreateVps не делает.
func (p *Provisioner) InstallOnConfirmed(ctx context.Context, opID, profileID int64, sshUser string, auth sshx.Auth, vars map[string]any) (control.Operation, error) {
	op, err := p.store.Operation(opID)
	if err != nil {
		return control.Operation{}, err
	}
	switch op.State {
	case control.OpDone:
		return op, ErrAlreadyDone
	case control.OpDeleted:
		return op, fmt.Errorf("%w: operation is deleted", ErrValidation)
	}
	if op.ProviderVPSID == "" {
		return op, fmt.Errorf("%w: operation has no provider vps id", ErrValidation)
	}
	ip, ready := p.waitRunning(ctx, op.ProviderVPSID)
	if !ready {
		_ = p.store.UpdateOperationState(op.ID, control.OpWaitingVps, "", "")
		op.State, op.LastError = control.OpWaitingVps, ""
		return op, nil
	}
	installed, _, installErr := p.InstallProfile(ctx, InstallRequest{
		OperationID: op.ID, ProfileID: profileID, Host: ip,
		User: sshUser, Auth: auth, Vars: vars,
	})
	return installed, installErr
}

func (p *Provisioner) settleCreate(op control.Operation, resp *beget.CreateVpsResponse, err error) (control.Operation, error) {
	if err != nil {
		var apiErr *beget.APIError
		msg := providerErrMessage(err)
		if errors.As(err, &apiErr) && apiErr.IsValidation() {
			_ = p.store.UpdateOperationState(op.ID, control.OpFailed, "", msg)
			op.State, op.LastError = control.OpFailed, msg
			return op, err
		}
		// Таймаут/сеть/5xx — неоднозначно: unknown, только сверка GetList.
		_ = p.store.UpdateOperationState(op.ID, control.OpUnknown, "", msg)
		op.State, op.LastError = control.OpUnknown, msg
		return op, nil
	}
	if resp == nil || resp.Vps == nil || resp.Vps.ID == "" {
		_ = p.store.UpdateOperationState(op.ID, control.OpUnknown, "", "empty create response")
		op.State = control.OpUnknown
		return op, nil
	}
	_ = p.store.UpdateOperationState(op.ID, control.OpConfirmed, resp.Vps.ID, "")
	_ = p.store.UpsertCloudResource(control.CloudResource{
		VPSID: resp.Vps.ID, UserID: op.UserID, NodeID: op.NodeID,
		Region: op.Region, ConfigID: op.ConfigID, State: "active",
	})
	op.State, op.ProviderVPSID = control.OpConfirmed, resp.Vps.ID
	return op, nil
}

// Reconcile — сверка unknown-операции через GetList (по hostname из node_id).
func (p *Provisioner) Reconcile(ctx context.Context, idemKey string) (control.Operation, error) {
	op, err := p.store.OperationByIdemKey(idemKey)
	if err != nil {
		return control.Operation{}, err
	}
	return p.reconcileOp(ctx, op)
}

func (p *Provisioner) reconcileOp(ctx context.Context, op control.Operation) (control.Operation, error) {
	if op.State != control.OpUnknown {
		return op, nil
	}
	list, err := p.beget.GetList(ctx)
	if err != nil {
		return op, err
	}
	want := hostnameFor(op.NodeID)
	for _, v := range list.Vps {
		if v.Hostname == want {
			_ = p.store.UpdateOperationState(op.ID, control.OpConfirmed, v.ID, "")
			_ = p.store.UpsertCloudResource(control.CloudResource{
				VPSID: v.ID, UserID: op.UserID, NodeID: op.NodeID,
				Region: op.Region, ConfigID: op.ConfigID, State: "active",
			})
			op.State, op.ProviderVPSID = control.OpConfirmed, v.ID
			return op, nil
		}
	}
	return op, nil
}

// ReconcileUnknown — фоновая сверка всех unknown; возвращает число подтверждённых.
func (p *Provisioner) ReconcileUnknown(ctx context.Context) (int, error) {
	ops, err := p.store.UnknownOperations()
	if err != nil {
		return 0, err
	}
	confirmed := 0
	for _, op := range ops {
		updated, err := p.reconcileOp(ctx, op)
		if err == nil && updated.State == control.OpConfirmed {
			confirmed++
		}
	}
	return confirmed, nil
}

// Remove — удаление по явному действию. Ошибка провайдера НЕ очищает
// provider_vps_id; state остаётся deleting с backoff.
func (p *Provisioner) Remove(ctx context.Context, opID int64) (control.Operation, error) {
	op, err := p.store.Operation(opID)
	if err != nil {
		return control.Operation{}, err
	}
	if op.ProviderVPSID == "" {
		return op, fmt.Errorf("%w: operation has no provider vps id", ErrValidation)
	}
	if op.State == control.OpDeleting {
		if p.now().Sub(time.Unix(op.UpdatedAt, 0)) < p.cfg.RemoveBackoff {
			return op, ErrBackoff
		}
	}
	if err := p.store.UpdateOperationState(op.ID, control.OpDeleting, op.ProviderVPSID, ""); err != nil {
		return op, err
	}
	op.State = control.OpDeleting
	if err := p.beget.Remove(ctx, op.ProviderVPSID); err != nil {
		msg := providerErrMessage(err)
		_ = p.store.UpdateOperationState(op.ID, control.OpDeleting, op.ProviderVPSID, msg)
		op.LastError = msg
		return op, err
	}
	_ = p.store.UpdateOperationState(op.ID, control.OpDeleted, op.ProviderVPSID, "")
	_ = p.store.SetCloudResourceState(op.ProviderVPSID, "deleted")
	op.State = control.OpDeleted
	return op, nil
}

// ApplyProfile — прогон закреплённого профиля на VPS пользователя по SSH.
// Валидация переменных (addendum §6) — до запуска; первый упавший шаг
// прерывает профиль; значения секретов маскируются в выводе.
func (p *Provisioner) ApplyProfile(ctx context.Context, profileID int64, target sshx.Target, vars map[string]any) (RunResult, error) {
	return p.applyProfile(ctx, 0, profileID, target, vars, nil)
}

// applyProfile — общее тело ApplyProfile. opID != 0 включает пошаговый журнал
// (plan-ui-batch.md Фаза 3): один ряд operation_steps на шаг. extraSecrets —
// дополнительные значения для маскировки (совпадает с финальным last_error).
func (p *Provisioner) applyProfile(ctx context.Context, opID, profileID int64, target sshx.Target, vars map[string]any, extraSecrets []string) (RunResult, error) {
	if target.Empty() {
		return RunResult{}, ErrSSHUnavailable
	}
	pv, err := p.store.ProfileVersion(profileID, 0)
	if err != nil {
		return RunResult{}, err
	}
	if err := control.ValidateExtraVars(vars); err != nil {
		return RunResult{}, err
	}
	steps := control.ParseProfileSteps(pv.Steps)
	if len(steps) == 0 {
		return RunResult{}, fmt.Errorf("%w: profile has no steps", ErrValidation)
	}
	if err := p.exec.WaitSSH(ctx, target); err != nil {
		return RunResult{Host: target.Host}, fmt.Errorf("ssh wait: %w", err)
	}
	strVars := make(map[string]string, len(vars))
	for k, v := range vars {
		strVars[k] = fmt.Sprint(v)
	}
	secrets := append(secretValues(pv.VarsSchema, vars), extraSecrets...)
	output, runErr := p.runSteps(ctx, opID, target, steps, strVars, secrets)
	res := RunResult{Host: target.Host, Steps: len(steps), Output: truncateOutput(MaskSecrets(output, secrets))}
	return res, runErr
}

// runSteps выполняет шаги по порядку; останавливается на первой ошибке.
// При opID != 0 каждый шаг журналируется в operation_steps: status running →
// ok|error, длительность и ХВОСТ вывода шага (секреты замаскированы secrets).
func (p *Provisioner) runSteps(ctx context.Context, opID int64, target sshx.Target, steps []control.ProfileStep, vars map[string]string, secrets []string) (string, error) {
	var out strings.Builder
	for i, step := range steps {
		cmd, err := control.RenderStep(step.Command, vars)
		if err != nil {
			return out.String(), fmt.Errorf("шаг %d %q: шаблон: %w", i+1, step.Name, err)
		}
		var stepID int64
		if opID != 0 {
			if id, e := p.store.BeginOperationStep(opID, i, step.Name); e == nil {
				stepID = id
			}
		}
		// Каждый шаг — свежий bash с `-e -o pipefail` (тело на stdin): ошибка
		// середины многострочного шага или пустой вход пайпа (`curl … |
		// bash`) больше не даёт ложный «успех» по коду последней команды (H3).
		res, err := p.exec.Run(ctx, target, "bash -eo pipefail -s", cmd, p.cfg.StepTimeout)
		out.WriteString(res.Stdout)
		if res.Stderr != "" {
			out.WriteString("\n")
			out.WriteString(res.Stderr)
		}
		if stepID != 0 {
			stepOut := res.Stdout
			if res.Stderr != "" {
				stepOut += "\n" + res.Stderr
			}
			status := control.StepOK
			if err != nil || res.Code != 0 {
				status = control.StepError
			}
			_ = p.store.FinishOperationStep(stepID, status, truncateOutput(MaskSecrets(stepOut, secrets)))
		}
		if err != nil {
			return out.String(), fmt.Errorf("шаг %d/%d %q: %w", i+1, len(steps), step.Name, err)
		}
		if res.Code != 0 {
			return out.String(), fmt.Errorf("шаг %d/%d %q: код выхода %d", i+1, len(steps), step.Name, res.Code)
		}
	}
	return out.String(), nil
}

// secretValues — значения, маскируемые в выводе: объявленные secret-переменные
// профиля + конвенционально секретные ключи (node_secret/node_token/authkey/
// password/proxy_secret — node_token это общий секрет реестра, proxy_secret —
// встроенный MTProto-креденшл; оба тоже не должны утечь).
func secretValues(varsSchema string, vars map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	add := func(v any) {
		s := fmt.Sprint(v)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, name := range control.SecretVarNames(varsSchema) {
		if v, ok := vars[name]; ok {
			add(v)
		}
	}
	for _, name := range []string{"node_secret", "node_token", "authkey", "password", "proxy_secret"} {
		if v, ok := vars[name]; ok {
			add(v)
		}
	}
	return out
}

// MaskSecrets заменяет значения секретов в выводе на "***" (addendum §6).
func MaskSecrets(output string, secrets []string) string {
	for _, s := range secrets {
		if s != "" {
			output = strings.ReplaceAll(output, s, "***")
		}
	}
	return output
}

// vpsReady — статус VPS означает готовность. Пустой статус считаем готовым.
func vpsReady(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return true
	}
	for _, k := range []string{"run", "active", "started", "online", "ready"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// waitRunning ограниченно ждёт RUNNING и возвращает IP. false = окно истекло.
func (p *Provisioner) waitRunning(ctx context.Context, vpsID string) (string, bool) {
	deadline := p.now().Add(p.cfg.VpsReadyTimeout)
	for {
		if info, err := p.beget.GetInfo(ctx, vpsID); err == nil && info != nil && info.Vps != nil &&
			vpsReady(info.Vps.Status) && info.Vps.IPAddress != "" {
			return info.Vps.IPAddress, true
		}
		if !p.now().Before(deadline) {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(p.cfg.VpsPollInterval):
		}
	}
}

// InstallRequest — установка профиля на уже подтверждённый VPS.
type InstallRequest struct {
	OperationID int64
	ProfileID   int64
	// Host — IP VPS/host (становится target.Host); Port — SSH-порт (0 → 22);
	// User/Auth — SSH-креды.
	Host string
	Port int
	User string
	Auth sshx.Auth
	Vars map[string]any
	// Secrets — дополнительные значения, маскируемые в выводе прогона (секрет
	// manual-сервера: пароль/ключ не должен утечь в last_error).
	Secrets []string
	// Confirm — необязательная пост-проверка ПОСЛЕ успешных шагов и ДО перевода
	// операции в done. Ненулевая ошибка → операция failed (например, нода не
	// подтвердила регистрацию через /api/node/status). nil → без проверки.
	Confirm func() error
}

// InstallProfile проводит операцию installing → done|failed. Вывод прогона
// (обрезанный) пишется в last_error при провале. done ставится только после
// успешных шагов И успешного Confirm (если задан).
func (p *Provisioner) InstallProfile(ctx context.Context, req InstallRequest) (control.Operation, RunResult, error) {
	op, err := p.store.Operation(req.OperationID)
	if err != nil {
		return control.Operation{}, RunResult{}, err
	}
	_ = p.store.UpdateOperationState(op.ID, control.OpInstalling, "", "")
	op.State = control.OpInstalling
	// Фаза 3: журнал шагов начинается заново (retry-install) — старые ряды
	// той же операции убираем, чтобы idx не дублировался в выдаче.
	_ = p.store.ClearOperationSteps(op.ID)
	target := sshx.Target{Host: req.Host, Port: req.Port, User: orSSHUser(req.User), Auth: req.Auth}
	res, runErr := p.applyProfile(ctx, op.ID, req.ProfileID, target, req.Vars, req.Secrets)
	if runErr != nil {
		out := res.Output
		if len(req.Secrets) > 0 {
			out = MaskSecrets(out, req.Secrets)
		}
		// Причина провала (индекс/имя шага + код выхода) идёт ПЕРВОЙ, чтобы
		// её не срезало truncateOutput; хвост вывода — после разделителя.
		msg := truncateOutput(runErr.Error() + "\n---\n" + strings.TrimSpace(out))
		_ = p.store.UpdateOperationState(op.ID, control.OpFailed, "", msg)
		op.State, op.LastError = control.OpFailed, msg
		return op, res, runErr
	}
	if req.Confirm != nil {
		if err := req.Confirm(); err != nil {
			msg := truncateOutput(err.Error())
			_ = p.store.UpdateOperationState(op.ID, control.OpFailed, "", msg)
			op.State, op.LastError = control.OpFailed, msg
			return op, res, err
		}
	}
	_ = p.store.UpdateOperationState(op.ID, control.OpDone, "", "")
	op.State, op.LastError = control.OpDone, ""
	return op, res, nil
}

// Provision — полный поток создания VPS (addendum §4.3): SSH-гейт → Create
// (ровно один платный заказ) → IP → установка шагов → installing → done.
// Отсутствие SSH-кредов → durable операция failed БЕЗ платного заказа.
func (p *Provisioner) Provision(ctx context.Context, req CreateRequest, profileID int64, sshUser string, auth sshx.Auth, vars map[string]any) (control.Operation, error) {
	req.ProfileID = profileID
	if auth.Empty() {
		op, _, err := p.store.CreateOperation(control.Operation{
			UserID: req.UserID, NodeID: req.NodeID, Kind: control.OpKindCreate,
			State: control.OpFailed, IdemKey: req.IdemKey, Region: req.Region,
			ConfigID: req.ConfigID, ProfileID: req.ProfileID, LastError: ErrSSHUnavailable.Error(),
		})
		if err != nil {
			return control.Operation{}, err
		}
		_, _ = p.store.ReleaseNode(req.NodeID)
		return op, ErrSSHUnavailable
	}
	op, err := p.Create(ctx, req)
	if err != nil {
		return op, err
	}
	if op.State != control.OpConfirmed {
		return op, nil // pending/sent/unknown/failed — установку не запускаем
	}
	return p.InstallOnConfirmed(ctx, op.ID, profileID, sshUser, auth, vars)
}

// RetryInstall — повтор установки на УЖЕ созданный VPS: только GetInfo +
// InstallProfile, НИКАКОГО CreateVps (addendum §7.2). Отказ, если VPS ещё не
// создан, операция активна или уже done/deleted. Профиль берётся из
// operations.profile_id; profileID — fallback для legacy-операций (0).
func (p *Provisioner) RetryInstall(ctx context.Context, opID, profileID int64, sshUser string, auth sshx.Auth, vars map[string]any) (control.Operation, error) {
	op, err := p.store.Operation(opID)
	if err != nil {
		return control.Operation{}, err
	}
	if op.ProfileID != 0 {
		profileID = op.ProfileID
	}
	if op.ProviderVPSID == "" {
		return op, fmt.Errorf("%w: operation has no provider vps id", ErrValidation)
	}
	switch op.State {
	case control.OpDone:
		return op, ErrAlreadyDone
	case control.OpDeleted:
		return op, fmt.Errorf("%w: operation is deleted", ErrValidation)
	case control.OpPending, control.OpSent, control.OpUnknown, control.OpInstalling, control.OpDeleting:
		return op, ErrInProgress
	}
	if auth.Empty() {
		_ = p.store.UpdateOperationState(op.ID, control.OpFailed, "", ErrSSHUnavailable.Error())
		op.State, op.LastError = control.OpFailed, ErrSSHUnavailable.Error()
		return op, ErrSSHUnavailable
	}
	return p.InstallOnConfirmed(ctx, op.ID, profileID, sshUser, auth, vars)
}

func orSSHUser(s string) string {
	if strings.TrimSpace(s) == "" {
		return "root"
	}
	return s
}

// truncateOutput ограничивает вывод прогона в last_error, сохраняя ХВОСТ:
// причина провала обычно в конце вывода (в начале — баннер установщика).
func truncateOutput(s string) string {
	const max = 16000
	if len(s) <= max {
		return s
	}
	return "…(truncated) " + s[len(s)-max:]
}

// ── внутреннее ────────────────────────────────────────────────────

// providerErrMessage — безопасное для пользователя сообщение об ошибке
// провайдера (MED-6): без сырого тела ответа и request-id.
func providerErrMessage(err error) string {
	if err == nil {
		return ""
	}
	var apiErr *beget.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Summary()
	}
	return err.Error()
}

// mapReserveErr переводит sentinel-ошибки резерва control в ошибки provision.
func mapReserveErr(err error) error {
	switch {
	case errors.Is(err, control.ErrQuotaExceeded):
		return ErrQuotaExceeded
	case errors.Is(err, control.ErrRateLimited):
		return ErrRateLimited
	case errors.Is(err, control.ErrCooldown):
		return ErrCooldown
	case errors.Is(err, control.ErrCreateInProgress):
		return ErrInProgress
	default:
		return err
	}
}

func (p *Provisioner) validateRegion(ctx context.Context, region string) error {
	regions, err := p.beget.GetRegions(ctx)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	for _, r := range regions {
		if r.ID == region && r.Available {
			return nil
		}
	}
	return fmt.Errorf("%w: region %q not in catalog or unavailable", ErrValidation, region)
}

// hostnameFor — детерминированный hostname для сопоставления при сверке.
func hostnameFor(nodeID string) string {
	return "sharedd-" + strings.ToLower(strings.TrimSpace(nodeID))
}

func requestJSON(req CreateRequest) string {
	b, _ := json.Marshal(map[string]any{
		"region": req.Region, "config_id": req.ConfigID, "hostname": hostnameFor(req.NodeID),
	})
	return string(b)
}
