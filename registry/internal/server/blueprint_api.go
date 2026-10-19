package server

// Graph blueprint API (docs/specs/hosting-blueprints/03-cabinet-api.md §6,
// stage-08a): block registry, stateless graph validate, and immutable graph
// profile versions. No execution engine and no runs here (stage-08b).

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"sharedd/registry/internal/blueprint"
	"sharedd/registry/internal/control"
)

// maxGraphBodyBytes — graph validate/CRUD body cap (03 §2).
const maxGraphBodyBytes = 256 << 10

// decodeGraphJSONStrictLimit is the strict counterpart of
// decodeCabinetJSONLimit for the graph/profile authoring routes (03 §2):
// unknown envelope fields and any trailing JSON value are rejected. Local to
// these handlers so the shared permissive decoder used by legacy routes is
// untouched.
func decodeGraphJSONStrictLimit(w http.ResponseWriter, req *http.Request, dst any, max int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing json data")
	}
	return nil
}

func (r *Registry) mountBlueprintProfiles(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/blueprint-blocks", r.requireSession(r.handleBlueprintBlocks))
	mux.HandleFunc("POST /api/me/profiles/validate", r.requireSession(r.requireCSRF(r.handleBlueprintValidate)))
	mux.HandleFunc("GET /api/me/profiles/{id}/versions/{version}", r.requireSession(r.handleProfileVersionGet))
}

func emptyDiagnostics(ds []blueprint.Diagnostic) []blueprint.Diagnostic {
	if ds == nil {
		return []blueprint.Diagnostic{}
	}
	return ds
}

// GET /api/me/blueprint-blocks — allowlisted block registry (05 §4).
func (r *Registry) handleBlueprintBlocks(w http.ResponseWriter, req *http.Request) {
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"schema_version": blueprint.SchemaVersion,
		"blocks":         blueprint.Blocks(),
	})
}

type blueprintValidateBody struct {
	Format    string                     `json:"format"`
	Blueprint json.RawMessage            `json:"blueprint"`
	Inputs    map[string]json.RawMessage `json:"inputs"`
}

// POST /api/me/profiles/validate — no side effects (03 §6). Diagnostics are
// returned in a 200 DTO with valid:false, never as an error envelope.
func (r *Registry) handleBlueprintValidate(w http.ResponseWriter, req *http.Request) {
	var body blueprintValidateBody
	if err := decodeGraphJSONStrictLimit(w, req, &body, maxGraphBodyBytes); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.Format != control.FormatGraph {
		controlAPIError(w, http.StatusBadRequest, "unsupported_format", "only format \"graph\" is supported")
		return
	}
	bp, ds := blueprint.ValidateBytes(body.Blueprint)
	if len(ds) == 0 && body.Inputs != nil {
		ds = append(ds, blueprint.ValidateInputs(bp, body.Inputs)...)
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"valid":       len(ds) == 0,
		"diagnostics": emptyDiagnostics(ds),
	})
}

type profileVersionJSON struct {
	ProfileID        int64           `json:"profile_id"`
	Version          int             `json:"version"`
	ProfileVersionID int64           `json:"profile_version_id"`
	Format           string          `json:"format"`
	Steps            string          `json:"steps,omitempty"`
	Blueprint        json.RawMessage `json:"blueprint,omitempty"`
	VarsSchema       string          `json:"vars_schema,omitempty"`
}

// GET /api/me/profiles/{id}/versions/{version} — kept for old clients: the
// number is ignored and the current (single) content row is returned.
// Ownership: own profile or published profile; otherwise 404.
func (r *Registry) handleProfileVersionGet(w http.ResponseWriter, req *http.Request) {
	u := sessionFrom(req).user
	id, ok := parseID(req.PathValue("id"))
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "bad profile id")
		return
	}
	p, err := r.control.Profile(id)
	if err != nil {
		writeControlErr(w, err, "user.profiles.version.profile")
		return
	}
	if !p.Published && p.CreatedBy != u.ID {
		writeControlErr(w, control.ErrNotFound, "user.profiles.version.owner")
		return
	}
	v, err := r.control.ProfileVersion(id, 0)
	if err != nil {
		writeControlErr(w, err, "user.profiles.version.lookup")
		return
	}
	format := v.Format
	if format == "" {
		format = control.FormatText
	}
	out := profileVersionJSON{
		ProfileID: id, Version: v.Version, ProfileVersionID: v.ID,
		Format: format, VarsSchema: v.VarsSchema,
	}
	if format == control.FormatGraph {
		if json.Valid([]byte(v.Blueprint)) {
			out.Blueprint = json.RawMessage(v.Blueprint)
		}
	} else {
		out.Steps = v.Steps
	}
	writeCabinetJSON(w, http.StatusOK, out)
}

// writeBlueprintInvalid maps validator diagnostics onto a 422 response.
func writeBlueprintInvalid(w http.ResponseWriter, code, message string, ds []blueprint.Diagnostic) {
	writeCabinetJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"error": code, "message": message, "diagnostics": emptyDiagnostics(ds),
	})
}

// graphVersionDraft is a validated graph version ready to append.
type graphVersionDraft struct {
	canon         string
	schemaVersion int
	hash          string
}

// prepareGraphVersion validates raw and computes the canonical body + hash.
// On failure it writes the 422 response and returns ok=false; no mutation.
func prepareGraphVersion(w http.ResponseWriter, raw json.RawMessage, operation string) (graphVersionDraft, bool) {
	bp, ds := blueprint.ValidateBytes(raw)
	if len(ds) > 0 {
		writeBlueprintInvalid(w, "invalid_blueprint", "blueprint failed validation", ds)
		return graphVersionDraft{}, false
	}
	canon, err := blueprint.CanonicalBytes(bp)
	if err != nil {
		writeControlErr(w, err, operation+".canonical")
		return graphVersionDraft{}, false
	}
	hash, err := blueprint.CanonicalHash(bp)
	if err != nil {
		writeControlErr(w, err, operation+".hash")
		return graphVersionDraft{}, false
	}
	return graphVersionDraft{canon: string(canon), schemaVersion: bp.SchemaVersion, hash: hash}, true
}

// appendGraphVersion persists an already-validated draft. Returns false
// (response already written) on failure.
func (r *Registry) appendGraphVersion(w http.ResponseWriter, profileID int64, d graphVersionDraft, operation string) bool {
	if _, err := r.control.CreateProfileVersionGraph(profileID, d.canon, d.schemaVersion, d.hash); err != nil {
		writeControlErr(w, err, operation+".append")
		return false
	}
	return true
}

// addGraphVersion validates raw and writes it in place. Returns false
// (response already written) on failure.
func (r *Registry) addGraphVersion(w http.ResponseWriter, profileID int64, raw json.RawMessage, operation string) bool {
	d, ok := prepareGraphVersion(w, raw, operation)
	if !ok {
		return false
	}
	return r.appendGraphVersion(w, profileID, d, operation)
}
