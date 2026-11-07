package mtprotocheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCheckSendsKeyAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/check" || r.URL.Query().Get("timeout") != "15" || r.Header.Get("X-API-Key") != "rf_k" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["server"] != "1.2.3.4" || body["port"] != float64(443) || body["secret"] != "eeabc" {
			http.Error(w, "bad body", http.StatusUnprocessableEntity)
			return
		}
		_, _ = w.Write([]byte(`{"server":"1.2.3.4","port":443,"secret_type":"faketls","status":"DATA_CUT",
			"local":{"label":"RU","handshake":true,"data":false,"tcp":true},"remote":{"label":"NL","handshake":true,"data":true,"tcp":true}}`))
	}))
	defer srv.Close()
	res, err := New(srv.URL, "rf_k").Check(context.Background(), "1.2.3.4", 443, "eeabc", 15)
	if err != nil {
		t.Fatal(err)
	}
	if ok, conclusive := res.Verdict(); !ok || !conclusive || res.Status != StatusDataCut {
		t.Fatalf("only RKN_BLOCK is negative: %+v", res)
	}
	if s := res.Summary(); !strings.Contains(s, "RU tcp+ hs+ data-") || !strings.Contains(s, "NL tcp+ hs+ data+") {
		t.Fatalf("summary: %s", s)
	}
}

func TestCheckRateLimitIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "20")
		http.Error(w, `{"error":"rate limit"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "rf_k").Check(context.Background(), "1.2.3.4", 443, "eeabc", 15)
	if err == nil || !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "Retry-After 20") {
		t.Fatalf("429 must be an error with Retry-After: %v", err)
	}
}
