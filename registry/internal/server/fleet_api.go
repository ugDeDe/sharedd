package server

// Fleet autopilot API (docs/specs/hosting-blueprints/fleet-autopilot.md):
// POST /api/me/fleets accepts the whole batch, GET /api/me/fleets/{id} shows
// per-item progress and GET /api/me/fleets lists aggregates. All routes are
// session+CSRF; commands also require Idempotency-Key. beget → 501.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/nodehost"
)

type fleetCreateBody struct {
	ConnectionID  int64                   `json:"connection_id"`
	Name          string                  `json:"name"`
	Count         int                     `json:"count"`
	Configuration *nodehost.CreateRequest `json:"configuration"`
	// InstallationProfileID is authoritative. The legacy
	// installation_profile_version_id is resolved to its ProfileID.
	InstallationProfileID        int64 `json:"installation_profile_id"`
	InstallationProfileVersionID int64 `json:"installation_profile_version_id"`
	AutoReplace                  bool  `json:"auto_replace"`
}

func (r *Registry) mountFleets(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/me/fleets", r.requireSession(r.requireCSRF(r.handleFleetCreate)))
	mux.HandleFunc("GET /api/me/fleets", r.requireSession(r.handleFleetList))
	mux.HandleFunc("GET /api/me/fleets/{id}", r.requireSession(r.handleFleetGet))
}

// fleetLabels — "name-01".."name-NN". Name is capped so every label passes
// the node-id NAME prefix rule (1..10 chars before the generated hash).
func fleetLabels(name string, count int) ([]string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "fleet"
	}
	if len(name) > 7 || count < 1 || count > 20 {
		return nil, false
	}
	labels := make([]string, count)
	for i := 1; i <= count; i++ {
		label := fmt.Sprintf("%s-%02d", name, i)
		if validateNodeID(label+"-00000") != nil {
			return nil, false
		}
		labels[i-1] = label
	}
	return labels, true
}

// fleetProfileVersion — single content row of an installable text profile,
// own or published.
func (r *Registry) fleetProfileVersion(u control.User, profileID int64) (control.ProfileVersion, error) {
	if profileID <= 0 {
		return control.ProfileVersion{}, control.ErrValidation
	}
	p, err := r.control.Profile(profileID)
	if err != nil {
		return control.ProfileVersion{}, err
	}
	if !p.Published && p.CreatedBy != u.ID {
		return control.ProfileVersion{}, control.ErrNotFound
	}
	if !p.Published && !r.userProfilesEnabled() {
		return control.ProfileVersion{}, errUserProfilesDisabled
	}
	pv, err := r.control.ProfileVersion(profileID, 0)
	if err != nil {
		return control.ProfileVersion{}, err
	}
	if pv.Format != "" && pv.Format != control.FormatText {
		return control.ProfileVersion{}, control.ErrValidation
	}
	return pv, nil
}

func writeFleetProfileErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUserProfilesDisabled):
		controlAPIError(w, http.StatusForbidden, "user_profiles_disabled", "user profiles are disabled")
	case errors.Is(err, control.ErrNotFound), errors.Is(err, control.ErrValidation):
		controlAPIError(w, http.StatusUnprocessableEntity, "profile_invalid", "profile version is not an installable text version")
	default:
		controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
	}
}

func (r *Registry) handleFleetCreate(w http.ResponseWriter, req *http.Request) {
	if !r.requireMasterKey(w) {
		return
	}
	idemKey, ok := cloudIdemKey(w, req)
	if !ok {
		return
	}
	var body fleetCreateBody
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	// Имя каждого элемента задаётся флотом (base-01…), поэтому пустое
	// configuration.name допустимо: подставляем base-name для валидации.
	cfgForValidation := body.Configuration
	if cfgForValidation != nil && strings.TrimSpace(cfgForValidation.Name) == "" {
		copied := *cfgForValidation
		if n := strings.TrimSpace(body.Name); n != "" {
			copied.Name = n
		} else {
			copied.Name = "node"
		}
		cfgForValidation = &copied
	}
	if body.Count < 1 || body.Count > 20 || cfgForValidation == nil || !validCloudConfig(*cfgForValidation) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid fleet size or configuration")
		return
	}
	labels, ok := fleetLabels(body.Name, body.Count)
	if !ok {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "name must be 1..7 characters and yield valid node labels")
		return
	}
	u := sessionFrom(req).user
	conn, err := r.control.HostingConnectionOwned(body.ConnectionID, u.ID)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	if conn.Provider != control.ProviderNodehost {
		controlAPIError(w, http.StatusNotImplemented, "not_implemented", "beget fleets are not implemented")
		return
	}
	profileID := body.InstallationProfileID
	if profileID == 0 && body.InstallationProfileVersionID > 0 {
		row, err := r.control.ProfileVersionByNumericID(body.InstallationProfileVersionID)
		if err != nil {
			writeFleetProfileErr(w, err)
			return
		}
		profileID = row.ProfileID
	}
	pv, err := r.fleetProfileVersion(u, profileID)
	if err != nil {
		writeFleetProfileErr(w, err)
		return
	}
	cfgJSON, err := json.Marshal(body.Configuration)
	if err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "configuration is not JSON-serialisable")
		return
	}

	if prev, err := r.control.FleetByKey(u.ID, idemKey); err == nil {
		if !fleetReplayMatches(prev, body, pv.ID, string(cfgJSON)) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused with a different request body")
			return
		}
		if prev.State == control.FleetRunning {
			r.startFleetWorker(prev.ID) // resume after a crash between persist and launch
		}
		writeCabinetJSON(w, http.StatusAccepted, map[string]int64{"fleet_id": prev.ID})
		return
	} else if !errors.Is(err, control.ErrNotFound) {
		r.writeControlUnavailable(w)
		return
	}

	f, created, err := r.control.CreateFleet(control.Fleet{
		UserID: u.ID, ConnectionID: conn.ID, Count: body.Count, AutoReplace: body.AutoReplace,
		// With in-place edits the content row ID is stable, so it identifies
		// the profile for the fleet worker.
		ProfileVersionID: pv.ID, ConfigurationJSON: string(cfgJSON), FleetKey: idemKey,
		State: control.FleetRunning,
	}, labels)
	if err != nil {
		controlAPIError(w, http.StatusServiceUnavailable, "control_unavailable", controlUnavailableMsg)
		return
	}
	if !created {
		if !fleetReplayMatches(f, body, pv.ID, string(cfgJSON)) {
			controlAPIError(w, http.StatusConflict, "idempotency_conflict", "idempotency key was reused with a different request body")
			return
		}
	} else {
		r.startFleetWorker(f.ID)
	}
	writeCabinetJSON(w, http.StatusAccepted, map[string]int64{"fleet_id": f.ID})
}

func fleetReplayMatches(f control.Fleet, body fleetCreateBody, pvID int64, cfgJSON string) bool {
	return f.ConnectionID == body.ConnectionID && f.Count == body.Count &&
		f.AutoReplace == body.AutoReplace && f.ProfileVersionID == pvID &&
		f.ConfigurationJSON == cfgJSON
}

// fleetIP — current IP of the item's resource (empty when missing/deleted).
func (r *Registry) fleetIP(f control.Fleet, resourceID int64) string {
	if resourceID <= 0 {
		return ""
	}
	res, err := r.control.CloudResourceOwned(resourceID, f.UserID)
	if err != nil {
		return ""
	}
	return res.IP
}

type fleetItemJSON struct {
	Idx        int    `json:"idx"`
	Label      string `json:"label"`
	ResourceID *int64 `json:"resource_id"`
	NodeID     string `json:"node_id"`
	State      string `json:"state"`
	LastError  string `json:"last_error"`
	IP         string `json:"ip"`
}

type fleetDetailJSON struct {
	ID          int64           `json:"id"`
	State       string          `json:"state"`
	Count       int             `json:"count"`
	AutoReplace bool            `json:"auto_replace"`
	CreatedAt   int64           `json:"created_at"`
	Items       []fleetItemJSON `json:"items"`
}

func (r *Registry) handleFleetGet(w http.ResponseWriter, req *http.Request) {
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "fleet is not owned by the current user")
		return
	}
	f, err := r.control.FleetOwned(id, sessionFrom(req).user.ID)
	if err != nil {
		cloudControlErr(w, err)
		return
	}
	items, err := r.control.FleetItems(f.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := fleetDetailJSON{ID: f.ID, State: f.State, Count: f.Count, AutoReplace: f.AutoReplace, CreatedAt: f.CreatedAt}
	out.Items = make([]fleetItemJSON, 0, len(items))
	for _, it := range items {
		ij := fleetItemJSON{Idx: it.Idx, Label: it.Label, NodeID: it.NodeID, State: it.State, LastError: it.LastError, IP: r.fleetIP(f, it.ResourceID)}
		if it.ResourceID > 0 {
			x := it.ResourceID
			ij.ResourceID = &x
		}
		out.Items = append(out.Items, ij)
	}
	writeCabinetJSON(w, http.StatusOK, out)
}

type fleetSummaryJSON struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	Count       int    `json:"count"`
	AutoReplace bool   `json:"auto_replace"`
	CreatedAt   int64  `json:"created_at"`
	Succeeded   int    `json:"succeeded"`
	Errors      int    `json:"errors"`
}

func (r *Registry) handleFleetList(w http.ResponseWriter, req *http.Request) {
	fleets, err := r.control.FleetsByUser(sessionFrom(req).user.ID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	out := make([]fleetSummaryJSON, 0, len(fleets))
	for _, f := range fleets {
		items, err := r.control.FleetItems(f.ID)
		if err != nil {
			r.writeControlUnavailable(w)
			return
		}
		s := fleetSummaryJSON{ID: f.ID, State: f.State, Count: f.Count, AutoReplace: f.AutoReplace, CreatedAt: f.CreatedAt}
		for _, it := range items {
			switch it.State {
			case control.FleetItemAdmitted:
				s.Succeeded++
			case control.FleetItemError:
				s.Errors++
			}
		}
		out = append(out, s)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{"fleets": out})
}
