package blueprint

import (
	"encoding/json"
	"testing"
)

func TestParseRejectsUnknownAndTrailing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown top-level", `{"schema_version":1,"name":"n","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[],"edges":[],"bogus":1}`},
		{"unknown node field", `{"schema_version":1,"name":"n","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{},"x":1}],"edges":[]}`},
		{"trailing json", `{"schema_version":1,"name":"n","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[],"edges":[]}{}`},
		{"unknown binding field", `{"schema_version":1,"name":"n","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{"x":{"bogus":1}}}],"edges":[]}`},
	}
	for _, tc := range cases {
		if _, err := Parse([]byte(tc.raw)); err == nil {
			t.Fatalf("%s: expected parse error, got nil", tc.name)
		}
	}
}

func TestParseAcceptsMinimal(t *testing.T) {
	bp, err := Parse([]byte(`{"schema_version":1,"name":"n","provider":"nodehost","inputs":{},"entries":{"initial":"a","observe":"b"},"nodes":[{"id":"a","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}},{"id":"b","type":"terminal.stop","params":{"reason":"policy_inactive"},"in":{}}],"edges":[]}`))
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if bp.SchemaVersion != 1 || len(bp.Nodes) != 2 {
		t.Fatalf("unexpected blueprint: %+v", bp)
	}
}

func TestCanonicalHashStableAcrossMapOrder(t *testing.T) {
	a := Blueprint{SchemaVersion: 1, Name: "n", Provider: ProviderNodehost, Inputs: map[string]Input{"a": {Type: TypeNumber}, "b": {Type: TypeString}}, Entries: Entries{Initial: "x", Observe: "y"}}
	b := Blueprint{SchemaVersion: 1, Name: "n", Provider: ProviderNodehost, Inputs: map[string]Input{"b": {Type: TypeString}, "a": {Type: TypeNumber}}, Entries: Entries{Initial: "x", Observe: "y"}}
	ha, err := CanonicalHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := CanonicalHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Fatalf("canonical hash not stable: %s != %s", ha, hb)
	}
	if len(ha) != 64 {
		t.Fatalf("expected sha256 hex, got %q", ha)
	}
}

func TestBindingOneOf(t *testing.T) {
	var b Binding
	if err := json.Unmarshal([]byte(`{"ref":"input.a"}`), &b); err != nil {
		t.Fatal(err)
	}
	if b.Ref != "input.a" || len(b.Literal) != 0 {
		t.Fatalf("bad ref binding: %+v", b)
	}
}
