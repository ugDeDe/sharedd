package control

// Durable-настройки (invitations-migration-contract.md §3): таблица
// settings(key,value) в control.db. Ключ legacy_shared_secret = "on"|"off".
// Бутстрап: при отсутствии строки — значение из [users] allow_shared_secret
// (default true), строка создаётся при первом чтении/записи.

import (
	"database/sql"
	"errors"
)

// LegacySecretKey — единственный на сегодня ключ settings.
const LegacySecretKey = "legacy_shared_secret"

// LegacySecretEnabled — прочитать режим; при отсутствии строки записать
// значение defaultOn (бутстрап) и вернуть его.
func (s *Store) LegacySecretEnabled(defaultOn bool) (bool, error) {
	var v string
	err := s.sql.QueryRow(`SELECT value FROM settings WHERE key = ?`, LegacySecretKey).Scan(&v)
	switch {
	case err == nil:
		return v == "on", nil
	case errors.Is(err, sql.ErrNoRows):
		if err := s.SetLegacySecret(defaultOn); err != nil {
			return false, err
		}
		return defaultOn, nil
	default:
		return false, err
	}
}

// SetLegacySecret — один UPDATE (частичный успех невозможен по контракту §3).
func (s *Store) SetLegacySecret(enabled bool) error {
	v := "off"
	if enabled {
		v = "on"
	}
	_, err := s.sql.Exec(
		`INSERT INTO settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, LegacySecretKey, v)
	return err
}
