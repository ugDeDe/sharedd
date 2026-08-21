package main

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// newTestRegistryWithConfigFile — как newTestRegistry, но с настоящим TOML-файлом,
// чтобы persistConfigLocked имел куда писать.
func newTestRegistryWithConfigFile(t *testing.T) *Registry {
	t.Helper()
	r := newTestRegistry(t)
	r.cfg.PanelEnabled = true
	r.cfg.Panel.Token = "admintoken"
	dir := t.TempDir()
	r.cfg.configPath = filepath.Join(dir, "registry.toml")
	r.cfg.SharedProxy.TLSDomain = "www.old-sni.com"
	r.cfg.SharedProxy.Users = map[string]string{"alice": "0123456789abcdef0123456789abcdef"}
	r.cfg.Cloudflare.APIToken = "tok-old"
	r.cfg.Cloudflare.ZoneID = "zone-old"
	r.cfg.Cloudflare.Domains = []string{"mtp1.example.com"}
	r.cfg.Cloudflare.DNSTTL = 60
	r.cfg.NodeDefaults.HeartbeatMs = 15000
	r.cfg.NodeDefaults.GlobalpingMs = 300000
	r.cfg.NodeDefaults.MetricsMs = 60000
	r.cfg.NodeDefaults.SyncMs = 60000
	return r
}

func putConfig(t *testing.T, r *Registry, body string, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/panel/api/config", strings.NewReader(body))
	if token != "-" {
		if token == "" {
			token = "admintoken"
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	r.handlePutConfig(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func getNodeFacingConfig(t *testing.T, r *Registry) sharedConfigResponse {
	t.Helper()
	mux := r.buildMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out sharedConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Hot-apply: SNI/пользователи применяются немедленно и видны нодам в GET /config.
func TestConfigEditorHotApply(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)

	rec, resp := putConfig(t, r, `{"shared_proxy":{"tls_domain":"www.new-sni.com","port":8443,"users":{
		"alice":"0123456789abcdef0123456789abcdef",
		"bob":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT config: %d %s", rec.Code, rec.Body.String())
	}
	if persisted, _ := resp["persisted"].(bool); !persisted {
		t.Fatalf("expected persisted=true, got %v", resp)
	}

	// ноды сразу видят новые значения
	nc := getNodeFacingConfig(t, r)
	if nc.TLSDomain != "www.new-sni.com" {
		t.Fatalf("tls_domain not hot-applied: %q", nc.TLSDomain)
	}
	if nc.ProxyPort != 8443 {
		t.Fatalf("proxy_port not hot-applied: %d", nc.ProxyPort)
	}
	if _, ok := nc.Users["bob"]; !ok || len(nc.Users) != 2 {
		t.Fatalf("users not hot-applied: %+v", nc.Users)
	}

	// событие аудита
	if !hasEvent(eventTypes(r), EventConfigChanged) {
		t.Fatalf("config_changed event expected, got %v", eventTypes(r))
	}

	// TOML персистится и парсится обратно с теми же значениями
	data, err := os.ReadFile(r.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed RegistryConfig
	if err := toml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("persisted TOML must parse: %v\n%s", err, data)
	}
	if parsed.SharedProxy.TLSDomain != "www.new-sni.com" || parsed.SharedProxy.Port != 8443 || parsed.SharedProxy.Users["bob"] == "" {
		t.Fatalf("persisted TOML wrong: %+v", parsed.SharedProxy)
	}
	//.bak появляется при повторной записи
	putConfig(t, r, `{"node_defaults":{"heartbeat_ms":20000,"globalping_ms":300000,"metrics_ms":60000,"sync_ms":60000}}`, "")
	if _, err := os.Stat(r.cfg.configPath + ".bak"); err != nil {
		t.Fatal("expected.bak after second write")
	}
	nc = getNodeFacingConfig(t, r)
	if nc.Intervals.HeartbeatMs != 20000 {
		t.Fatalf("node_defaults not applied: %+v", nc.Intervals)
	}
}

func TestConfigEditorSharedProxyFieldsSaveIndependently(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	originalUsers := maps.Clone(r.cfg.SharedProxy.Users)

	rec, _ := putConfig(t, r, `{"shared_proxy":{"tls_domain":"sni-only.example.com"}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("SNI-only PUT: %d %s", rec.Code, rec.Body.String())
	}
	if r.cfg.SharedProxy.TLSDomain != "sni-only.example.com" || !maps.Equal(r.cfg.SharedProxy.Users, originalUsers) {
		t.Fatalf("SNI save changed users: domain=%q users=%v", r.cfg.SharedProxy.TLSDomain, r.cfg.SharedProxy.Users)
	}

	rec, _ = putConfig(t, r, `{"shared_proxy":{"users":{"bob":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("users-only PUT: %d %s", rec.Code, rec.Body.String())
	}
	if r.cfg.SharedProxy.TLSDomain != "sni-only.example.com" || len(r.cfg.SharedProxy.Users) != 1 || r.cfg.SharedProxy.Users["bob"] == "" {
		t.Fatalf("users save changed SNI or users incorrectly: domain=%q users=%v", r.cfg.SharedProxy.TLSDomain, r.cfg.SharedProxy.Users)
	}
}

func TestConfigEditorValidation(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)

	bad := []string{
		`{"shared_proxy":{"tls_domain":"no dots","users":{"a":"0123456789abcdef0123456789abcdef"}}}`,
		`{"shared_proxy":{"tls_domain":"ok.com","unexpected":true}}`,
		`{"shared_proxy":{"tls_domain":"ok.com","users":{"a":"tooshort"}}}`,
		`{"shared_proxy":{"tls_domain":"ok.com","users":{}}}`,
		`{"cloudflare":{"api_token":"x","zone_id":"z","domains":["not a hostname!"],"dns_ttl":60,"proxied":false}}`,
		`{"cloudflare":{"api_token":"x","zone_id":"z","domains":["ok.com"],"dns_ttl":30,"proxied":false}}`,
		`{"node_defaults":{"heartbeat_ms":10,"globalping_ms":300000,"metrics_ms":60000,"sync_ms":60000}}`,
		`{"panel":{"token":"x","events_max":5}}`,
		`{"unknown_section":{}}`,
	}
	for _, body := range bad {
		rec, _ := putConfig(t, r, body, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for %s, got %d", body, rec.Code)
		}
	}

	// валидный PUT меняет ровно одну секцию
	rec, resp := putConfig(t, r, `{"panel":{"token":"rotated-panel-token","events_max":777}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("valid PUT: %d %s", rec.Code, rec.Body.String())
	}
	if changed, _ := resp["changed"].([]any); len(changed) != 1 {
		t.Fatalf("only panel section must change, got %v", changed)
	}

	// новый токен работает, старый — нет (hot-apply авторизации)
	req := httptest.NewRequest(http.MethodGet, "/panel/api/overview", nil)
	if r.panelAuthorized(req) {
		t.Fatal("no token must NOT pass (token rotated to non-empty)")
	}
	req.Header.Set("Authorization", "Bearer admintoken")
	if r.panelAuthorized(req) {
		t.Fatal("old token must NOT pass after rotation")
	}
	req.Header.Set("Authorization", "Bearer rotated-panel-token")
	if !r.panelAuthorized(req) {
		t.Fatal("new token must pass")
	}
}

func TestConfigEditorCloudflareSwap(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)

	// CF-клиент заменяется; мастера нет → push честно сообщает, что нечего писать
	rec, resp := putConfig(t, r, `{"cloudflare":{"api_token":"tok-new","zone_id":"zone-new","domains":["a.example.com","b.example.com"],"dns_ttl":120,"proxied":true}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	push, _ := resp["dns_push"].(map[string]any)
	if push == nil {
		t.Fatal("dns_push block expected on cloudflare change")
	}
	if errs, _ := push["errors"].([]any); len(errs) == 0 {
		t.Fatal("expected 'no active master' error, got none")
	}

	r.cfgMu.RLock()
	defer r.cfgMu.RUnlock()
	if r.cfg.Cloudflare.ZoneID != "zone-new" || len(r.cfg.Cloudflare.Domains) != 2 || !r.cfg.Cloudflare.Proxied || r.cfg.Cloudflare.DNSTTL != 120 {
		t.Fatalf("cf not applied: %+v", r.cfg.Cloudflare)
	}
	if r.cf == nil {
		t.Fatal("cf client must be rebuilt")
	}
}

// GET /panel/api/config отдаёт и редактируемые секции, и статику; закрыт токеном.
func TestConfigEditorGet(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)

	mux := r.buildMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// без токена — 401
	resp, err := http.Get(srv.URL + "/panel/api/config")
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %v (%v)", resp.StatusCode, err)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/panel/api/config", nil)
	req.Header.Set("Authorization", "Bearer admintoken")
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %v (%v)", resp.StatusCode, err)
	}
	var data struct {
		Config         editableConfig     `json:"config"`
		Static         staticConfigInfo   `json:"static"`
		NodeOnboarding nodeOnboardingInfo `json:"node_onboarding"`
	}
	json.NewDecoder(resp.Body).Decode(&data)
	resp.Body.Close()

	if data.Config.SharedProxy == nil || data.Config.SharedProxy.TLSDomain != "www.old-sni.com" {
		t.Fatalf("GET config: %+v", data.Config.SharedProxy)
	}
	if data.Static.ReportFreshnessMin != 0 && data.Static.HTTPAddr == "" {
		t.Fatalf("static block expected to carry http addr etc: %+v", data.Static)
	}
	if data.NodeOnboarding.Token != r.cfg.Security.NodeToken {
		t.Fatalf("node onboarding token mismatch: %+v", data.NodeOnboarding)
	}
	if data.NodeOnboarding.InstallCommand != "" {
		t.Fatalf("request Host must not be used as onboarding URL: %+v", data.NodeOnboarding)
	}
}

func TestConfigEditorGetNodeOnboardingUsesConfiguredPublicURL(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	r.cfg.Security.NodeToken = "node-token-'quoted"
	r.cfg.Panel.PublicURL = "https://registry.example.com"
	req := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	req.Host = "attacker.example"
	req.Header.Set("Authorization", "Bearer admintoken")
	rec := httptest.NewRecorder()
	r.handleGetConfig(rec, req)

	var data struct {
		NodeOnboarding nodeOnboardingInfo `json:"node_onboarding"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.NodeOnboarding.RegistryURL != "https://registry.example.com" || !strings.Contains(data.NodeOnboarding.InstallCommand, "--registry='https://registry.example.com'") || strings.Contains(data.NodeOnboarding.InstallCommand, "attacker.example") {
		t.Fatalf("unexpected onboarding DTO: %+v", data.NodeOnboarding)
	}
	if !strings.Contains(data.NodeOnboarding.InstallCommand, `'node-token-'"'"'quoted'`) {
		t.Fatalf("token is not shell-quoted: %q", data.NodeOnboarding.InstallCommand)
	}
}

// persistConfigLocked: если файл недоступен — APPLY всё равно происходит,
// но ответ честно говорит persisted=false (рантайм не обваливается).
func TestConfigPersistFailureStillApplies(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	r.cfg.configPath = filepath.Join(t.TempDir(), "no-such-dir", "registry.toml")

	rec, resp := putConfig(t, r, `{"shared_proxy":{"tls_domain":"www.runtime-only.com","users":{"a":"0123456789abcdef0123456789abcdef"}}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT must succeed even without persistence: %d", rec.Code)
	}
	if persisted, _ := resp["persisted"].(bool); persisted {
		t.Fatal("persisted must be false for unwritable path")
	}
	if resp["persist_warning"] == nil {
		t.Fatal("persist_warning expected")
	}
	nc := getNodeFacingConfig(t, r)
	if nc.TLSDomain != "www.runtime-only.com" {
		t.Fatal("runtime apply must happen regardless of persistence")
	}
	if time.Since(r.startedAt) < 0 {
		t.Fatal("sanity")
	}
}

// #2: число GP-попыток карантина правится из настроек панели —
// применяется на лету и персистится в TOML.
func TestConfigEditorQuarantineAttempts(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	r.cfg.QuarantineAttempts = 3
	r.cfg.Healthcheck.QuarantineAttempts = 3

	rec, resp := putConfig(t, r, `{"healthcheck":{"quarantine_attempts":5}}`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT healthcheck: %d %s", rec.Code, rec.Body.String())
	}
	if persisted, _ := resp["persisted"].(bool); !persisted {
		t.Fatalf("expected persisted=true, got %v", resp)
	}
	if r.cfg.QuarantineAttempts != 5 || r.cfg.Healthcheck.QuarantineAttempts != 5 {
		t.Fatalf("hot-apply broken: %+v/%+v", r.cfg.QuarantineAttempts, r.cfg.Healthcheck.QuarantineAttempts)
	}
	data, err := os.ReadFile(r.cfg.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed RegistryConfig
	if err := toml.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("persisted TOML must parse: %v\n%s", err, data)
	}
	if parsed.Healthcheck.QuarantineAttempts != 5 {
		t.Fatalf("persisted TOML wrong: %+v", parsed.Healthcheck)
	}

	// GET отдаёт секцию в настройки
	greq := httptest.NewRequest(http.MethodGet, "/panel/api/config", nil)
	greq.Header.Set("Authorization", "Bearer admintoken")
	grec := httptest.NewRecorder()
	r.handleGetConfig(grec, greq)
	var got struct {
		Config struct {
			Healthcheck struct {
				QuarantineAttempts int `json:"quarantine_attempts"`
			} `json:"healthcheck"`
		} `json:"config"`
	}
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil || got.Config.Healthcheck.QuarantineAttempts != 5 {
		t.Fatalf("GET config must expose healthcheck.quarantine_attempts=5: %s err=%v", grec.Body.String(), err)
	}

	// валидация: 0 и 21 отклоняются
	for _, bad := range []string{
		`{"healthcheck":{"quarantine_attempts":0}}`,
		`{"healthcheck":{"quarantine_attempts":21}}`,
	} {
		if rec, _ := putConfig(t, r, bad, ""); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s must be 400, got %d", bad, rec.Code)
		}
	}
	if r.cfg.QuarantineAttempts != 5 {
		t.Fatal("rejected values must not be applied")
	}
}

func TestConfigEditorGlobalpingValidity(t *testing.T) {
	r := newTestRegistryWithConfigFile(t)
	r.cfg.NodeDefaults.GlobalpingMs = 5 * 60 * 1000
	r.cfg.Healthcheck.GlobalpingValidityMin = 15
	r.cfg.GlobalpingValidityTTL = 15 * time.Minute

	rec, _ := putConfig(t, r, `{"healthcheck":{"quarantine_attempts":3,"globalping_validity_min":20}}`, "")
	if rec.Code != http.StatusOK || r.cfg.GlobalpingValidityTTL != 20*time.Minute {
		t.Fatalf("GP validity hot-apply failed: code=%d ttl=%s body=%s", rec.Code, r.cfg.GlobalpingValidityTTL, rec.Body.String())
	}
	for _, body := range []string{
		`{"healthcheck":{"quarantine_attempts":3,"globalping_validity_min":5}}`,
		`{"node_defaults":{"heartbeat_ms":15000,"globalping_ms":1200000,"metrics_ms":60000,"sync_ms":60000}}`,
	} {
		rec, _ := putConfig(t, r, body, "")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("GP validity/interval conflict must be rejected: code=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}
