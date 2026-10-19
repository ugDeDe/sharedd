package server

import (
	"reflect"
	"testing"
	"time"
)

// TestNormalizeTelemtMetrics — telemt 3.5.9 `_total` counters and legacy bare
// names both become the sharedd_* keys the traffic pipeline consumes.
func TestNormalizeTelemtMetrics(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]float64
		want map[string]float64
	}{
		{
			name: "total suffix",
			in: map[string]float64{
				`telemt_user_octets_from_client_total{user="a"}`: 100,
				`telemt_user_octets_to_client_total{user="a"}`:   200,
				`telemt_user_octets_from_client_total{user="b"}`: 30,
				`telemt_user_octets_to_client_total{user="b"}`:   40,
			},
			want: map[string]float64{
				userIngressMetric + `{user="a"}`: 100,
				userEgressMetric + `{user="a"}`:  200,
				userIngressMetric + `{user="b"}`: 30,
				userEgressMetric + `{user="b"}`:  40,
				trafficIngressMetric:             130,
				trafficEgressMetric:              240,
			},
		},
		{
			name: "bare names",
			in: map[string]float64{
				`telemt_user_octets_from_client{user="a"}`: 5,
				`telemt_user_octets_to_client{user="a"}`:   7,
			},
			want: map[string]float64{
				userIngressMetric + `{user="a"}`: 5,
				userEgressMetric + `{user="a"}`:  7,
				trafficIngressMetric:             5,
				trafficEgressMetric:              7,
			},
		},
		{
			name: "real sharedd values win",
			in: map[string]float64{
				`telemt_user_octets_from_client_total{user="a"}`: 100,
				`telemt_user_octets_to_client_total{user="a"}`:   200,
				`telemt_user_octets_from_client_total{user="b"}`: 30,
				userIngressMetric + `{user="a"}`:                 111,
				trafficIngressMetric:                             999,
				trafficEgressMetric:                              888,
				trafficUsersMetric:                               7,
			},
			want: map[string]float64{
				userIngressMetric + `{user="a"}`: 111, // untouched
				userEgressMetric + `{user="a"}`:  200, // synthesized
				userIngressMetric + `{user="b"}`: 30,  // synthesized
				trafficIngressMetric:             999,
				trafficEgressMetric:              888,
				trafficUsersMetric:               7,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTelemtMetrics(tt.in)
			for key, want := range tt.want {
				if got[key] != want {
					t.Fatalf("%s = %v, want %v", key, got[key], want)
				}
			}
		})
	}
}

// TestNormalizeTelemtMetricsIdempotent — a second pass changes nothing, so
// normalizing the stored snapshot too is safe.
func TestNormalizeTelemtMetricsIdempotent(t *testing.T) {
	snapshot := map[string]float64{
		`telemt_user_octets_from_client_total{user="a"}`: 10,
		`telemt_user_octets_to_client_total{user="a"}`:   20,
	}
	first := normalizeTelemtMetrics(snapshot)
	second := normalizeTelemtMetrics(first)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("normalization must be idempotent:\nfirst=%v\nsecond=%v", first, second)
	}
	if first[trafficUsersMetric] == 0 {
		t.Fatal("users fingerprint must be synthesized")
	}
}

// TestTrafficFromTelemtTotalCounters — reports from an old agent carrying raw
// telemt `_total` counters record traffic and per-user deltas, survive a
// counter reset, and leave synthetic keys in the stored snapshot.
func TestTrafficFromTelemtTotalCounters(t *testing.T) {
	r := newTestRegistry(t)
	r.db = openHistoryDB(t.TempDir() + "/hist.db")
	if r.db == nil {
		t.Fatal("history db must open")
	}
	defer r.db.Close()
	r.register(registerRequest{NodeID: "node-a1b2c", IP: "1.2.3.4"})

	base := time.Now().Add(-time.Hour)
	reportHostMetrics(t, r, "node-a1b2c", map[string]float64{
		`telemt_user_octets_from_client_total{user="a"}`: 100,
		`telemt_user_octets_to_client_total{user="a"}`:   200,
		`telemt_user_octets_from_client_total{user="b"}`: 50,
		`telemt_user_octets_to_client_total{user="b"}`:   80,
	}, base)
	reportHostMetrics(t, r, "node-a1b2c", map[string]float64{
		`telemt_user_octets_from_client_total{user="a"}`: 160,
		`telemt_user_octets_to_client_total{user="a"}`:   240,
		`telemt_user_octets_from_client_total{user="b"}`: 70, // +20
		`telemt_user_octets_to_client_total{user="b"}`:   80, // +0
	}, base.Add(time.Minute))

	c := r.state.Candidates["node-a1b2c"]
	if c == nil {
		t.Fatal("candidate missing")
	}
	if got := c.MetricsSnapshot[trafficIngressMetric]; got != 230 {
		t.Fatalf("stored ingress total = %v, want 230", got)
	}
	if got := c.MetricsSnapshot[trafficEgressMetric]; got != 320 {
		t.Fatalf("stored egress total = %v, want 320", got)
	}
	if _, ok := c.MetricsSnapshot[userIngressMetric+`{user="b"}`]; !ok {
		t.Fatal("per-user synthetic ingress key must be stored for node detail")
	}

	rows, err := r.db.TrafficSeriesSince("node-a1b2c", base)
	if err != nil {
		t.Fatalf("traffic rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Ingress != 80 || rows[0].Egress != 40 {
		t.Fatalf("traffic deltas = %+v, want one 80/40 row", rows)
	}
	userRows, err := r.db.UserTrafficSince("node-a1b2c", base)
	if err != nil {
		t.Fatalf("user traffic rows: %v", err)
	}
	var userA, userB int
	for _, row := range userRows {
		switch row.User {
		case "a":
			userA++
			if row.Ingress != 60 || row.Egress != 40 {
				t.Fatalf("user a delta = %+v, want 60/40", row)
			}
		case "b":
			userB++
			if row.Ingress != 20 || row.Egress != 0 {
				t.Fatalf("user b delta = %+v, want 20/0", row)
			}
		}
	}
	if userA != 1 || userB != 1 {
		t.Fatalf("per-user rows = %+v, want one each for a and b", userRows)
	}

	// telemt reset: counters drop -> only bytes since reset count, no panic.
	reportHostMetrics(t, r, "node-a1b2c", map[string]float64{
		`telemt_user_octets_from_client_total{user="a"}`: 10,
		`telemt_user_octets_to_client_total{user="a"}`:   20,
		`telemt_user_octets_from_client_total{user="b"}`: 5,
		`telemt_user_octets_to_client_total{user="b"}`:   5,
	}, base.Add(2*time.Minute))
	rows, err = r.db.TrafficSeriesSince("node-a1b2c", base)
	if err != nil {
		t.Fatalf("reset traffic rows: %v", err)
	}
	last := rows[len(rows)-1]
	if last.Ingress != 15 || last.Egress != 25 {
		t.Fatalf("post-reset delta = %+v, want 15/25", last)
	}
}
