package control

// Владельцы нод и квоты. Ключевая гарантия: захват места транзакционен —
// два конкурентных POST /register при одном свободном месте дают ровно
// один успех (BEGIN IMMEDIATE + COUNT внутри транзакции + UNIQUE(node_id)).
// node_owners переживает prune/expiry/terminate навсегда (контракт §1):
// место освобождает только release-node.

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

// UsedNodes — занятые места пользователя: привязанные ноды (node_owners,
// legacy=0) ПЛЮС незаявленные pending-брони manual-установок. Pending-секрет
// держит место в квоте до claim'а (/register) — та же семантика, что в
// AuthorizeManualOperation/ClaimPendingNodeSecret, поэтому кабинет не должен
// показывать 0 «занято», пока пользователь уже не может занять второе место.
// Legacy-строки в квоту не входят.
func (s *Store) UsedNodes(userID int64) (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT
		(SELECT COUNT(*) FROM node_owners WHERE user_id = ? AND legacy = 0) +
		(SELECT COUNT(*) FROM pending_node_secrets WHERE user_id = ?)`, userID, userID).Scan(&n)
	return n, err
}

// AuthorizeNode — атомарная привязка ноды к пользователю с проверкой квоты:
//   - нода уже за этим пользователем → OK, место НЕ тратится повторно;
//   - за другим → ErrOwnerMismatch;
//   - свободна → INSERT node_owners + quota_reservations, затем COUNT;
//     если мест стало больше max_nodes → ROLLBACK, ErrQuotaExceeded.
func (s *Store) AuthorizeNode(nodeID string, u User) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// BEGIN (deferred) повышается до write-лока на первом INSERT — два
	// конкурирующих писателя сериализуются, оба COUNT'а видят честное состояние.
	// На всякий случай страхуемся от гонки PK (одновременный bind той же ноды
	// разными пользователями) явной проверкой владельца внутри транзакции.
	var owner int64
	err = tx.QueryRow(`SELECT user_id FROM node_owners WHERE node_id = ?`, nodeID).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// свободна — захватываем место ниже
	case err != nil:
		return err
	default:
		if owner != u.ID {
			return ErrOwnerMismatch
		}
		return tx.Commit() // своя нода: новое место не занимает
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO node_owners (node_id, user_id, registered_at, legacy) VALUES (?,?,?,0)`,
		nodeID, u.ID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO quota_reservations (node_id, user_id, created_at) VALUES (?,?,?)`,
		nodeID, u.ID, now); err != nil {
		return err
	}
	var used int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM node_owners WHERE user_id = ? AND legacy = 0`, u.ID).Scan(&used); err != nil {
		return err
	}
	if used > u.MaxNodes {
		return ErrQuotaExceeded // defer Rollback откатит INSERT'ы
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
