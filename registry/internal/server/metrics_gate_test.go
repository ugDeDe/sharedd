package server

import (
	"strings"
	"testing"

	"sharedd/registry/internal/machineapi"
)

func TestMetricsGateReason(t *testing.T) {
	online := map[string]float64{uniqueIPsMetric: 120}
	dcs := func(rtt float64) *machineapi.DCStats {
		return &machineapi.DCStats{DCs: []machineapi.DCStat{{DC: 1, RTTMs: 9000}, {DC: 2, RTTMs: rtt}}}
	}
	for _, tc := range []struct {
		name string
		p    machineapi.HealthReportPayload
		max  int
		want string
	}{
		{"healthy", machineapi.HealthReportPayload{MetricsSnapshot: online, DCStats: dcs(25)}, 3000, ""},
		{"no online", machineapi.HealthReportPayload{MetricsSnapshot: map[string]float64{"x": 1}}, 3000, "no online"},
		{"slow DC2", machineapi.HealthReportPayload{MetricsSnapshot: online, DCStats: dcs(3500)}, 3000, "DC2 RTT 3500 ms > 3000 ms"},
		{"only DC2 counts", machineapi.HealthReportPayload{MetricsSnapshot: online, DCStats: dcs(2999)}, 3000, ""},
		{"rtt check off", machineapi.HealthReportPayload{MetricsSnapshot: online, DCStats: dcs(9000)}, 0, ""},
		{"no snapshots — agent verdict decides", machineapi.HealthReportPayload{}, 3000, ""},
	} {
		got := metricsGateReason(&tc.p, tc.max)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
