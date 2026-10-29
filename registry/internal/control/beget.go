package control

// Личные Beget-аккаунты: связка login/password (зашифрованные) + SSH-ключ для
// выполнения шагов профиля по SSH (docs/specs/beget-provisioning.md §1, §3, §5).
// login_enc/
// password_enc/ssh_priv_enc хранятся ТОЛЬКО в зашифрованном виде (AES-GCM,
// ключ вне БД). Наружу (API/логи) секреты не отдаются — BegetLinkView их не
// содержит, а JSON-теги внутренней структуры исключают утечку при сериализации.

import (
	"database/sql"
	"errors"
	"time"
)

// BegetLink — внутренняя запись связки; секретные поля json:"-" на случай
// случайной сериализации (тест §7.5 проверяет отсутствие секретов в выводе).
type BegetLink struct {
	ID          int64  `json:"id"`
	UserID      int64  `json:"user_id"`
	LoginEnc    string `json:"-"`
	PasswordEnc string `json:"-"`
	SSHKeyID    uint32 `json:"ssh_key_id"`
	SSHPub      string `json:"ssh_pub"`
	SSHPrivEnc  string `json:"-"`
	CreatedAt   int64  `json:"created_at"`
	VerifiedAt  int64  `json:"-"`
	LastError   string `json:"-"`
}

// BegetLinkView — безопасное представление для API: без шифротекста и ошибок.
type BegetLinkView struct {
	UserID    int64  `json:"user_id"`
	SSHPub    string `json:"ssh_pub"`
	SSHKeyID  uint32 `json:"ssh_key_id"`
	Verified  bool   `json:"verified"`
	CreatedAt int64  `json:"created_at"`
}

// UpsertBegetLink создаёт/перезаписывает связку пользователя (user_id UNIQUE).
func (s *Store) UpsertBegetLink(l BegetLink) error {
	_, err := s.sql.Exec(
		`INSERT INTO beget_links (user_id, login_enc, password_enc, ssh_key_id, ssh_pub, ssh_priv_enc, created_at, verified_at, last_error)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   login_enc=excluded.login_enc, password_enc=excluded.password_enc,
		   ssh_key_id=excluded.ssh_key_id, ssh_pub=excluded.ssh_pub, ssh_priv_enc=excluded.ssh_priv_enc,
		   verified_at=excluded.verified_at, last_error=excluded.last_error`,
		l.UserID, l.LoginEnc, l.PasswordEnc, l.SSHKeyID, l.SSHPub, l.SSHPrivEnc, orNow(l.CreatedAt), l.VerifiedAt, l.LastError)
	return err
}

func (s *Store) BegetLink(userID int64) (BegetLink, error) {
	var l BegetLink
	err := s.sql.QueryRow(
		`SELECT id, user_id, login_enc, password_enc, ssh_key_id, ssh_pub, ssh_priv_enc, created_at, verified_at, last_error
		 FROM beget_links WHERE user_id = ?`, userID).
		Scan(&l.ID, &l.UserID, &l.LoginEnc, &l.PasswordEnc, &l.SSHKeyID, &l.SSHPub, &l.SSHPrivEnc, &l.CreatedAt, &l.VerifiedAt, &l.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return BegetLink{}, ErrNotFound
	}
	return l, err
}

// BegetLinkView — представление для API (тест §7.5: секретов нет).
func (s *Store) BegetLinkView(userID int64) (BegetLinkView, error) {
	l, err := s.BegetLink(userID)
	if err != nil {
		return BegetLinkView{}, err
	}
	return BegetLinkView{
		UserID:    l.UserID,
		SSHPub:    l.SSHPub,
		SSHKeyID:  l.SSHKeyID,
		Verified:  l.VerifiedAt > 0,
		CreatedAt: l.CreatedAt,
	}, nil
}

// MarkBegetLinkVerified — успешная авторизация в личном Beget.
func (s *Store) MarkBegetLinkVerified(userID int64) error {
	_, err := s.sql.Exec(`UPDATE beget_links SET verified_at = ?, last_error = '' WHERE user_id = ?`, time.Now().Unix(), userID)
	return err
}

// SetBegetLinkError — последняя ошибка авторизации/валидации связки.
func (s *Store) SetBegetLinkError(userID int64, msg string) error {
	_, err := s.sql.Exec(`UPDATE beget_links SET verified_at = 0, last_error = ? WHERE user_id = ?`, msg, userID)
	return err
}

// SetBegetLinkSSHKey — привязка созданного у провайдера SSH-ключа.
func (s *Store) SetBegetLinkSSHKey(userID int64, keyID uint32, pub string) error {
	_, err := s.sql.Exec(`UPDATE beget_links SET ssh_key_id = ?, ssh_pub = ? WHERE user_id = ?`, keyID, pub, userID)
	return err
}
