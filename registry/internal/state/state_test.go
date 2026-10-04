package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry_state.json")

	st := &State{
		Candidates: map[string]*Candidate{
			"helsinki-6an4o": {
				NodeID:         "helsinki-6an4o",
				IP:             "203.0.113.7",
				RegisteredAt:   time.Unix(1700000000, 0),
				Healthy:        true,
				FullyHealthy:   true,
				GlobalpingOK:   true,
				MetricsHealthy: true,
				LastReportAt:   time.Now(),
				Quarantine:     nil,
				TCPHist:        []TCPPoint{{At: time.Unix(1700000001, 0), OK: true}},
				GPLast:         &GPDetail{MeasurementID: "m-x", Ratio: 1, OK: true},
			},
		},
		Assignments:      map[string]string{"mtp.example.com": "helsinki-6an4o"},
		AssignmentsSince: map[string]time.Time{"mtp.example.com": time.Unix(1700000100, 0)},
		DNSOperations:    map[string]*DNSOperation{"mtp.example.com": {DesiredType: "A", DesiredTarget: "203.0.113.7"}},
		Terminated:       map[string]*TerminatedRecord{"dead-node": {NodeID: "dead-node", Reason: BanReasonDead, Message: "dead"}},
		PruneStrikes:     map[string]*PruneTombstone{"p": {Strikes: 2}},
		SRMD:             SRMDState{DomainClients: map[string]int{"mtp.example.com": 42}, Created: []string{"shared1.example.com"}},
		ManagedDomains:   []string{"mtp.example.com"},
	}
	st.Counters.Registrations = 5
	st.Events = append(st.Events, Event{Type: EventNodeRegistered, NodeID: "helsinki-6an4o"})

	if err := Save(path, st); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c := got.Candidates["helsinki-6an4o"]
	if c == nil || c.IP != "203.0.113.7" || !c.IsFullyHealthy(time.Minute) {
		t.Fatalf("candidate round trip broken: %+v", c)
	}
	if got.Terminated["dead-node"].Reason != BanReasonDead {
		t.Fatalf("terminated reason lost")
	}
	if got.SRMD.DomainClients["mtp.example.com"] != 42 {
		t.Fatalf("srmd clients lost")
	}
	if len(got.Events) != 1 || got.Events[0].Type != EventNodeRegistered {
		t.Fatalf("events lost: %+v", got.Events)
	}
}

// Старый state без защёлки metrics и с null в candidates не должен ронять
// загрузку: защёлка переносится из последнего вердикта, nil-записи пропускаются.
func TestLoadLegacyStateMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	legacy := `{
	  "candidates": {"n1": {"node_id":"n1","metrics_ok":true}, "broken": null},
	  "counters": {"registrations_total": 3}
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("legacy load must succeed, got %v", err)
	}
	c := got.Candidates["n1"]
	if c == nil || !c.MetricsHealthy {
		t.Fatalf("metrics latch migration failed: %+v", c)
	}
	if got.Assignments == nil {
		t.Fatal("assignments map must be normalized")
	}
}
