package state

import (
	"encoding/json"
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
		Assignments:      map[string]AssignmentSlots{"mtp.example.com": {"helsinki-6an4o"}},
		AssignmentsSince: map[string]SinceSlots{"mtp.example.com": {time.Unix(1700000100, 0)}},
		DNSOperations:    map[string]*DNSOperation{"mtp.example.com": {DesiredType: "A", DesiredTargets: []string{"203.0.113.7"}, DesiredNodes: []string{"helsinki-6an4o"}}},
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
	// новый формат закреплён: назначения и DNS-наборы — упорядоченные списки
	if got := got.Assignments["mtp.example.com"]; len(got) != 1 || got[0] != "helsinki-6an4o" {
		t.Fatalf("assignments round trip broken: %+v", got)
	}
	if since := got.AssignmentsSince["mtp.example.com"]; len(since) != 1 || !since[0].Equal(time.Unix(1700000100, 0)) {
		t.Fatalf("assignments_since round trip broken: %+v", since)
	}
	op := got.DNSOperations["mtp.example.com"]
	if op == nil || op.DesiredType != "A" || len(op.DesiredTargets) != 1 || op.DesiredTargets[0] != "203.0.113.7" || len(op.DesiredNodes) != 1 || op.DesiredNodes[0] != "helsinki-6an4o" {
		t.Fatalf("dns operation round trip broken: %+v", op)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// сохранение — ВСЕГДА новый формат (списки), даже для одного элемента
	var rawState struct {
		Assignments map[string]json.RawMessage            `json:"assignments"`
		DNSOp       map[string]map[string]json.RawMessage `json:"dns_operations"`
	}
	if err := json.Unmarshal(raw, &rawState); err != nil {
		t.Fatal(err)
	}
	if v := rawState.Assignments["mtp.example.com"]; len(v) == 0 || v[0] != '[' {
		t.Fatalf("assignments must be saved as an array, got %s", v)
	}
	opRaw := rawState.DNSOp["mtp.example.com"]
	if v := opRaw["desired_targets"]; len(v) == 0 || v[0] != '[' {
		t.Fatalf("desired_targets must be saved as an array, got %s", v)
	}
	for _, legacyKey := range []string{"desired_target", "desired_node", "applied_target"} {
		if _, ok := opRaw[legacyKey]; ok {
			t.Fatalf("legacy singular DNS key %q must not be written", legacyKey)
		}
	}
}

// Старый state без защёлки metrics и с null в candidates не должен ронять
// загрузку: защёлка переносится из последнего вердикта, nil-записи пропускаются.
func TestLoadLegacyStateMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.json")
	legacy := `{
	  "candidates": {"n1": {"node_id":"n1","metrics_ok":true}, "broken": null},
	  "counters": {"registrations_total": 3},
	  "srmd": {"created": ["shared1.example.com"], "last_action_at": "2024-01-01T00:00:00Z"}
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
	// удалённое поле анти-флапа last_action_at не должно ломать загрузку
	// (unknown legacy field игнорируется, известные поля SRMD читаются)
	if len(got.SRMD.Created) != 1 || got.SRMD.Created[0] != "shared1.example.com" {
		t.Fatalf("srmd known fields must survive an unknown legacy field: %+v", got.SRMD)
	}
}

// Миграция со старого формата (одна строка на домен, одиночные поля
// DNSOperation): читается как список из одного элемента, сохраняется в новом.
func TestLoadLegacySingleMasterAssignments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-masters.json")
	legacy := `{
	  "candidates": {"n1": {"node_id":"n1","ip":"203.0.113.7"}},
	  "assignments": {"mtp.example.com": "n1"},
	  "assignments_since": {"mtp.example.com": "2023-11-14T22:15:00Z"},
	  "dns_operations": {
	    "mtp.example.com": {"desired_type":"A","desired_target":"203.0.113.7","desired_node":"n1","applied_type":"A","applied_target":"203.0.113.7"}
	  }
	}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("legacy load must succeed, got %v", err)
	}
	nodes := got.Assignments["mtp.example.com"]
	if len(nodes) != 1 || nodes[0] != "n1" {
		t.Fatalf("legacy string assignment must load as a one-element list, got %+v", nodes)
	}
	since := got.AssignmentsSince["mtp.example.com"]
	if len(since) != 1 || !since[0].Equal(time.Unix(1700000100, 0).UTC()) {
		t.Fatalf("legacy single since must load as a one-element list, got %+v", since)
	}
	op := got.DNSOperations["mtp.example.com"]
	if op == nil {
		t.Fatal("dns operation must load")
	}
	if len(op.DesiredTargets) != 1 || op.DesiredTargets[0] != "203.0.113.7" {
		t.Fatalf("legacy desired_target must migrate to DesiredTargets, got %+v", op.DesiredTargets)
	}
	if len(op.DesiredNodes) != 1 || op.DesiredNodes[0] != "n1" {
		t.Fatalf("legacy desired_node must migrate to DesiredNodes, got %+v", op.DesiredNodes)
	}
	if len(op.AppliedTargets) != 1 || op.AppliedTargets[0] != "203.0.113.7" {
		t.Fatalf("legacy applied_target must migrate to AppliedTargets, got %+v", op.AppliedTargets)
	}
	if op.Drifted() {
		t.Fatalf("converged legacy operation must not be drifted: %+v", op)
	}

	// перезапись — уже новый формат
	if err := Save(path, got); err != nil {
		t.Fatalf("save migrated state: %v", err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := again.Assignments["mtp.example.com"]; len(n) != 1 || n[0] != "n1" {
		t.Fatalf("migrated state must round-trip: %+v", n)
	}
}

// Drifted сравнивает наборы целей БЕЗ учёта порядка (DNS-набор — множество).
func TestDNSOperationDriftedSetSemantics(t *testing.T) {
	base := DNSOperation{DesiredType: "A", AppliedType: "A"}
	cases := []struct {
		desired, applied []string
		drifted          bool
	}{
		{[]string{"1.1.1.1"}, []string{"1.1.1.1"}, false},
		{[]string{"1.1.1.1", "2.2.2.2"}, []string{"2.2.2.2", "1.1.1.1"}, false},
		{[]string{"1.1.1.1", "2.2.2.2"}, []string{"1.1.1.1"}, true},
		{[]string{"1.1.1.1"}, []string{"2.2.2.2"}, true},
		{nil, nil, false},
	}
	for _, tc := range cases {
		op := base
		op.DesiredTargets, op.AppliedTargets = tc.desired, tc.applied
		if got := op.Drifted(); got != tc.drifted {
			t.Fatalf("Drifted(%v vs %v) = %v, want %v", tc.desired, tc.applied, got, tc.drifted)
		}
	}
}

// Fast-path UnmarshalJSON: при наличии нового формата (desired_targets)
// legacy-поля игнорируются (раньше ошибочный '&&' заставлял перечитывать
// legacy и мог втянуть stale applied_target).
func TestDNSOperationUnmarshalPrefersNewFormat(t *testing.T) {
	b := []byte(`{"desired_type":"A","desired_targets":["1.1.1.1"],"applied_target":"2.2.2.2"}`)
	var op DNSOperation
	if err := json.Unmarshal(b, &op); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(op.DesiredTargets) != 1 || op.DesiredTargets[0] != "1.1.1.1" {
		t.Fatalf("desired_targets = %v, want [1.1.1.1]", op.DesiredTargets)
	}
	if len(op.AppliedTargets) != 0 {
		t.Fatalf("new-format record must not absorb legacy applied_target, got %v", op.AppliedTargets)
	}
	// истинно legacy-запись (нет новых списков) по-прежнему мигрируется
	b2 := []byte(`{"desired_type":"A","desired_target":"3.3.3.3"}`)
	var op2 DNSOperation
	if err := json.Unmarshal(b2, &op2); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if len(op2.DesiredTargets) != 1 || op2.DesiredTargets[0] != "3.3.3.3" {
		t.Fatalf("legacy migration failed: %v", op2.DesiredTargets)
	}
}
