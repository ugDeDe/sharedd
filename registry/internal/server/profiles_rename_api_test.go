package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// PUT /panel/api/admin/profiles/{id} с непустым name переименовывает профиль:
// это метаданные (версий не плодит), коллизия без учёта регистра → 409
// duplicate_name, неизменное имя — no-op.
func TestAdminProfileRenameHTTP(t *testing.T) {
	r, _ := newCabinetRegistry(t)
	r.cfg.Panel.Token = testPanelToken

	create := func(name string) profileAdminJSON {
		t.Helper()
		rec := adminCall(t, r, http.MethodPost, "/panel/api/admin/profiles", testPanelToken,
			jsonBody(t, map[string]any{"name": name, "steps": "# s\necho hi\n"}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, rec.Code, rec.Body.String())
		}
		var p profileAdminJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	create("ren-a")
	b := create("ren-b")

	// переименование успешно, видно в листе, версий не прибавилось.
	rec := adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", b.ID), testPanelToken,
		jsonBody(t, map[string]any{"name": "ren-c"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	var got profileAdminJSON
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "ren-c" || got.Steps != "# s\necho hi\n" {
		t.Fatalf("rename must keep the single content row: %+v", got)
	}
	rec = adminCall(t, r, http.MethodGet, "/panel/api/admin/profiles", testPanelToken, "")
	if body := rec.Body.String(); !strings.Contains(body, `"ren-c"`) || strings.Contains(body, `"ren-b"`) || !strings.Contains(body, `"ren-a"`) {
		t.Fatalf("list must show ren-c+ren-a and not ren-b: %s", body)
	}

	// коллизия без учёта регистра → 409 duplicate_name.
	rec = adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", b.ID), testPanelToken,
		jsonBody(t, map[string]any{"name": "REN-A"}))
	if rec.Code != http.StatusConflict || errCode(t, rec) != "duplicate_name" {
		t.Fatalf("duplicate rename: want 409 duplicate_name, got %d %s", rec.Code, rec.Body.String())
	}

	// неизменное имя — no-op (200, имя и версии те же).
	rec = adminCall(t, r, http.MethodPut, fmt.Sprintf("/panel/api/admin/profiles/%d", b.ID), testPanelToken,
		jsonBody(t, map[string]any{"name": "ren-c"}))
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op rename: %d %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Name != "ren-c" || got.Steps != "# s\necho hi\n" {
		t.Fatalf("no-op result = %+v", got)
	}
}
