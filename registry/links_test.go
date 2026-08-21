package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLinksPageRouteAndHeaders(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	mux := r.buildMux()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/links", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /links: got %d", rec.Code)
	}
	body := rec.Body.String()
	for _, marker := range []string{"прокси-ссылки", "fetch(\"/proxylinks\"", "navigator.clipboard.writeText", "QR"} {
		if !strings.Contains(body, marker) {
			t.Errorf("embedded links page missing marker %q", marker)
		}
	}
	if strings.Contains(body, ".innerHTML") {
		t.Fatal("links page must not render API values through innerHTML")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("unexpected CSP: %q", csp)
	}
}

func TestPublicHTMLRoutesUseSharedHeaders(t *testing.T) {
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	mux := r.buildMux()
	for _, path := range []string{"/statistics", "/dashboard", "/links"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s: got %d", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q", got)
			}
		})
	}
}

func TestPublicPageEmbedMarkers(t *testing.T) {
	checks := []struct {
		name string
		html []byte
		want []string
	}{
		{"statistics", statsHTML, []string{"Публичная статистика", "status-pill", "href=\"/links\"", "theme-toggle"}},
		{"dashboard", dashboardHTML, []string{"Дашборд блокировок", "metric-seg", "range-seg", "chart-title"}},
		{"links", linksHTML, []string{"fetch(\"/proxylinks\"", "textContent", "прокси-ссылки", "qr-modal"}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			body := string(tc.html)
			for _, marker := range tc.want {
				if !strings.Contains(body, marker) {
					t.Errorf("missing embed marker %q", marker)
				}
			}
		})
	}
}
