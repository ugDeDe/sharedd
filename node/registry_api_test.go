package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegistryRequestAddsBearerToken(t *testing.T) {
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/register"},
		{http.MethodPost, "/heartbeat"},
		{http.MethodPost, "/report"},
		{http.MethodPost, "/retire"},
		{http.MethodGet, "/config"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if got := req.Header.Get("Authorization"); got != "Bearer shared-secret" {
					t.Errorf("Authorization = %q", got)
				}
				if tc.method == http.MethodPost && req.Header.Get("Content-Type") != "application/json" {
					t.Errorf("Content-Type = %q", req.Header.Get("Content-Type"))
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			cfg := &NodeConfig{}
			cfg.Registry.URL = srv.URL
			cfg.Registry.Token = "shared-secret"
			var body io.Reader
			if tc.method == http.MethodPost {
				body = strings.NewReader(`{}`)
			}
			resp, err := registryRequest(srv.Client(), cfg, tc.method, tc.path, body)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
		})
	}
}
