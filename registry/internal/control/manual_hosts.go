package control

// Свои серверы (docs/specs/addendum-manual-servers.md §2): пользователь
// подключает уже существующий хост (IP + пароль ИЛИ свой SSH-ключ).
// Провайдер не участвует, платных вызовов нет. Секрет (пароль/ключ) хранится
// ТОЛЬКО в зашифрованном виде (secret_enc, AES-GCM master.key) и наружу не
// отдаётся ни одним эндпоинтом.

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// ManualHost — запись своего сервера. SecretEnc — шифротекст, json:"-" на
// случай случайной сериализации (наружу секрет не уходит никогда).
type ManualHost struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	Label       string `json:"label"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	SSHUser     string `json:"ssh_user"`
	AuthType    string `json:"auth_type"`
	SecretEnc   string `json:"-"`
	CreatedAt   int64  `json:"created_at"`
	VerifiedAt  int64  `json:"verified_at"`
	LastError   string `json:"last_error"`
	NodeID      string `json:"node_id,omitempty"`
	OperationID int64  `json:"operation_id,omitempty"`
	InstallKey  string `json:"-"`
}

const manualHostCols = `id, user_id, label, host, port, ssh_user, auth_type, secret_enc, created_at, verified_at, last_error, node_id, operation_id, install_key`

func scanManualHost(row interface{ Scan(...any) error }) (ManualHost, error) {
	var h ManualHost
	err := row.Scan(&h.ID, &h.UserID, &h.Label, &h.Host, &h.Port, &h.SSHUser, &h.AuthType,
		&h.SecretEnc, &h.CreatedAt, &h.VerifiedAt, &h.LastError, &h.NodeID, &h.OperationID, &h.InstallKey)
	return h, err
}

// CreateManualHost добавляет запись сервера и возвращает её с присвоенным id.
func (s *Store) CreateManualHost(h ManualHost) (ManualHost, error) {
	key, err := newManualInstallKey()
	if err != nil {
		return ManualHost{}, err
	}
	h.InstallKey = key // Always fresh, even if the caller supplies a previous record's key.
	h.CreatedAt = orNow(h.CreatedAt)
	res, err := s.sql.Exec(
		`INSERT INTO manual_hosts (user_id, label, host, port, ssh_user, auth_type, secret_enc, created_at, verified_at, last_error, node_id, operation_id, install_key)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		h.UserID, h.Label, h.Host, h.Port, h.SSHUser, h.AuthType, h.SecretEnc,
		h.CreatedAt, h.VerifiedAt, h.LastError, h.NodeID, h.OperationID, h.InstallKey)
	if err != nil {
		// B2: UNIQUE(user_id, host, port) — один и тот же host:port нельзя
		// подключить дважды (иначе второй сервер съедает второе место квоты).
		if isUniqueViolation(err) {
			return ManualHost{}, ErrDuplicate
		}
		return ManualHost{}, err
	}
	h.ID, err = res.LastInsertId()
	return h, err
}

func newManualInstallKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate manual install key: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Backfill only missing keys. The marker permits narrow pre-generation legacy
// lookup; newly inserted records never use it, including same-second ID reuse.
func migrateManualInstallKeys(db *sql.DB) error {
	rows, err := db.Query(`SELECT id FROM manual_hosts WHERE install_key = ''`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, id := range ids {
		key, err := newManualInstallKey()
		if err != nil {
			return err
		}
		if _, err := db.Exec(`UPDATE manual_hosts SET install_key = ? WHERE id = ? AND install_key = ''`, "legacy-"+key, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ManualHost(id int64) (ManualHost, error) {
	h, err := scanManualHost(s.sql.QueryRow(`SELECT `+manualHostCols+` FROM manual_hosts WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ManualHost{}, ErrNotFound
	}
	return h, err
}

// ManualHostsByUser — серверы пользователя (новые первыми).
func (s *Store) ManualHostsByUser(userID int64) ([]ManualHost, error) {
	rows, err := s.sql.Query(`SELECT `+manualHostCols+` FROM manual_hosts WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ManualHost{}
	for rows.Next() {
		h, err := scanManualHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// DeleteManualHost removes the host and releases its pending install
// reservation in the same transaction. Without the release a failed/deleted
// install with no retry target would hold a quota slot forever.
func (s *Store) DeleteManualHost(id int64) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Host row still exists: match the install operation by its durable link
	// (operation_id) or by ManualInstallIdem. Release before the host delete.
	if _, err := tx.Exec(`DELETE FROM pending_node_secrets WHERE operation_id IN (
		SELECT o.id FROM operations o JOIN manual_hosts h ON h.id = ?
		WHERE o.id = h.operation_id
		   OR o.idem_key = 'manual-' || h.id || '-' || h.install_key)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM manual_hosts WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetManualHostCheck — результат проверки SSH-доступа/версии ОС.
func (s *Store) SetManualHostCheck(id int64, verifiedAt int64, lastErr string) error {
	_, err := s.sql.Exec(`UPDATE manual_hosts SET verified_at = ?, last_error = ? WHERE id = ?`, verifiedAt, lastErr, id)
	return err
}
