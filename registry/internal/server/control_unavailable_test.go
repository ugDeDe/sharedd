package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// 503 control_unavailable: код неизменен, текст — понятный русский, старого
// английского «control db is unavailable» больше нет. Окно перезапуска
// (control ещё не открыт) и сломанная БД дают один и тот же user-facing текст.
func TestControlUnavailableMessage(t *testing.T) {
	// окно перезапуска: users.enabled=true, openControl ещё не вызван → nil.
	restart := newTestRegistry(t)
	restart.cfg.Users.Enabled = boolPtr(true)
	restart.cfg.Users.LegacyMaxNodes = 100000
	rec := registerCall(t, restart, "node-a1b2c", "1.2.3.4", "sda_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("restart window: want 503, got %d %s", rec.Code, rec.Body.String())
	}
	var m map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["error"] != "control_unavailable" {
		t.Fatalf("code = %q, want control_unavailable", m["error"])
	}
	if m["message"] != controlUnavailableMsg {
		t.Fatalf("message = %q, want %q", m["message"], controlUnavailableMsg)
	}
	// contract §1 фиксирует единый текст «Хранилище пользователей недоступно…»;
	// рестарт не утверждается без фактов, поэтому старой формулировки быть не должно.
	if !strings.Contains(m["message"], "Хранилище пользователей недоступно") || strings.Contains(m["message"], "control db is unavailable") {
		t.Fatalf("message must be the Russian explanation, got %q", m["message"])
	}

	// сломанная БД → тот же текст.
	broken := newTestRegistry(t)
	broken.cfg.Users.Enabled = boolPtr(true)
	broken.cfg.Users.LegacyMaxNodes = 100000
	broken.cfg.Database.ControlDB = filepath.Join(t.TempDir(), "no-such-dir", "control.db")
	broken.openControl()
	if broken.control != nil {
		t.Fatal("control must be nil after failed open")
	}
	rec = registerCall(t, broken, "node-a1b2c", "1.2.3.4", "sda_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("broken db: want 503, got %d", rec.Code)
	}
	m = map[string]string{}
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["message"] != controlUnavailableMsg {
		t.Fatalf("broken-db message = %q, want same Russian text", m["message"])
	}

	// рабочий store никогда не отдаёт control_unavailable.
	ok := newUsersTestRegistry(t)
	rec = registerCall(t, ok, "node-a1b2c", "1.2.3.4", "sda_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if rec.Code == http.StatusServiceUnavailable && strings.Contains(rec.Body.String(), "control_unavailable") {
		t.Fatalf("working store must not return control_unavailable: %d %s", rec.Code, rec.Body.String())
	}
}
