package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"sharedd/registry/internal/machineapi"
)

// Bearer-аутентификация machine API. Сами контракты и валидация тел —
// в internal/machineapi; здесь только привязка к Registry.

func (r *Registry) nodeAPISecurityEnabled() bool {
	return r != nil && r.cfg != nil && r.cfg.Security.NodeToken != ""
}

// ── Матрица machine-аутентификации (invitations-migration-contract.md §1) ──
//
// Bearer классифицируется один раз на запрос (middleware requireNodeAuth):
// это либо общий [security] node_token, либо персональный node_secret
// (поиск по sha256 среди node_owners). Матрица применяется ГЕЙТОМ в каждом
// хендлере (machineGate) — после разбора тела, т.к. /register при OFF
// пропускает общий Bearer с authkey в теле.
//
// | Режим      | Bearer общий            | Bearer node_secret            |
// | ON         | принимается             | принимается, node_id == владельцу (иначе 403) |
// | OFF        | 401 shared_token_disabled | принимается, node_id == владельцу (иначе 403) |
//
// Привязка владельца для персонального секрета действует ВСЕГДА (не зависит
// от legacy-переключателя) — см. gateMachineAuth.

type machineAuthKind int

const (
	machineAuthShared machineAuthKind = iota
	machineAuthNode
	// machineAuthPending — Bearer совпал с pending_node_secrets: секрет
	// установки manual-сервера, ещё не claim'нутый /register'ом. Валиден
	// только на POST /register (claim-on-first-register).
	machineAuthPending
)

type machineAuthInfo struct {
	kind        machineAuthKind
	ownerNodeID string // только kindNode
	// pendingSecret — сам Bearer (только kindPending): claim хеширует его.
	pendingSecret string
}

type machineAuthKey struct{}

// machineAuthFrom — классификация Bearer запроса (shared по умолчанию:
// запросы мимо requireNodeAuth, напр. прямые вызовы хендлеров в тестах).
func machineAuthFrom(req *http.Request) machineAuthInfo {
	info, _ := req.Context().Value(machineAuthKey{}).(machineAuthInfo)
	return info
}

// requireNodeAuth — классификация Bearer (общий vs node_secret) на каждом
// machine-маршруте. Неизвестный Bearer → 401 (прежнее поведение).
func (r *Registry) requireNodeAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Production config loading rejects an empty token. This branch permits
		// small unit-test Registry literals that never pass through startup.
		if !r.nodeAPISecurityEnabled() {
			next.ServeHTTP(w, req)
			return
		}
		provided := bearerToken(req)
		info := machineAuthInfo{kind: machineAuthShared}
		wantHash := sha256.Sum256([]byte(r.cfg.Security.NodeToken))
		gotHash := sha256.Sum256([]byte(provided))
		switch {
		case subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) == 1:
			// общий токен — самый частый случай, без обращений к БД.
		default:
			if owner, ok := r.nodeSecretOwner(provided); ok {
				info = machineAuthInfo{kind: machineAuthNode, ownerNodeID: owner}
			} else if r.pendingSecret(provided) {
				info = machineAuthInfo{kind: machineAuthPending, pendingSecret: provided}
			} else {
				w.Header().Set("WWW-Authenticate", `Bearer realm="node-api"`)
				// Неизвестный Bearer при ON — скорее всего протухший node_secret
				// прошлого состояния реестра. Код сообщает агенту «сбрось секрет
				// и перерегистрируйся общим токеном»; принимаем по-прежнему
				// ТОЛЬКО точный общий токен, произвольные строки не валидны.
				code := "unauthorized"
				if provided != "" && r.legacySharedSecretEnabled() {
					code = "stale_node_secret"
				}
				controlAPIError(w, http.StatusUnauthorized, code, "authentication required")
				return
			}
		}
		next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), machineAuthKey{}, info)))
	})
}

func bearerToken(req *http.Request) string {
	if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// nodeSecretOwner — владелец предъявленного node_secret (sha256-lookup).
// control не открыта → секретов не существует (legacy-режим).
func (r *Registry) nodeSecretOwner(secret string) (string, bool) {
	if r.control == nil {
		return "", false
	}
	owner, ok, err := r.control.NodeSecretOwner(secret)
	if err != nil {
		return "", false // fail-closed: ошибка БД не повышает привилегии
	}
	return owner, ok
}

// pendingSecret — предъявленный Bearer совпал с незаявленным pending-секретом
// manual-установки. control не открыта → pending не существует.
func (r *Registry) pendingSecret(secret string) bool {
	if r.control == nil || secret == "" {
		return false
	}
	_, ok, err := r.control.PendingSecret(secret)
	return err == nil && ok
}

// legacySharedSecretEnabled — «Разрешить общий секрет». control нет
// ([users] enabled=false) → переключателя не существует, общий секрет
// остаётся единственным транспортом (прежнее поведение).
func (r *Registry) legacySharedSecretEnabled() bool {
	if r.control == nil {
		return true
	}
	return r.legacyShared.Load()
}

// gateMachineAuth — применить матрицу к конкретному запросу.
// nodeID — node_id маршрута (тело либо X-ShareDD-Node-ID для /config);
// registerAuthKey — непустой authkey тела /register (OFF пропускает общий
// Bearer с ним: персональные учётные данные, валидность проверит
// authorizeUserRegister). false = ответ уже записан.
func (r *Registry) gateMachineAuth(w http.ResponseWriter, req *http.Request, nodeID, registerAuthKey string) bool {
	if !r.nodeAPISecurityEnabled() {
		return true
	}
	info, _ := req.Context().Value(machineAuthKey{}).(machineAuthInfo)
	if info.kind == machineAuthPending {
		// Pending-секрет валиден ровно для claim'а на POST /register; любой
		// другой machine-маршрут требует уже привязанный node_secret. Claim
		// дополнительно проверяет node_id и квоту в authorizeUserRegister.
		if req.Pattern == "POST /register" {
			return true
		}
		controlAPIError(w, http.StatusForbidden, "pending_secret",
			"pending node secret is only valid for registration")
		return false
	}
	if info.kind == machineAuthNode {
		// Персональный секрет привязан ровно к своей ноде: владелец секрета
		// обязан совпадать с node_id маршрута ВСЕГДА, независимо от
		// legacy-переключателя общего секрета. Иначе нода с личным секретом
		// могла бы действовать за чужой node_id (heartbeat/config/report).
		if info.ownerNodeID != nodeID {
			controlAPIError(w, http.StatusForbidden, "node_id_mismatch",
				"bearer node secret belongs to another node")
			return false
		}
		return true
	}
	if r.legacySharedSecretEnabled() {
		return true
	}
	if registerAuthKey != "" {
		return true
	}
	controlAPIError(w, http.StatusUnauthorized, "shared_token_disabled",
		"shared token is disabled; use the personal node secret")
	return false
}

// ── GET /api/node/status (DEF-B) ──────────────────────────────────
//
// Read-only подтверждение, что реестр ЗНАЕТ ноду: Bearer — персональный
// node_secret ИМЕННО этой ноды (общий токен не принимается — он не доказывает
// личность), node_id из query. 200 только если нода уже успешно вызвала
// /register (живой кандидат); иначе 404. Секретов в ответе нет. Используется
// verify-шагом штатного профиля: провал /register делает установку failed, а не
// ложный done.
func (r *Registry) handleNodeStatus(w http.ResponseWriter, req *http.Request) {
	nodeID := strings.TrimSpace(req.URL.Query().Get("node_id"))
	info := machineAuthFrom(req)
	if info.kind != machineAuthNode {
		// Pending (ещё не claim'нутый) секрет сюда не проходит: установка не
		// завершена. Verify-шаг ретраит до claim'а.
		controlAPIError(w, http.StatusForbidden, "node_id_mismatch",
			"personal node secret required for this node_id")
		return
	}
	if nodeID == "" {
		// node_id manual-сервера неизвестен до claim'а: резолвим по Bearer'у.
		nodeID = info.ownerNodeID
	} else if err := validateNodeID(nodeID); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	if info.ownerNodeID != nodeID {
		controlAPIError(w, http.StatusForbidden, "node_id_mismatch",
			"personal node secret required for this node_id")
		return
	}
	r.mu.Lock()
	_, registered := r.state.Candidates[nodeID]
	r.mu.Unlock()
	if !registered {
		controlAPIError(w, http.StatusNotFound, "not_registered",
			"node has not registered with the registry")
		return
	}
	owner := ""
	if r.control != nil {
		if uid, _, ok, err := r.control.NodeOwner(nodeID); err == nil && ok {
			if u, err := r.control.User(uid); err == nil {
				owner = u.Username
			}
		}
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"node_id": nodeID, "registered": true, "owner": owner,
	})
}

var (
	validateNodeID          = machineapi.ValidateNodeID
	validatePublicIPv4      = machineapi.ValidatePublicIPv4
	validateRegisterRequest = machineapi.ValidateRegisterRequest
	validateHealthReport    = machineapi.ValidateHealthReport
	decodeNodeJSON          = machineapi.DecodeJSON
)

// Алиасы wire-контрактов: сервер и тесты используют исторические имена.
type (
	registerRequest      = machineapi.RegisterRequest
	nodeIntervals        = machineapi.Intervals
	sharedConfigResponse = machineapi.SharedConfigResponse
	HealthReportPayload  = machineapi.HealthReportPayload
)

const (
	maxNodeJSONBytes       = machineapi.MaxNodeJSONBytes
	maxMetricsSnapshotSize = machineapi.MaxMetricsSnapshotSize
	maxReportAge           = machineapi.MaxReportAge
	maxReportFutureSkew    = machineapi.MaxReportFutureSkew
)
