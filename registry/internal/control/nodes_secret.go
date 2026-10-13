package control

// Персональные секреты нод (invitations-migration-contract.md §1):
// sha256(секрета) + короткий префикс в node_owners. Полный секрет существует
// только в ответе POST /register и у агента; различение с общим токеном —
// по наличию хеша среди нод.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ── Pending-секреты (claim-on-first-register, manual installs) ──────
//
// node_id manual-сервера неизвестен до первой регистрации агента: реестр НЕ
// выдумывает имя (его задаёт пользователь полем label, агент добавляет свой
// -<5hex> при регистрации). Поэтому на accept установки секрет кладётся в
// pending_node_secrets, а node_owners пишется атомарно при первом /register
// с этим Bearer'ом. Плейнтекст секрета существует только в install-переменных
// ({{ node_secret }}); здесь — sha256 и восстановимая AES-GCM копия (для
// retry-install той же командой).

// PendingNodeSecret — ожидающий claim секрет установки.
type PendingNodeSecret struct {
	SecretHash  string
	UserID      int64
	OperationID int64
	Label       string
	SecretEnc   string
	CreatedAt   int64
}

// PendingSecret — найти pending-запись по предъявленному плейнтексту.
func (s *Store) PendingSecret(secret string) (PendingNodeSecret, bool, error) {
	if secret == "" {
		return PendingNodeSecret{}, false, nil
	}
	return s.scanPending(s.sql.QueryRow(
		`SELECT secret_hash, user_id, operation_id, label, secret_enc, created_at
		 FROM pending_node_secrets WHERE secret_hash = ?`, hashAuthkey(secret)))
}

// PendingSecretByOperation — pending-запись операции (retry-install: тот же
// секрет до claim). ErrNotFound, если операции ещё нет/секрет уже claim'нут.
func (s *Store) PendingSecretByOperation(opID int64) (PendingNodeSecret, error) {
	p, ok, err := s.scanPending(s.sql.QueryRow(
		`SELECT secret_hash, user_id, operation_id, label, secret_enc, created_at
		 FROM pending_node_secrets WHERE operation_id = ?`, opID))
	if err != nil {
		return PendingNodeSecret{}, err
	}
	if !ok {
		return PendingNodeSecret{}, ErrNotFound
	}
	return p, nil
}

func (s *Store) scanPending(row interface{ Scan(...any) error }) (PendingNodeSecret, bool, error) {
	var p PendingNodeSecret
	err := row.Scan(&p.SecretHash, &p.UserID, &p.OperationID, &p.Label, &p.SecretEnc, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingNodeSecret{}, false, nil
	}
	if err != nil {
		return PendingNodeSecret{}, false, err
	}
	return p, true, nil
}

// UpsertPendingSecret — сохранить/перезаписать pending-секрет операции. Ровно
// одна pending-строка на операцию: старые хеши (если были) удаляются.
func (s *Store) UpsertPendingSecret(userID, opID int64, label, secret, enc string) error {
	if secret == "" || opID == 0 {
		return ErrValidation
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM pending_node_secrets WHERE operation_id = ?`, opID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO pending_node_secrets (secret_hash, user_id, operation_id, label, secret_enc, created_at)
		 VALUES (?,?,?,?,?,?)`,
		hashAuthkey(secret), userID, opID, label, enc, time.Now().Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// DeletePendingSecretByOperation — снять pending-бронь (удаление сервера до
// claim). Claim'нутая операция pending-строки уже не имеет.
func (s *Store) DeletePendingSecretByOperation(opID int64) error {
	_, err := s.sql.Exec(`DELETE FROM pending_node_secrets WHERE operation_id = ?`, opID)
	return err
}

// PurgePendingSecrets — убрать pending-секреты старше before (напр. 7 суток):
// брошенные установки не должны вечно держать место в квоте.
func (s *Store) PurgePendingSecrets(before int64) (int64, error) {
	res, err := s.sql.Exec(`DELETE FROM pending_node_secrets WHERE created_at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetNodeSecret — выдать (перевыдать) секрет ноде: старый хеш затирается
// (перерегистрация своей ноды инвалидирует прежний секрет). Пустой секрет =
// снять хеш («секрета нет» — используется тестами и миграционными сценами).
// false → нода не привязана (секрет выдавать некому).
func (s *Store) SetNodeSecret(nodeID, secret string) (prefix string, ok bool, err error) {
	return s.SetNodeSecretWithEnc(nodeID, secret, "")
}

// SetNodeSecretWithEnc — как SetNodeSecret, но дополнительно сохраняет
// восстановимую копию секрета (уже зашифрованную вызывающим, AES-GCM
// master.key). Пустой enc = восстановимой копии нет (legacy-строка или
// вызов без мастер-ключа). Хеш — по-прежнему единственный источник истины
// аутентификации; enc нужен retry-install, чтобы отрендерить реальный токен
// (DEF-A), а не пустую строку.
func (s *Store) SetNodeSecretWithEnc(nodeID, secret, enc string) (prefix string, ok bool, err error) {
	hash, prefix := "", ""
	if secret != "" {
		hash, prefix = hashAuthkey(secret), nodeSecretPrefix(secret)
	} else {
		enc = "" // снятие секрета убирает и восстановимую копию
	}
	res, err := s.sql.Exec(
		`UPDATE node_owners SET node_secret_hash = ?, node_secret_prefix = ?, node_secret_enc = ? WHERE node_id = ?`,
		hash, prefix, enc, nodeID)
	if err != nil {
		return "", false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	return prefix, n > 0, nil
}

// NodeSecretEnc — восстановимая (зашифрованная) копия секрета. "" = копии нет
// (legacy-строка / секрет не выдавался). Расшифровку делает владелец ключа.
func (s *Store) NodeSecretEnc(nodeID string) (string, error) {
	var enc string
	err := s.sql.QueryRow(`SELECT node_secret_enc FROM node_owners WHERE node_id = ?`, nodeID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return enc, err
}

// NodeHasSecret — у ноды уже выдан node_secret (хеш непустой). Нода не
// привязана → false. Нужно retry-install: не перевыдавать секрет живого агента
// (M6).
func (s *Store) NodeHasSecret(nodeID string) (bool, error) {
	var hash string
	err := s.sql.QueryRow(`SELECT node_secret_hash FROM node_owners WHERE node_id = ?`, nodeID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hash != "", nil
}

// NodeSecretOwner — нода, чей хеш совпал с предъявленным секретом.
// Не найден → ok=false; ошибки БД → err (вызывающий решает fail-closed).
func (s *Store) NodeSecretOwner(secret string) (nodeID string, ok bool, err error) {
	if secret == "" {
		return "", false, nil
	}
	err = s.sql.QueryRow(
		`SELECT node_id FROM node_owners WHERE node_secret_hash = ?`,
		hashAuthkey(secret)).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return nodeID, true, nil
}

// CountNodesWithoutSecret — contract §1/§3: COUNT(node_owners WHERE
// node_secret_hash=” AND node_id IN живых кандидатов) — счётчик миграции.
func (s *Store) CountNodesWithoutSecret(nodeIDs []string) (int, error) {
	if len(nodeIDs) == 0 {
		return 0, nil
	}
	q := `SELECT COUNT(*) FROM node_owners WHERE node_secret_hash = '' AND node_id IN (` +
		strings.TrimSuffix(strings.Repeat("?,", len(nodeIDs)), ",") + `)`
	args := make([]any, len(nodeIDs))
	for i, id := range nodeIDs {
		args[i] = id
	}
	var n int
	err := s.sql.QueryRow(q, args...).Scan(&n)
	return n, err
}

func nodeSecretPrefix(secret string) string {
	if len(secret) > prefixLen {
		return secret[:prefixLen]
	}
	return secret
}
