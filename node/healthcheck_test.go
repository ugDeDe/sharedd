package main

import (
	"strconv"
	"strings"
	"testing"
)

func TestParsePrometheusSamplesLabels(t *testing.T) {
	text := `# comment
telemt_user_unique_ips_current{user="alice"} 2
telemt_user_unique_ips_current{user="bob"} 3
telemt_me_writers_active_current 5
bad{unbalanced 7
no_value_here
telemt_gauge{odd="}"} 9 1690000000000
`
	samples := parsePrometheusSamples(text)
	byKey := make(map[string]float64)
	for _, s := range samples {
		k := s.name
		if s.labels != "" {
			k += "{" + s.labels + "}"
		}
		byKey[k] = s.value
	}
	if byKey[`telemt_user_unique_ips_current{user="alice"}`] != 2 {
		t.Fatalf("alice series lost: %v", byKey)
	}
	if byKey[`telemt_user_unique_ips_current{user="bob"}`] != 3 {
		t.Fatalf("bob series lost: %v", byKey)
	}
	if byKey["telemt_me_writers_active_current"] != 5 {
		t.Fatalf("scalar lost: %v", byKey)
	}
	if byKey[`telemt_gauge{odd="}"}`] != 9 {
		t.Fatalf("label with brace inside quotes must survive: %v", byKey)
	}
	if _, ok := byKey["bad"]; ok {
		t.Fatal("unbalanced-brace line must be skipped")
	}
	// 4 валидных: 2 per-user серии + scalar + gauge с '}' в label;
	// битая `bad{unbalanced` и беззначная `no_value_here` отброшены.
	if len(samples) != 4 {
		t.Fatalf("want 4 samples, got %d (%v)", len(samples), samples)
	}
}

func TestBuildMetricsSnapshotUniqueIPs(t *testing.T) {
	samples := []promSample{
		{name: "telemt_me_writers_active_current", value: 4},
		{name: "telemt_upstream_connect_attempt_total", labels: `direction="in"`, value: 42},
		{name: "telemt_me_reconnect_attempts_total", value: 7},
		{name: "telemt_user_unique_ips_current", labels: `user="alice"`, value: 2},
		{name: "telemt_user_unique_ips_current", labels: `user="bob"`, value: 3},
		{name: "telemt_user_connections_current", labels: `user="alice"`, value: 2},
		{name: "telemt_user_connections_current", labels: `user="bob"`, value: 5},
	}
	snapshot, values := buildMetricsSnapshot(samples)
	if snapshot[uniqueIPsMetricName] != 5 {
		t.Fatalf("unique IPs aggregate must be 2+3=5, got %v", snapshot[uniqueIPsMetricName])
	}
	if snapshot[userConnsMetricName] != 7 {
		t.Fatalf("conns aggregate must be 2+5=7, got %v", snapshot[userConnsMetricName])
	}
	if snapshot[`telemt_user_unique_ips_current{user="alice"}`] != 2 {
		t.Fatal("per-user labeled series must be preserved")
	}
	if values[healthMetricName] != 4 {
		t.Fatalf("flat values must feed the health gate: %v", values)
	}
	if snapshot["telemt_upstream_connect_attempt_total"] != 42 {
		t.Fatal("legacy scalar keys must stay")
	}
}

func TestBuildMetricsSnapshotNoUserTelemetry(t *testing.T) {
	// user_enabled=false в telemt: per-user серий нет вообще.
	// Агрегаты НЕ должны появляться — панель отличит "нет данных" от нуля.
	samples := []promSample{{name: healthMetricName, value: 1}}
	snapshot, _ := buildMetricsSnapshot(samples)
	if _, ok := snapshot[uniqueIPsMetricName]; ok {
		t.Fatal("aggregate without any per-user series would fake zero clients")
	}
	if _, ok := snapshot[userConnsMetricName]; ok {
		t.Fatal("conns aggregate must also be absent")
	}
}

func TestBuildMetricsSnapshotSeriesCap(t *testing.T) {
	samples := []promSample{{name: healthMetricName, value: 1}}
	for i := 0; i < snapshotMaxUserSeries+10; i++ {
		samples = append(samples, promSample{
			name:   uniqueIPsMetricName,
			labels: `user="u` + strconv.Itoa(i) + `"`,
			value:  1,
		})
	}
	snapshot, _ := buildMetricsSnapshot(samples)
	labeled := 0
	for k := range snapshot {
		if strings.HasPrefix(k, uniqueIPsMetricName+"{") {
			labeled++
		}
	}
	if labeled != snapshotMaxUserSeries {
		t.Fatalf("labeled series must be capped at %d, got %d", snapshotMaxUserSeries, labeled)
	}
	// агрегат считается по ВСЕМ сериям, не по уместившимся в снапшот
	if snapshot[uniqueIPsMetricName] != float64(snapshotMaxUserSeries+10) {
		t.Fatalf("aggregate must sum all series, got %v", snapshot[uniqueIPsMetricName])
	}
}

func TestEvaluateSuccessRatio(t *testing.T) {
	m := &globalpingMeasurement{Results: []globalpingProbeMeasurement{
		{Result: globalpingProbeResult{Status: "finished", StatusCode: 200}},
		{Result: globalpingProbeResult{Status: "finished", StatusCode: 302}},
		{Result: globalpingProbeResult{Status: "finished", StatusCode: 500}},
		{Result: globalpingProbeResult{Status: "failed", StatusCode: 0}},
	}}
	if r := evaluateSuccessRatio(m); r != 0.5 {
		t.Fatalf("expected 0.5, got %v", r)
	}
	if r := evaluateSuccessRatio(nil); r != 0 {
		t.Fatalf("nil measurement must be 0, got %v", r)
	}
	if r := evaluateSuccessRatio(&globalpingMeasurement{}); r != 0 {
		t.Fatalf("empty results must be 0, got %v", r)
	}
}

func TestMetricsURLFromTelemt(t *testing.T) {
	cases := []struct {
		port   int
		listen string
		want   string
	}{
		{0, "", "http://127.0.0.1:9090/metrics"},
		{8888, "", "http://127.0.0.1:8888/metrics"},
		{0, "127.0.0.1:9091", "http://127.0.0.1:9091/metrics"},    // listen переопределяет дефолт
		{8888, "127.0.0.1:9091", "http://127.0.0.1:9091/metrics"}, // listen приоритетнее port
		{0, "0.0.0.0:8888", "http://127.0.0.1:8888/metrics"},
		{0, "127.0.0.1", "http://127.0.0.1:9090/metrics"},
	}
	for _, c := range cases {
		if got := metricsURLFromTelemt(c.port, c.listen); got != c.want {
			t.Errorf("metricsURLFromTelemt(%d, %q) = %q, want %q", c.port, c.listen, got, c.want)
		}
	}
}
