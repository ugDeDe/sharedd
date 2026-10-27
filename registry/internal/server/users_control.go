package server

// User-режим (docs/api-contract.md): контрольная БД, authkey-авторизация
// /register, миграция legacy-владельцев. Fail-closed по контракту: БД не
// открылась → user-API и регистрация, требующая владельца, отвечают 503;
// лог — единожды при неудачном Open (не спамим на каждый запрос).

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/cryptoutil"
)

// usersEnabled — [users] enabled=true (в противном случае точное прежнее
// поведение пула; control-БД не открывается вовсе).
func (r *Registry) usersEnabled() bool {
	return r.cfg != nil && r.cfg.Users.Enabled != nil && *r.cfg.Users.Enabled
}

// openControl открывает контрольную БД, если user-режим включён. Ошибка не
// фатальна для пула (legacy-регистрация общим токеном продолжает работать,
// контракт §1), но user-API обязаны отвечать 503 — control остаётся nil.
func (r *Registry) openControl() {
	if !r.usersEnabled() {
		return
	}
	s, err := control.Open(r.cfg.Database.ControlDB, r.cfg.Users.LegacyMaxNodes)
	if err != nil {
		logProfileControlErr("control.open", err)
		r.controlErrMu.Lock()
		r.controlErr = err
		r.controlErrMu.Unlock()
		return
	}
	r.controlErrMu.Lock()
	r.controlErr = nil
	r.controlErrMu.Unlock()
	r.control = s
	_ = s.PurgeSessions(time.Now())
	// Claim-on-first-register: брошенные pending-секреты старше 7 суток не
	// должны вечно держать место в квоте.
	if n, err := s.PurgePendingSecrets(time.Now().Add(-pendingSecretTTL).Unix()); err != nil {
		log.Printf("control: purge pending node secrets: %v", err)
	} else if n > 0 {
		log.Printf("control: purged %d stale pending node secret(s)", n)
	}
	// Бутстрап «Разрешить общий секрет» (contract §3): строка settings
	// создаётся при первом чтении значением [users] allow_shared_secret
	// (default true). Ошибка чтения — не блокируем пул: держим конфиг-дефолт.
	defaultAllow := r.cfg.Users.AllowSharedSecret == nil || *r.cfg.Users.AllowSharedSecret
	if on, err := s.LegacySecretEnabled(defaultAllow); err != nil {
		log.Printf("legacy shared secret settings unavailable, keeping config default (%t): %v", defaultAllow, err)
		r.legacyShared.Store(defaultAllow)
	} else {
		r.legacyShared.Store(on)
	}
	// Секреты Beget шифруются AES-GCM ключом вне БД. Неудачная загрузка не
	// роняет пул — только beget-секция кабинета отвечает 503.
	if path := strings.TrimSpace(r.cfg.Database.MasterKeyPath); path != "" {
		key, err := cryptoutil.LoadMasterKey(path)
		if err != nil {
			log.Printf("master key unavailable, beget endpoints degraded to 503: %v", err)
		} else {
			r.masterKey = key
		}
	}
	r.setupProvisioning()
	// Постоянные номера для уже привязанных неlegacy-нод. Идемпотентно:
	// повторный запуск не перенумеровывает существующие слоты.
	if n, err := s.BackfillNodeSlots(); err != nil {
		log.Printf("control: backfill node slots: %v", err)
	} else if n > 0 {
		log.Printf("control: assigned permanent node slots to %d node(s)", n)
	}
}

// setupProvisioning — штатный профиль sharedd-node (addendum-profiles-vps.md
// §4.4). Установка выполняется шагами по SSH; отдельного раннера/каталога
// playbook'ов больше нет. Пользовательские профили отключаются флагом
// [provisioning] user_profiles_enabled=false.
func (r *Registry) setupProvisioning() {
	if _, err := r.control.EnsureDefaultProfile(); err != nil {
		log.Printf("provisioning: ensure default profile: %v", err)
	}
}

func (r *Registry) closeControl() {
	if r.control != nil {
		r.control.Close()
		r.control = nil
	}
}

// migrateLegacyOwners — контракт §4: все ноды, зарегистрированные ДО
// включения user-режима, привязываются к системному пользователю legacy
// (место в квотах не занимают). Идемпотентно: уже привязанные не трогаем.
func (r *Registry) migrateLegacyOwners() {
	if r.control == nil {
		return
	}
	r.mu.RLock()
	nodeIDs := make([]string, 0, len(r.state.Candidates))
	for id := range r.state.Candidates {
		nodeIDs = append(nodeIDs, id)
	}
	r.mu.RUnlock()
	bound := 0
	for _, id := range nodeIDs {
		if err := r.control.BindLegacyNode(id); err != nil {
			log.Printf("control: legacy migration %s: %v", id, err)
			continue
		}
		bound++
	}
	if bound > 0 {
		log.Printf("control: %d existing node(s) bound to legacy user", bound)
	}
	// Активные кандидаты должны занимать место в квоте: после смены семантики
	// (used_nodes = reservations, не node_owners) до-апгрейдные регистрации
	// иначе остались бы бесплатными до следующего /register.
	if err := r.control.ReconcileQuota(nodeIDs); err != nil {
		log.Printf("control: reconcile pool quota: %v", err)
	}
}

// controlAPIError пишем контрактным форматом §5: {"error":"<code>"} + текст.
func controlAPIError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": msg})
}

// No restart diagnosis without runtime evidence.
// controlUnavailableMsg — единый текст отказов fail-closed.
const controlUnavailableMsg = "Хранилище пользователей недоступно — обратитесь к администратору для проверки журнала регистратора"

// pendingSecretTTL — срок жизни незаявленного pending-секрета manual-установки.
const pendingSecretTTL = 7 * 24 * time.Hour

// controlUnavailableLogInterval — троттлинг серверного лога: при наплыве
// запросов в окне перезапуска пишем причину не чаще раза в интервал.
const controlUnavailableLogInterval = time.Minute

// writeControlUnavailable — fail-closed ответ (503) для user-API. Поведение
// (код/статус) не меняется; дополнительно пишем причину в лог (троттлинг).
func (r *Registry) writeControlUnavailable(w http.ResponseWriter) {
	r.logControlUnavailable()
	controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
}

// logControlUnavailable — одна запись в controlUnavailableLogInterval с
// underlying-причиной, чтобы оператор отличал окно перезапуска (control ещё
// не открыт) от genuinely сломанной БД (ошибка Open).
func (r *Registry) logControlUnavailable() {
	r.controlErrMu.Lock()
	defer r.controlErrMu.Unlock()
	if !r.controlLogAt.IsZero() && time.Since(r.controlLogAt) < controlUnavailableLogInterval {
		return
	}
	r.controlLogAt = time.Now()
	if r.controlErr != nil {
		logProfileControlErr("control.unavailable.open_failed", r.controlErr)
	} else if r.control == nil {
		log.Printf("user API 503 control_unavailable: control store is nil; initialization cause not recorded")
	} else {
		log.Printf("user API 503 control_unavailable: request failed with an initialized store; inspect operation diagnostics")
	}
}

// authorizeUserRegister — gates /register в user-режиме (контракт §3).
// Bearer уже проверен middleware — authkey/node_secret поверх него.
// auth — классификация Bearer (machineAuthFrom): node_secret владельца
// этой ноды заменяет authkey при перерегистрации (contract §1 «перевыпуск»).
// Порядок: БД жива? → свой node_secret / authkey найден/жив? → пользователь
// активен? → владелец совпадает / место захвачено (транзакционно, с квотой).
// Возвращает (userID, ok): userID=0 в legacy/без-authkey пути (нет арендного
// контекста); ok=false = ответ уже записан, регистрацию прекратить.
func (r *Registry) authorizeUserRegister(w http.ResponseWriter, body registerRequest, auth machineAuthInfo) (int64, bool) {
	if r.control == nil {
		r.writeControlUnavailable(w)
		return 0, false
	}
	// Claim-on-first-register (manual installs): Bearer — pending-секрет
	// установки. Валидируем node_id против NAME-HASH + label, применяем квоту
	// AuthorizeNode-семантики и привязываем владельца. Место в квоте НЕ
	// расходуется, если квота превышена (pending сохраняется для повтора).
	if auth.kind == machineAuthPending {
		uid, err := r.claimPendingRegister(body, auth)
		if err != nil {
			writeClaimErr(w, err)
			return 0, false
		}
		return uid, true
	}
	// Перевыпуск по node_secret (contract §1): Bearer — персональный секрет
	// ИМЕННО этой ноды → авторизация как владельца, authkey не требуется.
	if auth.kind == machineAuthNode && auth.ownerNodeID == body.NodeID {
		ownerID, legacy, ok, err := r.control.NodeOwner(body.NodeID)
		if err != nil {
			r.writeControlUnavailable(w)
			return 0, false
		}
		if ok {
			if !legacy {
				u, err := r.control.User(ownerID)
				if err != nil {
					r.writeControlUnavailable(w)
					return 0, false
				}
				if err := r.control.AuthorizeNode(body.NodeID, u.User); err != nil {
					writeClaimErr(w, err)
					return 0, false
				}
			}
			return ownerID, true
		}
	}
	// FULL legacy-совместимость при ON (owner intent): валидный общий токен
	// без authkey. Новая нода привязывается к системному `legacy` в пределах
	// legacy_max_nodes; уже привязанная к реальному пользователю по-прежнему
	// требует authkey. OFF сюда не доходит — gateMachineAuth режет общий
	// Bearer без authkey на /register раньше (401 shared_token_disabled), а
	// nodeAPISecurityEnabled гарантирует, что Bearer — именно общий токен,
	// а не «пустой» default без настроенного node_token.
	if auth.kind == machineAuthShared && body.AuthKey == "" &&
		r.nodeAPISecurityEnabled() && r.legacySharedSecretEnabled() {
		return r.authorizeLegacySharedRegister(w, body.NodeID)
	}
	if body.AuthKey == "" {
		// Legacy-агент без authkey: перерегистрация ноды, привязанной к
		// системному legacy-пользователю, разрешена (контракт §4). Всё
		// остальное (новые ноды, ноды реальных пользователей) — отказ.
		_, legacy, ok, err := r.control.NodeOwner(body.NodeID)
		switch {
		case err != nil:
			r.writeControlUnavailable(w)
			return 0, false
		case ok && legacy:
			return 0, true
		case ok:
			controlAPIError(w, http.StatusForbidden, "bad_authkey", "node belongs to a user account, authkey required")
			return 0, false
		default:
			controlAPIError(w, http.StatusForbidden, "bad_authkey", "authkey required")
			return 0, false
		}
	}
	u, err := r.control.LookupAuthkey(body.AuthKey)
	if errors.Is(err, control.ErrNotFound) {
		controlAPIError(w, http.StatusForbidden, "bad_authkey", "unknown or revoked authkey")
		return 0, false
	}
	if err != nil {
		r.writeControlUnavailable(w)
		return 0, false
	}
	if u.Disabled {
		controlAPIError(w, http.StatusForbidden, "user_disabled", "user account is disabled")
		return 0, false
	}
	if err := r.control.AuthorizeNode(body.NodeID, u); err != nil {
		switch {
		case errors.Is(err, control.ErrOwnerMismatch):
			controlAPIError(w, http.StatusForbidden, "owner_mismatch", "node belongs to another user")
		case errors.Is(err, control.ErrQuotaExceeded):
			controlAPIError(w, http.StatusForbidden, "quota_exceeded", fmt.Sprintf("max_nodes=%d reached", u.MaxNodes))
		case errors.Is(err, control.ErrNodeDeleting):
			controlAPIError(w, http.StatusForbidden, "node_deleting", "node was deleted by the owner; reinstall with a new identity")
		default:
			r.writeControlUnavailable(w)
		}
		return 0, false
	}
	return u.ID, true
}

// claimPendingRegister — claim pending-секрета manual-установки при /register.
// node_id валидируется против NAME-HASH и обязан начинаться с label'а
// пользователя (реестр имя не выдумывает).
func (r *Registry) claimPendingRegister(body registerRequest, auth machineAuthInfo) (int64, error) {
	if err := validateNodeID(body.NodeID); err != nil {
		return 0, err
	}
	return r.control.ClaimPendingNodeSecret(auth.pendingSecret, body.NodeID)
}

// writeClaimErr маппит ошибку claim'а на коды machine-API.
func writeClaimErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, control.ErrQuotaExceeded):
		controlAPIError(w, http.StatusForbidden, "quota_exceeded", "max_nodes reached; pending secret kept")
	case errors.Is(err, control.ErrUserDisabled):
		controlAPIError(w, http.StatusForbidden, "user_disabled", "user account is disabled")
	case errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusForbidden, "bad_node_id", "node_id must match NAME-HASH and the installer label")
	case errors.Is(err, control.ErrOwnerMismatch):
		controlAPIError(w, http.StatusForbidden, "owner_mismatch", "node_id is already bound to another account")
	case errors.Is(err, control.ErrNotFound):
		controlAPIError(w, http.StatusForbidden, "stale_node_secret", "pending secret is no longer valid; reinstall")
	default:
		controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
	}
}

// authorizeLegacySharedRegister — /register при «Разрешить общий секрет»=ON,
// Bearer = общий токен, authkey отсутствует (FULL legacy-совместимость).
// Новая нода привязывается к системному `legacy` тем же транзакционным
// AuthorizeNode — квота legacy_max_nodes и гонки бесплатно. Уже привязанная к
// реальному пользователю — прежний отказ bad_authkey (её владелец обязан
// предъявить authkey или свой node_secret). Привязанная к legacy — пропуск.
func (r *Registry) authorizeLegacySharedRegister(w http.ResponseWriter, nodeID string) (int64, bool) {
	ownerID, _, ok, err := r.control.NodeOwner(nodeID)
	if err != nil {
		r.writeControlUnavailable(w)
		return 0, false
	}
	if ok {
		if ownerID == r.control.LegacyUserID() {
			return 0, true // legacy-нода: прежнее поведение
		}
		controlAPIError(w, http.StatusForbidden, "bad_authkey", "node belongs to a user account, authkey required")
		return 0, false
	}
	legacy, err := r.control.User(r.control.LegacyUserID())
	if err != nil {
		r.writeControlUnavailable(w)
		return 0, false
	}
	if err := r.control.AuthorizeNode(nodeID, legacy.User); err != nil {
		switch {
		case errors.Is(err, control.ErrQuotaExceeded):
			controlAPIError(w, http.StatusForbidden, "quota_exceeded",
				fmt.Sprintf("legacy_max_nodes=%d reached", legacy.MaxNodes))
		case errors.Is(err, control.ErrOwnerMismatch):
			controlAPIError(w, http.StatusForbidden, "owner_mismatch", "node belongs to another user")
		default:
			r.writeControlUnavailable(w)
		}
		return 0, false
	}
	return legacy.ID, true
}
