package control

// Личные значения по умолчанию для профиля установки.
//
// Хранятся только несекретные переменные: ad_tag, порт, SNI и объявленные
// обычные параметры. Пароли, SSH-ключи и node_secret сюда не попадают.
// Значения применяются при установке, если пользователь не переопределил их
// явно в форме.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type UserProfileDefaults struct {
	ProfileID int64             `json:"profile_id"`
	Vars      map[string]string `json:"vars"`
	Revision  int64             `json:"revision"`
	UpdatedAt int64             `json:"updated_at"`
}

// GetUserProfileDefaults — сохранённые значения или ErrNotFound.
func (s *Store) GetUserProfileDefaults(userID, profileID int64) (UserProfileDefaults, error) {
	var (
		raw       string
		revision  int64
		updatedAt int64
	)
	err := s.sql.QueryRow(`SELECT vars_json, revision, updated_at FROM user_profile_defaults
		WHERE user_id = ? AND profile_id = ?`, userID, profileID).Scan(&raw, &revision, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UserProfileDefaults{}, ErrNotFound
	}
	if err != nil {
		return UserProfileDefaults{}, err
	}
	out := UserProfileDefaults{ProfileID: profileID, Vars: map[string]string{}, Revision: revision, UpdatedAt: updatedAt}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &out.Vars); err != nil {
			return UserProfileDefaults{}, fmt.Errorf("stored profile defaults: %w", err)
		}
	}
	return out, nil
}

// SetUserProfileDefaults — upsert с optimistic locking. expectedRevision=0
// означает «создать/перезаписать без проверки» (первая настройка).
func (s *Store) SetUserProfileDefaults(userID, profileID int64, vars map[string]string, expectedRevision int64) (UserProfileDefaults, error) {
	if userID <= 0 || profileID <= 0 || len(vars) > MaxExtraVars {
		return UserProfileDefaults{}, ErrValidation
	}
	clean := make(map[string]string, len(vars))
	for k, v := range vars {
		k = strings.TrimSpace(k)
		if err := validateEditableVarName(k); err != nil {
			return UserProfileDefaults{}, err
		}
		switch k {
		case "proxy_secret", "secret", "password", "authkey", "node_secret":
			return UserProfileDefaults{}, fmt.Errorf("%w: secret variable %q cannot be stored as a default", ErrValidation, k)
		}
		if len(v) > MaxVarValueLen {
			return UserProfileDefaults{}, ErrValidation
		}
		clean[k] = v
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return UserProfileDefaults{}, err
	}
	now := time.Now().Unix()
	tx, err := s.sql.Begin()
	if err != nil {
		return UserProfileDefaults{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var current int64
	err = tx.QueryRow(`SELECT revision FROM user_profile_defaults WHERE user_id = ? AND profile_id = ?`,
		userID, profileID).Scan(&current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if expectedRevision > 0 {
			return UserProfileDefaults{}, ErrRevisionConflict
		}
		if _, err := tx.Exec(`INSERT INTO user_profile_defaults (user_id, profile_id, vars_json, revision, updated_at)
			VALUES (?,?,?,1,?)`, userID, profileID, string(raw), now); err != nil {
			return UserProfileDefaults{}, err
		}
		current = 1
	case err != nil:
		return UserProfileDefaults{}, err
	default:
		if expectedRevision > 0 && expectedRevision != current {
			return UserProfileDefaults{}, ErrRevisionConflict
		}
		if _, err := tx.Exec(`UPDATE user_profile_defaults SET vars_json = ?, revision = revision + 1, updated_at = ?
			WHERE user_id = ? AND profile_id = ?`, string(raw), now, userID, profileID); err != nil {
			return UserProfileDefaults{}, err
		}
		current++
	}
	if err := tx.Commit(); err != nil {
		return UserProfileDefaults{}, err
	}
	return UserProfileDefaults{ProfileID: profileID, Vars: clean, Revision: current, UpdatedAt: now}, nil
}
