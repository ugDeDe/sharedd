package control

// Authkeys: sha256-хеш полного ключа + короткий префикс для UI (sda_XXXXXXXX…).
// Полный ключ показывается ОДИН раз в ответе rotate/create; хранится только хеш.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// AuthkeyPrefix — всем ключам выдаём узнаваемый префикс (агенты и UI).
const AuthkeyPrefix = "sda_"

// prefixLen — сколько символов после sda_ храним для показа в панели.
const prefixLen = 8

func hashAuthkey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func newAuthkey() (key, prefix string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("control: crypto/rand: %v", err))
	}
	key = AuthkeyPrefix + hex.EncodeToString(b)
	return key, AuthkeyPrefix + hex.EncodeToString(b)[:prefixLen]
}

// RotateAuthkey — отзывает ВСЕ ключи пользователя и выдаёт новый.
// Полный ключ возвращается один раз; повторно доступен только rotate.
func (s *Store) RotateAuthkey(userID int64) (string, error) {
	if _, err := s.User(userID); err != nil {
		return "", err
	}
	key, prefix := newAuthkey()
	now := time.Now().Unix()
	tx, err := s.sql.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE authkeys SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO authkeys (key_hash, user_id, prefix, created_at) VALUES (?,?,?,?)`,
		hashAuthkey(key), userID, prefix, now); err != nil {
		return "", err
	}
	return key, tx.Commit()
}

// RevokeAuthkeys — все ключи пользователя больше не авторизуют регистрации.
func (s *Store) RevokeAuthkeys(userID int64) error {
	if _, err := s.User(userID); err != nil {
		return err
	}
	_, err := s.sql.Exec(`UPDATE authkeys SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, time.Now().Unix(), userID)
	return err
}

// LiveAuthkey — метаданные текущего живого ключа пользователя для кабинета:
// только префикс и дата создания, полный ключ недоступен (показывается один раз
// при rotate). Нет живого ключа → active=false, не ошибка.
func (s *Store) LiveAuthkey(userID int64) (prefix string, createdAt int64, active bool, err error) {
	err = s.sql.QueryRow(
		`SELECT prefix, created_at FROM authkeys WHERE user_id = ? AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`,
		userID).Scan(&prefix, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return prefix, createdAt, true, nil
}

// LookupAuthkey — пользователь, которому принадлежит живой (не отозванный)
// ключ. Не найден/отозван → ErrNotFound; заблокированность пользователя
// проверяет вызывающий (у него на руках User.Disabled).
func (s *Store) LookupAuthkey(key string) (User, error) {
	row := s.sql.QueryRow(`
SELECT u.`+userCols+`
FROM authkeys k JOIN users u ON u.id = k.user_id
WHERE k.key_hash = ? AND k.revoked_at IS NULL`, hashAuthkey(key))
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}
