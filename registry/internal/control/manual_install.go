package control

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Manual-install: node_id НЕ предсоздаётся реестром. Пользователь задаёт label
// (manual_hosts.label), агент добавляет свой -<5hex> при регистрации; реестр
// узнаёт node_id только при первом /register и claim'ит pending-секрет
// (claim-on-first-register). См. nodes_secret.go (pending_node_secrets).

// ManualInstallIdem identifies a record generation, not SQLite's reusable PK.
func ManualInstallIdem(host ManualHost) string {
	return fmt.Sprintf("manual-%d-%s", host.ID, host.InstallKey)
}

func (s *Store) ManualInstallOperation(host ManualHost) (Operation, error) {
	return manualInstallOperation(s.sql, host)
}

// The same resolver runs before preparation and inside the acceptance transaction.
func manualInstallOperation(q interface{ QueryRow(string, ...any) *sql.Row }, host ManualHost) (Operation, error) {
	if host.InstallKey == "" {
		return Operation{}, ErrOwnerMismatch
	}
	idem := ManualInstallIdem(host)
	legacyIdem := fmt.Sprintf("manual-%d", host.ID)
	query, arg := `SELECT `+opCols+` FROM operations WHERE idem_key = ?`, any(idem)
	if host.OperationID != 0 {
		query, arg = `SELECT `+opCols+` FROM operations WHERE id = ?`, host.OperationID
	}
	op, err := scanOp(q.QueryRow(query, arg))
	if errors.Is(err, sql.ErrNoRows) && host.OperationID == 0 && strings.HasPrefix(host.InstallKey, "legacy-") {
		op, err = scanOp(q.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, legacyIdem))
		if err == nil && op.CreatedAt < host.CreatedAt && host.NodeID == "" {
			return Operation{}, ErrNotFound // Deleted record's operation is history, never reclaim it.
		}
	}
	if errors.Is(err, sql.ErrNoRows) && (host.OperationID != 0 || host.NodeID != "") {
		return Operation{}, ErrOwnerMismatch
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, ErrNotFound
		}
		return Operation{}, err
	}
	// node_id операции пуст до claim; после claim обязан совпасть с host.NodeID.
	if op.UserID != host.UserID || op.Kind != OpKindManualInstall || op.ProviderVPSID != "" ||
		op.CreatedAt < host.CreatedAt || (op.IdemKey != idem && op.IdemKey != legacyIdem) ||
		(host.NodeID != "" && host.NodeID != op.NodeID) {
		return Operation{}, ErrOwnerMismatch
	}
	return op, nil
}

// AuthorizeManualOperation accepts an installation atomically: owner, quota,
// pending-secret reservation, operation state and host linkage either all
// persist or all roll back. node_id остаётся пустым до claim при /register.
// The caller must serialize acceptance with registration (nodeClaimMu).
//
// Переустановка (expected.State == OpDone) допустима: та же операция (id и
// idem_key) возвращается в installing с новым profile_id; прежняя привязка
// ноды снимается, место квоты переносится в pending и переоформляется на
// claim'е (/register) — при той же или новой node_id. Секрет не ротируется.
func (s *Store) AuthorizeManualOperation(host ManualHost, expected Operation, u User, profileID int64, secret, enc string) (Operation, error) {
	if host.ID <= 0 || host.InstallKey == "" || host.UserID != u.ID || u.ID == s.legacyUserID || secret == "" || enc == "" {
		return Operation{}, ErrOwnerMismatch
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Acquire the SQLite write lock before reading evidence (also across stores).
	if _, err := tx.Exec(`UPDATE manual_hosts SET id = id WHERE id = ?`, host.ID); err != nil {
		return Operation{}, err
	}
	persisted, err := scanManualHost(tx.QueryRow(`SELECT `+manualHostCols+` FROM manual_hosts WHERE id = ?`, host.ID))
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if persisted != host {
		return Operation{}, ErrOwnerMismatch // host changed/deleted while preparing credentials
	}
	op, err := manualInstallOperation(tx, host)
	idem := ManualInstallIdem(host)
	if errors.Is(err, ErrNotFound) {
		if expected.ID != 0 || host.OperationID != 0 || host.NodeID != "" {
			return Operation{}, ErrOwnerMismatch
		}
		op = Operation{UserID: u.ID, Kind: OpKindManualInstall, IdemKey: idem, ProfileID: profileID}
	} else if err != nil {
		return Operation{}, err
	} else {
		idem = op.IdemKey
		// Профиль НЕ сверяем: переустановка (done) может выбрать другой профиль,
		// retry — тоже (выбор уже прошёл selectableProfileVersion у вызывающего).
		if op.ID != expected.ID || op.IdemKey != expected.IdemKey || op.UserID != u.ID ||
			op.Kind != OpKindManualInstall || op.ProviderVPSID != "" ||
			op.CreatedAt < host.CreatedAt || (host.OperationID != 0 && host.OperationID != op.ID) ||
			(host.NodeID != "" && host.NodeID != op.NodeID) {
			return Operation{}, ErrOwnerMismatch
		}
		if op.State != expected.State || (op.State != OpFailed && op.State != OpInstalling && op.State != OpDone) {
			return Operation{}, ErrCreateInProgress
		}
		// Reinstall reclaims identity without erasing durable ownership or
		// releasing a candidate still in the pool. Admission enforces quota.
		if op.State == OpDone && op.NodeID != "" {
			if _, err := tx.Exec(`UPDATE manual_hosts SET node_id = '' WHERE id = ?`, host.ID); err != nil {
				return Operation{}, err
			}
			op.NodeID = ""
		}
	}
	var disabled int
	if err := tx.QueryRow(`SELECT disabled FROM users WHERE id = ?`, u.ID).Scan(&disabled); err != nil {
		return Operation{}, err
	}
	if disabled != 0 {
		return Operation{}, ErrUserDisabled
	}
	now := time.Now().Unix()
	if op.ID == 0 {
		res, err := tx.Exec(`INSERT INTO operations (user_id,node_id,kind,state,idem_key,profile_id,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`,
			u.ID, "", OpKindManualInstall, OpInstalling, idem, profileID, now, now)
		if err != nil {
			return Operation{}, err
		}
		op.ID, err = res.LastInsertId()
		if err != nil {
			return Operation{}, err
		}
		op.CreatedAt = now
	} else if _, err := tx.Exec(`UPDATE operations SET state = ?, profile_id = ?, node_id = ?, last_error = '', updated_at = ? WHERE id = ?`,
		OpInstalling, profileID, op.NodeID, now, op.ID); err != nil {
		return Operation{}, err
	}
	// Pending-бронь — только пока node_id неизвестен. Claim'нутая нода уже в
	// node_owners: новую pending не создаём (иначе второе место квоты).
	if op.NodeID == "" {
		if _, err := tx.Exec(`DELETE FROM pending_node_secrets WHERE operation_id = ?`, op.ID); err != nil {
			return Operation{}, err
		}
		if _, err := tx.Exec(
			`INSERT INTO pending_node_secrets (secret_hash,user_id,operation_id,label,secret_enc,created_at) VALUES (?,?,?,?,?,?)`,
			hashAuthkey(secret), u.ID, op.ID, host.Label, enc, now); err != nil {
			return Operation{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE manual_hosts SET operation_id = ? WHERE id = ?`, op.ID, host.ID); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	op.State, op.UpdatedAt, op.LastError = OpInstalling, now, ""
	op.ProfileID = profileID
	return op, nil
}

// ClaimPendingNodeSecret атомарно claim'ит pending-секрет при /register:
// валидирует node_id против label, применяет AuthorizeNode-семантику квоты
// (превышение → ErrQuotaExceeded, pending НЕ удаляется), пишет владельца +
// секрет (хеш и AES-GCM копию, привязанную к node_id), связывает операцию и
// manual_hosts.node_id, удаляет pending. Возвращает user_id владельца.
func (s *Store) ClaimPendingNodeSecret(secret, nodeID string) (int64, error) {
	if secret == "" || nodeID == "" {
		return 0, ErrValidation
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE pending_node_secrets SET created_at = created_at WHERE secret_hash = ?`, hashAuthkey(secret)); err != nil {
		return 0, err
	}
	var uid, opID int64
	var label, enc string
	err = tx.QueryRow(
		`SELECT user_id, operation_id, label, secret_enc FROM pending_node_secrets WHERE secret_hash = ?`,
		hashAuthkey(secret)).Scan(&uid, &opID, &label, &enc)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	// Имя ноды обязано быть label-<suffix>: реестр не выдумывает имя, а
	// пользователь задал его label'ом. Иначе claim отклоняется.
	if label == "" || !strings.HasPrefix(nodeID, label+"-") {
		return 0, ErrValidation
	}
	op, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE id = ?`, opID))
	if err != nil {
		return 0, err
	}
	if op.UserID != uid {
		return 0, ErrOwnerMismatch
	}
	if op.Kind == OpKindCloudInstall {
		// Ожидаемый ID — только рендер-подсказка для установки (шаги ссылаются
		// на {{ node_id }}). Агент мог сгенерировать собственный суффикс
		// (сторонний установщик принимает короткое имя) — принимаем любой
		// node_id с корректным префиксом label- (проверка выше): подлинность
		// гарантирует сам pending-секрет, а не совпадение суффикса.
		if _, err := CloudInstallExpectedNodeID(op); err != nil {
			return 0, err
		}
	}
	var maxNodes, disabled int
	if err := tx.QueryRow(`SELECT max_nodes, disabled FROM users WHERE id = ?`, uid).Scan(&maxNodes, &disabled); err != nil {
		return 0, err
	}
	if disabled != 0 {
		return 0, ErrUserDisabled
	}
	var owner int64
	var legacy int
	var ownerHash string
	err = tx.QueryRow(`SELECT user_id, legacy, node_secret_hash FROM node_owners WHERE node_id = ?`, nodeID).Scan(&owner, &legacy, &ownerHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		now := time.Now().Unix()
		if _, err := tx.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy, node_secret_hash, node_secret_prefix, node_secret_enc)
			VALUES (?,?,?,0,?,?,?)`, nodeID, uid, now, hashAuthkey(secret), nodeSecretPrefix(secret), enc); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if owner != uid || legacy != 0 {
			return 0, ErrOwnerMismatch
		}
		if op.Kind == OpKindCloudInstall && ownerHash != hashAuthkey(secret) {
			return 0, ErrOwnerMismatch
		}
		// Уже привязана этим пользователем — идемпотентно, секрет не ротируем
		// (записываем ту же pending-копию, если хеш совпадает).
		if _, err := tx.Exec(`UPDATE node_owners SET node_secret_hash = ?, node_secret_prefix = ?, node_secret_enc = ? WHERE node_id = ?`,
			hashAuthkey(secret), nodeSecretPrefix(secret), enc, nodeID); err != nil {
			return 0, err
		}
	}
	now := time.Now().Unix()
	res, err := tx.Exec(`INSERT OR IGNORE INTO quota_reservations (node_id, user_id, created_at) VALUES (?,?,?)`, nodeID, uid, now)
	if err != nil {
		return 0, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if inserted > 0 {
		var used int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM quota_reservations WHERE user_id = ?`, uid).Scan(&used); err != nil {
			return 0, err
		}
		if used > maxNodes {
			return 0, ErrQuotaExceeded // rollback preserves pending and ownership
		}
	}
	// A reinstalled identity may change; retire only the old secret, not its
	// durable owner or pool reservation. Two identities cannot share a token.
	if _, err := tx.Exec(`UPDATE node_owners SET node_secret_hash = '', node_secret_prefix = '', node_secret_enc = ''
		WHERE node_secret_hash = ? AND node_id <> ? AND user_id = ?`, hashAuthkey(secret), nodeID, uid); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE operations SET node_id = ?, updated_at = ? WHERE id = ?`, nodeID, now, opID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE manual_hosts SET node_id = ? WHERE operation_id = ?`, nodeID, opID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_node_secrets WHERE secret_hash = ?`, hashAuthkey(secret)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uid, nil
}
