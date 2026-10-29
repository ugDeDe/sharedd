package control

// Пользователи: создание/правка админом через панель, bcrypt-пароли,
// защита последнего активного админа и системного legacy-пользователя.

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	// MinPasswordLen — контракт §2: пароль ≥ 10 символов.
	MinPasswordLen = 10
	// MaxUserNodes — контракт §2: max_nodes 0..10000 (legacy не ограничен этим).
	MaxUserNodes = 10000
	// bcrypt cost: 10 — дефолт библиотеки, ~65мс на Современном CPU.
	bcryptCost = 10
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9_.-]{3,32}$`)

type User struct {
	ID       int64
	Username string
	Role     string
	MaxNodes int
	Disabled bool
}

// UserWithUsage — пользователь + занятые места (используется панелью).
type UserWithUsage struct {
	User
	UsedNodes int
}

// UserPatch — частичная правка: nil-поля не трогаются.
type UserPatch struct {
	MaxNodes *int
	Disabled *bool
	Password *string
}

func (s *Store) ensureLegacyUser(legacyMaxNodes int) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(randomSecret(24)), bcryptCost)
	if err != nil {
		return err
	}
	res, err := s.sql.Exec(
		`INSERT INTO users (username, password_hash, role, max_nodes, disabled, created_at)
		 VALUES (?, ?, 'user', ?, 0, ?) ON CONFLICT(username) DO NOTHING`,
		LegacyUsername, string(hash), legacyMaxNodes, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		row := s.sql.QueryRow(`SELECT id FROM users WHERE username = ?`, LegacyUsername)
		if err := row.Scan(&s.legacyUserID); err != nil {
			return err
		}
		return nil
	}
	row := s.sql.QueryRow(`SELECT id FROM users WHERE username = ?`, LegacyUsername)
	if err := row.Scan(&s.legacyUserID); err != nil {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return nil
}

func validateNewUser(username, password, role string, maxNodes int) error {
	if !usernamePattern.MatchString(username) {
		return fmt.Errorf("%w: username must match [a-z0-9_.-]{3,32}", ErrValidation)
	}
	if len(password) < MinPasswordLen {
		return fmt.Errorf("%w: password must be at least %d characters", ErrValidation, MinPasswordLen)
	}
	if role != "admin" && role != "user" {
		return fmt.Errorf("%w: role must be admin or user", ErrValidation)
	}
	if maxNodes < 0 || maxNodes > MaxUserNodes {
		return fmt.Errorf("%w: max_nodes must be 0..%d", ErrValidation, MaxUserNodes)
	}
	return nil
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	return string(hash), err
}

func (s *Store) CreateUser(username, password, role string, maxNodes int) (User, error) {
	if err := validateNewUser(username, password, role, maxNodes); err != nil {
		return User{}, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return User{}, err
	}
	res, err := s.sql.Exec(
		`INSERT INTO users (username, password_hash, role, max_nodes, disabled, created_at)
		 VALUES (?,?,?,?,0,?)`,
		username, string(hash), role, maxNodes, time.Now().Unix())
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrDuplicate
		}
		return User{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, err
	}
	return User{ID: id, Username: username, Role: role, MaxNodes: maxNodes}, nil
}

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var disabled int
	if err := row.Scan(&u.ID, &u.Username, &u.Role, &u.MaxNodes, &disabled); err != nil {
		return User{}, err
	}
	u.Disabled = disabled != 0
	return u, nil
}

const userCols = `id, username, role, max_nodes, disabled`

// User — пользователь по id (использование мест — через Users/UsedNodes).
func (s *Store) User(id int64) (UserWithUsage, error) {
	u, err := scanUser(s.sql.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return UserWithUsage{}, ErrNotFound
	}
	if err != nil {
		return UserWithUsage{}, err
	}
	used, err := s.UsedNodes(id)
	if err != nil {
		return UserWithUsage{}, err
	}
	return UserWithUsage{User: u, UsedNodes: used}, nil
}

// Users — список всех пользователей с занятыми местами.
func (s *Store) Users() ([]UserWithUsage, error) {
	rows, err := s.sql.Query(`SELECT ` + userCols + ` FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []UserWithUsage{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, UserWithUsage{User: u})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		used, err := s.UsedNodes(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].UsedNodes = used
	}
	return out, nil
}

func (s *Store) countActiveAdmins() (int, error) {
	var n int
	err := s.sql.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// UpdateUser — частичная правка пользователя. Guard'ы: нельзя заблокировать
// последнего активного админа; нельзя блокировать системный legacy-пользователь
// (сломало бы перерегистрацию legacy-нод). max_nodes вниз НЕ удаляет ноды —
// только блокирует новые регистрации (квота проверяется при bind).
func (s *Store) UpdateUser(id int64, patch UserPatch) (UserWithUsage, error) {
	cur, err := s.User(id)
	if err != nil {
		return UserWithUsage{}, err
	}
	if patch.Disabled != nil && *patch.Disabled {
		if cur.Username == LegacyUsername {
			return UserWithUsage{}, ErrLegacyProtected
		}
		if cur.Role == "admin" && !cur.Disabled {
			n, err := s.countActiveAdmins()
			if err != nil {
				return UserWithUsage{}, err
			}
			if n <= 1 {
				return UserWithUsage{}, ErrLastAdmin
			}
		}
	}
	if patch.Password != nil {
		if len(*patch.Password) < MinPasswordLen {
			return UserWithUsage{}, fmt.Errorf("%w: password must be at least %d characters", ErrValidation, MinPasswordLen)
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*patch.Password), bcryptCost)
		if err != nil {
			return UserWithUsage{}, err
		}
		if _, err := s.sql.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, string(hash), id); err != nil {
			return UserWithUsage{}, err
		}
	}
	if patch.MaxNodes != nil {
		if *patch.MaxNodes < 0 || *patch.MaxNodes > MaxUserNodes {
			return UserWithUsage{}, fmt.Errorf("%w: max_nodes must be 0..%d", ErrValidation, MaxUserNodes)
		}
		if _, err := s.sql.Exec(`UPDATE users SET max_nodes = ? WHERE id = ?`, *patch.MaxNodes, id); err != nil {
			return UserWithUsage{}, err
		}
	}
	if patch.Disabled != nil {
		if _, err := s.sql.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, b2i(*patch.Disabled), id); err != nil {
			return UserWithUsage{}, err
		}
	}
	return s.User(id)
}

// DeleteUser — полностью удаляет пользователя (панельный
// DELETE /panel/api/admin/users/{id}). Отказы: системный `legacy`
// (ErrLegacyProtected) и последний активный админ (ErrLastAdmin). Ноды
// пользователя перепривязываются к legacy (остаются в пуле, место
// освобождается — как ReleaseNode); снимаются резервы квоты и удаляются его
// authkeys/сессии/профили (с версиями)/свои серверы/beget-связки/операции/
// облачные ресурсы/pending-секреты.
func (s *Store) DeleteUser(id int64) error {
	cur, err := s.User(id)
	if err != nil {
		return err
	}
	if cur.Username == LegacyUsername {
		return ErrLegacyProtected
	}
	if cur.Role == "admin" && !cur.Disabled {
		n, err := s.countActiveAdmins()
		if err != nil {
			return err
		}
		if n <= 1 {
			return ErrLastAdmin
		}
	}
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Ноды: владелец → legacy, legacy=1, released_user_id фиксирует прежнего
	// владельца (как ReleaseNode). Физически ноды/state не трогаем.
	if _, err := tx.Exec(`UPDATE node_owners SET released_user_id = CASE WHEN legacy = 0 THEN user_id ELSE released_user_id END,
		user_id = ?, legacy = 1 WHERE user_id = ?`, s.legacyUserID, id); err != nil {
		return err
	}
	// Места в квоте, привязанные к удаляемому пользователю, снимаются.
	if _, err := tx.Exec(`DELETE FROM quota_reservations WHERE user_id = ?`, id); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM sessions WHERE user_id = ?`,
		`DELETE FROM authkeys WHERE user_id = ?`,
		`DELETE FROM pending_node_secrets WHERE user_id = ?`,
		`DELETE FROM manual_hosts WHERE user_id = ?`,
		`DELETE FROM beget_links WHERE user_id = ?`,
		`DELETE FROM operations WHERE user_id = ?`,
		`DELETE FROM cloud_resources WHERE user_id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	// Профили: версии первыми (FK profile_versions.profile_id → profiles.id).
	if _, err := tx.Exec(`DELETE FROM profile_versions WHERE profile_id IN (SELECT id FROM profiles WHERE created_by = ?)`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM profiles WHERE created_by = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// Authenticate — вход в кабинет: пользователь по имени + bcrypt-проверка
// пароля. Неизвестный пользователь и неверный пароль дают один и тот же
// ErrBadCredentials (не раскрываем существование имени); заблокированный —
// ErrUserDisabled.
func (s *Store) Authenticate(username, password string) (User, error) {
	u, err := scanUser(s.sql.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrBadCredentials
	}
	if err != nil {
		return User{}, err
	}
	if u.Disabled {
		return User{}, ErrUserDisabled
	}
	if err := s.VerifyPassword(u.ID, password); err != nil {
		return User{}, ErrBadCredentials
	}
	return u, nil
}

// VerifyPassword — проверка пароля (для будущей user-панели; админ-API M1
// ходит под panel-token). Инженерно безопасно: bcrypt.CompareHashAndPassword.
func (s *Store) VerifyPassword(userID int64, password string) error {
	var hash string
	err := s.sql.QueryRow(`SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		return ErrBadCredentials
	}
	return nil
}

// ── мелочи ───────────────────────────────────────────────────────

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// randomSecret — hex-строка из n случайных байт (для пароля legacy и сессий).
func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand не подводит на Linux; паника здесь = сломанная система
		panic(fmt.Sprintf("control: crypto/rand: %v", err))
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hex[c>>4], hex[c&0x0f])
	}
	return string(out)
}
