package blueprint

import "sort"

// Port is a typed data input or output (05 §4).
type Port struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// BlockPorts groups the declared ports of a block.
type BlockPorts struct {
	Inputs  []Port   `json:"inputs"`
	Outputs []Port   `json:"outputs"`
	Control []string `json:"control"`
}

// Param is one strict block parameter declaration (05 §4).
type Param struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"` // string | integer
	Required bool     `json:"required"`
	Enum     []string `json:"enum,omitempty"`
}

// Block is one allowlisted block type (05 §4).
type Block struct {
	Type         string     `json:"type"`
	Ports        BlockPorts `json:"ports"`
	ParamsSchema []Param    `json:"params_schema"`
	Effects      string     `json:"effects"`
	RetryClass   string     `json:"retry_class"`
	Capabilities []string   `json:"capabilities"`
}

func in(name, typ string) Port  { return Port{Name: name, Type: typ} }
func out(name, typ string) Port { return Port{Name: name, Type: typ} }

func timeout() Param {
	// ponytail: single integer bound; if provider limits differ, split per block.
	return Param{Name: "timeout_seconds", Type: "integer", Required: true}
}

// blocks is the full §4 table. Control output defaults to ["ok"] unless noted.
var blocks = []Block{
	{
		Type:         "server.create",
		Ports:        BlockPorts{Inputs: []Port{in("connection", TypeNumber), in("configuration", TypeConfig)}, Outputs: []Port{out("operation", TypeOperation)}, Control: []string{"ok"}},
		ParamsSchema: []Param{{Name: "purpose", Type: "string", Required: true, Enum: []string{"initial", "replacement"}}},
		Effects:      EffectPurchase, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:         "server.ready",
		Ports:        BlockPorts{Inputs: []Port{in("operation", TypeOperation)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		ParamsSchema: []Param{timeout()}, Effects: EffectRead, RetryClass: RetrySafeRead,
		Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:         "install.text",
		Ports:        BlockPorts{Inputs: []Port{in("server", TypeServer), in("version", TypeNumber)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		ParamsSchema: []Param{timeout()}, Effects: EffectMutation, RetryClass: RetryNone,
		Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:         "pool.admit",
		Ports:        BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		ParamsSchema: []Param{timeout()}, Effects: EffectRead, RetryClass: RetrySafeRead,
		Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "event.ip_ban",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer), out("episode", TypeString)}, Control: []string{"ok"}},
		Effects: EffectRead, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "policy.guard",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer), in("episode", TypeString)}, Outputs: []Port{out("server", TypeServer), out("episode", TypeString)}, Control: []string{"active", "stop"}},
		Effects: EffectRead, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "server.distinct_ip",
		Ports:   BlockPorts{Inputs: []Port{in("old", TypeServer), in("new", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectRead, RetryClass: RetrySafeRead, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "resource.fence",
		Ports:   BlockPorts{Inputs: []Port{in("old", TypeServer), in("new", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "resource.switch",
		Ports:   BlockPorts{Inputs: []Port{in("old", TypeServer), in("new", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:    "server.delete",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:         "server.absent",
		Ports:        BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		ParamsSchema: []Param{timeout()}, Effects: EffectRead, RetryClass: RetrySafeRead,
		Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		// Control: none — 05 §4 marks the output "нет" (server → нет):
		// observe.arm ends the run as succeeded; it is terminal, no fan-out.
		Type:    "observe.arm",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Control: []string{}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
	{
		Type:         "ip.order",
		Ports:        BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok", "fallback"}},
		ParamsSchema: []Param{{Name: "family", Type: "string", Required: true, Enum: []string{"IP_V4"}}},
		Effects:      EffectPurchase, RetryClass: RetryNone, Capabilities: []string{ProviderBeget},
	},
	{
		Type:    "ip.attach",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok", "fallback"}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderBeget},
	},
	{
		Type:         "ip.switch_advertised",
		Ports:        BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok", "fallback"}},
		ParamsSchema: []Param{timeout()}, Effects: EffectMutation, RetryClass: RetryNone,
		Capabilities: []string{ProviderBeget},
	},
	{
		Type:    "ip.cleanup",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectMutation, RetryClass: RetryNone, Capabilities: []string{ProviderBeget},
	},
	{
		Type:    "ip.verify",
		Ports:   BlockPorts{Inputs: []Port{in("server", TypeServer)}, Outputs: []Port{out("server", TypeServer)}, Control: []string{"ok"}},
		Effects: EffectRead, RetryClass: RetrySafeRead, Capabilities: []string{ProviderBeget},
	},
	{
		Type:         "terminal.stop",
		Ports:        BlockPorts{Control: []string{}},
		ParamsSchema: []Param{{Name: "reason", Type: "string", Required: true, Enum: []string{"policy_inactive"}}},
		Effects:      EffectControl, RetryClass: RetryNone, Capabilities: []string{ProviderNodehost, ProviderBeget},
	},
}

var blockIndex = func() map[string]Block {
	m := make(map[string]Block, len(blocks))
	for _, b := range blocks {
		m[b.Type] = b
	}
	return m
}()

// Blocks returns every allowlisted block, sorted by type.
func Blocks() []Block {
	out := make([]Block, len(blocks))
	copy(out, blocks)
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// BlockFor returns the registry entry for a node type.
func BlockFor(typ string) (Block, bool) {
	b, ok := blockIndex[typ]
	return b, ok
}

// IsDatatype reports whether t is one of the 05 §2 data types.
func IsDatatype(t string) bool {
	switch t {
	case TypeString, TypeNumber, TypeBool, TypeConfig, TypeServer, TypeOperation, TypeSSHTarget, TypeSecretRef:
		return true
	}
	return false
}

// handleDatatypes are passed by ref only; a literal value is forbidden (05 §3).
func handleDatatype(t string) bool {
	switch t {
	case TypeConfig, TypeServer, TypeOperation, TypeSSHTarget, TypeSecretRef:
		return true
	}
	return false
}
