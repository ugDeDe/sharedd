package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadyzReflectsMandatoryLocalDependencies(t *testing.T) {
	r := newTestRegistry(t)
	mux := r.buildMux()

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy mode must be ready: %d %s", rec.Code, rec.Body)
	}

	// User mode without an opened control DB / master key is not ready: the
	// cabinet cannot authenticate or seal install secrets.
	r.cfg.Users.Enabled = boolPtr(true)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("user mode without control must be 503, got %d %s", rec.Code, rec.Body)
	}
}
