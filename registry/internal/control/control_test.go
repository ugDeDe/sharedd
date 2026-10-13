package control

// Тесты контрольной БД (docs/api-contract.md §1-§2, docs/specs/accounts-quota.md).
// Обязательный список поведений из спеки покрыт здесь и в server-тестах.

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "control.db"), 100000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func mustUser(t *testing.T, s *Store, name string, maxNodes int) User {
	t.Helper()
	u, err := s.CreateUser(name, "password-123", "user", maxNodes)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return u
}

// ── схема и миграция legacy-пользователя ─────────────────────────

func TestOpenCreatesLegacyUser(t *testing.T) {
	s := openTest(t)
	if s.LegacyUserID() == 0 {
		t.Fatal("legacy user must exist after Open")
	}
	u, err := s.User(s.LegacyUserID())
	if err != nil {
		t.Fatalf("legacy user: %v", err)
	}
	if u.Role != "user" || u.MaxNodes != 100000 || u.Disabled {
		t.Fatalf("legacy user mismatch: %+v", u.User)
	}
}

// operations.profile_id — ensureColumn-миграция старой БД: журнал без колонки
// получает profile_id=0 для существующих строк, чтение не падает (addendum §7.2).
func TestOpenMigratesOperationsProfileID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE operations (
		id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, node_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL, state TEXT NOT NULL, idem_key TEXT UNIQUE NOT NULL,
		provider_vps_id TEXT NOT NULL DEFAULT '', region TEXT NOT NULL DEFAULT '',
		config_id TEXT NOT NULL DEFAULT '', request_json TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operations (user_id, kind, state, idem_key, created_at, updated_at)
		VALUES (1,'create','failed','legacy-key',1,1)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer s.Close()
	op, err := s.OperationByIdemKey("legacy-key")
	if err != nil {
		t.Fatalf("read legacy op: %v", err)
	}
	if op.ProfileID != 0 {
		t.Fatalf("legacy profile_id = %d, want 0 (default)", op.ProfileID)
	}
}

// B2 (QA round 7): UNIQUE(user_id, host, port) на manual_hosts + безопасная
// миграция. CreateManualHost отвергает повтор того же host:port; старые БД с
// дублями схлопываются (лишняя строка + её нода/место удаляются), затем индекс.
func TestManualHostUniqueConstraintAndDedupeMigration(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 5)
	h := ManualHost{UserID: u.ID, Label: "box", Host: "8.8.8.8", Port: 22, SSHUser: "root", AuthType: "password", SecretEnc: "enc"}
	if _, err := s.CreateManualHost(h); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := s.CreateManualHost(h); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same host:port must be ErrDuplicate, got %v", err)
	}
	h.Port = 2222
	if _, err := s.CreateManualHost(h); err != nil {
		t.Fatalf("different port must be allowed: %v", err)
	}

	path := filepath.Join(t.TempDir(), "control.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT UNIQUE NOT NULL, password_hash TEXT NOT NULL, role TEXT NOT NULL CHECK(role IN ('admin','user')), max_nodes INTEGER NOT NULL DEFAULT 0, disabled INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL)`,
		`CREATE TABLE manual_hosts (id INTEGER PRIMARY KEY, user_id INTEGER NOT NULL, label TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 22, ssh_user TEXT NOT NULL DEFAULT 'root', auth_type TEXT NOT NULL, secret_enc BLOB NOT NULL, created_at INTEGER NOT NULL, verified_at INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', node_id TEXT NOT NULL DEFAULT '', operation_id INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE node_owners (node_id TEXT PRIMARY KEY, user_id INTEGER NOT NULL, registered_at INTEGER NOT NULL, legacy INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE quota_reservations (node_id TEXT UNIQUE, user_id INTEGER NOT NULL, created_at INTEGER NOT NULL)`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	// Старый баг: два ряда на один host:port, каждый со своей нодой и местом.
	if _, err := db.Exec(`INSERT INTO manual_hosts (id,user_id,label,host,port,ssh_user,auth_type,secret_enc,created_at,node_id,operation_id) VALUES
		(1,1,'a','8.8.8.8',22,'root','password','x',1,'manual-1',5),
		(2,1,'b','8.8.8.8',22,'root','password','y',2,'manual-2',6)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO node_owners (node_id,user_id,registered_at,legacy) VALUES ('manual-1',1,1,0),('manual-2',1,2,0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO quota_reservations (node_id,user_id,created_at) VALUES ('manual-1',1,1),('manual-2',1,2)`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s2, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	hosts, err := s2.ManualHostsByUser(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].ID != 1 {
		t.Fatalf("dedupe must keep exactly the earliest row (id=1), got %+v", hosts)
	}
	var n int
	if err := s2.sql.QueryRow(`SELECT COUNT(*) FROM node_owners WHERE node_id='manual-2'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("removed duplicate's node reservation must be released")
	}
	if err := s2.sql.QueryRow(`SELECT COUNT(*) FROM node_owners WHERE node_id='manual-1'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("kept row's node reservation must survive")
	}
	s2.Close()
	if s3, err := Open(path, 100000); err != nil {
		t.Fatalf("reopen after dedupe: %v", err)
	} else {
		s3.Close()
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s1, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	if _, err := s1.CreateUser("alice", "password-123", "user", 3); err != nil {
		t.Fatalf("create: %v", err)
	}
	s1.Close()
	s2, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if s2.LegacyUserID() != s1.LegacyUserID() {
		t.Fatal("legacy user id must survive reopen")
	}
	users, err := s2.Users()
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("want 2 users after reopen, got %d", len(users))
	}
}

// ── пользователи: валидация, дубликаты, последний админ ──────────

func TestCreateUserValidation(t *testing.T) {
	for i, c := range []struct {
		name, pass string
		role       string
		maxNodes   int
		wantErr    error
	}{
		{"ab", "password-123", "user", 1, ErrValidation},          // username короче 3
		{"UPPER", "password-123", "user", 1, ErrValidation},       // заглавные запрещены
		{"sp ace", "password-123", "user", 1, ErrValidation},      // пробел
		{"ok-name", "short", "user", 1, ErrValidation},            // пароль < 10
		{"ok-name", "password-123", "root", 1, ErrValidation},     // неизвестная роль
		{"ok-name", "password-123", "user", -1, ErrValidation},    // отрицательная квота
		{"ok-name", "password-123", "user", 10001, ErrValidation}, // сверх 10000
		{"ok-name", "password-123", "admin", 5, nil},
	} {
		s := openTest(t) // свежая БД на кейс: невалидные имена не уникализируем
		_, err := s.CreateUser(c.name, c.pass, c.role, c.maxNodes)
		if c.wantErr == nil && err != nil {
			t.Fatalf("case %d (%s): unexpected error %v", i, c.name, err)
		}
		if c.wantErr != nil && !errors.Is(err, c.wantErr) {
			t.Fatalf("case %d (%s): want %v, got %v", i, c.name, c.wantErr, err)
		}
	}
}

func TestCreateUserDuplicate(t *testing.T) {
	s := openTest(t)
	if _, err := s.CreateUser("alice", "password-123", "user", 1); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.CreateUser("alice", "password-456", "user", 2); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
}

func TestPasswordIsHashed(t *testing.T) {
	s := openTest(t)
	u, err := s.CreateUser("alice", "password-123", "user", 1)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.VerifyPassword(u.ID, "password-123"); err != nil {
		t.Fatalf("verify correct password: %v", err)
	}
	if err := s.VerifyPassword(u.ID, "password-wrong"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password must be ErrBadCredentials, got %v", err)
	}
}

func TestUpdateUser(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 3)
	got, err := s.UpdateUser(u.ID, UserPatch{MaxNodes: ptr(7), Password: ptr("new-password-9")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.MaxNodes != 7 {
		t.Fatalf("max_nodes = %d, want 7", got.MaxNodes)
	}
	if err := s.VerifyPassword(u.ID, "new-password-9"); err != nil {
		t.Fatalf("password not updated: %v", err)
	}
	got, err = s.UpdateUser(u.ID, UserPatch{Disabled: ptr(true)})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !got.Disabled {
		t.Fatal("user must be disabled")
	}
	// max_nodes вниз НЕ удаляет ноды — это отдельное поведение (см. квоты).
}

func TestCannotDisableLastActiveAdmin(t *testing.T) {
	s := openTest(t)
	admin, err := s.CreateUser("root", "password-123", "admin", 0)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if _, err := s.UpdateUser(admin.ID, UserPatch{Disabled: ptr(true)}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin, got %v", err)
	}
	// второй активный админ появляется → первого можно заблокировать
	admin2, err := s.CreateUser("root2", "password-123", "admin", 0)
	if err != nil {
		t.Fatalf("create admin2: %v", err)
	}
	if _, err := s.UpdateUser(admin.ID, UserPatch{Disabled: ptr(true)}); err != nil {
		t.Fatalf("disable with second admin present: %v", err)
	}
	// но последнего активного — нельзя
	if _, err := s.UpdateUser(admin2.ID, UserPatch{Disabled: ptr(true)}); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("want ErrLastAdmin for second admin, got %v", err)
	}
}

func TestLegacyUserProtected(t *testing.T) {
	s := openTest(t)
	if _, err := s.UpdateUser(s.LegacyUserID(), UserPatch{Disabled: ptr(true)}); !errors.Is(err, ErrLegacyProtected) {
		t.Fatalf("legacy user must be protected, got %v", err)
	}
}

// ── authkey ──────────────────────────────────────────────────────

func TestRotateAuthkeyRevokesOldKeys(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 3)
	k1, err := s.RotateAuthkey(u.ID)
	if err != nil {
		t.Fatalf("rotate 1: %v", err)
	}
	if !strings.HasPrefix(k1, "sda_") {
		t.Fatalf("key must have sda_ prefix, got %q", k1)
	}
	if _, err := s.LookupAuthkey(k1); err != nil {
		t.Fatalf("lookup k1: %v", err)
	}
	k2, err := s.RotateAuthkey(u.ID)
	if err != nil {
		t.Fatalf("rotate 2: %v", err)
	}
	if k1 == k2 {
		t.Fatal("new key must differ")
	}
	// старый ключ мёртв (rotate инвалидирует ВСЕ старые ключи)
	if _, err := s.LookupAuthkey(k1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old key must be revoked, got %v", err)
	}
	if _, err := s.LookupAuthkey(k2); err != nil {
		t.Fatalf("new key lookup: %v", err)
	}
}

func TestLookupUnknownAuthkey(t *testing.T) {
	s := openTest(t)
	if _, err := s.LookupAuthkey("sda_deadbeefdeadbeefdeadbeefdeadbeef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRevokeAuthkeys(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 3)
	k, err := s.RotateAuthkey(u.ID)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := s.RevokeAuthkeys(u.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := s.LookupAuthkey(k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key must not resolve, got %v", err)
	}
}

// ── владельцы нод и квоты ────────────────────────────────────────

func TestAuthorizeNodeBindsOwnerOnce(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 5)
	if err := s.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("bind: %v", err)
	}
	// повторная регистрация своей ноды — 200, место НЕ занимает второй раз
	if err := s.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("re-bind own: %v", err)
	}
	used, err := s.UsedNodes(u.ID)
	if err != nil {
		t.Fatalf("used: %v", err)
	}
	if used != 1 {
		t.Fatalf("used = %d, want 1", used)
	}
	ownerID, legacy, ok, err := s.NodeOwner("node-aaa1b")
	if err != nil || !ok || ownerID != u.ID || legacy {
		t.Fatalf("owner mismatch: id=%d legacy=%t ok=%t err=%v", ownerID, legacy, ok, err)
	}
}

func TestAuthorizeNodeOwnerMismatch(t *testing.T) {
	s := openTest(t)
	a := mustUser(t, s, "alice", 5)
	b := mustUser(t, s, "bob", 5)
	if err := s.AuthorizeNode("node-aaa1b", a); err != nil {
		t.Fatalf("bind a: %v", err)
	}
	if err := s.AuthorizeNode("node-aaa1b", b); !errors.Is(err, ErrOwnerMismatch) {
		t.Fatalf("want ErrOwnerMismatch, got %v", err)
	}
}

func TestAuthorizeNodeQuotaExceeded(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	if err := s.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("bind 1: %v", err)
	}
	// max_nodes вниз: существующие ноды живут, новые регистрации сверх лимита отклонены
	if err := s.AuthorizeNode("node-bbb2c", u); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got %v", err)
	}
	// свое место всё ещё можно переподтвердить
	if err := s.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("re-bind own after quota: %v", err)
	}
}

func TestAuthorizeNodeConcurrentOneSlot(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = s.AuthorizeNode(fmt.Sprintf("node-c%02da1b2c", i), u)
		}(i)
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrQuotaExceeded):
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("exactly one register must win the last slot, got %d", won)
	}
}

func TestReleaseNodeFreesSlot(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	if err := s.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("bind: %v", err)
	}
	ok, err := s.ReleaseNode("node-aaa1b")
	if err != nil || !ok {
		t.Fatalf("release: ok=%t err=%v", ok, err)
	}
	// владелец теперь legacy, место свободно
	ownerID, legacy, _, err := s.NodeOwner("node-aaa1b")
	if err != nil || !legacy || ownerID != s.LegacyUserID() {
		t.Fatalf("released node must belong to legacy: id=%d legacy=%t err=%v", ownerID, legacy, err)
	}
	if err := s.AuthorizeNode("node-ccc3d", u); err != nil {
		t.Fatalf("new bind after release: %v", err)
	}
	if ok, err := s.ReleaseNode("node-missing0"); err != nil || ok {
		t.Fatalf("release unknown node: ok=%t err=%v", ok, err)
	}
}

func TestLegacyNodesDoNotCountTowardQuota(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	// нода, привязанная к legacy (миграция), не занимает места real-пользователя
	if err := s.BindLegacyNode("node-olda1"); err != nil {
		t.Fatalf("bind legacy: %v", err)
	}
	if err := s.AuthorizeNode("node-newb2", u); err != nil {
		t.Fatalf("bind real: %v", err)
	}
	legacyUsed, err := s.UsedNodes(s.LegacyUserID())
	if err != nil {
		t.Fatalf("legacy used: %v", err)
	}
	if legacyUsed != 0 {
		t.Fatalf("legacy rows must not count toward used_nodes, got %d", legacyUsed)
	}
}

// ── персистентность (рестарт регистратора сохраняет владельцев) ──

func TestOwnersSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s1, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	u, err := s1.CreateUser("alice", "password-123", "user", 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s1.AuthorizeNode("node-aaa1b", u); err != nil {
		t.Fatalf("bind: %v", err)
	}
	s1.Close()

	s2, err := Open(path, 100000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	ownerID, legacy, ok, err := s2.NodeOwner("node-aaa1b")
	if err != nil || !ok || legacy || ownerID != u.ID {
		t.Fatalf("owner must survive reopen: id=%d legacy=%t ok=%t err=%v", ownerID, legacy, ok, err)
	}
}

// ── сессии ───────────────────────────────────────────────────────

func TestSessions(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "alice", 1)
	tok, err := s.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := s.SessionUser(tok, time.Now())
	if err != nil || got.ID != u.ID {
		t.Fatalf("session user: %+v err=%v", got, err)
	}
	// истекшая сессия не проходит
	if _, err := s.SessionUser(tok, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	if _, err := s.SessionUser("sda_bogus", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session want ErrNotFound, got %v", err)
	}
	// purge удаляет истекшие
	if err := s.PurgeSessions(time.Now().Add(2 * time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}
}

func ptr[T any](v T) *T { return &v }
