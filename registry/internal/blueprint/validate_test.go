package blueprint

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func nd(id, typ, params string, in map[string]Binding) Node {
	return Node{ID: id, Type: typ, Params: json.RawMessage(params), In: in}
}

func ref(r string) Binding { return Binding{Ref: r} }
func lit(v string) Binding { return Binding{Literal: json.RawMessage(v)} }

func edge(from, port, to string) Edge { return Edge{From: from, Port: port, To: to} }

// validBP is a minimal nodehost blueprint that must validate cleanly.
func validBP() Blueprint {
	return Blueprint{
		SchemaVersion: 1, Name: "t", Provider: ProviderNodehost,
		Inputs:  map[string]Input{"a": {Type: TypeNumber}, "c": {Type: TypeConfig}},
		Entries: Entries{Initial: "create", Observe: "watch"},
		Nodes: []Node{
			nd("create", "server.create", `{"purpose":"initial"}`, map[string]Binding{"connection": ref("input.a"), "configuration": ref("input.c")}),
			nd("watch", "event.ip_ban", `{}`, map[string]Binding{"server": ref("run.current_server")}),
		},
		Edges: []Edge{},
	}
}

func hasCode(ds []Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestValidBaseClean(t *testing.T) {
	if ds := Validate(validBP()); len(ds) != 0 {
		t.Fatalf("expected clean validation, got %+v", ds)
	}
}

func TestStructuralCases(t *testing.T) {
	nodeCount129 := func() Blueprint {
		bp := validBP()
		for i := 0; i < 127; i++ {
			bp.Nodes = append(bp.Nodes, nd(string(rune('a'+i%22))+itoa(i), "terminal.stop", `{"reason":"policy_inactive"}`, map[string]Binding{}))
		}
		return bp
	}
	cases := []struct {
		name string
		bp   Blueprint
		code string
	}{
		{"unknown block", func() Blueprint { bp := validBP(); bp.Nodes[1].Type = "bogus"; return bp }(), "unknown_block"},
		{"unknown param", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].Params = json.RawMessage(`{"purpose":"initial","x":1}`)
			return bp
		}(), "unknown_param"},
		{"missing param", func() Blueprint { bp := validBP(); bp.Nodes[0].Params = json.RawMessage(`{}`); return bp }(), "missing_param"},
		{"bad param value", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].Params = json.RawMessage(`{"purpose":"other"}`)
			return bp
		}(), "bad_param_value"},
		{"129 nodes", nodeCount129(), "node_count"},
		{">256 edges", func() Blueprint {
			bp := validBP()
			for i := 0; i < 257; i++ {
				bp.Edges = append(bp.Edges, edge("create", "ok", "watch"))
			}
			return bp
		}(), "too_many_edges"},
		{"binding both", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].In["connection"] = Binding{Literal: json.RawMessage(`1`), Ref: "input.a"}
			return bp
		}(), "bad_binding"},
		{"wrong literal type", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].In["connection"] = lit(`"x"`)
			return bp
		}(), "literal_type_mismatch"},
		{"literal handle forbidden", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].In["configuration"] = lit(`{"x":1}`)
			return bp
		}(), "literal_handle_forbidden"},
		{"cycle", func() Blueprint {
			bp := validBP()
			bp.Edges = []Edge{edge("create", "ok", "watch"), edge("watch", "ok", "create")}
			return bp
		}(), "cycle"},
		{"fan-out", func() Blueprint {
			bp := validBP()
			bp.Nodes = append(bp.Nodes, nd("watch2", "event.ip_ban", `{}`, map[string]Binding{"server": ref("run.current_server")}))
			bp.Edges = []Edge{edge("create", "ok", "watch"), edge("create", "ok", "watch2")}
			return bp
		}(), "fan_out"},
		{"unreachable", func() Blueprint {
			bp := validBP()
			bp.Nodes = append(bp.Nodes, nd("orphan", "terminal.stop", `{"reason":"policy_inactive"}`, map[string]Binding{}))
			return bp
		}(), "unreachable"},
		{"dangling ref", func() Blueprint { bp := validBP(); bp.Nodes[0].In["connection"] = ref("ghost.x"); return bp }(), "dangling_ref"},
		{"missing entry", func() Blueprint { bp := validBP(); bp.Entries.Observe = "nope"; return bp }(), "entry_not_found"},
		{"retry on mutation", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].Retry = &Retry{MaxAttempts: 2, BackoffSeconds: 1}
			return bp
		}(), "retry_not_allowed"},
		{"retry bounds", func() Blueprint {
			bp := validBP()
			bp.Nodes[0].Retry = &Retry{MaxAttempts: 9, BackoffSeconds: 0}
			return bp
		}(), "bad_retry"},
		{"reserved id", func() Blueprint {
			bp := validBP()
			bp.Nodes[1].ID = "run"
			bp.Entries.Observe = "run"
			return bp
		}(), "reserved_id"},
		{"duplicate node", func() Blueprint {
			bp := validBP()
			bp.Nodes = append(bp.Nodes, nd("create", "terminal.stop", `{"reason":"policy_inactive"}`, map[string]Binding{}))
			return bp
		}(), "duplicate_node"},
		{"provider mismatch", func() Blueprint {
			bp := validBP()
			bp.Nodes = append(bp.Nodes, nd("ipverify", "ip.verify", `{}`, map[string]Binding{"server": ref("run.current_server")}))
			bp.Entries.Observe = "watch"
			return bp
		}(), "provider_incompatible"},
		{"replacement without guard", func() Blueprint {
			bp := validBP()
			bp.Nodes[1] = nd("createNew", "server.create", `{"purpose":"replacement"}`, map[string]Binding{"connection": ref("input.a"), "configuration": ref("input.c")})
			bp.Entries.Observe = "createNew"
			return bp
		}(), "replacement_without_guard"},
		{"delete without fence", func() Blueprint {
			bp := validBP()
			bp.Nodes[1] = nd("del", "server.delete", `{}`, map[string]Binding{"server": ref("run.current_server")})
			bp.Entries.Observe = "del"
			return bp
		}(), "delete_without_fence"},
		{"cleanup without admit", func() Blueprint {
			bp := validBP()
			bp.Provider = ProviderBeget
			bp.Nodes[1] = nd("cleanup", "ip.cleanup", `{}`, map[string]Binding{"server": ref("run.current_server")})
			bp.Entries.Observe = "cleanup"
			return bp
		}(), "cleanup_without_admit"},
		{"runtime ref on initial", func() Blueprint {
			bp := validBP()
			bp.Entries.Initial = "watch"
			return bp
		}(), "runtime_ref_on_initial"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ds := Validate(tc.bp); !hasCode(ds, tc.code) {
				t.Fatalf("expected code %q, got %+v", tc.code, ds)
			}
		})
	}
}

func TestMissingRequiredInput(t *testing.T) {
	bp := validBP()
	bp.Inputs["a"] = Input{Type: TypeNumber, Required: true}
	// observe entry (watch) does not reference input.a.
	if ds := Validate(bp); !hasCode(ds, "missing_required_input") {
		t.Fatalf("expected missing_required_input, got %+v", ds)
	}
}

func TestInactiveBranchDependency(t *testing.T) {
	bp := Blueprint{
		SchemaVersion: 1, Name: "t", Provider: ProviderNodehost,
		Inputs:  map[string]Input{"a": {Type: TypeNumber}, "c": {Type: TypeConfig}},
		Entries: Entries{Initial: "create", Observe: "ban"},
		Nodes: []Node{
			nd("create", "server.create", `{"purpose":"initial"}`, map[string]Binding{"connection": ref("input.a"), "configuration": ref("input.c")}),
			nd("ban", "event.ip_ban", `{}`, map[string]Binding{"server": ref("run.current_server")}),
			nd("guard", "policy.guard", `{}`, map[string]Binding{"server": ref("ban.server"), "episode": ref("ban.episode")}),
			nd("admit", "pool.admit", `{"timeout_seconds":600}`, map[string]Binding{"server": ref("ban.server")}),
			nd("stop", "terminal.stop", `{"reason":"policy_inactive"}`, map[string]Binding{}),
			nd("arm", "observe.arm", `{}`, map[string]Binding{"server": ref("admit.server")}),
		},
		Edges: []Edge{
			edge("ban", "ok", "guard"),
			edge("guard", "active", "admit"),
			edge("guard", "stop", "arm"), // arm reads admit.server but is reached via the stop branch
			edge("admit", "ok", "stop"),
		},
	}
	if ds := Validate(bp); !hasCode(ds, "inactive_branch_dep") {
		t.Fatalf("expected inactive_branch_dep, got %+v", ds)
	}
}

func TestSizeLimit(t *testing.T) {
	raw, err := json.Marshal(validBP())
	if err != nil {
		t.Fatal(err)
	}
	padded := append([]byte(strings.Repeat(" ", MaxBytes+1)), raw...)
	if _, ds := ValidateBytes(padded); !hasCode(ds, "too_large") {
		t.Fatalf("expected too_large, got %+v", ds)
	}
}

func TestTemplatesValidate(t *testing.T) {
	cases := []struct {
		file string
		prov string
	}{
		{"testdata/nodehost_replacement.json", ProviderNodehost},
		{"testdata/beget_ip_or_replacement.json", ProviderBeget},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			bp, ds := ValidateBytes(data)
			if len(ds) != 0 {
				t.Fatalf("template must validate, got %+v", ds)
			}
			if bp.Provider != tc.prov {
				t.Fatalf("provider = %s, want %s", bp.Provider, tc.prov)
			}
		})
	}
}

func TestValidateInputs(t *testing.T) {
	bp := validBP()
	bp.Inputs["a"] = Input{Type: TypeNumber, Required: true}
	ds := ValidateInputs(bp, map[string]json.RawMessage{"a": json.RawMessage(`"x"`)})
	if !hasCode(ds, "input_value_type") {
		t.Fatalf("expected input_value_type, got %+v", ds)
	}
	ds = ValidateInputs(bp, map[string]json.RawMessage{})
	if !hasCode(ds, "missing_input_value") {
		t.Fatalf("expected missing_input_value, got %+v", ds)
	}
	ds = ValidateInputs(bp, map[string]json.RawMessage{"a": json.RawMessage(`1`), "z": json.RawMessage(`1`)})
	if !hasCode(ds, "unknown_input_value") {
		t.Fatalf("expected unknown_input_value, got %+v", ds)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
