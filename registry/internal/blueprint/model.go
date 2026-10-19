// Package blueprint implements the normative graph profile format v1
// (docs/specs/hosting-blueprints/05-blueprint-profiles.md) to the extent of
// stage-08a: the data model, the allowlisted block registry, structural +
// semantic validation and a canonical content hash. No execution engine.
package blueprint

import "encoding/json"

// SchemaVersion is the only accepted blueprint schema version (05 §3).
const SchemaVersion = 1

// Hard limits from 05 §3/§5.
const (
	MaxNodes        = 128
	MaxEdges        = 256 // control edges + node-output data bindings
	MaxBytes        = 256 << 10
	MaxInputs       = 32
	MaxNodeIn       = 32
	MaxRetryMax     = 5
	MinRetryMax     = 1
	MaxRetryBackoff = 300
)

// Data types (05 §2).
const (
	TypeString    = "string"
	TypeNumber    = "number"
	TypeBool      = "bool"
	TypeConfig    = "config"
	TypeServer    = "server"
	TypeOperation = "operation"
	TypeSSHTarget = "ssh_target"
	TypeSecretRef = "secretref"
)

// Providers.
const (
	ProviderNodehost = "nodehost"
	ProviderBeget    = "beget"
)

// Effect classes.
const (
	EffectRead     = "read"
	EffectMutation = "mutation"
	EffectPurchase = "purchase"
	EffectControl  = "control"
)

// Retry classes.
const (
	RetrySafeRead = "safe-read"
	RetryNone     = "none"
)

// Blueprint is the v1 structural model (05 §3).
type Blueprint struct {
	SchemaVersion int              `json:"schema_version"`
	Name          string           `json:"name"`
	Provider      string           `json:"provider"`
	Inputs        map[string]Input `json:"inputs"`
	Entries       Entries          `json:"entries"`
	Nodes         []Node           `json:"nodes"`
	Edges         []Edge           `json:"edges"`
}

// Entries names the initial and observe entry nodes (05 §3).
type Entries struct {
	Initial string `json:"initial"`
	Observe string `json:"observe"`
}

// Input is a declared typed run input (05 §2/§3).
type Input struct {
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// Node is a graph node (05 §3).
type Node struct {
	ID     string             `json:"id"`
	Type   string             `json:"type"`
	Params json.RawMessage    `json:"params"`
	In     map[string]Binding `json:"in"`
	Retry  *Retry             `json:"retry,omitempty"`
}

// Binding is a literal or a ref — exactly one (05 §2). Literal secrets and
// non-primitive handle literals are rejected by the validator.
type Binding struct {
	Literal json.RawMessage `json:"literal,omitempty"`
	Ref     string          `json:"ref,omitempty"`
}

// Retry is the bounded retry policy (05 §3).
type Retry struct {
	MaxAttempts    int `json:"max_attempts"`
	BackoffSeconds int `json:"backoff_seconds"`
}

// Edge is a control edge (05 §3).
type Edge struct {
	From string `json:"from"`
	Port string `json:"port"`
	To   string `json:"to"`
}

// Diagnostic is a validation finding (03 §6).
type Diagnostic struct {
	NodeID  string `json:"node_id,omitempty"`
	Port    string `json:"port,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
