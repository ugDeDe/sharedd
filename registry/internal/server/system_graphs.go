package server

import (
	"errors"
	"log"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
)

// seedSystemGraphs installs the embedded graphs at startup. Idempotent: an
// existing profile/version is never overwritten (content is immutable).
func (r *Registry) seedSystemGraphs() {
	if r.control == nil {
		return
	}
	for _, g := range blueprint.SystemGraphs() {
		if g.ContentHash == "" || g.SchemaVersion <= 0 {
			log.Printf("system graph %q is invalid; skipping seed", g.Name)
			continue
		}
		if _, err := r.control.EnsureSystemGraphProfile(g.Name, g.Description, string(g.JSON), g.SchemaVersion, g.ContentHash); err != nil {
			log.Printf("system graph %q seed failed: %v", g.Name, err)
		}
	}
}

// systemGraphVersionID resolves a pinned system graph version for fleet policy
// arming. A missing seed is an operational error, never a silent default.
func (r *Registry) systemGraphVersionID(name string) (int64, error) {
	if r.control == nil {
		return 0, control.ErrNotFound
	}
	id, err := r.control.SystemGraphVersionID(name)
	if errors.Is(err, control.ErrNotFound) {
		r.seedSystemGraphs() // self-heal after a botched startup seed
		id, err = r.control.SystemGraphVersionID(name)
	}
	return id, err
}
