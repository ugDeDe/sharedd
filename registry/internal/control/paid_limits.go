package control

// Опциональный дневной лимит платных попыток (cloud/manual/fleet).
//
// Лимит включается только ненулевым [provisioning] max_paid_per_day.
// Резервирование атомарно: INSERT N попыток и COUNT в одной транзакции,
// поэтому два конкурентных заказа не превышают лимит вдвоём.

import (
	"time"
)

// ReservePaidAttempts — зарезервировать n платных попыток. limit <= 0 = выключено.
func (s *Store) ReservePaidAttempts(userID int64, n, limit int) error {
	if userID <= 0 || n <= 0 {
		return ErrValidation
	}
	if limit <= 0 {
		return nil
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO paid_action_attempts (user_id, created_at) VALUES (?,?)`, userID, now); err != nil {
			return err
		}
	}
	var used int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM paid_action_attempts
		WHERE user_id = ? AND created_at >= ?`, userID, now-24*60*60).Scan(&used); err != nil {
		return err
	}
	if used > limit {
		return ErrRateLimited
	}
	return tx.Commit()
}
