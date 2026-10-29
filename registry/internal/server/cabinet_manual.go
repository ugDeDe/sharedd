package server

// Свои серверы (docs/specs/addendum-manual-servers.md §2): пользователь
// подключает уже существующий хост (IP + пароль ИЛИ SSH-ключ). Провайдер НЕ
// участвует, платных вызовов нет. Установка переиспользует ТОТ ЖЕ Executor и
// шаги профиля, что и Beget (`provision.InstallProfile`), только источник
// сервера другой: `beget` (платный заказ) vs `manual` (готовый хост).
//
// Деньги-инварианты: одно место в квоте на сервер (AuthorizeNode), done ТОЛЬКО
// после подтверждённой регистрации ноды (/api/node/status), abort на первом
// упавшем шаге. Секрет (пароль/ключ) шифруется AES-GCM (master.key), наружу не
// отдаётся ни одним эндпоинтом и маскируется в operations.output.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/provision"
	"sharedd/registry/internal/sshx"
)

const (
	manualHostDefaultPort = 22
	manualHostDefaultUser = "root"
	maxManualKeyBytes     = 16 << 10 // 16 KiB
)

var manualSSHUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// manualLabelRe — имя ноды (1..10: латиница/цифры/._-, без ._- по краям). Тот
// же формат, что name-часть NAME-HASH: агент добавит свой -<5hex> при
// регистрации, поэтому label обязан уложиться в installer `--name`.
var manualLabelRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,8}[A-Za-z0-9])?$`)

func (r *Registry) mountMeServers(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/servers", r.requireSession(r.handleMeServersList))
	mux.HandleFunc("POST /api/me/servers", r.requireSession(r.requireCSRF(r.handleMeServerCreate)))
	mux.HandleFunc("POST /api/me/servers/{id}/install", r.requireSession(r.requireCSRF(r.handleMeServerInstall)))
	mux.HandleFunc("POST /api/me/servers/{id}/check", r.requireSession(r.requireCSRF(r.handleMeServerCheck)))
	mux.HandleFunc("DELETE /api/me/servers/{id}", r.requireSession(r.requireCSRF(r.handleMeServerDelete)))
}

// ── представление (без секретов) ──────────────────────────────────

type manualHostJSON struct {
	ID         int64          `json:"id"`
	Label      string         `json:"label"`
	Host       string         `json:"host"`
	Port       int            `json:"port"`
	SSHUser    string         `json:"ssh_user"`
	AuthType   string         `json:"auth_type"`
	CreatedAt  int64          `json:"created_at"`
	VerifiedAt int64          `json:"verified_at,omitempty"`
	LastError  string         `json:"last_error,omitempty"`
	NodeID     string         `json:"node_id,omitempty"`
	Operation  *operationJSON `json:"operation,omitempty"`
}

func toManualHostJSON(h control.ManualHost) manualHostJSON {
	return manualHostJSON{
		ID: h.ID, Label: h.Label, Host: h.Host, Port: h.Port, SSHUser: h.SSHUser,
		AuthType: h.AuthType, CreatedAt: h.CreatedAt, VerifiedAt: h.VerifiedAt,
		LastError: h.LastError, NodeID: h.NodeID,
	}
}

// ── GET /api/me/servers ───────────────────────────────────────────

func (r *Registry) handleMeServersList(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	hosts, err := r.control.ManualHostsByUser(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]manualHostJSON, 0, len(hosts))
	for _, h := range hosts {
		v := toManualHostJSON(h)
		if h.OperationID != 0 {
			if op, err := r.control.Operation(h.OperationID); err == nil && op.UserID == u.ID {
				oj := toOperationJSON(op)
				v.Operation = &oj
			}
		}
		out = append(out, v)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"servers": out})
}

// ── POST /api/me/servers ──────────────────────────────────────────

type manualHostBody struct {
	Label      string `json:"label"`
	Host       string `json:"host"`
	Port       *int   `json:"port"`
	SSHUser    string `json:"ssh_user"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
}

// normalizeManualHost валидирует тело и возвращает нормализованную запись
// (без шифрования) + плейнтекст секрета. Правила точно по контракту §2:
// label обязателен, host — публичный IPv4 (не loopback/private/link-local и не
// IP регистратора), port 1..65535 (дефолт 22), ssh_user шаблон, ровно одно из
// password|private_key, приватный ключ — валидный PEM/OpenSSH ≤ 16 KiB.
func (r *Registry) normalizeManualHost(b manualHostBody) (control.ManualHost, string, error) {
	// B1: все сообщения валидации — по-русски (кабинет русскоязычный);
	// machine-API коды (validation_failed и т.п.) не меняются. Общий хелпер
	// validatePublicIPv4 остаётся англоязычным для агента — причину различаем
	// локально, чтобы не менять общий контракт.
	label := strings.TrimSpace(b.Label)
	if !manualLabelRe.MatchString(label) {
		return control.ManualHost{}, "", errors.New("укажите название: 1..10 символов — латиница, цифры, . _ - (без . _ - по краям); оно станет именем ноды")
	}
	host := strings.TrimSpace(b.Host)
	if ip, perr := netip.ParseAddr(host); perr != nil || !ip.Is4() {
		return control.ManualHost{}, "", errors.New("IP-адрес должен быть корректным IPv4-адресом")
	} else if err := validatePublicIPv4(host); err != nil {
		return control.ManualHost{}, "", errors.New("IP-адрес должен быть публичным и маршрутизируемым")
	}
	if rip := r.registryIP(); rip != "" && host == rip {
		return control.ManualHost{}, "", errors.New("нельзя подключить сам регистратор как свой сервер")
	}
	port := manualHostDefaultPort
	if b.Port != nil {
		port = *b.Port
		if port < 1 || port > 65535 {
			return control.ManualHost{}, "", errors.New("порт должен быть в диапазоне 1..65535")
		}
	}
	sshUser := strings.TrimSpace(b.SSHUser)
	if sshUser == "" {
		sshUser = manualHostDefaultUser
	}
	if !manualSSHUserRe.MatchString(sshUser) {
		return control.ManualHost{}, "", errors.New("пользователь SSH должен соответствовать [a-z_][a-z0-9_-]{0,31}")
	}
	hasPass := b.Password != ""
	hasKey := strings.TrimSpace(b.PrivateKey) != ""
	if hasPass == hasKey {
		return control.ManualHost{}, "", errors.New("укажите ровно одно: пароль или приватный ключ")
	}
	authType, secret := "password", b.Password
	if hasKey {
		key := b.PrivateKey
		if len(key) > maxManualKeyBytes {
			return control.ManualHost{}, "", errors.New("приватный ключ превышает 16 КиБ")
		}
		if err := sshx.ValidatePrivateKey(key); err != nil {
			return control.ManualHost{}, "", errors.New("приватный ключ не является корректным PEM/OpenSSH-ключом")
		}
		authType, secret = "key", key
	}
	h := control.ManualHost{
		Label: label, Host: host, Port: port, SSHUser: sshUser, AuthType: authType,
	}
	return h, secret, nil
}

// registryIP — публичный IP самого регистратора, если panel.public_url задан
// IP-адресом (иначе пусто: проверить нечем). Нужен, чтобы пользователь не
// подключил сам реестр как «свой сервер» (маршрутизация к себе).
func (r *Registry) registryIP() string {
	r.cfgMu.RLock()
	pu := r.cfg.Panel.PublicURL
	r.cfgMu.RUnlock()
	if pu == "" {
		return ""
	}
	u, err := url.Parse(pu)
	if err != nil {
		return ""
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.Is4() {
		return ""
	}
	return ip.String()
}

func (r *Registry) handleMeServerCreate(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	u := sessionFrom(req).user
	var body manualHostBody
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "некорректный запрос")
		return
	}
	h, secret, err := r.normalizeManualHost(body)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	enc, err := cryptoutil.Seal(r.masterKey, secret)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	h.UserID = u.ID
	h.SecretEnc = enc
	h.CreatedAt = time.Now().Unix()
	created, err := r.control.CreateManualHost(h)
	if err != nil {
		// B2: тот же host:port у того же пользователя → 409 duplicate_host.
		if errors.Is(err, control.ErrDuplicate) {
			controlAPIError(w, http.StatusConflict, "duplicate_host",
				"Этот адрес уже подключён. Откройте его в списке или удалите прежнюю запись.")
			return
		}
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusCreated, toManualHostJSON(created))
}

// ── DELETE /api/me/servers/{id} ───────────────────────────────────

func (r *Registry) handleMeServerDelete(w http.ResponseWriter, req *http.Request) {
	host, ok := r.ownedManualHost(w, req)
	if !ok {
		return
	}
	if err := r.control.DeleteManualHost(host.ID); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	// Pending-бронь установки снимается в той же транзакции, что и запись
	// (DeleteManualHost); claim'нутая операция pending-строки уже не имеет.
	// Место освобождается, нода остаётся в пуле (владелец → legacy;
	// физическое удаление ноды — существующим механизмом DELETE /api/me/nodes).
	if host.NodeID != "" {
		if _, err := r.control.ReleaseNode(host.NodeID); err != nil {
			r.writeControlUnavailable(w)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ownedManualHost — загрузка записи с owner-проверкой: чужая/неизвестная → 404
// (существование не подтверждаем). false = ответ записан.
func (r *Registry) ownedManualHost(w http.ResponseWriter, req *http.Request) (control.ManualHost, bool) {
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "некорректный идентификатор сервера")
		return control.ManualHost{}, false
	}
	host, err := r.control.ManualHost(id)
	switch {
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "сервер не найден")
		return control.ManualHost{}, false
	case err != nil:
		if req.Pattern == "POST /api/me/servers/{id}/install" {
			writeManualInstallErr(w, err, "manual.install.lookup_host")
			return control.ManualHost{}, false
		}
		r.writeControlUnavailable(w)
		return control.ManualHost{}, false
	}
	if host.UserID != sessionFrom(req).user.ID {
		controlAPIError(w, http.StatusNotFound, "not_found", "сервер не найден")
		return control.ManualHost{}, false
	}
	return host, true
}

// ── POST /api/me/servers/{id}/check ───────────────────────────────

func (r *Registry) handleMeServerCheck(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	host, ok := r.ownedManualHost(w, req)
	if !ok {
		return
	}
	auth, err := r.manualHostAuth(host)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "ssh_unavailable", "сохранённый SSH-секрет непригоден")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	target := sshx.Target{Host: host.Host, Port: host.Port, User: host.SSHUser, Auth: auth}
	lastErr := ""
	if err := r.executor().WaitSSH(ctx, target); err != nil {
		lastErr = "ssh: " + err.Error()
	} else if res, err := r.executor().Run(ctx, target, "cat /etc/os-release", "", 20*time.Second); err != nil || res.Code != 0 {
		lastErr = "ssh command failed"
	}
	verifiedAt := int64(0)
	if lastErr == "" {
		verifiedAt = time.Now().Unix()
	}
	if err := r.control.SetManualHostCheck(host.ID, verifiedAt, lastErr); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"verified": lastErr == "", "last_error": lastErr})
}

// ── POST /api/me/servers/{id}/install ─────────────────────────────

func (r *Registry) handleMeServerInstall(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	u := sessionFrom(req).user
	host, ok := r.ownedManualHost(w, req)
	if !ok {
		return
	}
	var body struct {
		ProfileID int64          `json:"profile_id"`
		Vars      map[string]any `json:"vars"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil && !errors.Is(err, io.EOF) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "некорректный запрос")
		return
	}
	r.nodeClaimMu.Lock()
	defer r.nodeClaimMu.Unlock()

	var existing control.Operation
	if op, err := r.control.ManualInstallOperation(host); err == nil {
		if op.UserID != u.ID {
			controlAPIError(w, http.StatusNotFound, "not_found", "операция не найдена")
			return
		}
		existing = op
		if op.Kind != control.OpKindManualInstall {
			writeManualInstallErr(w, control.ErrOwnerMismatch, "manual.install.operation_kind")
			return
		}
		// A worker can have written failed/done but not yet left its running map.
		if r.provisionWorkers().runningOp(op.ID) {
			writeManualInstallErr(w, control.ErrCreateInProgress, "manual.install.running_worker")
			return
		}
		// OpDone НЕ отклоняем: это переустановка (reinstall) — операция
		// перезапускается с тем же id/idem_key, возможно другим профилем.
		switch op.State {
		case control.OpDeleted:
			controlAPIError(w, http.StatusBadRequest, "no_vps", "операция удалена")
			return
		case control.OpPending, control.OpSent, control.OpUnknown, control.OpDeleting:
			controlAPIError(w, http.StatusConflict, "in_progress", "установка уже выполняется")
			return
		}
	} else if !errors.Is(err, control.ErrNotFound) {
		writeManualInstallErr(w, err, "manual.install.lookup_operation")
		return
	}

	// Профиль: при первом install и переустановке — выбранный пользователем
	// (пусто = штатный); при retry без выбора — прежний профиль операции.
	effectiveProfile := existing.ProfileID
	if body.ProfileID != 0 {
		effectiveProfile = body.ProfileID
	}
	pv, err := r.selectableProfileVersion(u, effectiveProfile)
	if err != nil {
		logProfileControlErr("manual.install.select_profile", err)
		writeProfileSelectErr(w, err)
		return
	}
	vars, err := r.mergeInstallVars(u.ID, pv, body.Vars)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	vars, err = r.provisionBaseVars(vars)
	if err != nil {
		writeManualInstallErr(w, err, "manual.install.validate_vars")
		return
	}
	auth, err := r.manualHostAuth(host)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "ssh_unavailable", "сохранённый SSH-секрет непригоден")
		return
	}
	// Claim-on-first-register: node_id ещё неизвестен, секрет кладём как
	// pending. Retry переиспользует ТОТ ЖЕ pending-секрет до claim, не ротируя
	// токен живого агента.
	secret, enc, err := r.manualPendingSecret(existing)
	if err != nil {
		writeManualInstallErr(w, err, "manual.install.prepare_secret")
		return
	}
	vars["label"], vars["node_secret"] = host.Label, secret
	if err := control.ValidateExtraVars(vars); err != nil {
		writeManualInstallErr(w, err, "manual.install.validate_injected_vars")
		return
	}
	op, err := r.control.AuthorizeManualOperation(host, existing, u, pv.ProfileID, secret, enc)
	if err != nil {
		writeManualInstallErr(w, err, "manual.install.accept")
		return
	}

	secretForMask := auth.Password
	if auth.Type == "key" {
		secretForMask = auth.Key
	}
	prov := provision.New(r.control, nil, r.provisionConfig())
	steps := len(control.ParseProfileSteps(pv.Steps))
	if !r.provisionWorkers().run(op.ID, prov.ProvisionBudget(steps), func(wctx context.Context) {
		_, _, _ = prov.InstallProfile(wctx, provision.InstallRequest{
			OperationID: op.ID, ProfileID: pv.ProfileID,
			Host: host.Host, Port: host.Port, User: host.SSHUser, Auth: auth,
			Vars: vars, Secrets: []string{secretForMask},
			// done ТОЛЬКО после подтверждённой регистрации: /register claim'ит
			// pending-секрет и заполняет operation.node_id.
			Confirm: func() error {
				got, err := r.control.Operation(op.ID)
				if err != nil {
					return err
				}
				if got.NodeID == "" {
					return errors.New("node did not register with the registry")
				}
				return nil
			},
		})
	}) {
		writeManualInstallErr(w, control.ErrCreateInProgress, "manual.install.start_worker")
		return
	}
	writeCabinetJSON(w, http.StatusOK, toOperationJSON(op))
}

func writeManualInstallErr(w http.ResponseWriter, err error, operation string) {
	logProfileControlErr(operation, err)
	switch {
	case errors.Is(err, control.ErrOwnerMismatch):
		controlAPIError(w, http.StatusForbidden, "owner_mismatch", "личность ноды занята или не подтверждена этой операцией")
	case errors.Is(err, control.ErrQuotaExceeded):
		controlAPIError(w, http.StatusForbidden, "quota_exceeded", "достигнут лимит нод")
	case errors.Is(err, control.ErrUserDisabled):
		controlAPIError(w, http.StatusForbidden, "user_disabled", "учётная запись отключена")
	case errors.Is(err, control.ErrCreateInProgress):
		controlAPIError(w, http.StatusConflict, "in_progress", "установка уже выполняется или состояние изменилось")
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "некорректные переменные установки")
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "сервер не найден")
	default:
		controlAPIError(w, http.StatusInternalServerError, "internal", "не удалось подготовить установку; обратитесь к администратору")
	}
}

// nodeClaimMu is held through acceptance and worker dispatch, and through
// /register authorization and candidate insertion, closing the claim race.
//
// manualPendingSecret возвращает секрет pending-брони установки: на retry —
// тот же, из восстановимой AES-GCM копии; иначе — свежий. Ротация живого
// токена запрещена: нечитаемая копия → ошибка, а не новый секрет.
//
// Переустановка (existing.State == OpDone) ИДЁТ СЮДА ЖЕ: pending уже claim'нут,
// node_id непустой, поэтому секрет берётся из node_owners и ПЕРЕИСПОЛЬЗУЕТСЯ
// (без ротации). AuthorizeManualOperation снимает прежнюю привязку и кладёт
// тот же секрет как pending; нода перезаявляется claim-on-first-register —
// идемпотентно при сохранённом /var/lib/sharedd/node_id, корректно и при
// смене node_id (старая строка node_owners не остаётся сиротой).
func (r *Registry) manualPendingSecret(existing control.Operation) (string, string, error) {
	if existing.ID != 0 {
		p, err := r.control.PendingSecretByOperation(existing.ID)
		switch {
		case err == nil:
			secret, oerr := cryptoutil.Open(r.masterKey, p.SecretEnc)
			if oerr != nil {
				return "", "", fmt.Errorf("stored pending secret is not recoverable; refusing to reissue: %w", oerr)
			}
			if secret == "" {
				return "", "", errors.New("stored pending secret is not recoverable; refusing to reissue")
			}
			return secret, p.SecretEnc, nil
		case errors.Is(err, control.ErrNotFound):
			// pending уже claim'нут: нода привязана и секрет лежит в
			// node_owners. Retry И переустановка переиспользуют ЕГО, не ротируя.
		default:
			return "", "", err
		}
		if existing.NodeID != "" {
			enc, err := r.control.NodeSecretEnc(existing.NodeID)
			if err != nil {
				return "", "", err
			}
			secret, oerr := cryptoutil.Open(r.masterKey, enc)
			if oerr != nil {
				return "", "", fmt.Errorf("stored node secret is not recoverable; refusing to reissue: %w", oerr)
			}
			if secret == "" {
				return "", "", errors.New("stored node secret is not recoverable; refusing to reissue")
			}
			return secret, enc, nil
		}
	}
	secret := randomURLSecret(32)
	enc, err := cryptoutil.Seal(r.masterKey, secret)
	if err != nil {
		return "", "", err
	}
	return secret, enc, nil
}

// manualHostAuth — расшифрованный SSH-креды записи.
func (r *Registry) manualHostAuth(h control.ManualHost) (sshx.Auth, error) {
	plain, err := r.revealManualSecret(h)
	if err != nil {
		return sshx.Auth{}, err
	}
	if plain == "" {
		return sshx.Auth{}, errors.New("stored secret is empty")
	}
	if h.AuthType == "key" {
		return sshx.Auth{Type: "key", Key: plain}, nil
	}
	return sshx.Auth{Type: "password", Password: plain}, nil
}

// revealManualSecret — плейнтекст секрета (для SSH-аутентификации и маскировки
// в выводе). Никогда не логируется и не отдаётся наружу.
func (r *Registry) revealManualSecret(h control.ManualHost) (string, error) {
	return cryptoutil.Open(r.masterKey, h.SecretEnc)
}

// executor — исполнитель шагов: инъектированный (тесты) или боевой SSH.
func (r *Registry) executor() provision.Executor {
	if r.sshExec != nil {
		return r.sshExec
	}
	return provision.SSHExecutor()
}
