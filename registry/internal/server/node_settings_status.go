package server

import (
	"errors"
	"net/http"
	"strings"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/machineapi"
)

// POST /api/node/settings/status — агент подтверждает применение
// индивидуальных runtime-настроек. Личность подтверждается персональным
// node_secret; общий токен и произвольный X-ShareDD-Node-ID не принимаются.
func (r *Registry) handleNodeSettingsStatus(w http.ResponseWriter, req *http.Request) {
	var body machineapi.NodeSettingsStatus
	if err := decodeNodeJSON(w, req, &body); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	info := machineAuthFrom(req)
	if info.kind != machineAuthNode || info.ownerNodeID == "" || info.ownerNodeID != body.NodeID {
		controlAPIError(w, http.StatusForbidden, "node_id_mismatch", "personal node secret required for this node_id")
		return
	}
	if err := machineapi.ValidateNodeID(body.NodeID); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	if body.ProtocolVersion != 1 {
		controlAPIError(w, http.StatusBadRequest, "unsupported_protocol", "unsupported settings protocol")
		return
	}
	switch strings.TrimSpace(body.State) {
	case control.RuntimeApplied, control.RuntimeFailed, control.RuntimeUnsupported:
	default:
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid state")
		return
	}
	if body.AttemptedRevision < 0 || body.AppliedRevision < 0 || body.AppliedRevision > body.AttemptedRevision {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "invalid revisions")
		return
	}
	rt, err := r.control.ReportNodeRuntime(body.NodeID, body.AttemptedRevision, body.AppliedRevision, body.State, body.ErrorCode)
	if errors.Is(err, control.ErrNotFound) {
		controlAPIError(w, http.StatusNotFound, "not_found", "runtime settings are not managed for this node")
		return
	}
	if errors.Is(err, control.ErrValidation) {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
		return
	}
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	writeCabinetJSON(w, http.StatusOK, map[string]any{
		"node_id":          rt.NodeID,
		"desired_revision": rt.DesiredRevision,
		"applied_revision": rt.AppliedRevision,
		"state":            rt.State,
	})
}
