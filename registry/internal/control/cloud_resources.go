package control

// Cloud-ресурсы v2 + политика управления нодой (docs/specs/hosting-blueprints/
// 03-cabinet-api.md §4-§5, 04 §2, stage-05). Таблица cloud_resources2 — НОВАЯ
// форма (surrogate PK, ссылка на hosting_connections); legacy cloud_resources
// (vps_id PK, квотный контур beget) не трогается. Секреты VPS (root-пароль) —
// только в secrets_enc (AES-GCM), наружу json:"-". Состояние ресурса меняется
// CAS-ом по предыдущему состоянию: воркер и retry не могут дважды пройти один
// переход (0 строк → ErrRevisionConflict).
//
// Имена состояний: planned → creating → ready → deleting → deleted,
// error — терминально-видимое (retry delete/create-воркера разрешён).

import (
	"database/sql"
	"errors"
	"time"
)

// Состояния cloud_resources2.state (приложение управляет, без CHECK).
const (
	CloudResPlanned  = "planned"
	CloudResCreating = "creating"
	CloudResReady    = "ready"
	CloudResDeleting = "deleting"
	CloudResDeleted  = "deleted"
	CloudResError    = "error"
)

// Виды операций облака (operations.kind; CHECK на kind нет — таблица общая).
const (
	OpKindCloudCreate = "cloud_create"
	OpKindCloudDelete = "cloud_delete"
)

// CloudResourceV2 — управляемый ресурс. SecretsEnc никогда не сериализуется.
type CloudResourceV2 struct {
	ID                   int64  `json:"id"`
	UserID               int64  `json:"user_id"`
	ConnectionID         int64  `json:"connection_id"`
	Provider             string `json:"provider"`
	ExternalID           string `json:"external_id"`
	NodeID               string `json:"node_id"`
	Label                string `json:"label"`
	State                string `json:"state"`
	Revision             int64  `json:"revision"`
	IP                   string `json:"ip"`
	SecretsEnc           string `json:"-"`
	ReplacesResourceID   int64  `json:"-"` // 0 = нет; в DTO отдаёт *int64
	ReplacedByResourceID int64  `json:"-"`
	LastError            string `json:"last_error"`
	LastOperationID      int64  `json:"-"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"-"`
}

const cloudResCols = `id, user_id, connection_id, provider, external_id, node_id, label, state, revision, ip, secrets_enc, replaces_resource_id, replaced_by_resource_id, created_at, updated_at, last_error, last_operation_id`

func scanCloudResource(row interface{ Scan(...any) error }) (CloudResourceV2, error) {
	var (
		r             CloudResourceV2
		replaces, rep sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.UserID, &r.ConnectionID, &r.Provider, &r.ExternalID,
		&r.NodeID, &r.Label, &r.State, &r.Revision, &r.IP, &r.SecretsEnc,
		&replaces, &rep, &r.CreatedAt, &r.UpdatedAt, &r.LastError, &r.LastOperationID)
	if err != nil {
		return CloudResourceV2{}, err
	}
	r.ReplacesResourceID, r.ReplacedByResourceID = replaces.Int64, rep.Int64
	return r, nil
}

func validCloudState(st string) bool {
	switch st {
	case CloudResPlanned, CloudResCreating, CloudResReady, CloudResDeleting, CloudResDeleted, CloudResError:
		return true
	}
	return false
}

// CreateCloudResource — новая строка в состоянии planned (revision 1).
func (s *Store) CreateCloudResource(r CloudResourceV2) (CloudResourceV2, error) {
	if r.UserID == 0 || r.ConnectionID == 0 || !validHostingProvider(r.Provider) || !validCloudState(r.State) {
		return CloudResourceV2{}, ErrValidation
	}
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`INSERT INTO cloud_resources2
		 (user_id, connection_id, provider, external_id, node_id, label, state, revision, ip, secrets_enc, replaces_resource_id, replaced_by_resource_id, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,1,'','',?,?,?,?)`,
		r.UserID, r.ConnectionID, r.Provider, r.ExternalID, r.NodeID, r.Label, r.State,
		nullInt(r.ReplacesResourceID), nullInt(r.ReplacedByResourceID), now, now)
	if err != nil {
		return CloudResourceV2{}, err
	}
	r.ID, _ = res.LastInsertId()
	r.Revision, r.IP, r.SecretsEnc, r.CreatedAt, r.UpdatedAt = 1, "", "", now, now
	return r, nil
}

// DeletePlannedCloudResource — убрать осиротевшую planned-строку (проигравший
// конкурентный create с тем же Idempotency-Key). CAS по state: уже ушедший в
// creating ресурс чужого воркера не заденет.
func (s *Store) DeletePlannedCloudResource(id int64) error {
	res, err := s.sql.Exec(`DELETE FROM cloud_resources2 WHERE id = ? AND state = ?`, id, CloudResPlanned)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteCloudResourceLocal removes a row whose provider instance was never
// created (external_id == "") together with any live pointer still naming it.
// Unknown paid outcomes are blocked even when no worker survived a restart.
func (s *Store) DeleteCloudResourceLocal(id, userID int64) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE cloud_resources2 SET id = id WHERE id = ?`, id); err != nil {
		return err
	}
	// Owner-проверка ДО освобождения брони: чужой delete не должен трогать
	// pending-секреты владельца.
	var owner, opID int64
	var state, externalID string
	err = tx.QueryRow(`SELECT user_id, state, external_id, last_operation_id FROM cloud_resources2 WHERE id = ?`, id).
		Scan(&owner, &state, &externalID, &opID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != userID {
		return ErrNotFound
	}
	safe := state == CloudResPlanned && opID == 0
	if externalID != "" {
		return ErrCreateInProgress
	}
	if opID > 0 {
		op, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE id = ?`, opID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && op.Kind == OpKindCloudCreate && op.UserID == userID && op.ProviderVPSID == "" {
			safe = state == CloudResPlanned && op.State == OpPending && op.ProviderOpID == ""
			if state == CloudResError && op.State == OpFailed {
				safe = op.LastError == "insufficient_funds" ||
					(op.ProviderOpID == "" && (op.LastError == "credentials_unavailable" || op.LastError == "create_not_submitted"))
			}
		}
	}
	if !safe {
		return ErrCreateInProgress
	}
	if opID > 0 {
		if _, err := tx.Exec(`UPDATE operations SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
			OpFailed, time.Now().Unix(), opID, OpPending); err != nil {
			return err
		}
	}
	// Pending install бронь освобождается в той же транзакции: после удаления
	// ресурса retry невозможен, и бронь иначе держала бы квоту вечно. Строка
	// ресурса ещё существует — last_operation_id виден.
	if _, err := releasePendingForCloudResource(tx, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM cloud_resources2 WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(
		`UPDATE node_management SET current_resource_id = NULL, updated_at = ?
		 WHERE user_id = ? AND current_resource_id = ?`,
		time.Now().Unix(), userID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CloudResourceOwned — ресурс владельца; чужое/неизвестное → ErrNotFound.
func (s *Store) CloudResourceOwned(id, userID int64) (CloudResourceV2, error) {
	r, err := scanCloudResource(s.sql.QueryRow(
		`SELECT `+cloudResCols+` FROM cloud_resources2 WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return CloudResourceV2{}, ErrNotFound
	}
	return r, err
}

// CloudResourceIDByNode — последний живой (не deleted/error) ресурс владельца,
// привязанный к ноде; 0 если нет. Нужен для автолинка current_resource_id при
// включении profile на свежей ноде (blueprint ещё не привязывал ресурс).
func (s *Store) CloudResourceIDByNode(userID int64, nodeID string) int64 {
	if nodeID == "" {
		return 0
	}
	var id int64
	_ = s.sql.QueryRow(
		`SELECT id FROM cloud_resources2 WHERE user_id = ? AND node_id = ? AND state NOT IN ('deleted','error') ORDER BY id DESC LIMIT 1`,
		userID, nodeID).Scan(&id)
	return id
}

// cloudZombieUnknownAge — сколько ждём "unknown" операцию, прежде чем счесть
// ресурс осиротевшим (воркер мог умереть между dispatch и записью результата).
const cloudZombieUnknownAge = 3 * time.Minute

// ReconcileCloudResource — проекция зомби-ресурса: planned/creating, чья
// last_operation_id указывает на терминально failed (или давно unknown)
// операцию, переводится в error с безопасным кодом операции. CAS по
// ожидаемому состоянию: конкурентный воркер, успевший сдвинуть состояние,
// побеждает, и мы возвращаем его прогресс. Без вызовов провайдера и без
// блокировок вокруг I/O; ошибки БД → исходная строка (следующий список
// поправит).
func (s *Store) ReconcileCloudResource(r CloudResourceV2) CloudResourceV2 {
	if (r.State != CloudResPlanned && r.State != CloudResCreating) || r.LastOperationID <= 0 {
		return r
	}
	op, err := s.Operation(r.LastOperationID)
	if err != nil {
		return r
	}
	now := time.Now().Unix()
	zombie := op.State == OpFailed ||
		(op.State == OpUnknown && now-op.UpdatedAt >= int64(cloudZombieUnknownAge/time.Second))
	if !zombie {
		return r
	}
	code := op.LastError
	if code == "" || len(code) > 64 {
		code = "provider_error"
	}
	res, err := s.sql.Exec(
		`UPDATE cloud_resources2 SET state = ?, last_error = ?, revision = revision + 1, updated_at = ?
		 WHERE id = ? AND state = ?`,
		CloudResError, code, now, r.ID, r.State)
	if err != nil {
		return r
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Состояние уже сменил воркер — покажем актуальную строку, если CAS
		// ушёл не в error.
		if fresh, err := s.CloudResourceOwned(r.ID, r.UserID); err == nil && fresh.State != CloudResError {
			return fresh
		}
		return r
	}
	r.State, r.LastError, r.Revision = CloudResError, code, r.Revision+1
	return r
}

// CloudResourcesByUser — ресурсы пользователя (новые первыми). deleted — это
// история операций, по умолчанию скрыта; includeDeleted=true возвращает всё.
func (s *Store) CloudResourcesByUser(userID int64, includeDeleted bool) ([]CloudResourceV2, error) {
	q := `SELECT ` + cloudResCols + ` FROM cloud_resources2 WHERE user_id = ?`
	if !includeDeleted {
		q += ` AND state <> 'deleted'`
	}
	rows, err := s.sql.Query(q+` ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CloudResourceV2{}
	for rows.Next() {
		r, err := scanCloudResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CloudResourceClaimedExternalIDs — external_id, занятые другими живыми
// (не deleted) ресурсами пользователя на том же подключении. Нужно
// reconcile-инвентаризации: сервер инвентаря, уже привязанный к другому
// ресурсу, не «неучтённый», и его нельзя отдать третьему ресурсу.
func (s *Store) CloudResourceClaimedExternalIDs(userID, connectionID, excludeID int64) (map[string]bool, error) {
	rows, err := s.sql.Query(
		`SELECT external_id FROM cloud_resources2
		 WHERE user_id = ? AND connection_id = ? AND id <> ? AND external_id <> '' AND state <> 'deleted'`,
		userID, connectionID, excludeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// CloudResourcesInFlight — ресурсы в planned|creating. После рестарта их
// воркеры потеряны; серверный reconcile возобновляет read-only сверку.
func (s *Store) CloudResourcesInFlight() ([]CloudResourceV2, error) {
	rows, err := s.sql.Query(`SELECT ` + cloudResCols + ` FROM cloud_resources2
		WHERE state IN ('planned','creating') ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CloudResourceV2{}
	for rows.Next() {
		r, err := scanCloudResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CloudResourcesUncertain — кандидаты фоновой auto-reconcile сверки:
// planned/creating без внешнего id (исход create неизвестен) либо error с
// provider_reconcile_required. Такие строки нельзя удалить локально, пока
// инвентарь провайдера не докажет отсутствие инстанса.
func (s *Store) CloudResourcesUncertain() ([]CloudResourceV2, error) {
	rows, err := s.sql.Query(`SELECT ` + cloudResCols + ` FROM cloud_resources2
		WHERE state IN ('planned','creating','error')
		  AND (external_id = '' OR (state = 'error' AND last_error = 'provider_reconcile_required'))
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []CloudResourceV2{}
	for rows.Next() {
		r, err := scanCloudResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateCloudResourceState — CAS-переход WHERE state = expectedState.
// Непустые externalID/ip записываются, пустые — не трогают колонку. Каждый
// успешный переход bumpает revision. 0 строк → ErrNotFound (нет/чужой) либо
// ErrRevisionConflict (ресурс есть, состояние уже другое).
func (s *Store) UpdateCloudResourceState(id int64, expectedState, newState, externalID, ip string) error {
	if !validCloudState(newState) {
		return ErrValidation
	}
	q := `UPDATE cloud_resources2 SET state = ?, revision = revision + 1, updated_at = ?
	      , external_id = CASE WHEN ? = '' THEN external_id ELSE ? END
	      , ip = CASE WHEN ? = '' THEN ip ELSE ? END
	      , last_error = CASE WHEN ? = 'ready' THEN '' ELSE last_error END
	      WHERE id = ? AND state = ?`
	res, err := s.sql.Exec(q, newState, time.Now().Unix(), externalID, externalID, ip, ip, newState, id, expectedState)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		// Ресурс вообще есть → конфликт состояния; нет → не найден.
		var n2 int64
		if err := s.sql.QueryRow(`SELECT COUNT(*) FROM cloud_resources2 WHERE id = ?`, id).Scan(&n2); err != nil {
			return err
		}
		if n2 > 0 {
			return ErrRevisionConflict
		}
		return ErrNotFound
	}
	return nil
}

// SetCloudResourceError — безопасный код последнего провала (наружу — DTO
// last_error). Пустая строка очищает.
func (s *Store) SetCloudResourceError(id int64, code string) error {
	_, err := s.sql.Exec(`UPDATE cloud_resources2 SET last_error = ?, updated_at = ? WHERE id = ?`,
		code, time.Now().Unix(), id)
	return err
}

// SetCloudResourceOperation — последняя create/install/change-ip операция
// ресурса (DTO active_operation_id).
func (s *Store) SetCloudResourceOperation(id, opID int64) error {
	_, err := s.sql.Exec(`UPDATE cloud_resources2 SET last_operation_id = ?, updated_at = ? WHERE id = ?`,
		opID, time.Now().Unix(), id)
	return err
}

// SetCloudResourceSecrets — сохранить шифротекст секретов (root-пароль VPS).
func (s *Store) SetCloudResourceSecrets(id int64, sealed string) error {
	_, err := s.sql.Exec(`UPDATE cloud_resources2 SET secrets_enc = ?, updated_at = ? WHERE id = ?`,
		sealed, time.Now().Unix(), id)
	return err
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// ── node_management ───────────────────────────────────────────────

// NodeManagement — политика управления нодой (03 §5). Переживает candidate
// prune (владение через users(id)). Revision: 1 = «никогда не писали»
// (виртуальный дефолт manual); каждый успешный PUT +1 (первая запись даёт 2).
type NodeManagement struct {
	NodeID            string
	UserID            int64
	Mode              string // manual | profile
	ProfileVersionID  int64  // 0 = нет
	InputsJSON        string
	ProcessCurrentBan bool
	Revision          int64
	CurrentResourceID int64 // 0 = нет
	ActivatedAt       int64 // profile-mode activation watermark (unix)
	UpdatedAt         int64
}

const nodeMgmtCols = `node_id, user_id, mode, profile_version_id, inputs_json, process_current_ban, revision, current_resource_id, activated_at, updated_at`

func scanNodeManagement(row interface{ Scan(...any) error }) (NodeManagement, error) {
	var (
		m      NodeManagement
		pv, cr sql.NullInt64
		ban    int64
	)
	err := row.Scan(&m.NodeID, &m.UserID, &m.Mode, &pv, &m.InputsJSON, &ban, &m.Revision, &cr, &m.ActivatedAt, &m.UpdatedAt)
	if err != nil {
		return NodeManagement{}, err
	}
	m.ProfileVersionID, m.CurrentResourceID = pv.Int64, cr.Int64
	m.ProcessCurrentBan = ban != 0
	return m, err
}

func validMgmtMode(m string) bool { return m == "manual" || m == "profile" }

// PatchNodeManagement — CAS по expectedRevision. inputs_json всегда '{}'
// (graph-профили и inputs-bind — следующий этап). 0 строк: строки нет и
// expectedRevision == 1 (виртуальный дефолт) → INSERT c revision 2; иначе
// ErrNotFound (нет/чужой) либо ErrRevisionConflict.
func (s *Store) PatchNodeManagement(nodeID string, userID, expectedRevision int64, mode string, profileVersionID int64, processCurrentBan bool, currentResourceID int64) (NodeManagement, error) {
	if !validMgmtMode(mode) {
		return NodeManagement{}, ErrValidation
	}
	ban := 0
	if processCurrentBan {
		ban = 1
	}
	res, err := s.sql.Exec(
		`UPDATE node_management SET mode = ?, profile_version_id = ?, inputs_json = '{}',
		 process_current_ban = ?, current_resource_id = ?, revision = revision + 1, updated_at = ?
		 WHERE node_id = ? AND user_id = ? AND revision = ?`,
		mode, nullInt(profileVersionID), ban, nullInt(currentResourceID), time.Now().Unix(),
		nodeID, userID, expectedRevision)
	if err != nil {
		return NodeManagement{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return NodeManagement{}, err
	} else if n > 0 {
		return s.NodeManagement(nodeID, userID)
	}
	// 0 строк: разбираем почему.
	if _, err := s.NodeManagement(nodeID, userID); err == nil {
		return NodeManagement{}, ErrRevisionConflict // та же строка, ревизия ушла
	} else if !errors.Is(err, ErrNotFound) {
		return NodeManagement{}, err
	}
	// Строка есть под другим владельцем → ErrNotFound (чужая).
	var n2 int64
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM node_management WHERE node_id = ?`, nodeID).Scan(&n2); err != nil {
		return NodeManagement{}, err
	}
	if n2 > 0 {
		return NodeManagement{}, ErrNotFound
	}
	if expectedRevision != 1 {
		return NodeManagement{}, ErrRevisionConflict // нет строки, не базовая ревизия
	}
	_, err = s.sql.Exec(
		`INSERT INTO node_management (node_id, user_id, mode, profile_version_id, inputs_json, process_current_ban, revision, current_resource_id, updated_at)
		 VALUES (?,?,?,?,'{}',?,2,?,?)`,
		nodeID, userID, mode, nullInt(profileVersionID), ban, nullInt(currentResourceID), time.Now().Unix())
	if err != nil {
		return NodeManagement{}, err
	}
	return s.NodeManagement(nodeID, userID)
}

// NodeManagement — строка политики; отсутствует → ErrNotFound
// (GET отдаёт дефолт manual без создания строки).
func (s *Store) NodeManagement(nodeID string, userID int64) (NodeManagement, error) {
	m, err := scanNodeManagement(s.sql.QueryRow(
		`SELECT `+nodeMgmtCols+` FROM node_management WHERE node_id = ? AND user_id = ?`, nodeID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return NodeManagement{}, ErrNotFound
	}
	return m, err
}

// SetNodeManagementCurrentResource — привязать текущий ресурс политики.
func (s *Store) SetNodeManagementCurrentResource(nodeID string, userID, resourceID int64) error {
	res, err := s.sql.Exec(
		`UPDATE node_management SET current_resource_id = ?, updated_at = ? WHERE node_id = ? AND user_id = ?`,
		nullInt(resourceID), time.Now().Unix(), nodeID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCloudResourceReplacedBy links a superseded resource to its replacement
// (resource.fence MVP). Idempotent: the same link is a no-op.
func (s *Store) SetCloudResourceReplacedBy(oldID, newID int64) error {
	if oldID <= 0 || newID <= 0 || oldID == newID {
		return ErrValidation
	}
	res, err := s.sql.Exec(
		`UPDATE cloud_resources2 SET replaced_by_resource_id = ?, updated_at = ? WHERE id = ?`,
		newID, time.Now().Unix(), oldID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetNodeManagementProfile enables PROFILE mode with a pinned graph version
// and validated inputs (stage-08b-i). activatedAt is the ban-episode watermark:
// episodes created at/before it are historical and are not delivered unless
// processCurrentBan explicitly includes the latest one (the server computes it).
// CAS semantics mirror PatchNodeManagement.
func (s *Store) SetNodeManagementProfile(nodeID string, userID, expectedRevision int64, profileVersionID int64, inputsJSON string, processCurrentBan bool, currentResourceID, activatedAt int64) (NodeManagement, error) {
	if profileVersionID <= 0 || inputsJSON == "" {
		return NodeManagement{}, ErrValidation
	}
	ban := 0
	if processCurrentBan {
		ban = 1
	}
	now := time.Now().Unix()
	res, err := s.sql.Exec(
		`UPDATE node_management SET mode = 'profile', profile_version_id = ?, inputs_json = ?,
		 process_current_ban = ?, current_resource_id = ?, activated_at = ?, revision = revision + 1, updated_at = ?
		 WHERE node_id = ? AND user_id = ? AND revision = ?`,
		profileVersionID, inputsJSON, ban, nullInt(currentResourceID), activatedAt, now,
		nodeID, userID, expectedRevision)
	if err != nil {
		return NodeManagement{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return NodeManagement{}, err
	} else if n > 0 {
		return s.NodeManagement(nodeID, userID)
	}
	if _, err := s.NodeManagement(nodeID, userID); err == nil {
		return NodeManagement{}, ErrRevisionConflict
	} else if !errors.Is(err, ErrNotFound) {
		return NodeManagement{}, err
	}
	var n2 int64
	if err := s.sql.QueryRow(`SELECT COUNT(*) FROM node_management WHERE node_id = ?`, nodeID).Scan(&n2); err != nil {
		return NodeManagement{}, err
	}
	if n2 > 0 {
		return NodeManagement{}, ErrNotFound
	}
	if expectedRevision != 1 {
		return NodeManagement{}, ErrRevisionConflict
	}
	_, err = s.sql.Exec(
		`INSERT INTO node_management (node_id, user_id, mode, profile_version_id, inputs_json, process_current_ban, revision, current_resource_id, activated_at, updated_at)
		 VALUES (?,?,'profile',?,?,?,2,?,?,?)`,
		nodeID, userID, profileVersionID, inputsJSON, ban, nullInt(currentResourceID), activatedAt, now)
	if err != nil {
		return NodeManagement{}, err
	}
	return s.NodeManagement(nodeID, userID)
}
