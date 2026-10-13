package server

// Beget-секция кабинета (docs/api-contract.md §2.1, docs/specs/beget-provisioning.md):
// связка личного Beget-аккаунта (секреты шифруются AES-GCM, наружу не отдаются),
// каталог регионов, заказ VPS через provision-пакет (ровно один CreateVps на
// idem_key, операция персистится ДО платного вызова) и журнал операций.
// Реальные вызовы Beget в тестах подменяются фабрикой клиента.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"sharedd/registry/internal/beget"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/provision"
	"sharedd/registry/internal/sshx"
)

// begetClient — поверхность провайдера, нужная кабинету: проверка кредов,
// регистрация SSH-ключа и всё, что требует provision (создание/удаление/каталог).
type begetClient interface {
	Auth(ctx context.Context, code string) error
	AddSSHKey(ctx context.Context, name, pubKey string) (*beget.SSHKey, error)
	GetRegions(ctx context.Context) ([]beget.RegionInfo, error)
	GetConfigurations(ctx context.Context) ([]beget.ConfigurationInfo, error)
	CreateVps(ctx context.Context, req beget.CreateVpsRequest) (*beget.CreateVpsResponse, error)
	GetInfo(ctx context.Context, id string) (*beget.GetInfoResponse, error)
	GetList(ctx context.Context) (*beget.GetListResponse, error)
	Remove(ctx context.Context, id string) error
}

// begetClientFor — фабрика клиента (в тестах подменяется на мок).
func (r *Registry) begetClientFor(login, password string) begetClient {
	if r.begetFactory != nil {
		return r.begetFactory(login, password)
	}
	return beget.New(login, password, "")
}

// requireMasterKey — без AES-GCM ключа beget-секция отвечает 503 (секреты
// хранить негде). false = ответ записан.
func (r *Registry) requireMasterKey(w http.ResponseWriter) bool {
	if len(r.masterKey) == 0 {
		controlAPIError(w, http.StatusServiceUnavailable, "secrets_unavailable", "master key is not loaded")
		return false
	}
	return true
}

func providerCtx(req *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(req.Context(), 30*time.Second)
}

// ── GET/PUT /api/me/beget/link ────────────────────────────────────

type begetLinkJSON struct {
	Verified  bool   `json:"verified"`
	LastError string `json:"last_error,omitempty"`
}

func (r *Registry) handleBegetLinkGet(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	link, err := r.control.BegetLink(u.ID)
	switch {
	case errors.Is(err, control.ErrNotFound):
		writeCabinetJSON(w, http.StatusOK, begetLinkJSON{Verified: false})
		return
	case err != nil:
		r.writeControlUnavailable(w)
		return
	}
	// Секреты (login/password/ssh_priv) наружу не отдаются в принципе.
	writeCabinetJSON(w, http.StatusOK, begetLinkJSON{Verified: link.VerifiedAt > 0, LastError: link.LastError})
}

func (r *Registry) handleBegetLinkPut(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	u := sessionFrom(req).user
	var body struct {
		Login    string `json:"login"`
		Password string `json:"password"`
		// Code — необязательный 2FA/verification код (см. beget.Client.Auth).
		Code string `json:"code"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if strings.TrimSpace(body.Login) == "" || body.Password == "" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "login and password are required")
		return
	}
	ctx, cancel := providerCtx(req)
	defer cancel()
	client := r.begetClientFor(body.Login, body.Password)
	if err := client.Auth(ctx, strings.TrimSpace(body.Code)); err != nil {
		reason, msg := begetAuthFailure(err)
		// Диагностика для оператора: статус/код/сообщение провайдера, пароль
		// НИКОГДА не логируется (логин маскируется).
		status, code, pmsg := 0, "", ""
		var apiErr *beget.APIError
		if errors.As(err, &apiErr) {
			status, code, pmsg = apiErr.HTTPStatus, apiErr.Code, apiErr.Message
		} else {
			pmsg = err.Error()
		}
		log.Printf("beget auth failed: login=%s status=%d code=%s message=%s", maskBegetLogin(body.Login), status, code, pmsg)
		detail := code
		if detail == "" {
			detail = reason
		}
		_ = r.control.SetBegetLinkError(u.ID, "auth failed: "+detail)
		writeCabinetJSON(w, http.StatusBadRequest, map[string]string{
			"error": "beget_auth_failed", "message": msg, "reason": reason,
		})
		return
	}
	pub, priv, err := cryptoutil.GenerateSSHKeyPair()
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	key, err := client.AddSSHKey(ctx, "sharedd-"+u.Username, pub)
	if err != nil || key == nil {
		_ = r.control.SetBegetLinkError(u.ID, "ssh key registration failed")
		controlAPIError(w, http.StatusBadRequest, "beget_sshkey_failed", "could not register SSH key")
		return
	}
	loginEnc, err := cryptoutil.Seal(r.masterKey, body.Login)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	passEnc, err := cryptoutil.Seal(r.masterKey, body.Password)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	privEnc, err := cryptoutil.Seal(r.masterKey, priv)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	now := time.Now().Unix()
	if err := r.control.UpsertBegetLink(control.BegetLink{
		UserID: u.ID, LoginEnc: loginEnc, PasswordEnc: passEnc,
		SSHKeyID: key.ID, SSHPub: pub, SSHPrivEnc: privEnc,
		CreatedAt: now, VerifiedAt: now,
	}); err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"verified": true, "ssh_pub": pub})
}

// begetRateLimitedMessage — единый текст для троттлинга Beget (HTTP 429/403)
// и code-признаков лимита. Прямо предупреждает не повторять сразу: частые
// повторы продлевают блокировку.
const begetRateLimitedMessage = "Beget временно ограничил попытки входа для этого логина. Подождите примерно 30 минут и попробуйте один раз; частые повторы продлевают блокировку."

// begetAuthFailure маппит ошибку провайдера в machine-readable reason (для
// подсветки поля 2FA в UI) и конкретное русское сообщение. code — код/текст
// от Beget (аккаунт пользователя, не секрет; может попасть в ответ).
//
// HTTP-статус проверяется ПЕРЕД кодом: при троттлинге Beget отдаёт 429 (иногда
// 403) с ПУСТЫМ телом, то есть без code. Раньше такой ответ падал в generic
// provider_error «Не удалось связаться», что дезориентировало и провоцировало
// немедленный повтор, продлевающий блокировку.
func begetAuthFailure(err error) (reason, message string) {
	code, status := "", 0
	var apiErr *beget.APIError
	if errors.As(err, &apiErr) {
		status = apiErr.HTTPStatus
		code = apiErr.Code
		if code == "" {
			code = apiErr.Message
		}
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusForbidden:
		return "rate_limited", begetRateLimitedMessage
	}
	up := strings.ToUpper(code)
	switch {
	case up == "INCORRECT_CREDENTIALS":
		return "bad_credentials", "Неверный логин или пароль Beget. Нужен ЛОГИН аккаунта Beget (не e-mail); включите доступ к API на cp.beget.com/settings/security/api и отключите 2FA — API Beget не работает с 2FA."
	case strings.Contains(up, "CODE") || strings.Contains(up, "TOTP") || strings.Contains(up, "2FA") || strings.Contains(up, "VERIFY"):
		return "code_required", "Beget требует код подтверждения (2FA). Заполните поле «Код (2FA)» и повторите."
	case strings.Contains(up, "RATE") || strings.Contains(up, "LIMIT") || strings.Contains(up, "CAPTCHA") || strings.Contains(up, "TOO_MANY"):
		return "rate_limited", begetRateLimitedMessage
	default:
		if code == "" {
			if status == 0 {
				return "provider_error", "Не удалось связаться с Beget. Проверьте доступность api.beget.com и повторите."
			}
			code = fmt.Sprintf("HTTP %d", status)
		}
		return "provider_error", "Beget отклонил вход (код: " + code + ")"
	}
}

// maskBegetLogin — логин в логе маскируется (PII), пароль не логируется вовсе.
func maskBegetLogin(login string) string {
	if login == "" {
		return "***"
	}
	return login[:1] + "***"
}

// ── GET /api/me/beget/regions ─────────────────────────────────────

func (r *Registry) verifiedBegetClient(w http.ResponseWriter, req *http.Request) (begetClient, bool) {
	if !r.requireMasterKey(w) {
		return nil, false
	}
	u := sessionFrom(req).user
	link, err := r.control.BegetLink(u.ID)
	switch {
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusBadRequest, "beget_not_linked", "link your Beget account first")
		return nil, false
	case err != nil:
		r.writeControlUnavailable(w)
		return nil, false
	}
	if link.VerifiedAt == 0 {
		controlAPIError(w, http.StatusBadRequest, "beget_not_verified", "Beget link is not verified")
		return nil, false
	}
	login, err := cryptoutil.Open(r.masterKey, link.LoginEnc)
	if err != nil {
		r.writeControlUnavailable(w)
		return nil, false
	}
	password, err := cryptoutil.Open(r.masterKey, link.PasswordEnc)
	if err != nil {
		r.writeControlUnavailable(w)
		return nil, false
	}
	return r.begetClientFor(login, password), true
}

func (r *Registry) handleBegetRegions(w http.ResponseWriter, req *http.Request) {
	client, ok := r.verifiedBegetClient(w, req)
	if !ok {
		return
	}
	ctx, cancel := providerCtx(req)
	defer cancel()
	regions, err := client.GetRegions(ctx)
	if err != nil {
		controlAPIError(w, http.StatusBadGateway, "provider_error", "could not load regions")
		return
	}
	if regions == nil {
		regions = []beget.RegionInfo{}
	}
	// Каталог тарифов с ценой (addendum-profiles-vps.md §4.1): цена null,
	// если провайдер её не отдал — без выдумывания.
	configs, err := client.GetConfigurations(ctx)
	if err != nil {
		controlAPIError(w, http.StatusBadGateway, "provider_error", "could not load configurations")
		return
	}
	if configs == nil {
		configs = []beget.ConfigurationInfo{}
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"regions": regions, "configurations": configs})
}

// ── POST /api/me/vps ──────────────────────────────────────────────

type operationJSON struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id,omitempty"`
	Kind      string `json:"kind"`
	State     string `json:"state"`
	ProfileID int64  `json:"profile_id"`
	LastError string `json:"last_error,omitempty"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// HasVPS — у операции есть хост для повторной установки: созданный VPS
	// (provider_vps_id непустой) или manual-сервер (kind=manual_install —
	// хост есть всегда). Сам provider_vps_id наружу не отдаём.
	HasVPS bool `json:"has_vps"`
	// Steps — пошаговый журнал установки (plan-ui-batch.md Фаза 3).
	Steps []operationStepJSON `json:"steps,omitempty"`
}

// operationStepJSON — шаг операции для раскрывающегося лога кабинета.
type operationStepJSON struct {
	Idx        int    `json:"idx"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	DurationMs int64  `json:"duration_ms"`
	Output     string `json:"output,omitempty"`
}

func toOperationJSON(o control.Operation) operationJSON {
	return operationJSON{
		ID: o.ID, NodeID: o.NodeID, Kind: o.Kind, State: o.State,
		ProfileID: o.ProfileID,
		LastError: o.LastError, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
		HasVPS: o.ProviderVPSID != "" || o.Kind == control.OpKindManualInstall,
	}
}

// nodeIDForIdem — детерминированный node_id из idem_key (формат NAME-HASH:
// имя 1..4 символа + 5 lowercase-hex hash, всего <=10 символов). Длина
// критична: старый install_node_web.sh на нодах валидирует `--name` как
// 1..10 символов, а 15-символьный id ("vpsXXXXXX-XXXXX") установщик
// отвергает и регистрация не происходит. Отображаем 8 hex sha256(idem_key)
// (32 бита); повтор ключа даёт ту же ноду (сверка получает стабильный
// hostname). Итог: "p<3 hex>-<5 hex>", напр. "p3f2a-1b4c9".
func nodeIDForIdem(idemKey string) string {
	sum := sha256.Sum256([]byte(idemKey))
	h := hex.EncodeToString(sum[:])
	return "p" + h[:3] + "-" + h[3:8]
}

func (r *Registry) handleMeVps(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	var body struct {
		Region          string         `json:"region"`
		ConfigurationID string         `json:"configuration_id"`
		ProfileID       int64          `json:"profile_id"`
		Vars            map[string]any `json:"vars"`
		Confirm         bool           `json:"confirm"`
		IdemKey         string         `json:"idem_key"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if !body.Confirm {
		controlAPIError(w, http.StatusBadRequest, "confirmation_required", "confirm:true is required")
		return
	}
	body.IdemKey = strings.TrimSpace(body.IdemKey)
	if body.IdemKey == "" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "idem_key is required")
		return
	}
	// Идемпотентность + owner-проверка: чужой ключ не отдаём и не переиспользуем.
	if existing, err := r.control.OperationByIdemKey(body.IdemKey); err == nil {
		if existing.UserID != u.ID {
			controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
			return
		}
		writeCabinetJSON(w, http.StatusOK, toOperationJSON(existing))
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}
	// Предварительный (advisory) отказ по квоте: не тревожим Beget-связку и
	// не ждём платного пути, если мест и так нет. Авторитетная атомарная
	// проверка (Union ноды∪VPS∪заказы, лимит/cooldown, H3) — в provision.Create.
	if used, err := r.control.UsedSlots(u.ID); err != nil {
		r.writeControlUnavailable(w)
		return
	} else if used >= u.MaxNodes {
		controlAPIError(w, http.StatusForbidden, "quota_exceeded", "max_nodes reached")
		return
	}
	// Профиль установки: published ИЛИ собственный неопубликованный (§7.4);
	// profile_id=0 → штатный профиль sharedd-node (addendum §4.4).
	pv, err := r.selectableProfileVersion(u, body.ProfileID)
	if err != nil {
		writeProfileSelectErr(w, err)
		return
	}
	client, ok := r.verifiedBegetClient(w, req)
	if !ok {
		return
	}
	link, err := r.control.BegetLink(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	// H4: вся ВАЛИДАЦИЯ — до резерва слота. MergeProfileVars/ValidateExtraVars/
	// sshAuthFor могут упасть → при раннем AuthorizeNode место в квоте утекало
	// бы (node_owners остаётся привязанным без операции/VPS).
	vars, err := control.MergeProfileVars(pv.VarsSchema, body.Vars)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	// SSH-креды VPS (ключ, сгенерированный при привязке Beget, либо пароль
	// аккаунта). Не расшифровались/пусты → отказ ДО платного заказа.
	auth, err := r.sshAuthFor(link)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "ssh_creds_unavailable", "no SSH credentials for the owner")
		return
	}
	// Только теперь резервируем место: AuthorizeNode привязывает владельца к
	// детерминированному node_id (node_secret становится валиден для /register).
	// Любая ошибка ПОСЛЕ резерва освобождает слот (defer) — см. H4.
	nodeID := nodeIDForIdem(body.IdemKey)
	authorized := false
	defer func() {
		if authorized {
			_, _ = r.control.ReleaseNode(nodeID)
		}
	}()
	if err := r.control.AuthorizeNode(nodeID, u); err != nil {
		if errors.Is(err, control.ErrQuotaExceeded) {
			controlAPIError(w, http.StatusForbidden, "quota_exceeded", "max_nodes reached")
			return
		}
		r.writeControlUnavailable(w)
		return
	}
	authorized = true
	vars, err = r.provisionVars(u, nodeID, vars, true)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if err := control.ValidateExtraVars(vars); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	creq := provision.CreateRequest{
		UserID: u.ID, NodeID: nodeID, IdemKey: body.IdemKey,
		Region: body.Region, ConfigID: body.ConfigurationID,
		DisplayName: "sharedd-" + u.Username, SSHKeyIDs: []uint32{link.SSHKeyID},
		ProfileID: pv.ProfileID, MaxNodes: u.MaxNodes,
	}
	prov := provision.New(r.control, client, r.provisionConfig())
	// Платный заказ (ровно один CreateVps на idem_key) — синхронно: быстро
	// (один вызов провайдера), сохраняет has_vps и деньги-инвариант. Ожидание
	// RUNNING (vps_ready_timeout) и install-шаги (install_timeout) — в фоне.
	ctx, cancel := providerCtx(req)
	defer cancel()
	op, err := prov.Create(ctx, creq)
	if err != nil && op.ID == 0 {
		// Резерв не удался (квота/cooldown/…): слот отпустит defer выше.
		r.writeProvisionErr(w, err)
		return
	}
	authorized = false // исход операции (включая release при ошибке региона) — на prov
	if op.State == control.OpConfirmed {
		// H1: установка — фоновым воркером со своим бюджетом
		// (vps_ready_timeout + N×install_timeout), не под 30с ctx запроса.
		steps := len(control.ParseProfileSteps(pv.Steps))
		r.provisionWorkers().run(op.ID, prov.ProvisionBudget(steps), func(wctx context.Context) {
			_, _ = prov.InstallOnConfirmed(wctx, op.ID, pv.ProfileID, "root", auth, vars)
		})
	}
	writeCabinetJSON(w, http.StatusOK, toOperationJSON(op))
}

// selectableProfileVersion — опубликованный профиль ИЛИ собственный
// неопубликованный (§7.4). Чужой неопубликованный → ErrNotFound (404 не
// подтверждаем). profileID=0 → штатный sharedd-node.
func (r *Registry) selectableProfileVersion(u control.User, profileID int64) (control.ProfileVersion, error) {
	if profileID == 0 {
		dp, err := r.control.ProfileByName(control.DefaultProfileName)
		if err != nil {
			return control.ProfileVersion{}, err
		}
		profileID = dp.ID
	}
	p, err := r.control.Profile(profileID)
	if err != nil {
		return control.ProfileVersion{}, err
	}
	if !p.Published && p.CreatedBy != u.ID {
		return control.ProfileVersion{}, control.ErrNotFound
	}
	// Свой неопубликованный профиль — это user-authored: при выключенном
	// user_profiles_enabled выбирать его нельзя (опубликованные admin-профили
	// флагом не затрагиваются).
	if !p.Published && !r.userProfilesEnabled() {
		return control.ProfileVersion{}, errUserProfilesDisabled
	}
	return r.control.ProfileVersion(profileID, 0)
}

// errUserProfilesDisabled — собственный (неопубликованный) профиль выбран при
// выключенном [provisioning] user_profiles_enabled.
var errUserProfilesDisabled = errors.New("user profiles are disabled")

// writeProfileSelectErr маппит ошибку выбора профиля: чужой/неопубликованный
// не подтверждаем существованием (404); выключенные user-профили → 403;
// остальное — 400 profile_unavailable.
func writeProfileSelectErr(w http.ResponseWriter, err error) {
	// B1: сообщения кабинета — по-русски; машиночитаемые коды не меняются.
	if errors.Is(err, errUserProfilesDisabled) {
		controlAPIError(w, http.StatusForbidden, "user_profiles_disabled", "Пользовательские профили отключены администратором.")
		return
	}
	if errors.Is(err, control.ErrNotFound) {
		controlAPIError(w, http.StatusNotFound, "not_found", "Профиль не найден.")
		return
	}
	controlAPIError(w, http.StatusBadRequest, "profile_unavailable", "Профиль недоступен для установки.")
}

// provisionVars — переменные профиля + инъекция узла и общего прокси:
// registry_url из panel.public_url, персональный node_secret, предсозданный
// node_id, а также proxy_port/proxy_sni/proxy_secret из [shared_proxy], чтобы
// профиль мог сам поднять прокси и вытянуть клиентский креденшл. Общий
// node_token (Bearer всего реестра) НЕ инъектится вовсе (M5): он попадал в
// пользовательские шаги и исполнялся на чужом VPS. rotateSecret — при создании
// выдаём свежий секрет; при retry (false) достаём УЖЕ выданный секрет из
// восстановимой AES-GCM копии (DEF-A), чтобы отрендерить ту же команду с
// реальным токеном, а не `--registry-token=”`. Хеш остаётся источником истины
// аутентификации; при отсутствии копии (legacy-строка) новый секрет выдаётся
// ТОЛЬКО если нода ещё ни разу не регистрировалась (иначе живой агент был бы
// обесточен).
//
// Ordinary proxy variables: explicit install value > nonempty declared profile
// default > registry default. Only node_id/node_secret/registry_url are protected.
//   - proxy_port   — [shared_proxy] port (int, default 443);
//   - proxy_sni    — [shared_proxy] tls_domain;
//   - proxy_secret — MTProto client secret из [shared_proxy] users: ровно один
//     entry → его секрет; несколько → секрет пользователя с
//     АЛФАВИТНО-первым именем; ноль → пустая строка. Это
//     СЕКРЕТ: маскируется в выводе/логах через provision.
//     secretValues (см. provision.go) и в сводке визарда.
//     Переопределяется на конкретную установку как обычная
//     переменная (значение пользователя важнее дефолта).
func (r *Registry) provisionVars(u control.User, nodeID string, in map[string]any, rotateSecret bool) (map[string]any, error) {
	vars, err := r.provisionBaseVars(in)
	if err != nil {
		return nil, err
	}
	vars["node_id"] = nodeID
	// label — имя ноды для {{ label }} в профиле; у Beget это уже выделенный
	// NAME-HASH (установщик принимает полный id как есть).
	vars["label"] = nodeID
	if rotateSecret {
		return vars, r.issueNodeSecretInto(vars, nodeID)
	}
	has, err := r.control.NodeHasSecret(nodeID)
	if err != nil {
		return nil, err
	}
	if !has {
		return vars, r.issueNodeSecretInto(vars, nodeID)
	}
	// Секрет уже выдан: возвращаем сохранённую копию (M6: не ротируем).
	plain, err := r.revealNodeSecret(nodeID)
	if err != nil {
		return nil, err
	}
	if plain != "" {
		vars["node_secret"] = plain
		return vars, nil
	}
	// Legacy-строка без восстановимой копии. Живого агента не трогаем.
	if r.nodeRegistered(nodeID) {
		return nil, errors.New("node secret is not recoverable and the node is already registered; refusing to reissue")
	}
	return vars, r.issueNodeSecretInto(vars, nodeID)
}

// provisionBaseVars validates supplied values and defaults without reserving
// ownership or issuing a secret. Manual acceptance persists those atomically.
func (r *Registry) provisionBaseVars(in map[string]any) (map[string]any, error) {
	vars := map[string]any{}
	for k, v := range in {
		switch k {
		case "node_id", "node_secret", "registry_url", "label":
			return nil, fmt.Errorf("%w: variable %q is server controlled", control.ErrValidation, k)
		}
		vars[k] = v
	}
	r.cfgMu.RLock()
	regURL := r.cfg.Panel.PublicURL
	proxyPort := r.cfg.SharedProxy.Port
	proxySNI := r.cfg.SharedProxy.TLSDomain
	proxySecret := selectProxySecret(r.cfg.SharedProxy.Users)
	r.cfgMu.RUnlock()
	vars["registry_url"] = regURL
	if _, ok := vars["proxy_port"]; !ok {
		vars["proxy_port"] = proxyPort
	}
	if _, ok := vars["proxy_sni"]; !ok {
		vars["proxy_sni"] = proxySNI
	}
	if _, ok := vars["proxy_secret"]; !ok {
		vars["proxy_secret"] = proxySecret
	}
	if err := control.ValidateExtraVars(vars); err != nil {
		return nil, err
	}
	return vars, nil
}

// selectProxySecret — MTProto client secret для встроенной переменной
// proxy_secret: ровно один пользователь → его секрет; несколько →
// детерминированно секрет пользователя с алфавитно-первым именем; ноль → "".
// Детерминизм важен: повтор провижининга (retry) обязан дать тот же креденшл.
func selectProxySecret(users map[string]string) string {
	if len(users) == 0 {
		return ""
	}
	if len(users) == 1 {
		for _, secret := range users {
			return secret
		}
	}
	names := make([]string, 0, len(users))
	for name := range users {
		names = append(names, name)
	}
	sort.Strings(names)
	return users[names[0]]
}

// issueNodeSecretInto — сгенерировать секрет, сохранить хеш + AES-GCM копию и
// положить плейнтекст в vars (node_secret). Секрет существует в открытом виде
// только здесь (команда установки) и в ответе /register.
func (r *Registry) issueNodeSecretInto(vars map[string]any, nodeID string) error {
	secret := randomURLSecret(32)
	if _, ok, err := r.control.SetNodeSecretWithEnc(nodeID, secret, r.sealNodeSecret(secret)); err != nil {
		return err
	} else if !ok {
		return errors.New("node owner not bound")
	}
	vars["node_secret"] = secret
	return nil
}

// sealNodeSecret — AES-GCM копия секрета мастер-ключом. Пусто, если ключ не
// загружен (тогда retry опирается на fallback-правило).
func (r *Registry) sealNodeSecret(secret string) string {
	if secret == "" || len(r.masterKey) == 0 {
		return ""
	}
	enc, err := cryptoutil.Seal(r.masterKey, secret)
	if err != nil {
		return ""
	}
	return enc
}

// revealNodeSecret — расшифровать сохранённую копию. "" = копии нет/нечитаема
// (вызывающий решает по fallback-правилу).
func (r *Registry) revealNodeSecret(nodeID string) (string, error) {
	enc, err := r.control.NodeSecretEnc(nodeID)
	if err != nil {
		return "", err
	}
	if enc == "" || len(r.masterKey) == 0 {
		return "", nil
	}
	plain, err := cryptoutil.Open(r.masterKey, enc)
	if err != nil {
		return "", nil // порча/чужой ключ — как отсутствие копии
	}
	return plain, nil
}

// nodeRegistered — нода есть в живом состоянии реестра (значит успешно
// вызвала /register). Под mu.
func (r *Registry) nodeRegistered(nodeID string) bool {
	r.mu.Lock()
	_, ok := r.state.Candidates[nodeID]
	r.mu.Unlock()
	return ok
}

// sshAuthFor расшифровывает SSH-креды владельца из Beget-связки: приватный
// ключ (сгенерирован при привязке) в приоритете, иначе пароль аккаунта как
// root-пароль VPS. Пусто/не расшифровалось → установка невозможна.
func (r *Registry) sshAuthFor(link control.BegetLink) (sshx.Auth, error) {
	priv, err := cryptoutil.Open(r.masterKey, link.SSHPrivEnc)
	if err != nil {
		return sshx.Auth{}, err
	}
	if strings.TrimSpace(priv) != "" {
		return sshx.Auth{Type: "key", Key: priv}, nil
	}
	pass, err := cryptoutil.Open(r.masterKey, link.PasswordEnc)
	if err != nil {
		return sshx.Auth{}, err
	}
	if pass == "" {
		return sshx.Auth{}, errors.New("owner has no SSH credentials")
	}
	return sshx.Auth{Type: "password", Password: pass}, nil
}

// resumeInstallingOps (DEF-C) — при старте реестра до-подхватывает операции,
// застигнутые в installing: фоновые воркеры не персистятся, и после рестарта
// установка иначе висела бы вечно, а retry был бы отклонён. Бюджет ограничен
// (ProvisionBudget = vps_ready + (n+1)×step + запас); новых платных вызовов нет
// (InstallOnConfirmed = WaitRunning + шаги). Не удалось подготовить (нет
// креды/VPS) — операция терминализируется failed, чтобы retry был доступен.
func (r *Registry) resumeInstallingOps(ctx context.Context) {
	if r.control == nil {
		return
	}
	ops, err := r.control.InstallingOperations()
	if err != nil {
		log.Printf("resume installing: list: %v", err)
		return
	}
	for _, op := range ops {
		if op.Kind == control.OpKindManualInstall {
			// Manual installs have no provider VPS. Never auto-run their SSH
			// steps on restart; make interrupted work explicitly retryable.
			r.nodeClaimMu.Lock()
			if !r.provisionWorkers().runningOp(op.ID) {
				current, err := r.control.Operation(op.ID)
				if err == nil && current.State == control.OpInstalling {
					err = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "manual install interrupted; retry from the saved server")
				}
				if err != nil {
					logProfileControlErr("manual.install.restart_state", err)
				}
			}
			r.nodeClaimMu.Unlock()
			continue
		}
		if r.provisionWorkers().runningOp(op.ID) {
			continue
		}
		if err := r.resumeInstallOp(op); err != nil {
			log.Printf("resume installing op %d: %v", op.ID, err)
			_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "resume failed: "+err.Error())
		}
	}
}

// resumeInstallOp — подготовить и запустить фоновую установку для операции в
// installing (без CreateVps).
func (r *Registry) resumeInstallOp(op control.Operation) error {
	if op.ProviderVPSID == "" {
		return errors.New("operation has no provider vps id")
	}
	owner, err := r.control.User(op.UserID)
	if err != nil {
		return fmt.Errorf("owner: %w", err)
	}
	link, err := r.control.BegetLink(op.UserID)
	if err != nil {
		return fmt.Errorf("beget link: %w", err)
	}
	auth, err := r.sshAuthFor(link)
	if err != nil {
		return fmt.Errorf("ssh creds: %w", err)
	}
	login, err := cryptoutil.Open(r.masterKey, link.LoginEnc)
	if err != nil {
		return fmt.Errorf("decrypt login: %w", err)
	}
	password, err := cryptoutil.Open(r.masterKey, link.PasswordEnc)
	if err != nil {
		return fmt.Errorf("decrypt password: %w", err)
	}
	pv, err := r.installingProfileVersion(owner.User, op)
	if err != nil {
		return err
	}
	vars, err := control.MergeProfileVars(pv.VarsSchema, nil)
	if err != nil {
		return err
	}
	vars, err = r.provisionVars(owner.User, op.NodeID, vars, false)
	if err != nil {
		return err
	}
	prov := provision.New(r.control, r.begetClientFor(login, password), r.provisionConfig())
	steps := len(control.ParseProfileSteps(pv.Steps))
	r.provisionWorkers().run(op.ID, prov.ProvisionBudget(steps), func(wctx context.Context) {
		_, _ = prov.InstallOnConfirmed(wctx, op.ID, pv.ProfileID, "root", auth, vars)
	})
	return nil
}

// installingProfileVersion — версия профиля для resume: сохранённый в операции
// profile_id, иначе штатный профиль.
func (r *Registry) installingProfileVersion(u control.User, op control.Operation) (control.ProfileVersion, error) {
	if op.ProfileID == 0 {
		dp, err := r.control.ProfileByName(control.DefaultProfileName)
		if err != nil {
			return control.ProfileVersion{}, err
		}
		op.ProfileID = dp.ID
	}
	return r.selectableProfileVersion(u, op.ProfileID)
}

// provisionConfig — конфиг провижининга из [provisioning] + инъектированный
// исполнитель шагов (nil → provision.New подставит реальный SSH). Читает
// горячие поля под cfgMu (DEF-D): панель правит их на лету, иначе гонка с
// hot-apply.
func (r *Registry) provisionConfig() provision.Config {
	r.cfgMu.RLock()
	p := r.cfg.Provisioning
	r.cfgMu.RUnlock()
	return provision.Config{
		MaxCreatesPerDay: p.MaxCreatesPerDay,
		Cooldown:         time.Duration(p.CooldownMinutes) * time.Minute,
		VpsReadyTimeout:  time.Duration(p.VpsReadyTimeoutMin) * time.Minute,
		VpsPollInterval:  time.Duration(p.VpsPollIntervalSec) * time.Second,
		StepTimeout:      time.Duration(p.InstallTimeoutMin) * time.Minute,
		Exec:             r.sshExec,
	}
}

func (r *Registry) writeProvisionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, provision.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
	case errors.Is(err, provision.ErrQuotaExceeded):
		controlAPIError(w, http.StatusForbidden, "quota_exceeded", "max_nodes reached")
	case errors.Is(err, provision.ErrRateLimited), errors.Is(err, provision.ErrCooldown):
		controlAPIError(w, http.StatusTooManyRequests, "rate_limited", err.Error())
	case errors.Is(err, provision.ErrInProgress):
		controlAPIError(w, http.StatusConflict, "create_in_progress", "a previous order is still unresolved; retry the same idem_key or reconcile it")
	case errors.Is(err, provision.ErrAlreadyDone):
		controlAPIError(w, http.StatusConflict, "already_done", "operation already done")
	case errors.Is(err, provision.ErrSSHUnavailable):
		controlAPIError(w, http.StatusBadRequest, "ssh_unavailable", "SSH credentials are not available for this VPS")
	case errors.Is(err, provision.ErrIdemConflict):
		controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
	default:
		r.writeControlUnavailable(w)
	}
}

// ── GET /api/me/operations ────────────────────────────────────────

func (r *Registry) handleMeOperations(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	ops, err := r.control.OperationsByUser(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]operationJSON, 0, len(ops))
	for _, o := range ops {
		j := toOperationJSON(o)
		// ponytail: один запрос на операцию; батч-запрос, если список операций
		// заметно вырастет.
		if steps, err := r.control.OperationSteps(o.ID); err == nil {
			for _, st := range steps {
				j.Steps = append(j.Steps, operationStepJSON{
					Idx: st.Idx, Name: st.Name, Status: st.Status,
					DurationMs: st.DurationMs(), Output: st.Output,
				})
			}
		}
		out = append(out, j)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"operations": out})
}

// ── POST /api/me/operations/{id}/cancel ───────────────────────────

func (r *Registry) handleOperationCancel(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad operation id")
		return
	}
	op, err := r.control.Operation(id)
	switch {
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	case err != nil:
		r.writeControlUnavailable(w)
		return
	}
	// Чужая операция не подтверждаем существованием.
	if op.UserID != u.ID {
		controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	// Отмена разрешена ТОЛЬКО из pending (R1): sent/confirmed/unknown — платный
	// POST уже ушёл; deleting/deleted/failed — заказ завершён. Предварительная
	// проверка — дешёвый fail-fast; авторитетен CAS ниже.
	if op.State != control.OpPending {
		controlAPIError(w, http.StatusConflict, "already_sent", "paid request already sent; cancel refused")
		return
	}
	// Атомарный CAS pending→failed: гонка с MarkCreateSent (перед CreateVps)
	// гарантирует, что отмена либо выигрывает до платного вызова, либо
	// отклоняется — живой заказ в полёте слот не освобождает.
	cancelled, err := r.control.CancelPendingOperation(op.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if !cancelled {
		controlAPIError(w, http.StatusConflict, "already_sent", "paid request already sent; cancel refused")
		return
	}
	// Отмена выиграла ДО платного вызова — освобождаем зарезервированное место
	// (иначе слот висит до release-node). После sent отмена сюда не доходит.
	_, _ = r.control.ReleaseNode(op.NodeID)
	op.State, op.LastError = control.OpFailed, "cancelled by user"
	writeCabinetJSON(w, http.StatusOK, toOperationJSON(op))
}

// ── POST /api/me/operations/{id}/retry-install ────────────────────

// handleOperationRetryInstall (§7.2): повтор установки на УЖЕ созданный VPS.
// Переиспользует provider_vps_id, новых CreateVps НЕ делает; отказ, если VPS
// ещё не создан, операция активна или уже done/deleted.
func (r *Registry) handleOperationRetryInstall(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad operation id")
		return
	}
	op, err := r.control.Operation(id)
	switch {
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	case err != nil:
		r.writeControlUnavailable(w)
		return
	}
	if op.UserID != u.ID {
		controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
		return
	}
	if op.ProviderVPSID == "" {
		controlAPIError(w, http.StatusBadRequest, "no_vps", "VPS is not created yet")
		return
	}
	switch op.State {
	case control.OpDone:
		controlAPIError(w, http.StatusConflict, "already_done", "operation already done")
		return
	case control.OpDeleted:
		controlAPIError(w, http.StatusBadRequest, "no_vps", "operation is deleted")
		return
	case control.OpPending, control.OpSent, control.OpUnknown, control.OpDeleting:
		controlAPIError(w, http.StatusConflict, "in_progress", "operation is still in progress")
		return
	case control.OpInstalling:
		// DEF-C: orphaned installing (рестарт во время установки) не должен
		// блокировать retry навсегда — отказ только пока воркер реально держит
		// операцию. Иначе установку можно перезапустить (CAS ниже примет installing).
		if r.provisionWorkers().runningOp(op.ID) {
			controlAPIError(w, http.StatusConflict, "in_progress", "operation is still in progress")
			return
		}
	}
	var body struct {
		ProfileID int64          `json:"profile_id"`
		Vars      map[string]any `json:"vars"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil && !errors.Is(err, io.EOF) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	// Профиль повторяем ТОТ ЖЕ, что выбрал оператор при создании: тело retry
	// несёт profile_id=0 по умолчанию, и без этого retry подменил бы профиль
	// дефолтным. body.ProfileID — fallback только для legacy-операций (0).
	effectiveProfile := op.ProfileID
	if effectiveProfile == 0 {
		effectiveProfile = body.ProfileID
	}
	pv, err := r.selectableProfileVersion(u, effectiveProfile)
	if err != nil {
		writeProfileSelectErr(w, err)
		return
	}
	vars, err := control.MergeProfileVars(pv.VarsSchema, body.Vars)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	// M6: retry НЕ перевыдаёт node_secret живого агента (rotateSecret=false) —
	// существующий хеш сохраняется, старый секрет в конфиге агента не ломается.
	vars, err = r.provisionVars(u, op.NodeID, vars, false)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if err := control.ValidateExtraVars(vars); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	client, ok := r.verifiedBegetClient(w, req)
	if !ok {
		return
	}
	link, err := r.control.BegetLink(u.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	auth, err := r.sshAuthFor(link)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "ssh_creds_unavailable", "no SSH credentials for the owner")
		return
	}
	prov := provision.New(r.control, client, r.provisionConfig())
	// CAS installing (не затирает уже завершённую операцию): ответ отражает
	// старт и повторный retry отклоняется. H1: ожидание RUNNING + install-шаги
	// — фоновым воркером со своим бюджетом, не под 30с ctx запроса.
	started, err := r.control.MarkInstallingIfActive(op.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if !started {
		controlAPIError(w, http.StatusConflict, "already_done", "operation already done")
		return
	}
	op.State = control.OpInstalling
	steps := len(control.ParseProfileSteps(pv.Steps))
	r.provisionWorkers().run(op.ID, prov.ProvisionBudget(steps), func(wctx context.Context) {
		_, _ = prov.InstallOnConfirmed(wctx, op.ID, pv.ProfileID, "root", auth, vars)
	})
	writeCabinetJSON(w, http.StatusOK, toOperationJSON(op))
}
