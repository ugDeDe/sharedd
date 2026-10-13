package agent

import (
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"sharedd/node/internal/config"
)

// ── чистая функция критерия (таблица из specs/dc2-health.md) ──

func TestEvaluateDC2(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	freshGen := now.Unix() - 60 // 60s назад — свежо
	healthyRow := dc2DCStats{DC: 2, AvailableEndpoints: 1, AliveWriters: 7, FreshAliveWriters: 7, RTTMs: 66.0}
	mk := func(mod func(*dc2StatsResponse)) *dc2StatsResponse {
		r := &dc2StatsResponse{OK: true}
		r.Data.MiddleProxyEnabled = true
		r.Data.GeneratedAtEpochSecs = freshGen
		r.Data.DCs = []dc2DCStats{healthyRow}
		if mod != nil {
			mod(r)
		}
		return r
	}
	tests := []struct {
		name       string
		resp       *dc2StatsResponse
		wantOK     bool
		wantReason string
	}{
		{"healthy", mk(nil), true, ""},
		{"api not ok", mk(func(r *dc2StatsResponse) { r.OK = false }), false, dc2ReasonAPIError},
		{"me disabled", mk(func(r *dc2StatsResponse) { r.Data.MiddleProxyEnabled = false }), false, dc2ReasonMEDisabled},
		{"stale old", mk(func(r *dc2StatsResponse) { r.Data.GeneratedAtEpochSecs = now.Unix() - 301 }), false, dc2ReasonStale},
		{"stale future drift", mk(func(r *dc2StatsResponse) { r.Data.GeneratedAtEpochSecs = now.Unix() + 121 }), false, dc2ReasonStale},
		{"stale zero generated_at", mk(func(r *dc2StatsResponse) { r.Data.GeneratedAtEpochSecs = 0 }), false, dc2ReasonStale},
		{"dc2 absent", mk(func(r *dc2StatsResponse) {
			r.Data.DCs = []dc2DCStats{{DC: 1, AvailableEndpoints: 3, AliveWriters: 5, FreshAliveWriters: 5}}
		}), false, dc2ReasonAbsent},
		{"no endpoints", mk(func(r *dc2StatsResponse) { r.Data.DCs[0].AvailableEndpoints = 0 }), false, dc2ReasonNoEndpoints},
		{"no fresh writers", mk(func(r *dc2StatsResponse) { r.Data.DCs[0].FreshAliveWriters = 0 }), false, dc2ReasonNoFresh},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := evaluateDC2(tc.resp, now)
			if v.OK != tc.wantOK || v.Reason != tc.wantReason {
				t.Fatalf("evaluateDC2 = ok=%v reason=%q, want ok=%v reason=%q", v.OK, v.Reason, tc.wantOK, tc.wantReason)
			}
		})
	}

	// Диагностика здорового снапшота: alive/fresh dc2, rtt, generated_at.
	diag := evaluateDC2(mk(nil), now).Diag
	if diag["dc2_alive"] != 7 || diag["dc2_fresh"] != 7 || diag["dc2_rtt_ms"] != 66.0 || diag["dc2_generated_at"] != freshGen {
		t.Fatalf("unexpected diag: %v", diag)
	}
	// Диагностика при отсутствии dc2-строки: нули, generated_at сохранён.
	diag = evaluateDC2(mk(func(r *dc2StatsResponse) { r.Data.DCs = nil }), now).Diag
	if diag["dc2_alive"] != 0 || diag["dc2_fresh"] != 0 || diag["dc2_generated_at"] != freshGen {
		t.Fatalf("unexpected absent-diag: %v", diag)
	}
	// Пороговые значения ровно на границе — ЗДОРОВЫЕ (строгое неравенство).
	border := mk(func(r *dc2StatsResponse) {
		r.Data.GeneratedAtEpochSecs = now.Unix() - 300
	})
	if v := evaluateDC2(border, now); !v.OK {
		t.Fatalf("gen at exactly now-300s must be fresh, got %q", v.Reason)
	}
	border.Data.GeneratedAtEpochSecs = now.Unix() + 120
	if v := evaluateDC2(border, now); !v.OK {
		t.Fatalf("gen at exactly now+120s must be fresh, got %q", v.Reason)
	}
}

// TestFetchDCStatsSnapshot — снапшот Control API отдаётся целиком (все DC по
// порядку), вердикт не меняется; API-ошибка → nil-снапшот.
func TestFetchDCStatsSnapshot(t *testing.T) {
	now := time.Now()
	body := `{"ok":true,"data":{"middle_proxy_enabled":true,"generated_at_epoch_secs":` +
		strconv.FormatInt(now.Unix(), 10) + `,"dcs":[
			{"dc":1,"available_endpoints":3,"alive_writers":5,"fresh_alive_writers":5,"rtt_ms":11.5},
			{"dc":2,"available_endpoints":5,"alive_writers":12,"fresh_alive_writers":10,"rtt_ms":42.5}]}}`
	ts, _ := startTelemt(t, "x 1\n", body, http.StatusOK)
	client := &http.Client{Timeout: dc2APITimeout}

	verdict, stats := fetchDCStats(ts.URL, client, now)
	if !verdict.OK || verdict.Reason != "" {
		t.Fatalf("healthy snapshot verdict = %+v", verdict)
	}
	if stats == nil || !stats.MiddleProxyEnabled || stats.GeneratedAtEpochSecs != now.Unix() {
		t.Fatalf("stats header wrong: %+v", stats)
	}
	if len(stats.DCs) != 2 || stats.DCs[0].DC != 1 || stats.DCs[1].DC != 2 || stats.DCs[1].RTTMs != 42.5 {
		t.Fatalf("all DCs in order expected, got %+v", stats.DCs)
	}

	// Вердикт dc2_no_fresh сохраняется, но снапшот всё равно отдаётся.
	badBody := `{"ok":true,"data":{"middle_proxy_enabled":true,"generated_at_epoch_secs":` +
		strconv.FormatInt(now.Unix(), 10) +
		`,"dcs":[{"dc":2,"available_endpoints":1,"alive_writers":7,"fresh_alive_writers":0,"rtt_ms":66.0}]}}`
	tsBad, _ := startTelemt(t, "x 1\n", badBody, http.StatusOK)
	verdict, stats = fetchDCStats(tsBad.URL, client, now)
	if verdict.OK || verdict.Reason != dc2ReasonNoFresh || stats == nil {
		t.Fatalf("no-fresh verdict must be kept with snapshot, got %+v stats=%+v", verdict, stats)
	}

	// HTTP 500 → api_error и nil (Control API не ответил).
	tsErr, _ := startTelemt(t, "x 1\n", `{"ok":true}`, http.StatusInternalServerError)
	verdict, stats = fetchDCStats(tsErr.URL, client, now)
	if verdict.Reason != dc2ReasonAPIError || stats != nil {
		t.Fatalf("api error must yield nil snapshot, got %+v stats=%+v", verdict, stats)
	}
}

// TestRunMetricsCheckCarriesDCStats — dc_stats заполняется в dc2-режиме и
// остаётся nil в fallback (api_addr пуст).
func TestRunMetricsCheckCarriesDCStats(t *testing.T) {
	_, cfg := startTelemt(t, "some_other_metric 1\n", healthyDCSBody(), http.StatusOK)
	report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if report.DCStats == nil || len(report.DCStats.DCs) != 1 || report.DCStats.DCs[0].DC != 2 {
		t.Fatalf("dc2 mode must carry dc_stats, got %+v", report.DCStats)
	}

	_, cfg = startTelemt(t, healthMetricName+" 5\n", "", http.StatusOK)
	cfg.Telemt.APIAddr = ""
	if report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7"); report.DCStats != nil {
		t.Fatalf("fallback mode must not carry dc_stats, got %+v", report.DCStats)
	}
}

// ── интеграция RunMetricsCheck ──

// startTelemt — httptest с /metrics и /v1/stats/dcs + node-config на него.
func startTelemt(t *testing.T, metricsBody, dcsBody string, dcsStatus int) (*httptest.Server, *config.NodeConfig) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(metricsBody))
	})
	mux.HandleFunc("/v1/stats/dcs", func(w http.ResponseWriter, _ *http.Request) {
		if dcsStatus != http.StatusOK {
			w.WriteHeader(dcsStatus)
		}
		_, _ = w.Write([]byte(dcsBody))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	telemtToml := filepath.Join(dir, "telemt.toml")
	port := strings.TrimPrefix(ts.URL, "http://127.0.0.1:")
	if err := os.WriteFile(telemtToml, []byte("[server]\nmetrics_port = "+port+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.NodeConfig{}
	cfg.Telemt.ConfigPath = telemtToml
	cfg.Telemt.APIAddr = ts.URL
	return ts, cfg
}

func healthyDCSBody() string {
	return `{"ok":true,"data":{"middle_proxy_enabled":true,"generated_at_epoch_secs":` +
		strconv.FormatInt(time.Now().Unix(), 10) +
		`,"dcs":[{"dc":2,"available_endpoints":1,"alive_writers":7,"fresh_alive_writers":7,"rtt_ms":66.0}]}}`
}

func TestRunMetricsCheckDC2Mode(t *testing.T) {
	// В dc2-режиме вердикт берётся из Control API ДАЖЕ если ключевой
	// метрики в /metrics нет (снапшот при этом всё равно собирается).
	_, cfg := startTelemt(t, "some_other_metric 1\n", healthyDCSBody(), http.StatusOK)
	report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if !report.MetricsOK || !report.DC2OK {
		t.Fatalf("healthy dc2 must make the report ok, got metrics_ok=%v dc2_ok=%v err=%q",
			report.MetricsOK, report.DC2OK, report.Error)
	}
	if report.DC2Reason != "" {
		t.Fatalf("healthy dc2 must have empty reason, got %q", report.DC2Reason)
	}
	if report.DC2Diag["dc2_fresh"] != 7 {
		t.Fatalf("diag expected, got %v", report.DC2Diag)
	}
	if report.Error != "" {
		t.Fatalf("healthy dc2 must clear the error, got %q", report.Error)
	}
	if !report.Healthy {
		t.Fatal("report must be healthy")
	}
	if len(report.MetricsSnapshot) == 0 {
		t.Fatal("metrics snapshot must still be collected in dc2 mode")
	}
}

func TestRunMetricsCheckDC2ModeUnhealthy(t *testing.T) {
	_, cfg := startTelemt(t, healthMetricName+" 5\n",
		`{"ok":true,"data":{"middle_proxy_enabled":true,"generated_at_epoch_secs":`+strconv.FormatInt(time.Now().Unix(), 10)+
			`,"dcs":[{"dc":2,"available_endpoints":1,"alive_writers":7,"fresh_alive_writers":0,"rtt_ms":66.0}]}}`, http.StatusOK)
	report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if report.MetricsOK || report.DC2OK || report.Healthy {
		t.Fatalf("dc2_no_fresh must fail the report: %+v", report)
	}
	if report.DC2Reason != dc2ReasonNoFresh {
		t.Fatalf("reason dc2_no_fresh expected, got %q", report.DC2Reason)
	}
	if !strings.Contains(report.Error, dc2ReasonNoFresh) {
		t.Fatalf("report error must carry the reason code, got %q", report.Error)
	}
}

func TestRunMetricsCheckDC2APIError(t *testing.T) {
	// HTTP 500 от Control API → dc2_api_error, отчёт нездоров.
	_, cfg := startTelemt(t, healthMetricName+" 5\n", `{"ok":true}`, http.StatusInternalServerError)
	report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if report.MetricsOK || report.DC2OK {
		t.Fatalf("api error must fail the report: %+v", report)
	}
	if report.DC2Reason != dc2ReasonAPIError {
		t.Fatalf("reason dc2_api_error expected, got %q", report.DC2Reason)
	}
	if !strings.Contains(report.Error, dc2ReasonAPIError) {
		t.Fatalf("report error must carry the reason code, got %q", report.Error)
	}
}

func TestRunMetricsCheckFallbackMode(t *testing.T) {
	// api_addr пуст → прежний критерий по me_writers; отчёт помечен
	// dc2_fallback_metrics, dc2_ok зеркалит metrics_ok.
	_, cfg := startTelemt(t, healthMetricName+" 5\n", "", http.StatusOK)
	cfg.Telemt.APIAddr = ""
	report := RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if !report.MetricsOK || !report.DC2OK || !report.Healthy {
		t.Fatalf("writers>0 must be healthy in fallback mode: %+v", report)
	}
	if report.DC2Reason != dc2ReasonFallback {
		t.Fatalf("fallback reason expected, got %q", report.DC2Reason)
	}

	// Метрики нет → нездоров (старое поведение), телеметрия-совместимость
	// помечена (анти-спам покрывается отдельным тестом).
	_, cfg = startTelemt(t, "unrelated 1\n", "", http.StatusOK)
	cfg.Telemt.APIAddr = ""
	telemtCompatWarnAt = time.Time{} // сброс анти-спама для детерминизма
	report = RunMetricsCheck(cfg, "node-a1b2c", "203.0.113.7")
	if report.MetricsOK || report.DC2OK {
		t.Fatalf("missing writer metric must fail in fallback mode: %+v", report)
	}
	if report.DC2Reason != dc2ReasonFallback {
		t.Fatalf("fallback reason expected even on failure, got %q", report.DC2Reason)
	}
}

func TestTelemtCompatSuspicionDoesNotSpam(t *testing.T) {
	telemtCompatWarnAt = time.Time{} // сброс анти-спама
	defer func() { telemtCompatWarnAt = time.Time{} }()
	logs := &strings.Builder{}
	old := log.Writer()
	log.SetOutput(logs)
	defer log.SetOutput(old)
	noteTelemtCompatSuspicion([]byte("telemt_upstream_total 1"))
	first := logs.Len()
	noteTelemtCompatSuspicion([]byte("telemt_upstream_total 1"))
	if logs.Len() != first {
		t.Fatal("second suspicion within the hour must not log again (anti-spam)")
	}
}
