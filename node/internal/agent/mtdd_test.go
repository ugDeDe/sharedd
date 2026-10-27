package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

const mtddTestToken = "0123456789abcdef0123456789abcdef"

// mtddFixture поднимает fake mtdd API и пишет config.json/credentials.env/users.json.
type mtddFixture struct {
	dir     string
	cfgPath string
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []string
	nodeCfg *config.NodeConfig
}

func newMtddFixture(t *testing.T, users string) *mtddFixture {
	t.Helper()
	f := &mtddFixture{dir: t.TempDir()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+mtddTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path+" "+string(body))
		f.mu.Unlock()
		if r.URL.Path == "/metrics" {
			_, _ = io.WriteString(w, "mtdd_me_pool_ready{dc=\"2\"} 3\n")
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)

	host, portStr, _ := strings.Cut(strings.TrimPrefix(f.srv.URL, "http://"), ":")
	f.cfgPath = filepath.Join(f.dir, "config.json")
	usersPath := filepath.Join(f.dir, "users.json")
	cfgJSON := `{"proxy_port":443,"control_host":"` + host + `","control_port":` + portStr +
		`,"tls_domain":"old.example","users_path":"` + usersPath + `","workers":0}`
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(f.cfgPath, []byte(cfgJSON), 0644))
	must(os.WriteFile(usersPath, []byte(users), 0600))
	creds := filepath.Join(f.dir, "credentials.env")
	must(os.WriteFile(creds, []byte("MTDD_API_TOKEN="+mtddTestToken+"\n"), 0600))

	oldCreds := mtddCredsPath
	mtddCredsPath = creds
	t.Cleanup(func() { mtddCredsPath = oldCreds })

	f.nodeCfg = &config.NodeConfig{}
	f.nodeCfg.Telemt.ConfigPath = f.cfgPath
	return f
}

func (f *mtddFixture) callLog() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "\n")
}

func TestLoadTelemtConfigFromMtddJSON(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	tc, err := loadTelemtConfig(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if telemtProxyPort(tc) != 443 {
		t.Fatalf("proxy port = %d", telemtProxyPort(tc))
	}
	u, err := metricsURLForConfig(f.nodeCfg)
	if err != nil || u != f.srv.URL+"/metrics" {
		t.Fatalf("metrics url = %q err=%v, want %s/metrics", u, err, f.srv.URL)
	}
}

func TestMtddMetricsRequireBearerAndTranslate(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	u, _ := metricsURLForConfig(f.nodeCfg)
	resp, err := metricsGet(http.DefaultClient, f.nodeCfg, u)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authorized scrape failed: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if err := waitMetricsReady(f.nodeCfg, 0); err != nil {
		t.Fatalf("waitMetricsReady: %v", err)
	}

	samples := parsePrometheusSamples(`mtdd_me_pool_ready{dc="2"} 3
mtdd_me_pool_ready{dc="4"} 2
mtdd_me_reconnects_total{dc="2"} 5
mtdd_user_unique_ips_active{user="u1"} 4
mtdd_user_connections_active{user="u1"} 6
mtdd_user_payload_bytes_received_total{user="u1",direction="c2s"} 100
mtdd_user_payload_bytes_received_total{user="u1",direction="s2c"} 900
`)
	snap, values := buildMetricsSnapshot(translateMtddSamples(samples), map[string]string{"u1": "x"})
	if values[healthMetricName] != 5 {
		t.Fatalf("health metric = %v, want 5 (sum of pool_ready)", values[healthMetricName])
	}
	if snap[uniqueIPsMetricName] != 4 || snap[userConnsMetricName] != 6 {
		t.Fatalf("clients not translated: %v", snap)
	}
	if snap[trafficIngressMetricName] != 100 || snap[trafficEgressMetricName] != 900 {
		t.Fatalf("traffic not translated: %v", snap)
	}
	// без серии пула ключевая метрика отсутствует, а не равна нулю
	_, v2 := buildMetricsSnapshot(translateMtddSamples(parsePrometheusSamples("mtdd_uptime_seconds 1\n")), nil)
	if _, ok := v2[healthMetricName]; ok {
		t.Fatal("health metric must be absent without mtdd_me_pool_ready")
	}
}

func TestMtddMetricsWithoutTokenFails(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	if err := os.WriteFile(mtddCredsPath, []byte("# empty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := waitMetricsReady(f.nodeCfg, 0); err == nil {
		t.Fatal("expected failure without API token")
	}
}

func TestSyncMtddUsers(t *testing.T) {
	a := strings.Repeat("a", 32)
	b := strings.Repeat("b", 32)
	c := strings.Repeat("c", 32)
	f := newMtddFixture(t, `{"users":[`+
		`{"name":"same","secret":"`+a+`","enabled":true},`+
		`{"name":"rot","secret":"`+a+`","enabled":true},`+
		`{"name":"off","secret":"`+b+`","enabled":false}]}`)
	mc, err := loadMtddConfig(f.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	n, err := syncMtddUsers(mc, map[string]string{
		"same": a, "rot": strings.ToUpper(b), "off": b, "new": c, "bad": "zz",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("changes = %d, want 3 (rotate, enable, create)", n)
	}
	log := f.callLog()
	for _, want := range []string{
		"POST /v1/users/rot/rotate-secret",
		"PATCH /v1/users/off",
		`POST /v1/users {"name":"new","secret":"` + c + `"}`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("missing call %q in:\n%s", want, log)
		}
	}
	if strings.Contains(log, "/v1/users/same") || strings.Contains(log, `"name":"bad"`) {
		t.Fatalf("unexpected calls:\n%s", log)
	}
}

func TestApplyMtddSharedTLSDomainRollback(t *testing.T) {
	f := newMtddFixture(t, `{"users":[]}`)
	origText, _ := os.ReadFile(f.cfgPath)

	oldCtl, oldSD, oldWait := applyProxyCtl, applySystemdAvailable, applyWaitMetrics
	t.Cleanup(func() { applyProxyCtl, applySystemdAvailable, applyWaitMetrics = oldCtl, oldSD, oldWait })
	var restarts int
	applyProxyCtl = func(action, unit string) error {
		if unit != mtddUnit || action != "restart" {
			t.Fatalf("unexpected systemctl %s %s", action, unit)
		}
		restarts++
		return nil
	}
	applySystemdAvailable = func() bool { return true }

	// успех: tls_domain записан, прочие ключи сохранены, рестарт один
	applyWaitMetrics = func(*config.NodeConfig, time.Duration) error { return nil }
	if err := applyMtddShared(f.nodeCfg, SharedConfig{TLSDomain: "new.example"}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	data, _ := os.ReadFile(f.cfgPath)
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["tls_domain"] != "new.example" || doc["proxy_port"].(float64) != 443 || doc["workers"].(float64) != 0 {
		t.Fatalf("config not patched correctly: %s", data)
	}
	if restarts != 1 {
		t.Fatalf("restarts = %d", restarts)
	}
	// повтор без изменений не рестартует
	if err := applyMtddShared(f.nodeCfg, SharedConfig{TLSDomain: "new.example"}); err != nil || restarts != 1 {
		t.Fatalf("idempotent apply: err=%v restarts=%d", err, restarts)
	}

	// неподъём → откат файла на исходный
	if err := os.WriteFile(f.cfgPath, origText, 0644); err != nil {
		t.Fatal(err)
	}
	applyWaitMetrics = func(*config.NodeConfig, time.Duration) error { return os.ErrDeadlineExceeded }
	err := applyMtddShared(f.nodeCfg, SharedConfig{TLSDomain: "broken.example"})
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("expected rollback error, got %v", err)
	}
	if got, _ := os.ReadFile(f.cfgPath); string(got) != string(origText) {
		t.Fatalf("config not rolled back: %s", got)
	}
}

func TestDetectNodeTypeMtdd(t *testing.T) {
	withManagerDirs(t)
	if got := detectNodeType(); got != NodeTypeClassic {
		t.Fatalf("got %q", got)
	}
	if err := os.MkdirAll(filepath.Dir(mtddConfigPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mtddConfigPath, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := detectNodeType(); got != NodeTypeMTDD {
		t.Fatalf("got %q, want mtdd", got)
	}
	if nodeTypeLabel(NodeTypeMTDD) != "mtdd" {
		t.Fatal("label")
	}
}
