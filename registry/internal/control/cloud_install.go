package control

// Cloud-install (stage-06): установка агента на УЖЕ созданный (ready)
// cloud-ресурс — завершение ручной петли create→install→pool. Ожидаемый ID
// сохраняется до SSH, но owner, quota и operation.node_id — только при
// первом /register и claim pending-секрета (claim-on-first-register, см.
// manual_install.go / nodes_secret.go). Операция одна на ресурс
// (idem_key cloud_install:<resourceID>): повтор и переустановка переиспользуют
// ту же строку, не ротируя секрет живого агента.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"sharedd/registry/internal/machineapi"
)

// OpKindCloudInstall — установка на managed cloud-ресурс (провайдер владеет
// VPS; деньги-инварианты неприменимы, место держит claim, а не заказ).
const OpKindCloudInstall = "cloud_install"

// CloudInstallIdem — идентификатор операции установки ресурса.
func CloudInstallIdem(resourceID int64) string {
	return fmt.Sprintf("cloud_install:%d", resourceID)
}

// CloudInstallExpectedNodeID is preparation metadata, never proof of registration.
func CloudInstallExpectedNodeID(op Operation) (string, error) {
	var meta struct {
		ExpectedNodeID string `json:"expected_node_id"`
	}
	if op.RequestJSON != "" && json.Unmarshal([]byte(op.RequestJSON), &meta) != nil {
		return "", ErrValidation
	}
	if meta.ExpectedNodeID != "" && machineapi.ValidateNodeID(meta.ExpectedNodeID) != nil {
		return "", ErrValidation
	}
	return meta.ExpectedNodeID, nil
}

// AuthorizeCloudInstall принимает установку атомарно: ресурс принадлежит u,
// state=ready, external_id непуст; квота, pending-бронь, состояние операции и
// привязка ресурса либо все persist, либо все откатываются. node_id остаётся
// пустым до claim при /register. Вызывающий сериализует приём с регистрацией
// (nodeClaimMu на стороне сервера).
func (s *Store) AuthorizeCloudInstall(resourceID int64, u User, profileID int64, secret, enc string) (Operation, error) {
	if resourceID <= 0 || u.ID == s.legacyUserID || secret == "" || enc == "" {
		return Operation{}, ErrOwnerMismatch
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return Operation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// SQLite write lock до чтения evidence (как в AcceptManualOperation).
	if _, err := tx.Exec(`UPDATE cloud_resources2 SET id = id WHERE id = ?`, resourceID); err != nil {
		return Operation{}, err
	}
	var (
		owner      int64
		state      string
		externalID string
		label      string
	)
	err = tx.QueryRow(`SELECT user_id, state, external_id, label FROM cloud_resources2 WHERE id = ?`, resourceID).
		Scan(&owner, &state, &externalID, &label)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	if owner != u.ID {
		return Operation{}, ErrNotFound
	}
	// Не-ready/без внешнего ID — не при чём установка (гонка с delete/change):
	// конфликт состояния, а не ошибка формы.
	if state != CloudResReady || externalID == "" {
		return Operation{}, ErrCreateInProgress
	}
	if machineapi.ValidateNodeID(label+"-00000") != nil {
		return Operation{}, ErrValidation
	}

	idem := CloudInstallIdem(resourceID)
	op, err := scanOp(tx.QueryRow(`SELECT `+opCols+` FROM operations WHERE idem_key = ?`, idem))
	if errors.Is(err, sql.ErrNoRows) {
		op = Operation{UserID: u.ID, Kind: OpKindCloudInstall, IdemKey: idem, ProfileID: profileID}
	} else if err != nil {
		return Operation{}, err
	} else {
		if op.UserID != u.ID || op.Kind != OpKindCloudInstall || op.ProviderVPSID != "" {
			return Operation{}, ErrOwnerMismatch
		}
		switch op.State {
		case OpFailed, OpInstalling, OpDone:
		default:
			return Operation{}, ErrCreateInProgress
		}
	}
	expectedID, err := CloudInstallExpectedNodeID(op)
	if err != nil {
		return Operation{}, err
	}
	if op.NodeID != "" {
		if expectedID != "" && expectedID != op.NodeID {
			return Operation{}, ErrOwnerMismatch
		}
		expectedID = op.NodeID // Previously claimed identity, not a newly minted one.
	}
	minted := expectedID == ""
	if minted {
		var suffix [3]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return Operation{}, fmt.Errorf("generate cloud node id: %w", err)
		}
		expectedID = label + "-" + hex.EncodeToString(suffix[:])[:5]
	}
	if machineapi.ValidateNodeID(expectedID) != nil || !strings.HasPrefix(expectedID, label+"-") {
		return Operation{}, ErrValidation
	}
	// An existing candidate may only be reused with its exact owner's token.
	var nodeOwner int64
	var legacy int
	var nodeHash string
	err = tx.QueryRow(`SELECT user_id, legacy, node_secret_hash FROM node_owners WHERE node_id = ?`, expectedID).
		Scan(&nodeOwner, &legacy, &nodeHash)
	if err == nil && (minted || nodeOwner != u.ID || legacy != 0 || nodeHash != hashAuthkey(secret)) {
		return Operation{}, ErrOwnerMismatch
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Operation{}, err
	}
	if errors.Is(err, sql.ErrNoRows) && op.NodeID != "" {
		return Operation{}, ErrOwnerMismatch // Do not recreate an unproven old owner.
	}
	if op.ID != 0 {
		var pendingOwner int64
		var hash, storedEnc string
		err := tx.QueryRow(`SELECT user_id, secret_hash, secret_enc FROM pending_node_secrets WHERE operation_id = ?`, op.ID).
			Scan(&pendingOwner, &hash, &storedEnc)
		if err == nil && (pendingOwner != u.ID || hash != hashAuthkey(secret) || storedEnc != enc) {
			return Operation{}, ErrOwnerMismatch // Acceptance must not rotate a retry's token.
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Operation{}, err
		}
	}
	meta := map[string]json.RawMessage{}
	if op.RequestJSON != "" {
		if err := json.Unmarshal([]byte(op.RequestJSON), &meta); err != nil {
			return Operation{}, ErrValidation
		}
	}
	if meta == nil {
		return Operation{}, ErrValidation
	}
	meta["expected_node_id"], _ = json.Marshal(expectedID)
	requestJSON, err := json.Marshal(meta)
	if err != nil {
		return Operation{}, err
	}
	op.RequestJSON, op.NodeID = string(requestJSON), ""

	var disabled int
	if err := tx.QueryRow(`SELECT disabled FROM users WHERE id = ?`, u.ID).Scan(&disabled); err != nil {
		return Operation{}, err
	}
	if disabled != 0 {
		return Operation{}, ErrUserDisabled
	}
	now := time.Now().Unix()
	if op.ID == 0 {
		res, err := tx.Exec(`INSERT INTO operations (user_id,node_id,kind,state,idem_key,profile_id,request_json,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
			u.ID, "", OpKindCloudInstall, OpInstalling, idem, profileID, op.RequestJSON, now, now)
		if err != nil {
			return Operation{}, err
		}
		op.ID, err = res.LastInsertId()
		if err != nil {
			return Operation{}, err
		}
		op.CreatedAt = now
	} else if _, err := tx.Exec(`UPDATE operations SET state = ?, profile_id = ?, node_id = '', request_json = ?, last_error = '', updated_at = ? WHERE id = ?`,
		OpInstalling, profileID, op.RequestJSON, now, op.ID); err != nil {
		return Operation{}, err
	}
	// Every attempt requires a fresh real claim, reusing the same credential.
	// A pending credential is not a pool reservation.
	if _, err := tx.Exec(`DELETE FROM pending_node_secrets WHERE operation_id = ?`, op.ID); err != nil {
		return Operation{}, err
	}
	if _, err := tx.Exec(
		`INSERT INTO pending_node_secrets (secret_hash,user_id,operation_id,label,secret_enc,created_at) VALUES (?,?,?,?,?,?)`,
		hashAuthkey(secret), u.ID, op.ID, label, enc, now); err != nil {
		return Operation{}, err
	}
	// Identity metadata, pending token, attempt reset and resource linkage commit
	// together. Keep owner/quota untouched; Confirm still needs a fresh claim.
	if _, err := tx.Exec(`UPDATE cloud_resources2 SET last_operation_id = ?, node_id = '', updated_at = ? WHERE id = ?`,
		op.ID, now, resourceID); err != nil {
		return Operation{}, err
	}
	if err := tx.Commit(); err != nil {
		return Operation{}, err
	}
	op.State, op.UpdatedAt, op.LastError = OpInstalling, now, ""
	op.ProfileID = profileID
	return op, nil
}

// SetCloudResourceNode — CAS-привязка node_id ресурса после claim'а (ожидаемое
// состояние — ready). 0 строк → ErrRevisionConflict (состояние сменилось) либо
// ErrNotFound (ресурса нет).
func (s *Store) SetCloudResourceNode(resourceID int64, nodeID, expectedState string) error {
	if nodeID == "" || expectedState == "" {
		return ErrValidation
	}
	res, err := s.sql.Exec(
		`UPDATE cloud_resources2 SET node_id = ?, revision = revision + 1, updated_at = ? WHERE id = ? AND state = ?`,
		nodeID, time.Now().Unix(), resourceID, expectedState)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		var n2 int64
		if err := s.sql.QueryRow(`SELECT COUNT(*) FROM cloud_resources2 WHERE id = ?`, resourceID).Scan(&n2); err != nil {
			return err
		}
		if n2 > 0 {
			return ErrRevisionConflict
		}
		return ErrNotFound
	}
	return nil
}
