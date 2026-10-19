package server

import (
	"hash/fnv"
	"sort"
	"strings"
)

// telemt 3.5.9 exports octets counters with a `_total` suffix
// (telemt_user_octets_{from,to}_client_total), while agents older than the
// node-side fix match only the bare names and therefore emit no sharedd_*
// traffic keys. Normalize both shapes on the registry side so such nodes
// start reporting traffic without an agent rollout.
const (
	telemtOctetsFromClient = "telemt_user_octets_from_client"
	telemtOctetsToClient   = "telemt_user_octets_to_client"
)

// normalizeTelemtMetrics synthesizes the sharedd_* traffic keys consumed by the
// report handler and node detail from raw telemt octet counters found in the
// snapshot. Idempotent and never overwrites existing sharedd_* values.
func normalizeTelemtMetrics(snapshot map[string]float64) map[string]float64 {
	if len(snapshot) == 0 {
		return snapshot
	}
	from, _ := collectOctetSeries(snapshot, telemtOctetsFromClient)
	to, _ := collectOctetSeries(snapshot, telemtOctetsToClient)

	realUserSeries := hasAnyPrefix(snapshot, userIngressMetric+"{", userEgressMetric+"{")
	users := make(map[string]struct{}, len(from)+len(to))
	var ingress, egress float64
	synthetic := 0
	for _, s := range from {
		ingress += s.value
		if u := metricUserFromKey(s.key); u != "" {
			users[u] = struct{}{}
		}
		if s.labels != "" {
			synthesize(snapshot, userIngressMetric+s.labels, s.value)
			synthetic++
		}
	}
	for _, s := range to {
		egress += s.value
		if u := metricUserFromKey(s.key); u != "" {
			users[u] = struct{}{}
		}
		if s.labels != "" {
			synthesize(snapshot, userEgressMetric+s.labels, s.value)
			synthetic++
		}
	}
	if synthetic == 0 || realUserSeries {
		return snapshot
	}
	if _, ok := snapshot[trafficIngressMetric]; !ok {
		snapshot[trafficIngressMetric] = ingress
	}
	if _, ok := snapshot[trafficEgressMetric]; !ok {
		snapshot[trafficEgressMetric] = egress
	}
	if _, ok := snapshot[trafficUsersMetric]; !ok {
		snapshot[trafficUsersMetric] = octetUsersFingerprint(users)
	}
	return snapshot
}

type octetSeries struct {
	key    string // full snapshot key, used to read the user label
	labels string // "{...}" suffix, empty for aggregate keys
	value  float64
}

// collectOctetSeries returns the octet series for one direction, preferring the
// `_total` form when both are exported (avoids double counting).
func collectOctetSeries(snapshot map[string]float64, base string) ([]octetSeries, bool) {
	totalForm := hasMetricBase(snapshot, base+"_total")
	name := base
	if totalForm {
		name = base + "_total"
	}
	var out []octetSeries
	for key, v := range snapshot {
		labels, ok := cutMetricKey(key, name)
		if !ok {
			continue
		}
		out = append(out, octetSeries{key: key, labels: labels, value: v})
	}
	return out, totalForm
}

func cutMetricKey(key, name string) (labels string, ok bool) {
	if key == name {
		return "", true
	}
	if strings.HasPrefix(key, name+"{") {
		return key[len(name):], true
	}
	return "", false
}

func synthesize(snapshot map[string]float64, key string, value float64) {
	if _, exists := snapshot[key]; !exists {
		snapshot[key] = value
	}
}

func hasMetricBase(snapshot map[string]float64, name string) bool {
	for key := range snapshot {
		if _, ok := cutMetricKey(key, name); ok {
			return true
		}
	}
	return false
}

func hasAnyPrefix(snapshot map[string]float64, prefixes ...string) bool {
	for key := range snapshot {
		for _, p := range prefixes {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
	}
	return false
}

// octetUsersFingerprint matches the agent-side fingerprint (FNV-1a over the
// sorted user list) so the sameUsers reset gate holds across reports.
func octetUsersFingerprint(users map[string]struct{}) float64 {
	names := make([]string, 0, len(users))
	for name := range users {
		names = append(names, name)
	}
	sort.Strings(names)
	h := fnv.New64a()
	for _, name := range names {
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
	}
	return float64(h.Sum64() & ((1 << 53) - 1))
}
