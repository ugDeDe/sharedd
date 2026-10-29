package blueprint

// System graphs are embedded, versioned blueprints seeded into profiles at
// startup. They are the policies that the fleet autopilot pins: nodehost
// replacement is the only fleet graph (beget fleets are 501), while the beget
// graph stays embedded so the pair remains discoverable/validated together.

import (
	_ "embed"
)

const (
	SystemNodehostReplacement  = "system-nodehost-replacement"
	SystemBegetIPOrReplacement = "system-beget-ip-or-replacement"
)

//go:embed testdata/nodehost_replacement.json
var nodehostReplacementJSON []byte

//go:embed testdata/beget_ip_or_replacement.json
var begetIPOrReplacementJSON []byte

// SystemGraph is one embedded system blueprint ready to seed.
type SystemGraph struct {
	Name          string
	Description   string
	JSON          []byte
	SchemaVersion int
	ContentHash   string
}

// SystemGraphs returns both embedded graphs with canonical content hashes.
// Invalid embedded data is a programmer error; the caller still gets every
// graph so startup can log and skip only the broken one.
func SystemGraphs() []SystemGraph {
	return []SystemGraph{
		systemGraph(SystemNodehostReplacement, "Nodehost: замена узла при новом ip_ban", nodehostReplacementJSON),
		systemGraph(SystemBegetIPOrReplacement, "Beget: IPv4 либо связанная замена VPS", begetIPOrReplacementJSON),
	}
}

func systemGraph(name, description string, raw []byte) SystemGraph {
	g := SystemGraph{Name: name, Description: description, JSON: raw}
	bp, err := Parse(raw)
	if err != nil {
		return g
	}
	g.SchemaVersion = bp.SchemaVersion
	if hash, err := CanonicalHash(bp); err == nil {
		g.ContentHash = hash
	}
	if canon, err := CanonicalBytes(bp); err == nil {
		// Store the canonical form so equal graphs hash identically across hosts.
		g.JSON = canon
	}
	return g
}
