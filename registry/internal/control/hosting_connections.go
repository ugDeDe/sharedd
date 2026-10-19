package control

// Hosting-connections (docs/specs/hosting-blueprints/03-cabinet-api.md §3,
// 04-storage-and-execution.md §2, stage-04): подключение аккаунтов провайдеров
// (nodehost/beget) с owner-проверками. CredentialsEnc — AES-GCM шифротекст
// credentials-бандла (JSON), наружу не отдаётся (json:"-"). Балансы —
// connection_balances с CAS по credential_revision: смена кредов инвалидирует
// прежний снимок, конкурентная запись устаревшей ревизии отбрасывается.

import (
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrRevisionConflict — PATCH с чужим expected_revision (CAS-конфликт).
var ErrRevisionConflict = errors.New("revision conflict")

const (
	ProviderNodehost = "nodehost"
	ProviderBeget    = "beget"
)

// HostingConnection — подключение провайдера. Секретные/служебные поля
// json:"-" на случай случайной сериализации.
type HostingConnection struct {
	ID                 int64  `json:"id"`
	UserID             int64  `json:"user_id"`
	Provider           string `json:"provider"`
	Label              string `json:"label"`
	CredentialsEnc     string `json:"-"`
	CredentialRevision int64  `json:"credential_revision"`
	Revision           int64  `json:"revision"`
	VerifiedAt         int64  `json:"-"`
	CapabilitiesJSON   string `json:"-"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"-"`
}

// ConnectionBalance — последний снимок баланса подключения. Amount ""
// сериализуется наружу как null (semantics: пустая строка = «значения нет»).
type ConnectionBalance struct {
	ConnectionID       int64
	UserID             int64
	CredentialRevision int64
	Amount             string // "" = нет значения
	Currency           string
	Scope              string
	CheckedAt          int64 // 0 = успешного снятия ещё не было
	LastAttemptAt      int64
	SafeError          string // безопасный код ошибки, без текста провайдера
}

const hostingConnCols = `id, user_id, provider, label, credentials_enc, credential_revision, revision, verified_at, capabilities_json, created_at, updated_at`

func scanHostingConnection(row interface{ Scan(...any) error }) (HostingConnection, error) {
	var c HostingConnection
	err := row.Scan(&c.ID, &c.UserID, &c.Provider, &c.Label, &c.CredentialsEnc,
		&c.CredentialRevision, &c.Revision, &c.VerifiedAt, &c.CapabilitiesJSON, &c.CreatedAt, &c.UpdatedAt)
	return c, err
}

func validHostingProvider(p string) bool { return p == ProviderNodehost || p == ProviderBeget }

// validLabel — 1..100 рун (03-cabinet-api.md §3).
func validLabel(label string) bool {
	n := utf8.RuneCountInString(label)
	return n >= 1 && n <= 100
}

// CreateHostingConnection добавляет подключение (revision/credential_revision
// принудительно 1, verified_at/capabilities пустые) и возвращает её с id.
func (s *Store) CreateHostingConnection(c HostingConnection) (HostingConnection, error) {
	if !validHostingProvider(c.Provider) || !validLabel(c.Label) {
		return HostingConnection{}, ErrValidation
	}
	if c.CredentialRevision == 0 {
		c.CredentialRevision = 1
	}
	now := orNow(c.CreatedAt)
	res, err := s.sql.Exec(
		`INSERT INTO hosting_connections
		 (user_id, provider, label, credentials_enc, credential_revision, revision, verified_at, capabilities_json, created_at, updated_at)
		 VALUES (?,?,?,?,?,1,0,'',?,?)`,
		c.UserID, c.Provider, c.Label, c.CredentialsEnc, c.CredentialRevision, now, now)
	if err != nil {
		return HostingConnection{}, err
	}
	c.ID, err = res.LastInsertId()
	if err != nil {
		return HostingConnection{}, err
	}
	c.Revision, c.VerifiedAt, c.CapabilitiesJSON, c.CreatedAt, c.UpdatedAt = 1, 0, "", now, now
	return c, nil
}

// HostingConnectionOwned — подключение владельца; чужое/неизвестное → ErrNotFound.
func (s *Store) HostingConnectionOwned(id, userID int64) (HostingConnection, error) {
	c, err := scanHostingConnection(s.sql.QueryRow(
		`SELECT `+hostingConnCols+` FROM hosting_connections WHERE id = ? AND user_id = ?`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return HostingConnection{}, ErrNotFound
	}
	return c, err
}

// HostingConnectionsByUser — подключения пользователя (новые первыми).
func (s *Store) HostingConnectionsByUser(userID int64) ([]HostingConnection, error) {
	rows, err := s.sql.Query(`SELECT `+hostingConnCols+` FROM hosting_connections WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []HostingConnection{}
	for rows.Next() {
		c, err := scanHostingConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PatchHostingConnection — CAS по expected_revision. label/credsEnc nil =
// поле не меняется; credsEnc != nil = полная замена бандла
// (credential_revision +1). 0 строк → ErrNotFound (не владелец/нет записи)
// либо ErrRevisionConflict (запись есть, ревизия ушла).
func (s *Store) PatchHostingConnection(id, userID, expectedRevision int64, label, credsEnc *string) (HostingConnection, error) {
	if label != nil && !validLabel(*label) {
		return HostingConnection{}, ErrValidation
	}
	q := `UPDATE hosting_connections SET updated_at = ?, revision = revision + 1`
	args := []any{time.Now().Unix()}
	if label != nil {
		q += `, label = ?`
		args = append(args, *label)
	}
	if credsEnc != nil {
		q += `, credentials_enc = ?, credential_revision = credential_revision + 1`
		args = append(args, *credsEnc)
	}
	q += ` WHERE id = ? AND user_id = ? AND revision = ?`
	args = append(args, id, userID, expectedRevision)
	res, err := s.sql.Exec(q, args...)
	if err != nil {
		return HostingConnection{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return HostingConnection{}, err
		}
		if _, err := s.HostingConnectionOwned(id, userID); err != nil {
			return HostingConnection{}, ErrNotFound
		}
		return HostingConnection{}, ErrRevisionConflict
	}
	return s.HostingConnectionOwned(id, userID)
}

// DeleteHostingConnection удаляет подключение владельца вместе со снимком
// баланса. Ссылающихся таблиц пока нет (stage-04), поэтому удаление свободно;
// когда появятся cloud_resources — добавить проверку connection_in_use.
func (s *Store) DeleteHostingConnection(id, userID int64) error {
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM connection_balances WHERE connection_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM hosting_connections WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// UpdateHostingVerified — результат успешной verify-проверки провайдера
// (verified_at + capabilities snapshot) с CAS по credential_revision (и
// owner-проверкой в WHERE, как в PatchHostingConnection): ротация кредов или
// удаление во время in-flight verify инвалидирует запись (0 строк →
// ErrNotFound/ErrRevisionConflict по повторному чтению). revision при этом не
// меняется, чтобы параллельный label-PATCH не ловил ложный конфликт.
func (s *Store) UpdateHostingVerified(id, userID, credentialRevision, verifiedAt int64, capabilitiesJSON string) error {
	res, err := s.sql.Exec(
		`UPDATE hosting_connections SET verified_at = ?, capabilities_json = ?, updated_at = ?
		 WHERE id = ? AND user_id = ? AND credential_revision = ?`,
		verifiedAt, capabilitiesJSON, time.Now().Unix(), id, userID, credentialRevision)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		if _, err := s.HostingConnectionOwned(id, userID); err != nil {
			return ErrNotFound
		}
		return ErrRevisionConflict
	}
	return nil
}

// HostingBalance — снимок баланса; ErrNotFound, если попыток ещё не было.
func (s *Store) HostingBalance(connectionID int64) (ConnectionBalance, error) {
	var b ConnectionBalance
	err := s.sql.QueryRow(
		`SELECT connection_id, user_id, credential_revision, amount, currency, scope, checked_at, last_attempt_at, safe_error
		 FROM connection_balances WHERE connection_id = ?`, connectionID).
		Scan(&b.ConnectionID, &b.UserID, &b.CredentialRevision, &b.Amount, &b.Currency, &b.Scope,
			&b.CheckedAt, &b.LastAttemptAt, &b.SafeError)
	if errors.Is(err, sql.ErrNoRows) {
		return ConnectionBalance{}, ErrNotFound
	}
	return b, err
}

// SaveHostingBalance — upsert с CAS по credential_revision: запись
// применяется, только если её credential_revision не старше сохранённой
// (равна или новее). Странствующий in-flight баланс-запрос со старыми
// кредами после ротации перезаписать снимок не может; снимок новой
// винтажности проходит. false = отброшено, вызывающий просто отдаёт
// сохранённое.
func (s *Store) SaveHostingBalance(b ConnectionBalance) (bool, error) {
	if strings.TrimSpace(b.Amount) == "" && b.CheckedAt > 0 {
		// Пустой amount при успешном снятии не бывает; страховка от записи
		// «успешного» пустого снимка.
		return false, ErrValidation
	}
	res, err := s.sql.Exec(
		`INSERT INTO connection_balances
		 (connection_id, user_id, credential_revision, amount, currency, scope, checked_at, last_attempt_at, safe_error)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(connection_id) DO UPDATE SET
		   credential_revision = excluded.credential_revision, amount = excluded.amount,
		   currency = excluded.currency, scope = excluded.scope,
		   checked_at = excluded.checked_at, last_attempt_at = excluded.last_attempt_at,
		   safe_error = excluded.safe_error
		 WHERE excluded.credential_revision >= connection_balances.credential_revision`,
		b.ConnectionID, b.UserID, b.CredentialRevision, b.Amount, b.Currency, b.Scope,
		b.CheckedAt, b.LastAttemptAt, b.SafeError)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
