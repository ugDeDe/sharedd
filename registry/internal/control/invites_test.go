package control

// Обязательные тесты приглашений и настроек (invitations-migration-contract.md
// §5 пп.1-2, 5): гонка регистраций на одном invite, различимые коды отказа,
// валидация полей не расходует приглашение, переживание рестарта.

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fixedNow — детерминированный якорь (репозиторий запрещает
// wall-clock-относительные окна в календарных проверках).
func fixedNow() time.Time { return time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC) }

func mustInvite(t *testing.T, s *Store, ttlHours int) (token string, id int64) {
	t.Helper()
	id, token, _, _, err := s.CreateInvite(ttlHours, 1, fixedNow())
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return token, id
}

// §5.1 — два параллельных register с одним invite → ровно один аккаунт.
func TestInviteRaceExactlyOneAccount(t *testing.T) {
	s := openTest(t)
	token, _ := mustInvite(t, s, 72)
	now := fixedNow()

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var successes, inviteFails int
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateUserWithInvite(
				"user-"+string(rune('a'+i)), "password-123", token, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrInviteUsed):
				inviteFails++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if successes != 1 || inviteFails != n-1 {
		t.Fatalf("successes=%d inviteFails=%d, want exactly 1 success", successes, inviteFails)
	}
	users, err := s.Users()
	if err != nil {
		t.Fatal(err)
	}
	// ровно 1 новый аккаунт + системный legacy
	if got := len(users); got != 2 {
		t.Fatalf("accounts=%d, want 2 (one winner + legacy)", got)
	}
}

// §5.2 — expired/used/revoked/invalid дают различимые коды; невалидные
// поля не расходуют приглашение.
func TestInviteDistinctRejectionCodes(t *testing.T) {
	s := openTest(t)
	now := fixedNow()

	expiredTok, _ := mustInvite(t, s, 1) // expires через 1ч от fixedNow
	usedTok, _ := mustInvite(t, s, 72)
	revokedTok, id := mustInvite(t, s, 72)
	validTok, _ := mustInvite(t, s, 72)

	if _, err := s.RevokeInvite(id, now); err != nil {
		t.Fatal(err)
	}
	// «used»: одна успешная регистрация расходует токен.
	if _, err := s.CreateUserWithInvite("used-owner", "password-123", usedTok, now); err != nil {
		t.Fatalf("seed used invite: %v", err)
	}

	cases := []struct {
		name   string
		token  string
		at     time.Time
		fields func() (string, string)
		want   error
	}{
		{"empty", "", now, func() (string, string) { return "alice", "password-123" }, ErrInviteRequired},
		{"unknown", "no-such-token", now, func() (string, string) { return "bob", "password-123" }, ErrInviteInvalid},
		{"expired", expiredTok, now.Add(2 * time.Hour), func() (string, string) { return "carol", "password-123" }, ErrInviteExpired},
		{"used", usedTok, now, func() (string, string) { return "dave", "password-123" }, ErrInviteUsed},
		{"revoked", revokedTok, now, func() (string, string) { return "erin", "password-123" }, ErrInviteRevoked},
		{"bad-fields-do-not-consume", validTok, now, func() (string, string) { return "x", "short" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			username, password := tc.fields()
			_, err := s.CreateUserWithInvite(username, password, tc.token, tc.at)
			if tc.want == nil {
				// поле «bad-fields-do-not-consume»: ошибка валидации (не invite).
				if !errors.Is(err, ErrValidation) {
					t.Fatalf("want validation error, got %v", err)
				}
				// и приглашение всё ещё активно.
				if _, err := s.CreateUserWithInvite("frank", "password-123", tc.token, now); err != nil {
					t.Fatalf("invite must survive field-validation failure, got %v", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// §5.2 — duplicate username даёт ErrDuplicate (ошибка уровня полей),
// а не код приглашения.
func TestInviteDuplicateUsernameRollsBack(t *testing.T) {
	s := openTest(t)
	now := fixedNow()
	token, _ := mustInvite(t, s, 72)
	if _, err := s.CreateUserWithInvite("same-name", "password-123", token, now); err != nil {
		t.Fatal(err)
	}
	// Другой токен + существующее имя → конфликт имени, не приглашения.
	token2, _ := mustInvite(t, s, 72)
	if _, err := s.CreateUserWithInvite("same-name", "password-123", token2, now); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	// Этот токен не потрачен: регистрируется новым именем.
	if _, err := s.CreateUserWithInvite("other-name", "password-123", token2, now); err != nil {
		t.Fatalf("invite must survive duplicate-username failure, got %v", err)
	}
}

// §5.5 — переживание рестарта: OFF остаётся OFF; пустой settings → ON.
func TestLegacySecretSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "control.db")

	s1, err := Open(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	// Пустой settings → бутстрап из defaultOn.
	on, err := s1.LegacySecretEnabled(true)
	if err != nil || !on {
		t.Fatalf("fresh bootstrap must be ON (err=%v on=%v)", err, on)
	}
	if err := s1.SetLegacySecret(false); err != nil {
		t.Fatal(err)
	}
	s1.Close()

	s2, err := Open(path, 100000)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	on, err = s2.LegacySecretEnabled(true) // defaultOn игнорируется — строка есть
	if err != nil || on {
		t.Fatalf("OFF must survive restart (err=%v on=%v)", err, on)
	}
}

// addendum §5.2 — max_uses=10: 10 успешных регистраций, 11-я → invite_used;
// статус становится exhausted.
func TestInviteMultiUseExactLimit(t *testing.T) {
	s := openTest(t)
	now := fixedNow()
	id, token, _, _, err := s.CreateInvite(72, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.CreateUserWithInvite("multi-"+string(rune('a'+i)), "password-123", token, now); err != nil {
			t.Fatalf("registration %d must succeed: %v", i, err)
		}
	}
	if _, err := s.CreateUserWithInvite("multi-k", "password-123", token, now); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("11th registration err = %v, want ErrInviteUsed", err)
	}
	invites, err := s.Invites(now)
	if err != nil {
		t.Fatal(err)
	}
	var got *Invite
	for i := range invites {
		if invites[i].ID == id {
			got = &invites[i]
		}
	}
	if got == nil || got.UsedCount != 10 || got.MaxUses != 10 || got.Status(now) != "exhausted" {
		t.Fatalf("invite after 10 uses = %+v (status=%s)", got, got.Status(now))
	}
}

// addendum §5.2 — параллельные 12 при max_uses=10 → ровно 10.
func TestInviteMultiUseConcurrentCap(t *testing.T) {
	s := openTest(t)
	now := fixedNow()
	_, token, _, _, err := s.CreateInvite(72, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, used := 0, 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.CreateUserWithInvite("conc-"+string(rune('a'+i)), "password-123", token, now)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case errors.Is(err, ErrInviteUsed):
				used++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if success != 10 || used != 2 {
		t.Fatalf("success=%d used=%d, want exactly 10/2", success, used)
	}
}
