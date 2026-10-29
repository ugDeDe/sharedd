package control

// §7.5: секреты Beget/SSH не появляются в ответах API (проверка сериализации).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBegetLinkViewHasNoSecrets(t *testing.T) {
	s := openTest(t)
	u := mustUser(t, s, "bob", 5)
	if err := s.UpsertBegetLink(BegetLink{
		UserID: u.ID, LoginEnc: "ENC-LOGIN", PasswordEnc: "ENC-PASSWORD",
		SSHPub: "ssh-ed25519 AAAA", SSHPrivEnc: "ENC-PRIVATE-KEY", SSHKeyID: 7,
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.MarkBegetLinkVerified(u.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	view, err := s.BegetLinkView(u.ID)
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	blob, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(blob)
	for _, secret := range []string{"ENC-LOGIN", "ENC-PASSWORD", "ENC-PRIVATE-KEY"} {
		if strings.Contains(out, secret) {
			t.Fatalf("view leaks secret %q: %s", secret, out)
		}
	}
	if !view.Verified || view.SSHPub != "ssh-ed25519 AAAA" {
		t.Fatalf("view lost public fields: %+v", view)
	}
}

func TestInternalBegetLinkJSONHasNoSecrets(t *testing.T) {
	l := BegetLink{UserID: 1, LoginEnc: "SECRET-L", PasswordEnc: "SECRET-P", SSHPrivEnc: "SECRET-K", SSHPub: "ssh-ed25519 X"}
	blob, _ := json.Marshal(l)
	out := string(blob)
	for _, secret := range []string{"SECRET-L", "SECRET-P", "SECRET-K"} {
		if strings.Contains(out, secret) {
			t.Fatalf("internal link leaks %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "ssh-ed25519 X") {
		t.Fatalf("public key must remain: %s", out)
	}
}

func TestOperationJSONOmitsRequest(t *testing.T) {
	op := Operation{ID: 1, Kind: OpKindCreate, State: OpConfirmed, RequestJSON: `{"password":"TOP-SECRET"}`}
	blob, _ := json.Marshal(op)
	if strings.Contains(string(blob), "TOP-SECRET") {
		t.Fatalf("operation leaks request_json: %s", blob)
	}
}

// §6: переменные профиля — таблица name/default/description/secret; дефолты
// применяются, пользователь может добавлять сверх объявленных; запрещённые
// имена, размеры и число переменных ограничены.
func TestProfileVarsMergeAndLimits(t *testing.T) {
	schema := `{"vars":[{"name":"testvar","default":"dv"},{"name":"authkey","secret":true}]}`
	merged, err := MergeProfileVars(schema, map[string]any{"extra": "own"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged["testvar"] != "dv" || merged["extra"] != "own" {
		t.Fatalf("merged = %+v", merged)
	}
	// override wins over default
	merged, _ = MergeProfileVars(schema, map[string]any{"testvar": "ov"})
	if merged["testvar"] != "ov" {
		t.Fatalf("override must win: %+v", merged)
	}
	if names := SecretVarNames(schema); len(names) != 1 || names[0] != "authkey" {
		t.Fatalf("secret names = %v", names)
	}

	// нелегальные имена отклоняются на сохранении и перед запуском.
	for _, bad := range []string{"bad-name", "1bad", "with space"} {
		if err := ValidateProfileVarsSchema(`{"vars":[{"name":"` + bad + `"}]}`); err == nil {
			t.Fatalf("schema with %q must be rejected at save", bad)
		}
		if err := ValidateExtraVars(map[string]any{bad: "x"}); err == nil {
			t.Fatalf("var %q must be rejected before run", bad)
		}
	}
	// value > 4096 and count > 64
	if err := ValidateExtraVars(map[string]any{"v": strings.Repeat("a", MaxVarValueLen+1)}); err == nil {
		t.Fatal("oversized value must be rejected")
	}
	big := map[string]any{}
	for i := 0; i <= MaxExtraVars; i++ {
		big[fmt.Sprintf("v%d", i)] = "x"
	}
	if err := ValidateExtraVars(big); err == nil {
		t.Fatal("too many vars must be rejected")
	}
	if err := ValidateExtraVars(map[string]any{"bad-name": "x"}); err == nil {
		t.Fatal("illegal name must be rejected")
	}
	if err := ValidateExtraVars(map[string]any{"ok": "x"}); err != nil {
		t.Fatalf("valid vars rejected: %v", err)
	}
}
