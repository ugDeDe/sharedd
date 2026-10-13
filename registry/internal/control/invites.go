package control

// Приглашения (invitations-migration-contract.md §2): одноразовые токены
// саморегистрации. Полный токен показывается ОДИН раз при создании;
// хранится sha256 + короткий префикс. Резерв приглашения атомарен и идёт
// В ОДНОЙ транзакции с созданием пользователя: ошибка валидации полей
// приглашение не расходует, гонка двух регистраций даёт ровно один аккаунт.

import (
	"database/sql"
	"errors"
	"time"
)

// Invite — строка приглашения для админ-API (полный токен сюда не входит).
type Invite struct {
	ID         int64
	Prefix     string
	CreatedAt  int64
	ExpiresAt  int64
	UsedBy     sql.NullInt64
	UsedAt     sql.NullInt64
	UsedByName sql.NullString
	RevokedAt  sql.NullInt64
	MaxUses    int
	UsedCount  int
}

// Status — вычисляемый статус по addendum-profiles-vps.md §2:
// active|exhausted|expired|revoked. exhausted (использован до предела)
// приоритетнее revoked/expired — историческая правда о токене.
func (iv Invite) Status(now time.Time) string {
	switch {
	case iv.UsedCount >= iv.MaxUses:
		return "exhausted"
	case iv.RevokedAt.Valid:
		return "revoked"
	case iv.ExpiresAt <= now.Unix():
		return "expired"
	default:
		return "active"
	}
}

// CreateInvite — новый токен на maxUses регистраций (1..1000; валидация
// вызывающего). ttlHours 1..720. Возвращает полный токен ровно один раз.
//
// created_by остаётся 0 by design: панель аутентифицируется единым
// `[panel] token`, per-admin identity нет — атрибутировать автора нечем.
// Колонка заведена под будущий per-admin auth (known gap, см.
// docs/specs/implementation-summary-invitations.md).
func (s *Store) CreateInvite(ttlHours, maxUses int, now time.Time) (id int64, token, prefix string, expiresAt int64, err error) {
	if maxUses < 1 {
		maxUses = 1
	}
	token, prefix = newInviteToken()
	res, err := s.sql.Exec(
		`INSERT INTO invites (token_hash, prefix, created_by, created_at, expires_at, max_uses, used_count) VALUES (?,?,0,?,?,?,0)`,
		hashAuthkey(token), prefix, now.Unix(), now.Add(time.Duration(ttlHours)*time.Hour).Unix(), maxUses)
	if err != nil {
		return 0, "", "", 0, err
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, "", "", 0, err
	}
	return id, token, prefix, now.Add(time.Duration(ttlHours) * time.Hour).Unix(), nil
}

// Invites — список приглашений (стабильный порядок по id). used_by_username
// резолвится JOIN'ом; удалённый/системный владелец → NULL.
func (s *Store) Invites(now time.Time) ([]Invite, error) {
	rows, err := s.sql.Query(`
SELECT i.id, i.prefix, i.created_at, i.expires_at, i.used_by, i.used_at, i.revoked_at, u.username, i.max_uses, i.used_count
FROM invites i LEFT JOIN users u ON u.id = i.used_by
ORDER BY i.id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Invite{}
	for rows.Next() {
		var iv Invite
		if err := rows.Scan(&iv.ID, &iv.Prefix, &iv.CreatedAt, &iv.ExpiresAt,
			&iv.UsedBy, &iv.UsedAt, &iv.RevokedAt, &iv.UsedByName, &iv.MaxUses, &iv.UsedCount); err != nil {
			return nil, err
		}
		out = append(out, iv)
	}
	return out, rows.Err()
}

// RevokeInvite — отзыв (идемпотентен). Исчерпанное приглашение
// терминально: статус «exhausted» правдивее «revoked», отзыв его не меняет.
// false → не найдено/уже отозвано/уже исчерпано.
func (s *Store) RevokeInvite(id int64, now time.Time) (bool, error) {
	res, err := s.sql.Exec(
		`UPDATE invites SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL AND used_count < max_uses`,
		now.Unix(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// CreateUserWithInvite — саморегистрация: пользователь role=user, max_nodes=0,
// приглашение резервируется УСЛОВНЫМ UPDATE внутри той же транзакции
// (contract §2): `WHERE id=? AND used_by IS NULL AND revoked_at IS NULL
// AND expires_at>?` — параллельные регистрации дают ровно один успех,
// провал валидации полей/дубль username приглашение не расходуют.
func (s *Store) CreateUserWithInvite(username, password, inviteToken string, now time.Time) (User, error) {
	return s.CreateUserWithInviteQuota(username, password, inviteToken, 0, now)
}

// CreateUserWithInviteQuota — как CreateUserWithInvite, но задаёт квоту нового
// пользователя ([users] default_max_nodes; 0 = прежнее поведение). Квота
// проверяется тем же validateNewUser (0..MaxUserNodes).
func (s *Store) CreateUserWithInviteQuota(username, password, inviteToken string, maxNodes int, now time.Time) (User, error) {
	if inviteToken == "" {
		return User{}, ErrInviteRequired
	}
	if err := validateNewUser(username, password, "user", maxNodes); err != nil {
		return User{}, err // поля проверены ДО резерва — invite цел
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(
		`INSERT INTO users (username, password_hash, role, max_nodes, disabled, created_at)
		 VALUES (?,?, 'user', ?, 0, ?)`, username, string(hash), maxNodes, now.Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrDuplicate
		}
		return User{}, err
	}
	userID, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	upd, err := tx.Exec(
		`UPDATE invites SET used_count = used_count + 1, used_by = ?, used_at = ?
		 WHERE token_hash = ? AND revoked_at IS NULL AND expires_at > ? AND used_count < max_uses`,
		userID, now.Unix(), hashAuthkey(inviteToken), now.Unix())
	if err != nil {
		return User{}, err
	}
	if n, err := upd.RowsAffected(); err != nil {
		return User{}, err
	} else if n == 0 {
		return User{}, classifyInviteTx(tx, inviteToken, now) // гонка: точная причина
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return User{ID: userID, Username: username, Role: "user", MaxNodes: maxNodes}, nil
}

// classifyInviteTx — различимая причина провала резерва (contract §2 +
// addendum §2). Неизвестный токен → invalid, даже если он же expired/used
// (не раскрываем существование токенов). Исчерпание (used_count>=max_uses)
// остаётся кодом ErrInviteUsed — совместимо с UI.
func classifyInviteTx(tx *sql.Tx, inviteToken string, now time.Time) error {
	var usedCount, maxUses int
	var revokedAt sql.NullInt64
	var expiresAt int64
	err := tx.QueryRow(
		`SELECT used_count, max_uses, revoked_at, expires_at FROM invites WHERE token_hash = ?`,
		hashAuthkey(inviteToken)).Scan(&usedCount, &maxUses, &revokedAt, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrInviteInvalid
	}
	if err != nil {
		return err
	}
	switch {
	case usedCount >= maxUses:
		return ErrInviteUsed
	case revokedAt.Valid:
		return ErrInviteRevoked
	case expiresAt <= now.Unix():
		return ErrInviteExpired
	default:
		return ErrInviteInvalid
	}
}

// newInviteToken — 32B random + префикс для показа в списке (префикс —
// первые символы самого токена, как у authkeys).
func newInviteToken() (token, prefix string) {
	token = randomSecret(32)
	return token, token[:prefixLen]
}
