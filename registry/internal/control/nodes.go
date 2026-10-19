package control

// Владельцы нод и квоты. Ключевая гарантия: захват места транзакционен —
// два конкурентных POST /register при одном свободном месте дают ровно
// один успех (BEGIN IMMEDIATE + COUNT внутри транзакции + UNIQUE(node_id)).
//
// Квоту занимает только quota_reservations (нода, находящаяся в пуле).
// Pending-секреты не занимают места. node_owners — вечная карта владения (кто владеет
// node_id), в квоту НЕ входит: prune/expiry/terminate/замена по IP снимают
// резерв (ReleaseQuota), поэтому «0 активных нод, но 3/3» больше невозможен.
// Резерв возвращается в квоту при повторном /register владельца, если место
// свободно.

import (
	"database/sql"
	"errors"
	"time"
)

// NodeOwner — владелец ноды: (user_id, legacy-флаг, найдена ли).
func (s *Store) NodeOwner(nodeID string) (userID int64, legacy bool, ok bool, err error) {
	var u int64
	var l int
	err = s.sql.QueryRow(`SELECT user_id, legacy FROM node_owners WHERE node_id = ?`, nodeID).Scan(&u, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, nil
	}
	if err != nil {
		return 0, false, false, err
	}
	return u, l != 0, true, nil
}

// UsedNodes counts pool reservations only, never ownership or pending installs.
func (s *Store) UsedNodes(userID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(*) FROM quota_reservations WHERE user_id = ?`, userID).Scan(&n)
	return n, err
}

// BindNodeOwnership prepares a provisioned identity, not a pool admission.
// Its personal secret can authenticate /register, where POOL quota is enforced.
// Здесь проверяется ДЕНЕЖНЫЙ счётчик провиженных мест (ноды ∪ активные
// cloud_resources ∪ незавершённые заказы): иначе max_nodes обходится серией
// платных заказов до регистрации. Транзакция та же, что у AuthorizeNode.
func (s *Store) BindNodeOwnership(nodeID string, u User) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy) VALUES (?,?,?,0)
		ON CONFLICT(node_id) DO NOTHING`, nodeID, u.ID, time.Now().Unix()); err != nil {
		return err
	}
	var owner int64
	var legacy int
	err = tx.QueryRow(`SELECT user_id, legacy FROM node_owners WHERE node_id = ?`, nodeID).Scan(&owner, &legacy)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOwnerMismatch
	}
	if err != nil {
		return err
	}
	if owner != u.ID || legacy != 0 {
		return ErrOwnerMismatch
	}
	var used int
	if err := tx.QueryRow(
		`SELECT COUNT(DISTINCT node_id) FROM (
		   SELECT node_id FROM node_owners WHERE user_id = ? AND legacy = 0 AND node_id <> ''
		   UNION SELECT node_id FROM cloud_resources WHERE user_id = ? AND state <> 'deleted' AND node_id <> ''
		   UNION SELECT node_id FROM operations WHERE user_id = ? AND kind = ? AND state NOT IN ('failed','deleted') AND node_id <> ''
		 )`, u.ID, u.ID, u.ID, OpKindCreate).Scan(&used); err != nil {
		return err
	}
	if used > u.MaxNodes {
		return ErrQuotaExceeded
	}
	return tx.Commit()
}

// AuthorizeNode — атомарная привязка ноды к пользователю с проверкой квоты:
//   - нода уже за этим пользователем → восстанавливаем резерв, если его нет
//     (нода вернулась в пул). Уже существующий резерв место не удваивает;
//   - за другим → ErrOwnerMismatch;
//   - свободна → INSERT node_owners + quota_reservations, затем COUNT.
//
// Новое место проверяется строго: reservations > max_nodes → ROLLBACK,
// ErrQuotaExceeded (ни owner, ни reservation не остаются). Резерв уже
// существующей ноды позволяет переподтвердиться, даже если админ позже
// понизил max_nodes.
func (s *Store) AuthorizeNode(nodeID string, u User) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize before reading, including across separately opened stores.
	if _, err := tx.Exec(`UPDATE users SET id = id WHERE id = ?`, u.ID); err != nil {
		return err
	}
	var disabled int
	if err := tx.QueryRow(`SELECT max_nodes, disabled FROM users WHERE id = ?`, u.ID).Scan(&u.MaxNodes, &disabled); err != nil {
		return err
	}
	if disabled != 0 {
		return ErrUserDisabled
	}
	// BEGIN (deferred) повышается до write-лока на первом INSERT — два
	// конкурирующих писателя сериализуются, оба COUNT'а видят честное состояние.
	// На всякий случай страхуемся от гонки PK (одновременный bind той же ноды
	// разными пользователями) явной проверкой владельца внутри транзакции.
	now := time.Now().Unix()
	checkQuota := false
	var owner int64
	err = tx.QueryRow(`SELECT user_id FROM node_owners WHERE node_id = ?`, nodeID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// свободна — захватываем место ниже
		if _, err := tx.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy) VALUES (?,?,?,0)`,
			nodeID, u.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO quota_reservations (node_id, user_id, created_at) VALUES (?,?,?)`,
			nodeID, u.ID, now); err != nil {
			return err
		}
		checkQuota = true
	case err != nil:
		return err
	default:
		if owner != u.ID {
			return ErrOwnerMismatch
		}
		// Своя нода: резерв восстанавливаем, если нода была вне пула.
		res, err := tx.Exec(`INSERT OR IGNORE INTO quota_reservations (node_id, user_id, created_at) VALUES (?,?,?)`,
			nodeID, u.ID, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		checkQuota = n > 0 // существующий резерв не удваивается и не проверяется
	}
	if checkQuota {
		var used int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM quota_reservations WHERE user_id = ?`, u.ID).Scan(&used); err != nil {
			return err
		}
		if used > u.MaxNodes {
			return ErrQuotaExceeded // defer Rollback откатит INSERT'ы
		}
	}
	return tx.Commit()
}

// ReleaseQuota — нода покинула пул (expiry/prune/terminate/замена по IP):
// снимаем её место, ownership (node_owners) сохраняется для возврата.
// Идемпотентно; повторный /register восстановит резерв, если есть место.
func (s *Store) ReleaseQuota(nodeID string) error {
	if nodeID == "" {
		return nil
	}
	_, err := s.sql.Exec(`DELETE FROM quota_reservations WHERE node_id = ?`, nodeID)
	return err
}

// EnsureQuota — идемпотентно восстанавливает резерв для ноды, которая УЖЕ в
// пуле (startup-reconcile: после смены семантики квоты активные кандидаты
// должны занимать место, даже если их register был до апгрейда). Квоту не
// проверяет — нода уже активна; legacy-владение не резервирует место.
func (s *Store) EnsureQuota(nodeID string) error {
	if nodeID == "" {
		return nil
	}
	_, err := s.sql.Exec(`INSERT OR IGNORE INTO quota_reservations (node_id, user_id, created_at)
		SELECT node_id, user_id, ? FROM node_owners WHERE node_id = ? AND legacy = 0`,
		time.Now().Unix(), nodeID)
	return err
}

// ReconcileQuota replaces stale reservations with the persisted candidate set.
// Startup only, before admissions start; ownership and live candidates survive.
func (s *Store) ReconcileQuota(nodeIDs []string) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM quota_reservations`); err != nil {
		return err
	}
	for _, id := range nodeIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO quota_reservations (node_id, user_id, created_at)
			SELECT node_id, user_id, ? FROM node_owners WHERE node_id = ? AND legacy = 0`, time.Now().Unix(), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// OwnedNode — нода кабинета: id + флаг «помечена на удаление» (квота
// освобождается только release-node, удаление физически — отдельная фаза).
type OwnedNode struct {
	NodeID   string
	Deleting bool
}

// NodesByUser — ноды, реально принадлежащие пользователю (legacy-строки
// системного пользователя не входят). Порядок стабильный — по node_id.
func (s *Store) NodesByUser(userID int64) ([]OwnedNode, error) {
	rows, err := s.sql.Query(
		`SELECT node_id, deleting FROM node_owners WHERE user_id = ? AND legacy = 0 ORDER BY node_id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []OwnedNode{}
	for rows.Next() {
		var n OwnedNode
		var deleting int
		if err := rows.Scan(&n.NodeID, &deleting); err != nil {
			return nil, err
		}
		n.Deleting = deleting != 0
		out = append(out, n)
	}
	return out, rows.Err()
}

// MarkNodeDeleting — пометить ноду на удаление (идемпотентно). Место НЕ
// освобождает: квота держится до release-node/физического удаления.
func (s *Store) MarkNodeDeleting(nodeID string) error {
	_, err := s.sql.Exec(`UPDATE node_owners SET deleting = 1 WHERE node_id = ?`, nodeID)
	return err
}

// ReleaseNode — админ освобождает место физически удалённой ноды:
// владелец → legacy (строка сохраняется навсегда), резерв места снимается.
// Возвращает false, если нода не привязана.
func (s *Store) ReleaseNode(nodeID string) (bool, error) {
	tx, err := s.sql.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`UPDATE node_owners SET released_user_id = CASE WHEN legacy = 0 THEN user_id ELSE released_user_id END,
		user_id = ?, legacy = 1 WHERE node_id = ?`, s.legacyUserID, nodeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM quota_reservations WHERE node_id = ?`, nodeID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// BindLegacyNode — миграция §4: нода, существовавшая до включения user-режима,
// привязывается к системному legacy-пользователю (место в квотах не занимает).
func (s *Store) BindLegacyNode(nodeID string) error {
	_, _, ok, err := s.NodeOwner(nodeID)
	if err != nil {
		return err
	}
	if ok {
		return nil // уже привязана — миграция идемпотентна
	}
	_, err = s.sql.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy) VALUES (?,?,?,1)`,
		nodeID, s.legacyUserID, time.Now().Unix())
	if err != nil && isUniqueViolation(err) {
		return nil // проиграли гонке — нода уже привязана
	}
	return err
}

// ── pending-брони: освобождение при удалении ресурса/хоста ──────────
//
// Pending-секрет держит место квоты, пока установка не claim'нута. Удаление
// ресурса/сервера делает retry невозможным, поэтому бронь обязана уйти вместе
// с ним — иначе «0 активных нод, но 3/3» навсегда.

// releasePendingForCloudResource удаляет pending-брони install-операций
// ресурса. Идемпотентно: ключ cloud_install:<id> — durable-связь, а
// last_operation_id покрывает install, ставший последней мутацией ресурса
// (когда строка ресурса ещё существует).
func releasePendingForCloudResource(q interface {
	Exec(string, ...any) (sql.Result, error)
}, resourceID int64) (int64, error) {
	res, err := q.Exec(`DELETE FROM pending_node_secrets WHERE operation_id IN (
		SELECT id FROM operations WHERE idem_key = ?
		UNION
		SELECT o.id FROM operations o JOIN cloud_resources2 r ON r.last_operation_id = o.id
		 WHERE r.id = ? AND o.kind = ?)`,
		CloudInstallIdem(resourceID), resourceID, OpKindCloudInstall)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReleasePendingSecretsForCloudResource снимает pending-бронь ресурса после
// подтверждённого удаления у провайдера (строка ресурса остаётся state=deleted).
func (s *Store) ReleasePendingSecretsForCloudResource(resourceID int64) (int64, error) {
	if resourceID <= 0 {
		return 0, ErrValidation
	}
	return releasePendingForCloudResource(s.sql, resourceID)
}

// ReconcileOrphanPendingSecrets удаляет брони терминальных операций
// (failed/cancelled/deleted), у которых больше нет живого ресурса/хоста.
// Retryable install на существующем ресурсе/хосте сохраняет pending. Один
// DELETE: идемпотентно и безопасно при конкурентных вызовах. Возвращает
// только счётчик (секреты не логируются).
func (s *Store) ReconcileOrphanPendingSecrets() (int64, error) {
	const q = `DELETE FROM pending_node_secrets WHERE operation_id IN (
		SELECT o.id FROM operations o
		WHERE o.state IN ('failed','cancelled','deleted')
		  AND (
		    (o.kind = 'cloud_install' AND NOT EXISTS (
		      SELECT 1 FROM cloud_resources2 r
		      WHERE r.state <> 'deleted'
		        AND (r.last_operation_id = o.id OR o.idem_key = 'cloud_install:' || r.id)))
		    OR
		    (o.kind = 'manual_install' AND NOT EXISTS (
		      SELECT 1 FROM manual_hosts h
		      WHERE h.operation_id = o.id OR o.idem_key = 'manual-' || h.id || '-' || h.install_key))
		  )
	)`
	res, err := s.sql.Exec(q)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
