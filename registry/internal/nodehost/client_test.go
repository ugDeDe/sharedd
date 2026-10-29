package nodehost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New("key-test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.base = srv.URL
	return c
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want error %q, got nil", code)
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	if e.Code != code {
		t.Fatalf("want code %q, got %q", code, e.Code)
	}
}

func TestAccount(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/account" || r.Method != "GET" {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer key-test" {
			t.Errorf("auth header %q", got)
		}
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("idempotency key must be absent outside create")
		}
		_, _ = w.Write([]byte(`{"id":"a1","email":"x@y.z","username":"u","balance":"142.50","api":{"test":false,"mode":"live","tier":"pro","maxServers":10,"rateLimitRpm":120}}`))
	})
	a, err := c.Account(context.Background())
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if a.ID != "a1" || a.API.MaxServers != 10 {
		t.Fatalf("bad account %+v", a)
	}
	if a.Balance != "142.50" {
		t.Fatalf("balance %q", a.Balance)
	}
}

func TestAccountBalance(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"a1","balance":"0.99"}`))
	})
	b, err := c.AccountBalance(context.Background())
	if err != nil {
		t.Fatalf("AccountBalance: %v", err)
	}
	if b.Amount != "0.99" || b.Currency != "USD" || b.Scope != "nodehost_api" {
		t.Fatalf("bad balance %+v", b)
	}
}

func TestCatalog(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/catalog/locations":
			_, _ = w.Write([]byte(`[{"name":"Germany","code":"DE","flag":"🇩🇪","priceMarkup":"1.10"}]`))
		case r.URL.Path == "/v1/catalog/os-templates":
			// Live entries carry {id,name} only; title is optional/absent.
			_, _ = w.Write([]byte(`[{"id":7,"name":"ubuntu-24.04"}]`))
		case r.URL.Path == "/v1/catalog/cpu-models" && r.URL.Query().Get("location") == "DE":
			_, _ = w.Write([]byte(`["AMD Ryzen 9 7950X"]`))
		case r.URL.Path == "/v1/catalog/cpu-models":
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`["AMD Ryzen 9 7950X"]`))
		case r.URL.Path == "/v1/catalog/disk-types":
			// Live: objects, not strings.
			_, _ = w.Write([]byte(`[{"id":2,"name":"Storage1"}]`))
		case r.URL.Path == "/v1/catalog/network-options" && r.URL.Query().Get("location") == "DE":
			// Live body captured 2026-10-17: hasAvailableNodes, not available.
			_, _ = w.Write([]byte(`{"speeds":["1gbps"],"hasAvailableNodes":true}`))
		case r.URL.Path == "/v1/catalog/limits":
			// Live shape captured 2026-10-17.
			_, _ = w.Write([]byte(`{"types":{"VPS":true,"VDS":false,"HI_CPU":false},"limits":{"cpu":{"min":1,"max":24},"ram":{"min":1,"max":64},"disk":{"min":10,"max":1000}}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	ctx := context.Background()
	if locs, err := c.Locations(ctx); err != nil || len(locs) != 1 || locs[0].PriceMarkup != "1.10" {
		t.Fatalf("Locations: %v %+v", err, locs)
	}
	if os, err := c.OSTemplates(ctx); err != nil || len(os) != 1 || os[0].ID != 7 || os[0].Title != "" {
		t.Fatalf("OSTemplates: %v %+v", err, os)
	}
	if cpu, err := c.CPUModels(ctx, "DE"); err != nil || len(cpu) != 1 || cpu[0] != "AMD Ryzen 9 7950X" {
		t.Fatalf("CPUModels(DE): %v %+v", err, cpu)
	}
	if cpu, err := c.CPUModels(ctx, ""); err != nil || len(cpu) != 1 {
		t.Fatalf("CPUModels(): %v %+v", err, cpu)
	}
	if dt, err := c.DiskTypes(ctx); err != nil || len(dt) != 1 || dt[0].ID != 2 || dt[0].Name != "Storage1" {
		t.Fatalf("DiskTypes: %v %+v", err, dt)
	}
	no, err := c.NetworkOptions(ctx, "DE")
	if err != nil || !no.HasAvailableNodes || len(no.Speeds) != 1 || no.Speeds[0] != "1gbps" {
		t.Fatalf("NetworkOptions: %v %+v", err, no)
	}
	// hasAvailableNodes:false must parse as false (no stale prelaunch "available" key).
	cFalse := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"speeds":["1gbps"],"hasAvailableNodes":false}`))
	})
	if noF, err := cFalse.NetworkOptions(ctx, "DE"); err != nil || noF.HasAvailableNodes {
		t.Fatalf("NetworkOptions(false): %v %+v", err, noF)
	}
	lim, err := c.Limits(ctx)
	if err != nil || !lim.Types["VPS"] || lim.Types["VDS"] ||
		lim.Limits.CPU.Max != 24 || lim.Limits.RAM.Min != 1 || lim.Limits.Disk.Max != 1000 {
		t.Fatalf("Limits: %v %+v", err, lim)
	}
}

func TestEstimate(t *testing.T) {
	// Exact live body captured 2026-10-17 (success is 201; 200 also accepted).
	const liveBody = `{"cores":1,"ramGb":1,"diskGb":10,"networkSpeed":"1gbps","period":"1m","periodDays":30,"monthlyPrice":2.12,"periodPrice":2.12,"geoMarkup":1.1,"levelDiscountPct":6,"apiDiscountPct":0,"currency":"USD"}`
	for _, status := range []int{201, 200} {
		c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" || r.URL.Path != "/v1/pricing/estimate" {
				t.Errorf("bad %s %s", r.Method, r.URL.Path)
			}
			var req map[string]any
			b, _ := io.ReadAll(r.Body)
			if json.Unmarshal(b, &req) != nil {
				t.Errorf("bad request body %s", b)
			}
			// Live 400 ("property X should not exist"): create-only fields must
			// never ride on estimate requests.
			for _, k := range []string{"name", "cpu", "diskType", "os"} {
				if _, ok := req[k]; ok {
					t.Errorf("estimate request must not carry %q (create-only)", k)
				}
			}
			for _, k := range []string{"cores", "ram", "disk", "networkSpeed", "period", "location"} {
				if _, ok := req[k]; !ok {
					t.Errorf("estimate request missing %q", k)
				}
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(liveBody))
		})
		e, err := c.Estimate(context.Background(), EstimateRequest{Cores: 1, RAM: 1, Disk: 10, NetworkSpeed: "1gbps", Period: "1m", Location: "DE"})
		if err != nil || e.MonthlyPrice != "2.12" || e.PeriodDays != 30 {
			t.Fatalf("Estimate(%d): %v %+v", status, err, e)
		}
	}
}

func TestInputValidationNoIO(t *testing.T) {
	var called atomic.Bool
	c := testClient(t, func(http.ResponseWriter, *http.Request) { called.Store(true) })
	ctx := context.Background()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"servers-limit-0", func() error { _, err := c.ServersPage(ctx, "", 0); return err }},
		{"servers-limit-101", func() error { _, err := c.ServersPage(ctx, "", 101); return err }},
		{"network-empty-location", func() error { _, err := c.NetworkOptions(ctx, ""); return err }},
		{"cpu-bad-location", func() error { _, err := c.CPUModels(ctx, "\x00"); return err }},
		{"estimate-zero-cores", func() error { _, err := c.Estimate(ctx, EstimateRequest{RAM: 1, Disk: 1}); return err }},
		{"estimate-bad-period", func() error {
			_, err := c.Estimate(ctx, EstimateRequest{Cores: 1, RAM: 1, Disk: 1, Period: "2m"})
			return err
		}},
		{"create-empty-name", func() error {
			_, err := c.Create(ctx, CreateRequest{CPU: "x", Cores: 1, RAM: 1, Disk: 1, DiskType: "SSD", OS: "o", Location: "DE"}, "k")
			return err
		}},
		{"create-long-name", func() error {
			_, err := c.Create(ctx, CreateRequest{Name: strings.Repeat("a", 101), CPU: "x", Cores: 1, RAM: 1, Disk: 1, DiskType: "SSD", OS: "o", Location: "DE"}, "k")
			return err
		}},
		{"create-empty-idem", func() error {
			_, err := c.Create(ctx, CreateRequest{Name: "n", CPU: "x", Cores: 1, RAM: 1, Disk: 1, DiskType: "SSD", OS: "o", Location: "DE"}, "")
			return err
		}},
		{"changeip-bad-subnet", func() error { _, err := c.ChangeIP(ctx, "srv1", 0, true); return err }},
		{"server-dot-id", func() error { _, err := c.Server(ctx, "."); return err }},
		{"server-long-id", func() error { _, err := c.Server(ctx, strings.Repeat("a", 257)); return err }},
		{"delete-empty-id", func() error { _, err := c.Delete(ctx, " "); return err }},
		{"operation-empty-id", func() error { _, err := c.Operation(ctx, ""); return err }},
		{"subnets-empty-id", func() error { _, err := c.IPSubnets(ctx, ""); return err }},
	}
	for _, tc := range cases {
		if err := tc.fn(); err == nil {
			t.Errorf("%s: want invalid_input, got nil", tc.name)
		} else {
			wantCode(t, err, "invalid_input")
		}
	}
	if called.Load() {
		t.Error("server must not be reached on invalid input")
	}
}

func TestCreate202(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/servers" {
			t.Errorf("bad %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Idempotency-Key"); got != "idem-1" {
			t.Errorf("idempotency key %q", got)
		}
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"operationId":"op1","status":"pending","type":"server.create"}`))
	})
	res, err := c.Create(context.Background(), CreateRequest{Name: "pool-node", CPU: "AMD Ryzen 9 7950X", Cores: 2, RAM: 4, Disk: 40, DiskType: "SSD NVMe", OS: "ubuntu-24.04", Location: "DE"}, "idem-1")
	if err != nil || res.OperationID != "op1" || res.Status != "pending" {
		t.Fatalf("Create: %v %+v", err, res)
	}
}

func TestCreateEmptyOperationIDRejected(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(202)
		_, _ = w.Write([]byte(`{"status":"pending","type":"server.create"}`))
	})
	_, err := c.Create(context.Background(), CreateRequest{Name: "n", CPU: "x", Cores: 1, RAM: 1, Disk: 1, DiskType: "SSD", OS: "o", Location: "DE"}, "k")
	wantCode(t, err, "invalid_response")
}

func TestOperation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/operations/op1" {
			t.Errorf("path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"op1","type":"server.create","status":"succeeded","resourceType":"server","resourceId":"srv1","error":null}`))
	})
	op, err := c.Operation(context.Background(), "op1")
	if err != nil || op.Status != "succeeded" || op.ResourceID != "srv1" || op.Error != nil {
		t.Fatalf("Operation: %v %+v", err, op)
	}
}

func TestServersPagination(t *testing.T) {
	pages := map[string]string{
		"":   `{"data":[{"id":"srv1","name":"n1"}],"nextCursor":"c2"}`,
		"c2": `{"data":[{"id":"srv2","name":"n2"}],"nextCursor":null}`,
	}
	var gotLimit []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/servers" {
			t.Errorf("path %q", r.URL.Path)
		}
		gotLimit = append(gotLimit, r.URL.Query().Get("limit"))
		body, ok := pages[r.URL.Query().Get("cursor")]
		if !ok {
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
			return
		}
		_, _ = w.Write([]byte(body))
	})
	seen := map[string]bool{}
	cursor := ""
	pagesRead := 0
	for {
		if seen[cursor] {
			t.Fatalf("repeated cursor %q — loop guard fired", cursor)
		}
		seen[cursor] = true
		page, err := c.ServersPage(context.Background(), cursor, 50)
		if err != nil {
			t.Fatalf("ServersPage(%q): %v", cursor, err)
		}
		pagesRead++
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}
	if pagesRead != 2 || len(gotLimit) != 2 {
		t.Fatalf("pages=%d limits=%v", pagesRead, gotLimit)
	}
	for _, l := range gotLimit {
		if l != "50" {
			t.Errorf("limit %q", l)
		}
	}
}

func TestServerDetailPasswordCapturedOnlyInStruct(t *testing.T) {
	payload := `{"id":"srv1","name":"n1","status":"active","password":"S3cret!"}`
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/servers/srv1" {
			t.Errorf("path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(payload))
	})
	s, err := c.Server(context.Background(), "srv1")
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	if s.Password != "S3cret!" {
		t.Fatalf("password not captured: %q", s.Password)
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	if json.Unmarshal(b, &check) != nil {
		t.Fatal("re-marshal produced invalid JSON")
	}
	if _, ok := check["password"]; ok {
		t.Fatalf("password leaked on round-trip: %s", b)
	}
	if strings.Contains(string(b), "S3cret!") {
		t.Fatalf("secret in marshaled form: %s", b)
	}
}

func TestDelete(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" || r.URL.Path != "/v1/servers/srv1" {
			t.Errorf("bad %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"message":"Server deleted","refund":"0"}`))
	})
	res, err := c.Delete(context.Background(), "srv1")
	if err != nil || res.Refund != "0" {
		t.Fatalf("Delete: %v %+v", err, res)
	}
}

func TestIPSubnets(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/servers/srv1/ip-subnets" {
			t.Errorf("path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"rangeId":18,"name":"DE-1","free":3}]`))
	})
	sub, err := c.IPSubnets(context.Background(), "srv1")
	if err != nil || len(sub) != 1 || sub[0].RangeID != 18 || sub[0].Free != 3 {
		t.Fatalf("IPSubnets: %v %+v", err, sub)
	}
}

func TestChangeIPBothVariants(t *testing.T) {
	var lastBody string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/servers/srv1/change-ip" {
			t.Errorf("bad %s %s", r.Method, r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		if r.Header.Get("Idempotency-Key") != "" {
			t.Error("change-ip must not carry an idempotency key")
		}
		_, _ = w.Write([]byte(`{"ip":"203.0.113.10","charged":"0.50"}`))
	})
	ctx := context.Background()
	res, err := c.ChangeIP(ctx, "srv1", 0, false)
	if err != nil || res.IP != "203.0.113.10" || res.Charged != "0.50" {
		t.Fatalf("ChangeIP(no subnet): %v %+v", err, res)
	}
	if lastBody != "{}" {
		t.Fatalf("body %q", lastBody)
	}
	res, err = c.ChangeIP(ctx, "srv1", 18, true)
	if err != nil || res.Charged != "0.50" {
		t.Fatalf("ChangeIP(subnet): %v %+v", err, res)
	}
	if lastBody != `{"subnetId":18}` {
		t.Fatalf("body %q", lastBody)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[int]string{
		400: "validation_failed",
		401: "unauthorized",
		402: "insufficient_funds",
		403: "forbidden",
		404: "not_found",
		409: "conflict",
		429: "rate_limited",
		500: "provider_error",
		302: "provider_error",
	}
	for status, code := range cases {
		c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"code":"raw-provider-code","message":"internal detail"}}`))
		})
		_, err := c.Account(context.Background())
		wantCode(t, err, code)
		var e *Error
		if !errors.As(err, &e) || e.HTTPStatus != status {
			t.Fatalf("status %d: got %+v", status, e)
		}
		if strings.Contains(err.Error(), "raw-provider-code") || strings.Contains(err.Error(), "internal detail") {
			t.Errorf("provider text leaked: %v", err)
		}
	}
}

func TestBusinessErrorOnHTTP200(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"error":{"code":"no_quota","message":"x"}}`))
	})
	_, err := c.Account(context.Background())
	wantCode(t, err, "provider_error")
}

func TestNonJSONErrorBodyKeepsStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   string
	}{
		{502, `<html>oops</html>`, "provider_error"},
		{400, `oops`, "validation_failed"},
	}
	for _, tc := range cases {
		c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		})
		_, err := c.Account(context.Background())
		wantCode(t, err, tc.code)
		var e *Error
		if !errors.As(err, &e) || e.HTTPStatus != tc.status {
			t.Fatalf("status %d: got %+v", tc.status, e)
		}
	}
}

func TestOperationFailedCarriesSanitizedCode(t *testing.T) {
	for _, tc := range []struct{ name, wire, code string }{
		{"object", `{"code":"insufficient_funds","message":"password=DO-NOT-LEAK"}`, "insufficient_funds"},
		{"string", `"Insufficient funds: password=DO-NOT-LEAK"`, "insufficient_funds"},
		{"message", `{"message":"Insufficient balance 0 < 9.90; token=DO-NOT-LEAK"}`, "insufficient_funds"},
		{"unknown-string", `"password=DO-NOT-LEAK"`, "provider_error"},
		{"unknown-code", `{"code":"DO-NOT-LEAK"}`, "provider_error"},
		{"unicode-code", `{"code":"` + strings.Repeat("界", 40) + `"}`, "provider_error"},
		{"null", `null`, ""},
		{"schema", `[]`, "invalid_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":"op1","status":"failed","resourceId":"","error":` + tc.wire + `}`))
			})
			op, err := c.Operation(context.Background(), "op1")
			if tc.code == "invalid_response" {
				wantCode(t, err, tc.code)
				return
			}
			if err != nil || op.Status != "failed" {
				t.Fatalf("failed outcome lost: op=%+v err=%v", op, err)
			}
			if tc.code == "" {
				if op.Error != nil {
					t.Fatal("null error must remain nil")
				}
			} else if op.Error == nil || op.Error.Code != tc.code {
				t.Fatalf("safe code: %+v, want %s", op.Error, tc.code)
			}
			b, err := json.Marshal(op)
			if err != nil || strings.Contains(string(b), "DO-NOT-LEAK") {
				t.Fatalf("provider secret escaped: %s err=%v", b, err)
			}
		})
	}
}

func TestInvalidJSON(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`this is not json`))
	})
	_, err := c.Account(context.Background())
	wantCode(t, err, "invalid_json")
}

func TestOversizedBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", maxBody+1)))
	})
	_, err := c.Account(context.Background())
	wantCode(t, err, "response_too_large")
}

func TestRedirectNotFollowed(t *testing.T) {
	secondHit := atomic.Bool{}
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit.Store(true)
		_, _ = w.Write([]byte(`{"id":"evil"}`))
	}))
	t.Cleanup(second.Close)
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", second.URL+"/v1/account")
		w.WriteHeader(http.StatusFound)
	})
	_, err := c.Account(context.Background())
	wantCode(t, err, "provider_error")
	if secondHit.Load() {
		t.Error("redirect target must not be requested (no credential forward)")
	}
	var e *Error
	if !errors.As(err, &e) || e.HTTPStatus != 302 {
		t.Fatalf("want last response 302, got %+v", e)
	}
}

func TestContextCanceled(t *testing.T) {
	c := testClient(t, func(http.ResponseWriter, *http.Request) { time.Sleep(300 * time.Millisecond) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Account(ctx)
	wantCode(t, err, "request_canceled")
}

func TestTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	c, err := New("key-test")
	if err != nil {
		t.Fatal(err)
	}
	c.base = srv.URL
	srv.Close() // connection refused from here on
	_, err = c.Account(context.Background())
	wantCode(t, err, "transport_error")
}
