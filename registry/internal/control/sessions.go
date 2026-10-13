package control

// Сессии: токен показывается один раз, хранится sha256-хеш + срок жизни.
// M1 не имеет login-эндпоинта (панельная админ-API ходят под panel-токеном) —
// хелперы закладывают безопасную основу для user-UI следующих фаз.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

func hashSession(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateSession — новый токен сессии (валиден ttl; полный токен один раз).
func (s *Store) CreateSession(userID int64, ttl time.Duration) (string, error) {
	if _, err := s.User(userID); err != nil {
		return "", err
	}
	token := randomSecret(32)
	_, err := s.sql.Exec(`INSERT INTO sessions (token_hash, user_id, expires_at) VALUES (?,?,?)`,
		hashSession(token), userID, time.Now().Add(ttl).Unix())
	if err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser — пользователь живой (неистёкшей) сессии.
func (s *Store) SessionUser(token string, now time.Time) (User, error) {
	var userID int64
	var expiresAt int64
	err := s.sql.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token_hash = ?`, hashSession(token)).Scan(&userID, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	if now.Unix() >= expiresAt {
		return User{}, ErrExpired
	}
	u, err := scanUser(s.sql.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// DeleteSession — выход из кабинета: токен (его хеш) удаляется.
func (s *Store) DeleteSession(token string) error {
	_, err := s.sql.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashSession(token))
	return err
}

// PurgeSessions — ротация истекших сессий (дёшево звать при старте).
func (s *Store) PurgeSessions(now time.Time) error {
	_, err := s.sql.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	return err
}
