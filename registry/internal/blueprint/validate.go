package blueprint

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	idRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	portRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	refRe  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}\.[a-z][a-z0-9_]{0,63}$`)
)

var reservedIDs = map[string]bool{"input": true, "run": true}

// runtimeRefTypes are the only server-injected runtime refs (05 §2).
var runtimeRefTypes = map[string]string{
	"run.current_server":    TypeServer,
	"run.policy_revision":   TypeNumber,
	"run.episode_watermark": TypeString,
}

// Parse strictly decodes a blueprint (unknown fields and trailing data rejected).
func Parse(data []byte) (Blueprint, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var bp Blueprint
	if err := dec.Decode(&bp); err != nil {
		return Blueprint{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Blueprint{}, fmt.Errorf("trailing json data")
	}
	return bp, nil
}

// ValidateBytes enforces the size cap, parses and validates. Returns the parsed
// blueprint (zero value on parse failure).
func ValidateBytes(data []byte) (Blueprint, []Diagnostic) {
	if len(data) > MaxBytes {
		return Blueprint{}, []Diagnostic{{Code: "too_large", Message: fmt.Sprintf("blueprint exceeds %d bytes", MaxBytes)}}
	}
	bp, err := Parse(data)
	if err != nil {
		return Blueprint{}, []Diagnostic{{Code: "bad_json", Message: err.Error()}}
	}
	return bp, Validate(bp)
}

type diags struct{ list []Diagnostic }

func (d *diags) add(node, port, code, msg string) {
	d.list = append(d.list, Diagnostic{NodeID: node, Port: port, Code: code, Message: msg})
}

// Validate runs structural + semantic checks (05 §5) and returns diagnostics.
func Validate(bp Blueprint) []Diagnostic {
	d := &diags{}
	structural(bp, d)
	semantic(bp, d)
	sort.SliceStable(d.list, func(i, j int) bool {
		a, b := d.list[i], d.list[j]
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		return a.Code < b.Code
	})
	return d.list
}

func structural(bp Blueprint, d *diags) {
	if bp.SchemaVersion != SchemaVersion {
		d.add("", "", "schema_version", fmt.Sprintf("schema_version must be %d", SchemaVersion))
	}
	if n := utf8.RuneCountInString(bp.Name); n < 1 || n > 100 {
		d.add("", "", "name", "name must be 1..100 characters")
	}
	if bp.Provider != ProviderNodehost && bp.Provider != ProviderBeget {
		d.add("", "", "provider", "provider must be nodehost or beget")
	}
	if len(bp.Inputs) > MaxInputs {
		d.add("", "", "too_many_inputs", fmt.Sprintf("at most %d inputs", MaxInputs))
	}
	for name, in := range bp.Inputs {
		if !idRe.MatchString(name) {
			d.add("", "", "bad_input_name", fmt.Sprintf("bad input name %q", name))
		}
		if !IsDatatype(in.Type) {
			d.add("", "", "bad_input_type", fmt.Sprintf("bad type %q for input %q", in.Type, name))
		}
	}
	if !idRe.MatchString(bp.Entries.Initial) {
		d.add("", "", "bad_entry", "entries.initial is not a valid id")
	}
	if !idRe.MatchString(bp.Entries.Observe) {
		d.add("", "", "bad_entry", "entries.observe is not a valid id")
	}
	if len(bp.Nodes) < 1 || len(bp.Nodes) > MaxNodes {
		d.add("", "", "node_count", fmt.Sprintf("nodes must be 1..%d", MaxNodes))
	}
	seen := map[string]bool{}
	for _, n := range bp.Nodes {
		if !idRe.MatchString(n.ID) {
			d.add(n.ID, "", "bad_node_id", fmt.Sprintf("bad node id %q", n.ID))
		}
		if reservedIDs[n.ID] {
			d.add(n.ID, "", "reserved_id", fmt.Sprintf("id %q is reserved", n.ID))
		}
		if seen[n.ID] {
			d.add(n.ID, "", "duplicate_node", fmt.Sprintf("duplicate node id %q", n.ID))
		}
		seen[n.ID] = true
		blk, ok := BlockFor(n.Type)
		if !ok {
			d.add(n.ID, "", "unknown_block", fmt.Sprintf("unknown block type %q", n.Type))
		} else {
			checkParams(n, blk, d)
		}
		if len(n.In) > MaxNodeIn {
			d.add(n.ID, "", "too_many_bindings", fmt.Sprintf("at most %d bindings", MaxNodeIn))
		}
		for name, b := range n.In {
			hasLit := len(b.Literal) > 0
			hasRef := b.Ref != ""
			if hasLit == hasRef {
				d.add(n.ID, name, "bad_binding", "binding must be exactly one of literal or ref")
			}
		}
		if n.Retry != nil {
			if n.Retry.MaxAttempts < MinRetryMax || n.Retry.MaxAttempts > MaxRetryMax {
				d.add(n.ID, "", "bad_retry", "max_attempts must be 1..5")
			}
			if n.Retry.BackoffSeconds < 1 || n.Retry.BackoffSeconds > MaxRetryBackoff {
				d.add(n.ID, "", "bad_retry", "backoff_seconds must be 1..300")
			}
		}
	}
	// Edges: exist, valid port of the source block, single fan-out per output.
	seenOut := map[string]bool{}
	total := len(bp.Edges)
	for _, e := range bp.Edges {
		if !portRe.MatchString(e.Port) {
			d.add(e.From, e.Port, "bad_port", fmt.Sprintf("bad port %q", e.Port))
		}
		if !nodeExists(bp, e.From) || !nodeExists(bp, e.To) {
			d.add(e.From, e.Port, "dangling_edge", "edge references a missing node")
		}
		if blk, ok := blockOf(bp, e.From); ok {
			if !slices.Contains(blk.Ports.Control, e.Port) {
				d.add(e.From, e.Port, "unknown_control_port", fmt.Sprintf("block %s has no control port %q", e.From, e.Port))
			}
		}
		key := e.From + "\x00" + e.Port
		if seenOut[key] {
			d.add(e.From, e.Port, "fan_out", "a control output may have at most one edge")
		}
		seenOut[key] = true
	}
	for _, n := range bp.Nodes {
		for _, b := range n.In {
			if b.Ref != "" && !strings.HasPrefix(b.Ref, "input.") && !strings.HasPrefix(b.Ref, "run.") {
				total++
			}
		}
	}
	if total > MaxEdges {
		d.add("", "", "too_many_edges", fmt.Sprintf("at most %d edges including data bindings", MaxEdges))
	}
}

func checkParams(n Node, blk Block, d *diags) {
	if len(blk.ParamsSchema) == 0 {
		// No params allowed: require an empty object.
		var m map[string]json.RawMessage
		if len(n.Params) == 0 {
			d.add(n.ID, "", "missing_params", "params object is required")
			return
		}
		if err := json.Unmarshal(n.Params, &m); err != nil {
			d.add(n.ID, "", "bad_params", "params must be an object")
			return
		}
		if len(m) != 0 {
			d.add(n.ID, "", "unknown_param", "block accepts no params")
		}
		return
	}
	var m map[string]json.RawMessage
	if len(n.Params) == 0 {
		d.add(n.ID, "", "missing_params", "params object is required")
		return
	}
	if err := json.Unmarshal(n.Params, &m); err != nil {
		d.add(n.ID, "", "bad_params", "params must be an object")
		return
	}
	declared := map[string]Param{}
	for _, p := range blk.ParamsSchema {
		declared[p.Name] = p
	}
	for k := range m {
		if _, ok := declared[k]; !ok {
			d.add(n.ID, "", "unknown_param", fmt.Sprintf("unknown param %q", k))
		}
	}
	for _, p := range blk.ParamsSchema {
		raw, ok := m[p.Name]
		if !ok {
			if p.Required {
				d.add(n.ID, "", "missing_param", fmt.Sprintf("param %q is required", p.Name))
			}
			continue
		}
		if code := checkParamValue(p, raw); code != "" {
			d.add(n.ID, "", code, fmt.Sprintf("param %q: %s", p.Name, code))
		}
	}
}

func checkParamValue(p Param, raw json.RawMessage) string {
	switch p.Type {
	case "integer":
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "bad_param_type"
		}
		if _, err := n.Int64(); err != nil {
			return "bad_param_type"
		}
	case "string":
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "bad_param_type"
		}
		if len(p.Enum) > 0 && !slices.Contains(p.Enum, s) {
			return "bad_param_value"
		}
	}
	return ""
}

// ── semantic ──────────────────────────────────────────────────────

func semantic(bp Blueprint, d *diags) {
	// typed data refs / literals
	for _, n := range bp.Nodes {
		blk, ok := BlockFor(n.Type)
		if !ok {
			continue
		}
		if !slices.Contains(blk.Capabilities, bp.Provider) {
			d.add(n.ID, "", "provider_incompatible", fmt.Sprintf("block %s is not available for provider %s", n.Type, bp.Provider))
		}
		if n.Retry != nil && blk.RetryClass != RetrySafeRead {
			d.add(n.ID, "", "retry_not_allowed", fmt.Sprintf("retry is not allowed for effect class %s", blk.Effects))
		}
		inPorts := portMap(blk.Ports.Inputs)
		for name, b := range n.In {
			port, declared := inPorts[name]
			if !declared {
				d.add(n.ID, name, "unknown_input", fmt.Sprintf("block %s has no data input %q", n.Type, name))
				continue
			}
			if b.Ref != "" {
				checkRefType(bp, n.ID, name, port.Type, b.Ref, d)
			} else if len(b.Literal) > 0 {
				checkLiteral(n.ID, name, port.Type, b.Literal, d)
			}
		}
	}

	if !nodeExists(bp, bp.Entries.Initial) {
		d.add("", "", "entry_not_found", "entries.initial does not reference a node")
	}
	if !nodeExists(bp, bp.Entries.Observe) {
		d.add("", "", "entry_not_found", "entries.observe does not reference a node")
	}

	adj, preds := controlGraph(bp)
	if hasCycle(bp, adj) {
		d.add("", "", "cycle", "control/data dependencies must form a DAG")
	}

	reachI := reach(bp.Entries.Initial, adj)
	reachO := reach(bp.Entries.Observe, adj)
	for _, n := range bp.Nodes {
		if !reachI[n.ID] && !reachO[n.ID] {
			d.add(n.ID, "", "unreachable", "node is not reachable from either entry")
		}
	}

	for _, entry := range []struct {
		root      string
		isInitial bool
	}{{bp.Entries.Initial, true}, {bp.Entries.Observe, false}} {
		root := entry.root
		if !nodeExists(bp, root) {
			continue
		}
		R := reach(root, adj)
		dom := dominators(root, preds, R)
		checkEntry(bp, root, R, dom, entry.isInitial, d)
	}

	checkCleanupToCreate(bp, adj, d)
}

func checkEntry(bp Blueprint, root string, R map[string]bool, dom map[string]map[string]bool, isInitial bool, d *diags) {
	// Every required declared input must be sourced inside this entry subtree.
	for name, in := range bp.Inputs {
		if !in.Required {
			continue
		}
		if !entryRefsInput(bp, R, name) {
			d.add(root, "", "missing_required_input", fmt.Sprintf("required input %q is not bound in this entry", name))
		}
	}
	for _, n := range bp.Nodes {
		if !R[n.ID] {
			continue
		}
		for name, b := range n.In {
			if b.Ref == "" || strings.HasPrefix(b.Ref, "input.") {
				continue
			}
			if strings.HasPrefix(b.Ref, "run.") {
				if isInitial && b.Ref == "run.current_server" {
					d.add(n.ID, name, "runtime_ref_on_initial", "run.current_server is not available on the initial entry")
				}
				continue
			}
			prod, _, ok := splitRef(b.Ref)
			if !ok {
				continue
			}
			if !R[prod] || !dom[n.ID][prod] {
				d.add(n.ID, name, "inactive_branch_dep", fmt.Sprintf("producer %q does not dominate this node on every path", prod))
			}
		}
		switch n.Type {
		case "server.create":
			if paramString(n, "purpose") == "replacement" && !dominatedType(bp, dom, n.ID, "policy.guard") {
				d.add(n.ID, "", "replacement_without_guard", "replacement create must be dominated by a policy.guard")
			}
		case "resource.fence":
			if !dominatedType(bp, dom, n.ID, "pool.admit") {
				d.add(n.ID, "", "fence_without_admit", "resource.fence requires a dominated pool.admit")
			}
		case "resource.switch":
			if !dominatedType(bp, dom, n.ID, "resource.fence") {
				d.add(n.ID, "", "switch_without_fence", "resource.switch requires a dominated resource.fence")
			}
		case "server.delete":
			if !dominatedTypeAny(bp, dom, n.ID, "resource.fence", "resource.switch") {
				d.add(n.ID, "", "delete_without_fence", "server.delete requires a dominated resource.fence or resource.switch")
			}
		case "ip.cleanup":
			if !dominatedType(bp, dom, n.ID, "pool.admit") {
				d.add(n.ID, "", "cleanup_without_admit", "ip.cleanup requires a dominated pool.admit")
			}
		}
	}
}

// checkCleanupToCreate rejects any control path from a cleanup node to a new create.
func checkCleanupToCreate(bp Blueprint, adj map[string][]string, d *diags) {
	for _, n := range bp.Nodes {
		if n.Type != "ip.cleanup" && n.Type != "server.absent" {
			continue
		}
		for reached := range reach(n.ID, adj) {
			if reached == n.ID {
				continue
			}
			if blk, ok := blockOf(bp, reached); ok && blk.Type == "server.create" {
				d.add(n.ID, "", "cleanup_to_create", "cleanup must never lead back to a new purchase")
			}
		}
	}
}

func checkRefType(bp Blueprint, nodeID, input, want, ref string, d *diags) {
	switch {
	case strings.HasPrefix(ref, "input."):
		name := strings.TrimPrefix(ref, "input.")
		decl, ok := bp.Inputs[name]
		if !ok {
			d.add(nodeID, input, "unknown_input_ref", fmt.Sprintf("undeclared input ref %q", ref))
			return
		}
		if decl.Type != want {
			d.add(nodeID, input, "type_mismatch", fmt.Sprintf("input %q is %s, expects %s", name, decl.Type, want))
		}
	case strings.HasPrefix(ref, "run."):
		t, ok := runtimeRefTypes[ref]
		if !ok {
			d.add(nodeID, input, "unknown_runtime_ref", fmt.Sprintf("unknown runtime ref %q", ref))
			return
		}
		if t != want {
			d.add(nodeID, input, "type_mismatch", fmt.Sprintf("%s is %s, expects %s", ref, t, want))
		}
	default:
		if !refRe.MatchString(ref) {
			d.add(nodeID, input, "bad_ref", fmt.Sprintf("bad ref %q", ref))
			return
		}
		prod, port, ok := splitRef(ref)
		if !ok || !nodeExists(bp, prod) {
			d.add(nodeID, input, "dangling_ref", fmt.Sprintf("ref %q references a missing node", ref))
			return
		}
		blk, ok := blockOf(bp, prod)
		if !ok {
			return
		}
		outs := portMap(blk.Ports.Outputs)
		op, ok := outs[port]
		if !ok {
			d.add(nodeID, input, "dangling_ref", fmt.Sprintf("node %q has no output %q", prod, port))
			return
		}
		if op.Type != want {
			d.add(nodeID, input, "type_mismatch", fmt.Sprintf("%s is %s, expects %s", ref, op.Type, want))
		}
	}
}

func checkLiteral(nodeID, input, want string, raw json.RawMessage, d *diags) {
	if handleDatatype(want) {
		d.add(nodeID, input, "literal_handle_forbidden", fmt.Sprintf("type %s must be passed by ref, not literal", want))
		return
	}
	switch want {
	case TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			d.add(nodeID, input, "literal_type_mismatch", "literal is not a string")
		}
	case TypeNumber:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			d.add(nodeID, input, "literal_type_mismatch", "literal is not a number")
		}
	case TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			d.add(nodeID, input, "literal_type_mismatch", "literal is not a bool")
		}
	}
}

// ── graph helpers ─────────────────────────────────────────────────

func nodeExists(bp Blueprint, id string) bool {
	for _, n := range bp.Nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

func blockOf(bp Blueprint, id string) (Block, bool) {
	for _, n := range bp.Nodes {
		if n.ID == id {
			return BlockFor(n.Type)
		}
	}
	return Block{}, false
}

func nodeOf(bp Blueprint, id string) (Node, bool) {
	for _, n := range bp.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func portMap(ps []Port) map[string]Port {
	m := make(map[string]Port, len(ps))
	for _, p := range ps {
		m[p.Name] = p
	}
	return m
}

func splitRef(ref string) (node, port string, ok bool) {
	i := strings.IndexByte(ref, '.')
	if i <= 0 || i == len(ref)-1 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

func paramString(n Node, key string) string {
	var m map[string]any
	if err := json.Unmarshal(n.Params, &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// controlGraph returns the control adjacency and reverse (predecessor) maps.
func controlGraph(bp Blueprint) (adj, preds map[string][]string) {
	adj = map[string][]string{}
	preds = map[string][]string{}
	for _, e := range bp.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		preds[e.To] = append(preds[e.To], e.From)
	}
	return adj, preds
}

func reach(root string, adj map[string][]string) map[string]bool {
	seen := map[string]bool{}
	if root == "" {
		return seen
	}
	stack := []string{root}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] {
			continue
		}
		seen[n] = true
		stack = append(stack, adj[n]...)
	}
	return seen
}

// dominators computes, per reachable node, the set of nodes on every path from root.
func dominators(root string, preds map[string][]string, R map[string]bool) map[string]map[string]bool {
	dom := map[string]map[string]bool{}
	for n := range R {
		if n == root {
			dom[n] = map[string]bool{n: true}
		} else {
			dom[n] = map[string]bool{}
			for m := range R {
				dom[n][m] = true
			}
		}
	}
	changed := true
	for changed {
		changed = false
		for n := range R {
			if n == root {
				continue
			}
			var inter map[string]bool
			for _, p := range preds[n] {
				if !R[p] {
					continue
				}
				if inter == nil {
					inter = clone(dom[p])
					continue
				}
				for k := range inter {
					if !dom[p][k] {
						delete(inter, k)
					}
				}
			}
			if inter == nil {
				inter = map[string]bool{}
			}
			inter[n] = true
			if !sameSet(inter, dom[n]) {
				dom[n] = inter
				changed = true
			}
		}
	}
	return dom
}

func clone(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func dominatedType(bp Blueprint, dom map[string]map[string]bool, nodeID, typ string) bool {
	for _, n := range bp.Nodes {
		if n.Type == typ && n.ID != nodeID && dom[nodeID][n.ID] {
			return true
		}
	}
	return false
}

func dominatedTypeAny(bp Blueprint, dom map[string]map[string]bool, nodeID string, types ...string) bool {
	for _, t := range types {
		if dominatedType(bp, dom, nodeID, t) {
			return true
		}
	}
	return false
}

func entryRefsInput(bp Blueprint, R map[string]bool, name string) bool {
	want := "input." + name
	for _, n := range bp.Nodes {
		if !R[n.ID] {
			continue
		}
		for _, b := range n.In {
			if b.Ref == want {
				return true
			}
		}
	}
	return false
}

// hasCycle detects cycles over combined control edges and node-output data refs.
func hasCycle(bp Blueprint, adj map[string][]string) bool {
	edges := map[string][]string{}
	for k, v := range adj {
		edges[k] = append(edges[k], v...)
	}
	for _, n := range bp.Nodes {
		for _, b := range n.In {
			if b.Ref == "" || strings.HasPrefix(b.Ref, "input.") || strings.HasPrefix(b.Ref, "run.") {
				continue
			}
			prod, _, ok := splitRef(b.Ref)
			if ok {
				edges[prod] = append(edges[prod], n.ID)
			}
		}
	}
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var visit func(string) bool
	visit = func(n string) bool {
		color[n] = gray
		for _, m := range edges[n] {
			switch color[m] {
			case gray:
				return true
			case white:
				if visit(m) {
					return true
				}
			}
		}
		color[n] = black
		return false
	}
	for _, n := range bp.Nodes {
		if color[n.ID] == white {
			if visit(n.ID) {
				return true
			}
		}
	}
	return false
}

// ValidateInputs checks provided run-input values against declared inputs (03 §6).
func ValidateInputs(bp Blueprint, inputs map[string]json.RawMessage) []Diagnostic {
	var d diags
	for name, in := range bp.Inputs {
		raw, ok := inputs[name]
		if !ok {
			if in.Required {
				d.add("", "", "missing_input_value", fmt.Sprintf("required input %q is missing", name))
			}
			continue
		}
		if code := checkInputValue(in.Type, raw); code != "" {
			d.add("", "", "input_value_type", fmt.Sprintf("input %q: %s", name, code))
		}
	}
	for name := range inputs {
		if _, ok := bp.Inputs[name]; !ok {
			d.add("", "", "unknown_input_value", fmt.Sprintf("input %q is not declared", name))
		}
	}
	sort.SliceStable(d.list, func(i, j int) bool { return d.list[i].Code < d.list[j].Code })
	return d.list
}

func checkInputValue(typ string, raw json.RawMessage) string {
	switch typ {
	case TypeNumber:
		var n json.Number
		if err := json.Unmarshal(raw, &n); err != nil {
			return "expected number"
		}
	case TypeString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "expected string"
		}
	case TypeBool:
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return "expected bool"
		}
	}
	return ""
}

// CanonicalBytes returns a canonical JSON encoding (sorted keys, compact) of bp.
//
// Limitation: json.RawMessage fields (Binding.Literal, Node.Params) are
// compacted but not re-normalised, so numerically-equal literals (1 vs 1.0)
// hash differently. In practice params/literals are scalars with ≤1 key, so
// this is negligible; revisit before using content_hash for dedup.
// ponytail: not fixed — canonicalising arbitrary RawMessage risks changing
// existing hashes.
func CanonicalBytes(bp Blueprint) ([]byte, error) {
	raw, err := json.Marshal(bp)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// CanonicalHash returns the sha256 hex of the canonical JSON encoding of bp.
func CanonicalHash(bp Blueprint) (string, error) {
	canon, err := CanonicalBytes(bp)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}
