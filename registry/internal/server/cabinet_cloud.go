package server

// Cloud-ресурсы кабинета (docs/specs/hosting-blueprints/03-cabinet-api.md §4-§5,
// bounded stage-05): CRUD по cloud_resources2 + управление нодой.
// Ограничения этапа (см. implementation-stage-05-cloud-management.md):
//   - beget-подключения → 501 not_implemented (legacy /api/me/vps продолжает
//     работать для beget-заказов);
//   - воркер in-process; persisted provider handles resume with reads only;
//   - management mode=profile всегда 422 profile_incompatible (graph-движка
//     ещё нет, колонки format в profile_versions тоже);
//   - root-пароль VPS хранится AES-GCM в secrets_enc, в DTO никогда не входит.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
	"sharedd/registry/internal/nodehost"
	"sharedd/registry/internal/sshx"
)

// Поллинг-бюджеты воркеров (03 §4: create до 10м, delete до 3м, change-ip 60с).
// Интервал — var ради тестов (мгновенный фейковый провайдер).
var (
	cloudPollInterval = 3 * time.Second
	cloudCreateBudget = 10 * time.Minute
	cloudDeleteBudget = 3 * time.Minute
)

// opKindCloudChangeIP — вид операции change-ip (operations.kind без CHECK;
// строка живёт в namespace пользователя через idem_key). ChangeIP у провайдера
// не идемпотентен, поэтому ключ обязателен (03 §2.1).
const opKindCloudChangeIP = "cloud_change_ip"

// cloudBodyHash — канонический хэш тела команды: защита от повтора того же
// Idempotency-Key с другим телом (03 §2.1 → 409 idempotency_conflict).
func cloudBodyHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const confirmDeleteHeader = "X-Confirm-Action"

// ── провайдер ─────────────────────────────────────────────────────

// cloudProvider — поверхность nodehost API, нужная cloud-ресурсам.
type cloudProvider interface {
	Create(ctx context.Context, req nodehost.CreateRequest, idemKey string) (*nodehost.CreateResult, error)
	Operation(ctx context.Context, id string) (*nodehost.Operation, error)
	Server(ctx context.Context, id string) (*nodehost.Server, error)
	Delete(ctx context.Context, id string) (*nodehost.DeleteResult, error)
	ChangeIP(ctx context.Context, id string, subnetID int, useSubnet bool) (*nodehost.ChangeIPResult, error)
	ServersPage(ctx context.Context, cursor string, limit int) (*nodehost.ServerPage, error)
}

type cloudNodehost struct{ cl *nodehost.Client }

func (p cloudNodehost) Create(ctx context.Context, req nodehost.CreateRequest, key string) (*nodehost.CreateResult, error) {
	return p.cl.Create(ctx, req, key)
}
func (p cloudNodehost) Operation(ctx context.Context, id string) (*nodehost.Operation, error) {
	return p.cl.Operation(ctx, id)
}
func (p cloudNodehost) Server(ctx context.Context, id string) (*nodehost.Server, error) {
	return p.cl.Server(ctx, id)
}
func (p cloudNodehost) Delete(ctx context.Context, id string) (*nodehost.DeleteResult, error) {
	return p.cl.Delete(ctx, id)
}
func (p cloudNodehost) ChangeIP(ctx context.Context, id string, subnetID int, useSubnet bool) (*nodehost.ChangeIPResult, error) {
	return p.cl.ChangeIP(ctx, id, subnetID, useSubnet)
}
func (p cloudNodehost) ServersPage(ctx context.Context, cursor string, limit int) (*nodehost.ServerPage, error) {
	return p.cl.ServersPage(ctx, cursor, limit)
}

// cloudAllServers — read-only инвентаризация: все страницы ServersPage.
// Жёсткий предел страниц + отсечение повторяющегося курсора (nodehost/methods
// требует guard от зацикливания).
func cloudAllServers(ctx context.Context, prov cloudProvider) ([]nodehost.Server, error) {
	var out []nodehost.Server
	cursor := ""
	for page := 0; page < 100; page++ { // ponytail: hard 100-page cap; 10k servers
		p, err := prov.ServersPage(ctx, cursor, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Data...)
		if p.NextCursor == nil || *p.NextCursor == "" || *p.NextCursor == cursor {
			return out, nil
		}
		cursor = *p.NextCursor
	}
	return out, nil
}

// cloudProviderFor — фабрика (тесты подменяют r.cloudFactory). Реальный путь —
// только nodehost (beget-ресурсы в stage-05 не управляются новыми маршрутами).
func (r *Registry) cloudProviderFor(c control.HostingConnection) (cloudProvider, error) {
	if r.cloudFactory != nil {
		return r.cloudFactory(c)
	}
	if c.Provider != control.ProviderNodehost {
		return nil, control.ErrValidation
	}
	plain, err := cryptoutil.Open(r.masterKey, c.CredentialsEnc)
	if err != nil {
		return nil, err
	}
	var creds struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal([]byte(plain), &creds); err != nil {
		return nil, err
	}
	cl, err := nodehost.New(creds.APIKey)
	if err != nil {
		return nil, err
	}
	return cloudNodehost{cl: cl}, nil
}

// cloudOpErrDefinitive — ошибка поллинга операции, которую бессмысленно
// ретраить до конца бюджета: битый ключ или неизвестная операция провайдера.
// Транспортные/5xx/таймауты остаются retryable (поллинг до дедлайна).
func cloudOpErrDefinitive(err error) bool {
	var ne *nodehost.Error
	if !errors.As(err, &ne) {
		return false
	}
	switch ne.Code {
	case "invalid_json", "invalid_response", "response_too_large", "invalid_input":
		return true
	}
	switch ne.HTTPStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// cloudSafeErrCode — безопасный код ошибки провайдера (текст наружу не идёт).
func cloudSafeErrCode(err error) string {
	var ne *nodehost.Error
	if errors.As(err, &ne) {
		return ne.Code
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "provider_timeout"
	}
	return "provider_error"
}

// ── DTO и валидация ───────────────────────────────────────────────

type cloudResourceJSON struct {
	ID                   int64  `json:"id"`
	ConnectionID         int64  `json:"connection_id"`
	Provider             string `json:"provider"`
	Label                string `json:"label"`
	State                string `json:"state"`
	Revision             int64  `json:"revision"`
	ExternalID           string `json:"external_id"`
	IP                   string `json:"ip"`
	NodeID               string `json:"node_id"`
	ReplacesResourceID   *int64 `json:"replaces_resource_id"`
	ReplacedByResourceID *int64 `json:"replaced_by_resource_id"`
	ActiveOperationID    *int64 `json:"active_operation_id"`
	LastError            string `json:"last_error"`
}

func cloudResourceView(r control.CloudResourceV2) cloudResourceJSON {
	v := cloudResourceJSON{
		ID: r.ID, ConnectionID: r.ConnectionID, Provider: r.Provider, Label: r.Label,
		State: r.State, Revision: r.Revision, ExternalID: r.ExternalID, IP: r.IP, NodeID: r.NodeID,
		LastError: r.LastError,
	}
	if r.ReplacesResourceID > 0 {
		x := r.ReplacesResourceID
		v.ReplacesResourceID = &x
	}
	if r.ReplacedByResourceID > 0 {
		x := r.ReplacedByResourceID
		v.ReplacedByResourceID = &x
	}
	if r.LastOperationID > 0 {
		x := r.LastOperationID
		v.ActiveOperationID = &x
	}
	return v
}

func (r *Registry) cloudResourceFresh(id, userID int64) cloudResourceJSON {
	fresh, err := r.control.CloudResourceOwned(id, userID)
	if err != nil {
		return cloudResourceJSON{State: control.CloudResError}
	}
	return cloudResourceView(fresh)
}

// cloudControlErr — маппинг sentinel-ошибок control в HTTP контракта.
func cloudControlErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid input")
	case errors.Is(err, control.ErrRevisionConflict):
		controlAPIError(w, http.StatusConflict, "revision_conflict", "resource state changed concurrently")
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusNotFound, "not_found", "resource is not owned by the current user")
	default:
		log.Printf("cloud control error: %v", err)
		controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
	}
}

// cloudOwned — id из пути + ресурс владельца; false = ответ записан.
func (r *Registry) cloudOwned(w http.ResponseWriter, req *http.Request) (control.CloudResourceV2, bool) {
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "resource is not owned by the current user")
		return control.CloudResourceV2{}, false
	}
	res, err := r.control.CloudResourceOwned(id, sessionFrom(req).user.ID)
	if err != nil {
		cloudControlErr(w, err)
		return control.CloudResourceV2{}, false
	}
	return res, true
}

// cloudIdemKey — обязательный Idempotency-Key (03 §2.1). false = 400 записан.
func cloudIdemKey(w http.ResponseWriter, req *http.Request) (string, bool) {
	k := strings.TrimSpace(req.Header.Get("Idempotency-Key"))
	if k == "" || utf8.RuneCountInString(k) > 128 {
		controlAPIError(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header is required")
		return "", false
	}
	return k, true
}

// validCloudConfig — server-side границы конфигурации (провайдер
// перепроверяет своим invalid_input, но отказ должен случиться ДО создания строк).
func validCloudConfig(cfg nodehost.CreateRequest) bool {
	text := func(s string, max int) bool { n := utf8.RuneCountInString(s); return n >= 1 && n <= max }
	opt := func(s string, max int) bool { return s == "" || utf8.RuneCountInString(s) <= max }
	period := map[string]bool{"1d": true, "3d": true, "1w": true, "1m": true, "3m": true, "6m": true, "1y": true}
	return text(cfg.Name, 100) && text(cfg.CPU, 64) && cfg.Cores > 0 && cfg.RAM > 0 && cfg.Disk > 0 &&
		text(cfg.DiskType, 64) && text(cfg.OS, 64) && text(cfg.Location, 64) &&
		opt(cfg.NetworkSpeed, 64) && (cfg.Period == "" || period[cfg.Period])
}

// ── маршруты ──────────────────────────────────────────────────────

func (r *Registry) mountCloudResources(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/cloud-resources", r.requireSession(r.handleCloudList))
	mux.HandleFunc("GET /api/me/cloud-resources/{id}", r.requireSession(r.handleCloudGet))
	mux.HandleFunc("POST /api/me/cloud-resources/estimate", r.requireSession(r.handleCloudEstimate))
	mux.HandleFunc("POST /api/me/cloud-resources", r.requireSession(r.requireCSRF(r.handleCloudCreate)))
	mux.HandleFunc("POST /api/me/cloud-resources/{id}/replace", r.requireSession(r.requireCSRF(r.handleCloudReplace)))
	mux.HandleFunc("DELETE /api/me/cloud-resources/{id}", r.requireSession(r.requireCSRF(r.handleCloudDelete)))
	mux.HandleFunc("POST /api/me/cloud-resources/{id}/change-ip", r.requireSession(r.requireCSRF(r.handleCloudChangeIP)))
	mux.HandleFunc("POST /api/me/cloud-resources/{id}/install", r.requireSession(r.requireCSRF(r.handleCloudInstall)))
	mux.HandleFunc("POST /api/me/cloud-resources/{id}/check", r.requireSession(r.requireCSRF(r.handleCloudCheck)))
	mux.HandleFunc("POST /api/me/cloud-resources/{id}/reconcile", r.requireSession(r.requireCSRF(r.handleCloudReconcile)))
	mux.HandleFunc("GET /api/me/nodes/{node_id}/management", r.requireSession(r.handleNodeManagementGet))
	mux.HandleFunc("PUT /api/me/nodes/{node_id}/management", r.requireSession(r.requireCSRF(r.handleNodeManagementPut)))
}

// GET /api/me/cloud-resources
func (r *Registry) handleCloudList(w http.ResponseWriter, req *http.Request) {
	includeDeleted := req.URL.Query().Get("include_deleted") == "1"
	res, err := r.control.CloudResourcesByUser(sessionFrom(req).user.ID, includeDeleted)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]cloudResourceJSON, 0, len(res))
	for _, c := range res {
		// predicted zombie → error, чтобы UI не показывал вечное «ожидание IP».
		out = append(out, cloudResourceView(r.control.ReconcileCloudResource(c)))
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"resources": out})
}

// GET /api/me/cloud-resources/{id}
func (r *Registry) handleCloudGet(w http.ResponseWriter, req *http.Request) {
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	writeCabinetJSON(w, http.StatusOK, cloudResourceView(r.control.ReconcileCloudResource(res)))
}

// createAck — 202-ответ асинхронной команды (03 §2.1).
type createAck struct {
	OperationID int64  `json:"operation_id"`
	State       string `json:"state"`
	ResourceID  int64  `json:"resource_id"`
}

type cloudCreateBody struct {
	ConnectionID  int64                   `json:"connection_id"`
	Label         string                  `json:"label"`
	Configuration *nodehost.CreateRequest `json:"configuration"`
}

// cloudCreateCore — общий путь create/replace: строки (resource planned +
// operation pending) ДО платного вызова, воркер после ответа. replay=true —
// тот же Idempotency-Key уже принимался: вернуть тот же ack, провайдера не звать.
func (r *Registry) cloudCreateCore(w http.ResponseWriter, req *http.Request, connID int64, label string, cfg *nodehost.CreateRequest, idemKey, bodyHash string, replacesID int64) {
	if cfg == nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid configuration")
		return
	}
	ack, err := r.cloudCreateAccept(sessionFrom(req).user, connID, label, *cfg, idemKey, bodyHash, replacesID)
	switch {
	case err == nil:
		writeCabinetJSON(w, http.StatusAccepted, ack)
	case errors.Is(err, errCloudNotImplemented):
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
	case errors.Is(err, errIdemConflict):
		controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was already used for a different command")
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid configuration")
	case errors.Is(err, control.ErrNotFound):
		cloudControlErr(w, err)
	default:
		r.writeControlUnavailable(w)
	}
}

var (
	errCloudNotImplemented = errors.New("not implemented")
	errIdemConflict        = errors.New("idempotency conflict")
)

// cloudReplayResource validates a previously accepted create command and
// returns its resource id. Kind/body/ownership mismatches are conflicts; the
// caller maps them to the contract response.
func (r *Registry) cloudReplayResource(op control.Operation, bodyHash string) (int64, error) {
	if op.Kind != control.OpKindCloudCreate {
		return 0, errIdemConflict
	}
	var meta struct {
		ResourceID int64  `json:"resource_id"`
		BodyHash   string `json:"body_hash"`
	}
	_ = json.Unmarshal([]byte(op.RequestJSON), &meta)
	if meta.ResourceID == 0 || meta.BodyHash != bodyHash {
		return 0, errIdemConflict
	}
	if _, err := r.control.CloudResourceOwned(meta.ResourceID, op.UserID); err != nil {
		return 0, errIdemConflict
	}
	return meta.ResourceID, nil
}

// cloudCreateAccept — the single paid-create acceptance path used by both
// POST /api/me/cloud-resources and the fleet worker. It validates ownership
// and configuration, replays an existing Idempotency-Key, creates the planned
// resource + operation rows, then launches the worker. No HTTP.
func (r *Registry) cloudCreateAccept(u control.User, connID int64, label string, cfg nodehost.CreateRequest, idemKey, bodyHash string, replacesID int64) (createAck, error) {
	conn, err := r.control.HostingConnectionOwned(connID, u.ID)
	if err != nil {
		return createAck{}, err
	}
	if conn.Provider != control.ProviderNodehost {
		return createAck{}, errCloudNotImplemented
	}
	if !validCloudConfig(cfg) {
		return createAck{}, control.ErrValidation
	}
	opKey := fmt.Sprintf("cloud_create:%d:%s", u.ID, idemKey)
	if prev, err := r.control.OperationByIdemKey(opKey); err == nil {
		resourceID, rerr := r.cloudReplayResource(prev, bodyHash)
		if rerr != nil {
			return createAck{}, rerr
		}
		return createAck{OperationID: prev.ID, State: control.OpPending, ResourceID: resourceID}, nil
	} else if !errors.Is(err, control.ErrNotFound) {
		return createAck{}, err
	}

	resRow, err := r.control.CreateCloudResource(control.CloudResourceV2{
		UserID: u.ID, ConnectionID: conn.ID, Provider: conn.Provider,
		Label: label, State: control.CloudResPlanned, ReplacesResourceID: replacesID,
	})
	if err != nil {
		return createAck{}, err
	}
	reqJSON, _ := json.Marshal(map[string]any{"resource_id": resRow.ID, "body_hash": bodyHash})
	op, created, err := r.control.CreateOperation(control.Operation{
		UserID: u.ID, Kind: control.OpKindCloudCreate, State: control.OpPending,
		IdemKey: opKey, RequestJSON: string(reqJSON),
	})
	if err != nil {
		_ = r.control.DeletePlannedCloudResource(resRow.ID)
		return createAck{}, err
	}
	if !created {
		_ = r.control.DeletePlannedCloudResource(resRow.ID)
		resourceID, rerr := r.cloudReplayResource(op, bodyHash)
		if rerr != nil {
			return createAck{}, rerr
		}
		return createAck{OperationID: op.ID, State: control.OpPending, ResourceID: resourceID}, nil
	}
	if err := r.control.SetCloudResourceOperation(resRow.ID, op.ID); err != nil {
		r.failCloud(resRow.ID, op.ID, control.CloudResPlanned, "control_unavailable")
		return createAck{}, err
	}
	r.launchCloudCreate(resRow.ID, conn, cfg, idemKey, op.ID)
	return createAck{OperationID: op.ID, State: control.OpPending, ResourceID: resRow.ID}, nil
}

// POST /api/me/cloud-resources
func (r *Registry) handleCloudCreate(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	var body cloudCreateBody
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" && body.Configuration != nil {
		label = body.Configuration.Name
	}
	if utf8.RuneCountInString(label) > 100 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "label too long")
		return
	}
	r.cloudCreateCore(w, req, body.ConnectionID, label, body.Configuration, idemKey, cloudBodyHash(body), 0)
}

// POST /api/me/cloud-resources/{id}/replace — новый ресурс поверх старого
// (replaces_resource_id); старый не трогается, удаление — явный DELETE.
// Configuration обязательна: pinned snapshot не хранится (stage-05).
func (r *Registry) handleCloudReplace(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	old, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	var body cloudCreateBody
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	cfg := body.Configuration
	if cfg == nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "configuration snapshot is not stored; pass configuration explicitly")
		return
	}
	label := strings.TrimSpace(body.Label)
	if label == "" {
		label = old.Label
	}
	r.cloudCreateCore(w, req, old.ConnectionID, label, cfg, idemKey, cloudBodyHash(body), old.ID)
}

// DELETE /api/me/cloud-resources/{id} — без тела; X-Confirm-Action: delete +
// Idempotency-Key (единственный destructive header-контракт новых маршрутов).
func (r *Registry) handleCloudDelete(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	if req.Header.Get(confirmDeleteHeader) != "delete" {
		controlAPIError(w, http.StatusBadRequest, "confirmation_required", "X-Confirm-Action: delete header is required")
		return
	}
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	// No worker and no external id do NOT prove absence of a paid instance.
	// The store only permits unsubmitted plans or definitive purchase refusals.
	if res.ExternalID == "" {
		switch res.State {
		case control.CloudResPlanned, control.CloudResCreating, control.CloudResError:
			if r.cloudWorkerActive(res) {
				controlAPIError(w, http.StatusConflict, "resource_busy", "a worker is still in flight for this resource")
				return
			}
			if err := r.control.DeleteCloudResourceLocal(res.ID, res.UserID); err != nil {
				if errors.Is(err, control.ErrCreateInProgress) {
					controlAPIError(w, http.StatusConflict, "provider_reconcile_required", "paid outcome is uncertain; reconcile provider inventory before deletion")
					return
				}
				cloudControlErr(w, err)
				return
			}
			writeCabinetJSON(w, http.StatusAccepted, createAck{State: control.CloudResDeleted, ResourceID: res.ID})
			return
		}
		controlAPIError(w, http.StatusConflict, "resource_busy", "resource is not in a deletable state")
		return
	}
	conn, err := r.control.HostingConnectionOwned(res.ConnectionID, res.UserID)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	if conn.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
		return
	}
	// Retry разрешён из ready и error (03 §4); остальные состояния — busy.
	if res.State != control.CloudResReady && res.State != control.CloudResError {
		controlAPIError(w, http.StatusConflict, "resource_busy", "resource is not in a deletable state")
		return
	}

	opKey := fmt.Sprintf("cloud_delete:%d:%s", res.UserID, idemKey)
	if prev, err := r.control.OperationByIdemKey(opKey); err == nil {
		writeCabinetJSON(w, http.StatusAccepted, createAck{OperationID: prev.ID, State: control.OpPending, ResourceID: res.ID})
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}
	// F1: не обгонять install/change-ip на том же ресурсе (03 §2.1).
	if !r.cloudGuardAcquire(res.ID) {
		controlAPIError(w, http.StatusConflict, "resource_busy", "another mutation is in flight for this resource")
		return
	}
	guarded := true
	defer func() {
		if guarded {
			r.cloudGuardRelease(res.ID)
		}
	}()
	op, _, err := r.control.CreateOperation(control.Operation{
		UserID: res.UserID, NodeID: res.NodeID, Kind: control.OpKindCloudDelete,
		State: control.OpPending, IdemKey: opKey, ProviderVPSID: res.ExternalID,
	})
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	r.launchCloudDelete(res.ID, conn, res.ExternalID, op.ID)
	guarded = false // воркер владеет guard'ом и снимет его сам (или снимет launch при worker_busy)
	writeCabinetJSON(w, http.StatusAccepted, createAck{OperationID: op.ID, State: control.OpPending, ResourceID: res.ID})
}

// ── per-resource mutation guard (F1, review-stage-06) ─────────────

// cloudGuardAcquire/Release — один активный mutation на ресурс
// (03 §2.1): install/delete/change-ip взаимно исключены. In-process, НЕ
// crash-durable: рестарт registry снимает guard (durable 'installing' на
// cloud_resources2 — отдельный этап, см. implementation-stage-06 §3).
func (r *Registry) cloudGuardAcquire(resID int64) bool {
	r.cloudMu.Lock()
	defer r.cloudMu.Unlock()
	if r.cloudBusy == nil {
		r.cloudBusy = map[int64]bool{}
	}
	if r.cloudBusy[resID] {
		return false
	}
	r.cloudBusy[resID] = true
	return true
}

func (r *Registry) cloudGuardRelease(resID int64) {
	r.cloudMu.Lock()
	delete(r.cloudBusy, resID)
	r.cloudMu.Unlock()
}

// cloudWorkerActive — есть ли у ресурса живой mutation: guard (install/delete/
// change-ip) или запущенный воркер последней операции (create). Нужен phantom
// delete: строку без внешнего инстанса можно убрать только когда платный
// вызов точно не в полёте.
func (r *Registry) cloudWorkerActive(res control.CloudResourceV2) bool {
	r.cloudMu.Lock()
	busy := r.cloudBusy[res.ID]
	r.cloudMu.Unlock()
	if busy {
		return true
	}
	return res.LastOperationID > 0 && r.provisionWorkers().runningOp(res.LastOperationID)
}

// POST /api/me/cloud-resources/{id}/change-ip — синхронно, bounded 60с,
// один in-flight на ресурс; NEVER auto-create replacement.
func (r *Registry) handleCloudChangeIP(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	if res.State != control.CloudResReady || res.ExternalID == "" {
		controlAPIError(w, http.StatusConflict, "resource_busy", "change-ip requires a ready managed resource")
		return
	}
	conn, err := r.control.HostingConnectionOwned(res.ConnectionID, res.UserID)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	if conn.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
		return
	}
	var body struct {
		SubnetID int64 `json:"subnet_id"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}

	// ChangeIP у провайдера НЕ идемпотентен (nodehost/methods.go: никогда не
	// повторять). Ключная строка operations делает повтор тем же
	// Idempotency-Key воспроизведением без второго платного вызова (03 §2.1).
	opKey := fmt.Sprintf("cloud_change_ip:%d:%s", res.UserID, idemKey)
	if _, err := r.control.OperationByIdemKey(opKey); err == nil {
		writeCabinetJSON(w, http.StatusOK, r.cloudResourceFresh(res.ID, res.UserID))
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}
	op, created, err := r.control.CreateOperation(control.Operation{
		UserID: res.UserID, NodeID: res.NodeID, Kind: opKindCloudChangeIP,
		State: control.OpPending, IdemKey: opKey,
	})
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if !created {
		// Конкурентный дубль того же ключа — отдать текущее состояние без
		// второго ChangeIP.
		writeCabinetJSON(w, http.StatusOK, r.cloudResourceFresh(res.ID, res.UserID))
		return
	}

	_ = r.control.SetCloudResourceOperation(res.ID, op.ID)
	if !r.cloudGuardAcquire(res.ID) {
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "resource_busy")
		controlAPIError(w, http.StatusConflict, "resource_busy", "another mutation is in flight for this resource")
		return
	}
	defer r.cloudGuardRelease(res.ID)

	prov, err := r.cloudProviderFor(conn)
	if err != nil {
		code := cloudSafeErrCode(err)
		log.Printf("cloud change-ip %d: provider unavailable: %s", res.ID, code)
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", code)
		controlAPIError(w, http.StatusBadGateway, "provider_error", "provider client is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 60*time.Second)
	defer cancel()
	subnetID := int(body.SubnetID)
	out, err := prov.ChangeIP(ctx, res.ExternalID, subnetID, body.SubnetID > 0)
	if err != nil {
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", cloudSafeErrCode(err))
		controlAPIError(w, http.StatusBadGateway, "provider_error", cloudSafeErrCode(err))
		return
	}
	// Ответ ChangeIP авторитетен; Server() — только фолбэк, когда провайдер не
	// вернул IP (возможно устаревшее чтение не должно перекрывать результат).
	newIP := out.IP
	if newIP == "" {
		if srv, err := prov.Server(ctx, res.ExternalID); err == nil && srv.IP != "" {
			newIP = srv.IP
		}
	}
	// ready→ready CAS с новой ip: параллельный delete в полёте перезапись
	// отсечёт (состояние уже не ready).
	if err := r.control.UpdateCloudResourceState(res.ID, control.CloudResReady, control.CloudResReady, "", newIP); err != nil {
		cloudControlErr(w, err)
		return
	}
	_ = r.control.UpdateOperationState(op.ID, control.OpConfirmed, "", "")
	writeCabinetJSON(w, http.StatusOK, r.cloudResourceFresh(res.ID, res.UserID))
}

// POST /api/me/cloud-resources/{id}/install — установка агента на уже
// созданный ready-ресурс (завершение ручной петли create→install→pool).
// beget-ресурсы → 501 (legacy manual/beget-путь жив); профиль — text.
// label ресурса обязан быть префиксом имени ноды (claim-on-first-register).
func (r *Registry) handleCloudInstall(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	// Idempotency-Key обязателен по контракту новых команд (03 §2.1); дедуп —
	// на уровне операции cloud_install:<resourceID> (одна установка на ресурс).
	if _, ok := cloudIdemKey(w, req); !ok {
		return
	}
	u := sessionFrom(req).user
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	if res.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
		return
	}
	if res.State != control.CloudResReady {
		controlAPIError(w, http.StatusConflict, "not_ready", "resource is not ready for installation")
		return
	}
	if validateNodeID(res.Label+"-00000") != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "resource label must be a valid 1..10 character node-name prefix")
		return
	}
	var body struct {
		ProfileID        *int64          `json:"profile_id"`
		ProfileVersionID json.RawMessage `json:"profile_version_id"`
		Vars             map[string]any  `json:"vars"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil && !errors.Is(err, io.EOF) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}

	var existing control.Operation
	if op, err := r.control.OperationByIdemKey(control.CloudInstallIdem(res.ID)); err == nil {
		if op.UserID != u.ID {
			controlAPIError(w, http.StatusNotFound, "not_found", "operation not found")
			return
		}
		existing = op
		if op.Kind != control.OpKindCloudInstall {
			writeCloudInstallErr(w, control.ErrOwnerMismatch, "cloud.install.operation_kind")
			return
		}
		if r.provisionWorkers().runningOp(op.ID) {
			writeCloudInstallErr(w, control.ErrCreateInProgress, "cloud.install.running_worker")
			return
		}
		// OpDone НЕ отклоняем: переустановка (reinstall) перезапускает ту же
		// операцию с тем же id/idem_key, возможно другим профилем.
		switch op.State {
		case control.OpFailed, control.OpInstalling, control.OpDone:
		default:
			controlAPIError(w, http.StatusConflict, "in_progress", "installation is already in progress")
			return
		}
	} else if !errors.Is(err, control.ErrNotFound) {
		writeCloudInstallErr(w, err, "cloud.install.lookup_operation")
		return
	}

	var pv control.ProfileVersion
	var err error
	if len(body.ProfileVersionID) != 0 {
		// RawMessage distinguishes omission from null/zero: an explicitly
		// invalid selection must never fall back to a default installation.
		var versionID int64
		if json.Unmarshal(body.ProfileVersionID, &versionID) != nil || versionID <= 0 {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile_version_id must be a positive version-row ID")
			return
		}
		pv, err = r.control.ProfileVersionByNumericID(versionID)
		if err == nil {
			// Reuse existing ownership/publication and feature-flag gates;
			// discard the latest row, keeping the explicitly selected version.
			_, err = r.selectableProfileVersion(u, pv.ProfileID)
		}
		if err == nil && body.ProfileID != nil && *body.ProfileID != pv.ProfileID {
			controlAPIError(w, http.StatusBadRequest, "profile_mismatch", "profile_id does not match profile_version_id")
			return
		}
	} else {
		effectiveProfile := existing.ProfileID
		if body.ProfileID != nil && *body.ProfileID != 0 {
			effectiveProfile = *body.ProfileID
		}
		pv, err = r.selectableProfileVersion(u, effectiveProfile)
	}
	if err != nil {
		logProfileControlErr("cloud.install.select_profile", err)
		writeProfileSelectErr(w, err)
		return
	}
	if pv.Format != "" && pv.Format != control.FormatText {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "cloud installation requires a text profile version")
		return
	}
	op, err := r.startCloudInstall(u, res, pv, body.Vars)
	if err != nil {
		if errors.Is(err, errCloudResourceBusy) {
			controlAPIError(w, http.StatusConflict, "resource_busy", "another mutation is in flight for this resource")
			return
		}
		if errors.Is(err, errCloudNotImplemented) {
			controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
			return
		}
		if errors.Is(err, errCloudNoPassword) {
			controlAPIError(w, http.StatusConflict, "ssh_unavailable", "no sealed root password is stored for this resource")
			return
		}
		writeCloudInstallErr(w, err, "cloud.install")
		return
	}
	writeCabinetJSON(w, http.StatusOK, toOperationJSON(op))
}

// POST /api/me/cloud-resources/{id}/check — SSH-проверка доступности
// ready-ресурса (root + запечатанный пароль). Состояние ресурса не меняется.
var (
	cloudCheckAttempts = 3
	cloudCheckDelay    = 5 * time.Second
)

type cloudCheckJSON struct {
	SSHOK bool   `json:"ssh_ok"`
	Error string `json:"error"`
}

func (r *Registry) cloudCheckSSH(ctx context.Context, target sshx.Target) error {
	if r.sshExec != nil {
		return r.sshExec.WaitSSH(ctx, target)
	}
	return sshx.WaitSSH(ctx, target, cloudCheckAttempts, cloudCheckDelay)
}

func (r *Registry) handleCloudCheck(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	if res.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
		return
	}
	if res.State != control.CloudResReady {
		controlAPIError(w, http.StatusConflict, "not_ready", "resource is not ready")
		return
	}
	password, err := r.cloudInstallPassword(res)
	if err != nil || password == "" {
		controlAPIError(w, http.StatusConflict, "ssh_unavailable", "no sealed root password is stored for this resource")
		return
	}
	if res.IP == "" {
		writeCabinetJSON(w, http.StatusOK, cloudCheckJSON{Error: "no_ip"})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	target := sshx.Target{Host: res.IP, Port: 0, User: "root", Auth: sshx.Auth{Type: "password", Password: password}}
	if err := r.cloudCheckSSH(ctx, target); err != nil {
		log.Printf("cloud check %d: ssh unavailable: %v", res.ID, err)
		writeCabinetJSON(w, http.StatusOK, cloudCheckJSON{Error: "ssh_unreachable"})
		return
	}
	writeCabinetJSON(w, http.StatusOK, cloudCheckJSON{SSHOK: true})
}

// POST /api/me/cloud-resources/{id}/reconcile — read-only сверка с
// nodehost-инвентарём для ресурсов, застрявших в error/planned с кодом
// provider_reconcile_required (исход create неизвестен, DELETE заблокирован).
// Контракт: {"resolved":"absent"} — провайдер-инстанса нет, ресурс CAS-ом
// уходит в deleted и pending-бронь снимается; {"resolved":"found",
// "external_id":…} — инстанс есть: свой external_id подтверждается, застрявшие
// состояния дотягиваются CAS-ом до ready; неучтённые серверы при пустом
// external_id → 409 без мутаций (молча забирать чужой сервер нельзя).
// Никаких строк operations и записей журналов.
func (r *Registry) handleCloudReconcile(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	res, ok := r.cloudOwned(w, req)
	if !ok {
		return
	}
	if res.State == control.CloudResDeleted {
		controlAPIError(w, http.StatusNotFound, "not_found", "resource is already deleted")
		return
	}
	conn, err := r.control.HostingConnectionOwned(res.ConnectionID, res.UserID)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	if conn.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget resources are managed via the legacy beget flow")
		return
	}
	prov, err := r.cloudProviderFor(conn)
	if err != nil {
		code := cloudSafeErrCode(err)
		log.Printf("cloud reconcile %d: provider unavailable: %s", res.ID, code)
		controlAPIError(w, http.StatusBadGateway, "provider_error", "provider client is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
	defer cancel()
	servers, err := cloudAllServers(ctx, prov)
	if err != nil {
		code := cloudSafeErrCode(err)
		log.Printf("cloud reconcile %d: inventory unavailable: %s", res.ID, code)
		controlAPIError(w, http.StatusBadGateway, "provider_error", code)
		return
	}
	inventory := make(map[string]nodehost.Server, len(servers))
	for _, s := range servers {
		if s.ID != "" {
			inventory[s.ID] = s
		}
	}

	switch {
	case res.ExternalID != "":
		if srv, found := inventory[res.ExternalID]; found {
			// Инстанс есть и уже привязан этим external_id; застрявшие
			// состояния дотягиваются CAS-ом до ready с IP инвентаря, ready
			// не трогается.
			switch res.State {
			case control.CloudResPlanned, control.CloudResCreating, control.CloudResError:
				if err := r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResReady, res.ExternalID, srv.IP); err != nil {
					cloudControlErr(w, err)
					return
				}
			}
			writeCabinetJSON(w, http.StatusOK, map[string]string{"resolved": "found", "external_id": res.ExternalID})
			return
		}
		// Нет в инвентаре → провайдер-инстанса нет; падаем в общий
		// absent-переход ниже.
	default:
		claimed, err := r.control.CloudResourceClaimedExternalIDs(res.UserID, res.ConnectionID, res.ID)
		if err != nil {
			cloudControlErr(w, err)
			return
		}
		unaccounted := 0
		for id := range inventory {
			if !claimed[id] {
				unaccounted++
			}
		}
		if unaccounted > 0 {
			controlAPIError(w, http.StatusConflict, "provider_reconcile_required",
				fmt.Sprintf("%d unaccounted servers in provider inventory", unaccounted))
			return
		}
	}

	// Доказанное отсутствие инстанса: error/planned/creating → deleted
	// CAS-ом; ready/deleting (контракт) и живой воркер — resource_busy.
	if res.State != control.CloudResPlanned && res.State != control.CloudResCreating && res.State != control.CloudResError {
		controlAPIError(w, http.StatusConflict, "resource_busy", "resource is not in a reconcilable state")
		return
	}
	if r.cloudWorkerActive(res) {
		controlAPIError(w, http.StatusConflict, "resource_busy", "a worker is still in flight for this resource")
		return
	}
	if err := r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResDeleted, "", ""); err != nil {
		cloudControlErr(w, err)
		return
	}
	if _, err := r.control.ReleasePendingSecretsForCloudResource(res.ID); err != nil {
		log.Printf("cloud reconcile: release pending secrets for resource %d: %v", res.ID, err)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]string{"resolved": "absent"})
}

// cloudInstallPassword — расшифрованный root-пароль VPS из secrets_enc
// (сохранён при create). Наружу не отдаётся.
func (r *Registry) cloudInstallPassword(res control.CloudResourceV2) (string, error) {
	if res.SecretsEnc == "" {
		return "", errors.New("no sealed secret stored")
	}
	plain, err := cryptoutil.Open(r.masterKey, res.SecretsEnc)
	if err != nil {
		return "", err
	}
	var s struct {
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(plain), &s); err != nil {
		return "", err
	}
	return s.Password, nil
}

func writeCloudInstallErr(w http.ResponseWriter, err error, operation string) {
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
		controlAPIError(w, http.StatusNotFound, "not_found", "ресурс не найден")
	default:
		controlAPIError(w, http.StatusInternalServerError, "internal", "не удалось подготовить установку; обратитесь к администратору")
	}
}

// ── воркеры (in-process; общий provisionWorkers, но ключ — opID, как в
// beget/manual-путях: cloud_resources2.id — своя последовательность и НЕ
// должна использоваться как ключ общего guard. run=false (ключ занят) →
// операция failed "worker_busy", не молчаливый сброс) ─────────────

func (r *Registry) launchCloudCreate(resID int64, conn control.HostingConnection, cfg nodehost.CreateRequest, providerKey string, opID int64) {
	if !r.provisionWorkers().run(opID, cloudCreateBudget+cloudPollInterval, func(ctx context.Context) {
		defer r.cloudRecover(opID)
		_ = r.cloudCreateDo(ctx, resID, conn, cfg, providerKey, opID)
	}) {
		r.failCloud(resID, opID, control.CloudResPlanned, "worker_busy")
	}
}

func (r *Registry) launchCloudDelete(resID int64, conn control.HostingConnection, externalID string, opID int64) {
	if !r.provisionWorkers().run(opID, cloudDeleteBudget+cloudPollInterval, func(ctx context.Context) {
		defer r.cloudRecover(opID)
		defer r.cloudGuardRelease(resID)
		r.cloudDeleteDo(ctx, resID, conn, externalID, opID)
	}) {
		_ = r.control.UpdateOperationState(opID, control.OpFailed, "", "worker_busy")
		r.cloudGuardRelease(resID)
	}
}

// cloudRecover — паника cloud-воркера не должна ронять процесс: операция
// терминально failed, ресурс остаётся в наблюдаемом состоянии (L2).
func (r *Registry) cloudRecover(opID int64) {
	if p := recover(); p != nil {
		log.Printf("cloud worker panic op=%d: %v", opID, p)
		_ = r.control.UpdateOperationState(opID, control.OpFailed, "", "worker_panic")
	}
}

// failCreate — терминальный провал create: операция и last_error фиксируются
// ДО перехода ресурса в error, чтобы наблюдаемый error уже нёс причину.
func (r *Registry) failCloud(resID, opID int64, expectedState, code string) {
	_ = r.control.SetCloudResourceError(resID, code)
	_ = r.control.SetCloudResourceOperation(resID, opID)
	_ = r.control.UpdateOperationState(opID, control.OpFailed, "", code)
	_ = r.control.UpdateCloudResourceState(resID, expectedState, control.CloudResError, "", "")
}

// cloudCreateDo returns the provider operation id once Create was submitted.
// The provider operation id is persisted BEFORE the first poll so a restarted
// registry can resume with read-only Operation/Server calls (never Create).
func (r *Registry) cloudCreateDo(ctx context.Context, resID int64, conn control.HostingConnection, cfg nodehost.CreateRequest, providerKey string, opID int64) (providerOpID string) {
	if err := r.control.UpdateCloudResourceState(resID, control.CloudResPlanned, control.CloudResCreating, "", ""); err != nil {
		return "" // ресурс уже не наш (replay-уборка / рестарт)
	}
	deadline := time.Now().Add(cloudCreateBudget)
	prov, err := r.cloudProviderFor(conn)
	if err != nil {
		r.failCloud(resID, opID, control.CloudResCreating, "credentials_unavailable")
		return ""
	}
	if ok, err := r.control.MarkCreateSent(opID); err != nil || !ok {
		r.failCloud(resID, opID, control.CloudResCreating, "provider_reconcile_required")
		return ""
	}
	cr, err := prov.Create(ctx, cfg, providerKey)
	if err != nil {
		r.failCloud(resID, opID, control.CloudResCreating, cloudSafeErrCode(err))
		return ""
	}
	if cr == nil || cr.OperationID == "" {
		r.failCloud(resID, opID, control.CloudResCreating, "provider_reconcile_required")
		return ""
	}
	providerOpID = cr.OperationID
	if err := r.control.SetOperationProviderOpID(opID, providerOpID); err != nil {
		log.Printf("cloud create op=%d: persist provider operation id: %v", opID, err)
		r.failCloud(resID, opID, control.CloudResCreating, "provider_operation_persist_failed")
		return providerOpID
	}
	r.cloudCreatePoll(ctx, resID, prov, providerOpID, opID, control.CloudResCreating, deadline)
	return providerOpID
}

// cloudCreatePoll drives an already-submitted provider operation to
// ready/error. It is shared by the normal worker and the restart reconciler:
// it never calls Create. expectedState is the resource state used by CAS in
// failCloud.
func (r *Registry) cloudCreatePoll(ctx context.Context, resID int64, prov cloudProvider, providerOpID string, opID int64, expectedState string, deadline time.Time) {
	extID := ""
	// Поллинг операции (03 §4: до 10м).
	for {
		op, err := prov.Operation(ctx, providerOpID)
		if err != nil && cloudOpErrDefinitive(err) {
			// 401/403/404 не «подождут»: ключ/операция мертвы, полный бюджет
			// лишь растянул бы видимое «ожидание IP».
			r.failCloud(resID, opID, expectedState, cloudSafeErrCode(err))
			return
		}
		if err == nil && op != nil {
			switch op.Status {
			case "succeeded":
				extID = op.ResourceID
			case "failed":
				code := "provider_error"
				if op.Error != nil && op.Error.Code != "" {
					code = op.Error.Code
				}
				r.failCloud(resID, opID, expectedState, code)
				return
			}
			if extID != "" {
				break
			}
			if op.Status == "succeeded" {
				r.failCloud(resID, opID, expectedState, "invalid_response")
				return
			}
		}
		if time.Now().After(deadline) {
			r.failCloud(resID, opID, expectedState, "provider_timeout")
			return
		}
		select {
		case <-ctx.Done():
			// Worker budget == create budget + one poll: a scheduling delay can
			// expire the ctx before the deadline check. A timed-out worker must
			// not leave a silently stuck creating row.
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				r.failCloud(resID, opID, expectedState, "provider_timeout")
			}
			return // parent shutdown: ресурс остаётся creating (документировано)
		case <-time.After(cloudPollInterval):
		}
	}
	r.cloudWaitServer(ctx, resID, prov, extID, opID, expectedState, deadline)
}

// cloudWaitServer seals the root password before publishing ready and waits
// for a non-empty IP. expectedState is the CAS source state.
func (r *Registry) cloudWaitServer(ctx context.Context, resID int64, prov cloudProvider, extID string, opID int64, expectedState string, deadline time.Time) {
	// Keep the billable instance handle even if readiness/password reads fail.
	if err := r.control.UpdateCloudResourceState(resID, expectedState, expectedState, extID, ""); err != nil {
		return
	}
	if err := r.control.UpdateOperationState(opID, control.OpSent, extID, ""); err != nil {
		r.failCloud(resID, opID, expectedState, "control_unavailable")
		return
	}
	// Сервер должен отдать непустой IP и быть активным (пустой status —
	// провайдер его не сообщает). ready без IP никогда не публикуется.
	// Пароль запечатывается сразу, ДО ready (точка видимости состояния).
	ip, sealed, failCode := "", "", "provider_timeout"
	if op, err := r.control.Operation(opID); err == nil {
		if res, err := r.control.CloudResourceOwned(resID, op.UserID); err == nil {
			if password, err := r.cloudInstallPassword(res); err == nil && password != "" {
				sealed = res.SecretsEnc
			}
		}
	}
	for {
		srv, err := prov.Server(ctx, extID)
		if err != nil && cloudOpErrDefinitive(err) {
			r.failCloud(resID, opID, expectedState, cloudSafeErrCode(err))
			return
		}
		if err == nil && srv != nil {
			if srv.Password != "" && sealed == "" {
				enc, err := r.sealJSON(map[string]string{"password": srv.Password})
				if err != nil {
					r.failCloud(resID, opID, expectedState, "credentials_unavailable")
					return
				}
				if err := r.control.SetCloudResourceSecrets(resID, enc); err != nil {
					r.failCloud(resID, opID, expectedState, "control_unavailable")
					return
				}
				sealed = enc
			}
			active := strings.EqualFold(srv.Status, "ACTIVE") || strings.EqualFold(srv.Status, "RUNNING")
			if net.ParseIP(srv.IP) != nil && active && sealed != "" {
				ip, failCode = srv.IP, ""
				break
			}
			if net.ParseIP(srv.IP) == nil {
				failCode = "no_ip"
			} else if sealed == "" {
				failCode = "no_password"
			} else {
				failCode = "provider_timeout"
			}
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			if errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
				r.failCloud(resID, opID, expectedState, failCode)
			}
			return
		case <-time.After(cloudPollInterval):
		}
	}
	if failCode != "" {
		r.failCloud(resID, opID, expectedState, failCode)
		return
	}
	if err := r.control.UpdateCloudResourceState(resID, expectedState, control.CloudResReady, extID, ip); err != nil {
		return // ресурс удалён/перезаписан параллельно — не наша победа
	}
	_ = r.control.UpdateOperationState(opID, control.OpConfirmed, extID, "")
}

// cloudResumeInterval — период сверки ресурсов, застигнутых в planned|creating
// после рестарта/деплоя (воркеры in-process).
const cloudResumeInterval = 60 * time.Second

// cloudResumeLoop делает один проход на старте и затем периодически.
func (r *Registry) cloudResumeLoop(ctx context.Context) {
	r.reconcileStuckCloudCreates(ctx)
	t := time.NewTicker(cloudResumeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.reconcileStuckCloudCreates(ctx)
		}
	}
}

// cloudOpDeadline — остаток бюджета create-операции. Просроченная операция
// получает ровно одну read-only проверку (deadline = now), затем терминальный
// provider_timeout/no_ip.
func cloudOpDeadline(op control.Operation) time.Time {
	d := time.Unix(op.CreatedAt, 0).Add(cloudCreateBudget)
	if d.Before(time.Now()) {
		return time.Now()
	}
	return d
}

// reconcileStuckCloudCreates — startup/periodic проход по planned|creating.
// Никаких I/O под общими локами; провайдер опрашивается только на чтение
// (Operation/Server). Один ресурс не может получить два воркера (guard по opID).
func (r *Registry) reconcileStuckCloudCreates(ctx context.Context) {
	if r.control == nil {
		return
	}
	list, err := r.control.CloudResourcesInFlight()
	if err != nil {
		log.Printf("cloud reconcile: list in-flight: %v", err)
		return
	}
	for _, res := range list {
		r.reconcileStuckCloudCreate(ctx, res)
	}
}

func (r *Registry) reconcileStuckCloudCreate(ctx context.Context, res control.CloudResourceV2) {
	if res.LastOperationID <= 0 {
		_ = r.control.SetCloudResourceError(res.ID, "provider_reconcile_required")
		_ = r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResError, "", "")
		return
	}
	op, err := r.control.Operation(res.LastOperationID)
	if errors.Is(err, control.ErrNotFound) {
		_ = r.control.SetCloudResourceError(res.ID, "provider_reconcile_required")
		_ = r.control.UpdateCloudResourceState(res.ID, res.State, control.CloudResError, "", "")
		return
	}
	if err != nil || op.Kind != control.OpKindCloudCreate {
		return
	}
	if r.provisionWorkers().runningOp(op.ID) {
		return // живой воркер сам доведёт
	}
	if op.State == control.OpFailed || op.State == control.OpUnknown {
		r.control.ReconcileCloudResource(res) // существующая зомби-логика (CAS)
		return
	}
	if (op.State == control.OpPending || op.State == control.OpSent) && op.ProviderOpID == "" && op.ProviderVPSID == "" && res.ExternalID == "" {
		r.failCloud(res.ID, op.ID, res.State, "provider_reconcile_required")
		return // Never repeat Create, even for pre-migration pending rows.
	}
	conn, err := r.control.HostingConnectionOwned(res.ConnectionID, res.UserID)
	if err != nil {
		return
	}
	prov, err := r.cloudProviderFor(conn)
	if err != nil {
		r.failCloud(res.ID, op.ID, res.State, "credentials_unavailable")
		return
	}
	if op.ProviderVPSID != "" || res.ExternalID != "" {
		op.State = control.OpConfirmed // A known instance needs Server reads only.
	}
	switch op.State {
	case control.OpConfirmed:
		extID := op.ProviderVPSID
		if extID == "" {
			extID = res.ExternalID
		}
		if extID == "" {
			r.failCloud(res.ID, op.ID, res.State, "provider_reconcile_required")
			return
		}
		deadline := cloudOpDeadline(op)
		timeout := time.Until(deadline) + cloudPollInterval
		if timeout <= 0 {
			timeout = cloudPollInterval
		}
		resID, opID, expected := res.ID, op.ID, res.State
		r.provisionWorkers().run(opID, timeout, func(wctx context.Context) {
			defer r.cloudRecover(opID)
			r.cloudWaitServer(wctx, resID, prov, extID, opID, expected, deadline)
		})
	case control.OpPending, control.OpSent:
		deadline := cloudOpDeadline(op)
		timeout := time.Until(deadline) + cloudPollInterval
		if timeout <= 0 {
			timeout = cloudPollInterval
		}
		resID, opID, expected := res.ID, op.ID, res.State
		providerOpID := op.ProviderOpID
		r.provisionWorkers().run(opID, timeout, func(wctx context.Context) {
			defer r.cloudRecover(opID)
			r.cloudCreatePoll(wctx, resID, prov, providerOpID, opID, expected, deadline)
		})
	}
}

func (r *Registry) cloudDeleteDo(ctx context.Context, resID int64, conn control.HostingConnection, externalID string, opID int64) {
	// ready|error → deleting (retry после провала разрешён).
	if err := r.control.UpdateCloudResourceState(resID, control.CloudResReady, control.CloudResDeleting, "", ""); err != nil {
		if err := r.control.UpdateCloudResourceState(resID, control.CloudResError, control.CloudResDeleting, "", ""); err != nil {
			return
		}
	}
	// CAS pending→sent: конкурентная ретрансмиссия второго воркера отсечена.
	if ok, err := r.control.MarkCreateSent(opID); err != nil || !ok {
		return
	}
	prov, err := r.cloudProviderFor(conn)
	if err != nil {
		r.failCloud(resID, opID, control.CloudResDeleting, "credentials_unavailable")
		return
	}
	if _, err := prov.Delete(ctx, externalID); err != nil {
		r.failCloud(resID, opID, control.CloudResDeleting, cloudSafeErrCode(err))
		return
	}
	// Подтверждение чтением: Server() до not_found (Delete — не proof, §03 stage-03).
	deadline := time.Now().Add(cloudDeleteBudget)
	for {
		_, err := prov.Server(ctx, externalID)
		var ne *nodehost.Error
		if errors.As(err, &ne) && ne.Code == "not_found" {
			if err := r.control.UpdateCloudResourceState(resID, control.CloudResDeleting, control.CloudResDeleted, "", ""); err != nil {
				return
			}
			_ = r.control.UpdateOperationState(opID, control.OpDeleted, "", "")
			// Удалённый ресурс больше не цель retry — pending install бронь
			// освобождается, иначе она держала бы квоту вечно.
			if n, err := r.control.ReleasePendingSecretsForCloudResource(resID); err != nil {
				log.Printf("cloud delete: release pending secrets for resource %d: %v", resID, err)
			} else if n > 0 {
				log.Printf("cloud delete: released %d pending node secret(s) for resource %d", n, resID)
			}
			return
		}
		if time.Now().After(deadline) {
			r.failCloud(resID, opID, control.CloudResDeleting, "provider_timeout")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cloudPollInterval):
		}
	}
}

// ── management policy ноды (03 §5) ────────────────────────────────

type nodeManagementJSON struct {
	Mode              string         `json:"mode"`
	ProfileVersionID  *int64         `json:"profile_version_id"`
	Inputs            map[string]any `json:"inputs"`
	ProcessCurrentBan bool           `json:"process_current_ban"`
	Revision          int64          `json:"revision"`
	CurrentResourceID *int64         `json:"current_resource_id"`
}

func nodeManagementView(m control.NodeManagement) nodeManagementJSON {
	v := nodeManagementJSON{
		Mode: m.Mode, Inputs: map[string]any{}, ProcessCurrentBan: m.ProcessCurrentBan,
		Revision: m.Revision,
	}
	if m.ProfileVersionID > 0 {
		x := m.ProfileVersionID
		v.ProfileVersionID = &x
	}
	if m.CurrentResourceID > 0 {
		x := m.CurrentResourceID
		v.CurrentResourceID = &x
	}
	if m.InputsJSON != "" {
		_ = json.Unmarshal([]byte(m.InputsJSON), &v.Inputs)
	}
	return v
}

// nodeOwned — нода владельца через NodeOwner; legacy/чужая → 404.
func (r *Registry) nodeOwnedMgmt(w http.ResponseWriter, req *http.Request) (string, bool) {
	nodeID := req.PathValue("node_id")
	owner, legacy, ok, err := r.control.NodeOwner(nodeID)
	if err != nil {
		r.writeControlUnavailable(w)
		return "", false
	}
	if !ok || legacy || owner != sessionFrom(req).user.ID {
		controlAPIError(w, http.StatusNotFound, "not_found", "node is not owned by the current user")
		return "", false
	}
	return nodeID, true
}

// GET /api/me/nodes/{node_id}/management — без строки в БД отдаёт дефолт
// manual (строку не создаёт).
func (r *Registry) handleNodeManagementGet(w http.ResponseWriter, req *http.Request) {
	nodeID, ok := r.nodeOwnedMgmt(w, req)
	if !ok {
		return
	}
	m, err := r.control.NodeManagement(nodeID, sessionFrom(req).user.ID)
	if errors.Is(err, control.ErrNotFound) {
		writeCabinetJSON(w, http.StatusOK, nodeManagementJSON{
			Mode: "manual", Inputs: map[string]any{}, Revision: 1,
		})
		return
	}
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, nodeManagementView(m))
}

// PUT /api/me/nodes/{node_id}/management — CAS. manual keeps the legacy
// contract; profile (stage-08b-i) validates a pinned graph version, declared
// inputs, ownership and provider compatibility, then stores the policy with a
// ban-episode watermark (historical bans are not processed by default).
func (r *Registry) handleNodeManagementPut(w http.ResponseWriter, req *http.Request) {
	nodeID, ok := r.nodeOwnedMgmt(w, req)
	if !ok {
		return
	}
	var body struct {
		Mode              string                     `json:"mode"`
		ProfileVersionID  int64                      `json:"profile_version_id"`
		Inputs            map[string]json.RawMessage `json:"inputs"`
		ProcessCurrentBan bool                       `json:"process_current_ban"`
		ExpectedRevision  int64                      `json:"expected_revision"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	uid := sessionFrom(req).user.ID
	cur := int64(0)
	if m, err := r.control.NodeManagement(nodeID, uid); err == nil {
		cur = m.CurrentResourceID
	}
	if cur == 0 {
		// Автолинк: свежая нода без blueprint-прогона ещё не имеет
		// current_resource_id, но ресурс мог быть установлен на неё.
		cur = r.control.CloudResourceIDByNode(uid, nodeID)
	}
	if body.Mode == "manual" {
		m, err := r.control.PatchNodeManagement(nodeID, uid, body.ExpectedRevision, "manual", 0, false, cur)
		if err != nil {
			cloudControlErr(w, err)
			return
		}
		writeCabinetJSON(w, http.StatusOK, nodeManagementView(m))
		return
	}
	if body.Mode != "profile" {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "mode must be manual or profile")
		return
	}

	// 1. Pinned version exists, is a graph, and is own or published.
	pv, err := r.control.ProfileVersionByNumericID(body.ProfileVersionID)
	if err != nil {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "profile version is not available")
		return
	}
	if pv.Format != control.FormatGraph {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "profile version must be format=graph")
		return
	}
	p, err := r.control.Profile(pv.ProfileID)
	if err != nil || (!p.Published && p.CreatedBy != uid) {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "profile version is not available")
		return
	}
	bp, ds := blueprint.ValidateBytes([]byte(pv.Blueprint))
	if len(ds) > 0 {
		writeBlueprintInvalid(w, "profile_incompatible", "graph failed validation", ds)
		return
	}
	if body.Inputs == nil {
		body.Inputs = map[string]json.RawMessage{}
	}
	if ds := blueprint.ValidateInputs(bp, body.Inputs); len(ds) > 0 {
		writeBlueprintInvalid(w, "profile_incompatible", "inputs do not match the pinned graph", ds)
		return
	}

	// 2. Determine the node's provider: current resource, else the owned
	// connection declared in inputs. The pinned graph must support it.
	provider := ""
	if cur > 0 {
		if res, err := r.control.CloudResourceOwned(cur, uid); err == nil {
			provider = res.Provider
		}
	}
	if raw, ok := body.Inputs["connection_id"]; ok {
		var id int64
		if err := json.Unmarshal(raw, &id); err != nil || id <= 0 {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "input connection_id must be a number")
			return
		}
		conn, err := r.control.HostingConnectionOwned(id, uid)
		if err != nil {
			controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "input connection_id is not owned by the current user")
			return
		}
		provider = conn.Provider
	}
	if provider == "" || bp.Provider != provider {
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_incompatible", "pinned graph provider does not match the node")
		return
	}

	// 3. Activation watermark: default = now, so historical episodes are
	// skipped. process_current_ban=true admits exactly the latest episode.
	activatedAt := time.Now().Unix()
	if body.ProcessCurrentBan {
		if latest, err := r.control.LatestBanEpisode(nodeID); err == nil {
			activatedAt = latest.CreatedAt
		}
	}
	inputsJSON, _ := json.Marshal(body.Inputs)
	m, err := r.control.SetNodeManagementProfile(nodeID, uid, body.ExpectedRevision, pv.ID, string(inputsJSON), body.ProcessCurrentBan, cur, activatedAt)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	writeCabinetJSON(w, http.StatusOK, nodeManagementView(m))
}
