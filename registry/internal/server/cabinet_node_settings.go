package server

// Параметры логической ноды в кабинете.
//
// Имя ноды — это имя слота (node_slots), а не технический node_id: обычное
// переименование не меняет identity, секрет, владельца, квоту и историю.
// ad_tag на работающей ноде пока не применяется агентом; API честно отвечает
// unsupported вместо ложного "сохранено". Быстрые параметры установки
// задаются в форме создания/переустановки.

import (
	"errors"
	"net/http"
	"strings"

	"sharedd/registry/internal/control"
)

type nodeRuntimeSettingJSON struct {
	State           string `json:"state"`
	Message         string `json:"message,omitempty"`
	Mode            string `json:"mode,omitempty"`
	Value           string `json:"value,omitempty"`
	DesiredRevision int64  `json:"desired_revision,omitempty"`
	AppliedRevision int64  `json:"applied_revision,omitempty"`
}

type nodeSettingsJSON struct {
	NodeID   string                            `json:"node_id"`
	SlotID   int64                             `json:"slot_id"`
	Number   int64                             `json:"number"`
	Name     string                            `json:"name"`
	Revision int64                             `json:"revision"`
	Runtime  map[string]nodeRuntimeSettingJSON `json:"runtime"`
}

func (r *Registry) mountNodeSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me/nodes/{node_id}/settings", r.requireSession(r.handleNodeSettingsGet))
	mux.HandleFunc("PATCH /api/me/nodes/{node_id}/settings", r.requireSession(r.requireCSRF(r.handleNodeSettingsPatch)))
}

func (r *Registry) nodeSettingsView(nodeID string) (nodeSettingsJSON, bool) {
	slot, ok, err := r.control.NodeSlotForNode(nodeID)
	if err != nil {
		return nodeSettingsJSON{}, false
	}
	if !ok {
		return nodeSettingsJSON{}, false
	}
	rt, err := r.control.RuntimeSettingsForNode(nodeID)
	if err != nil {
		return nodeSettingsJSON{}, false
	}
	ad := nodeRuntimeSettingJSON{
		State:           rt.State,
		Mode:            rt.AdTagMode,
		Value:           rt.AdTagValue,
		DesiredRevision: rt.DesiredRevision,
		AppliedRevision: rt.AppliedRevision,
	}
	switch rt.State {
	case control.RuntimeUnmanaged:
		ad.Message = "Агент не управляет рекламным тегом на этой ноде."
	case control.RuntimePending:
		ad.Message = "Изменение сохранено. Ожидается подтверждение применения агентом."
	case control.RuntimeApplied:
		ad.Message = "Применено на ноде."
	case control.RuntimeUnsupported:
		ad.Message = "Текущая версия агента/прокси не поддерживает изменение ad_tag на живой ноде. Задайте его при переустановке."
	case control.RuntimeFailed:
		ad.Message = "Не удалось применить. " + rt.ErrorCode
	}
	return nodeSettingsJSON{
		NodeID:   nodeID,
		SlotID:   slot.ID,
		Number:   slot.Number,
		Name:     slot.Name,
		Revision: slot.Revision,
		Runtime:  map[string]nodeRuntimeSettingJSON{"ad_tag": ad},
	}, true
}

func (r *Registry) handleNodeSettingsGet(w http.ResponseWriter, req *http.Request) {
	nodeID, ok := r.nodeOwnedMgmt(w, req)
	if !ok {
		return
	}
	view, ok := r.nodeSettingsView(nodeID)
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "у ноды нет логического слота")
		return
	}
	writeCabinetJSON(w, http.StatusOK, view)
}

func (r *Registry) handleNodeSettingsPatch(w http.ResponseWriter, req *http.Request) {
	nodeID, ok := r.nodeOwnedMgmt(w, req)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision int64          `json:"expected_revision"`
		Name             *string        `json:"name"`
		Runtime          map[string]any `json:"runtime"`
	}
	if err := decodeCabinetJSON(w, req, &body); err != nil {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "bad json")
		return
	}
	if body.ExpectedRevision <= 0 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "expected_revision is required")
		return
	}
	if body.Name == nil && len(body.Runtime) == 0 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "nothing to change")
		return
	}
	if len(body.Runtime) > 1 {
		controlAPIError(w, http.StatusBadRequest, "validation_failed", "unsupported runtime setting")
		return
	}
	var adTag *string
	if len(body.Runtime) == 1 {
		raw, ok := body.Runtime["ad_tag"]
		if !ok {
			controlAPIError(w, http.StatusBadRequest, "validation_failed", "only ad_tag is supported at runtime")
			return
		}
		s, ok := raw.(string)
		if !ok {
			controlAPIError(w, http.StatusBadRequest, "validation_failed", "ad_tag must be a string")
			return
		}
		adTag = &s
	}
	// Проверяем возможность применения до записи: classic/MEKO не умеют
	// менять ad_tag на живой ноде. Неизвестная/офлайн нода допускается —
	// агент сам подтвердит unsupported.
	if adTag != nil {
		r.mu.RLock()
		c := r.state.Candidates[nodeID]
		nodeType := ""
		if c != nil {
			nodeType = c.NodeType
		}
		r.mu.RUnlock()
		if nodeType != "" && nodeType != "mtproxyl" {
			controlAPIError(w, http.StatusUnprocessableEntity, "unsupported_setting",
				"эта нода не MTProxyL: изменение ad_tag на живой ноде не поддерживается. Задайте его при переустановке.")
			return
		}
	}
	slot, ok, err := r.control.NodeSlotForNode(nodeID)
	if err != nil {
		r.writeControlUnavailable(w)
		return
	}
	if !ok {
		controlAPIError(w, http.StatusNotFound, "not_found", "у ноды нет логического слота")
		return
	}
	if body.Name != nil {
		name := *body.Name
		if !manualLabelRe.MatchString(name) {
			controlAPIError(w, http.StatusBadRequest, "validation_failed",
				"название: 1..10 символов — латиница, цифры, . _ - (без . _ - по краям)")
			return
		}
		if _, err := r.control.RenameNodeSlot(slot.UserID, slot.ID, name, body.ExpectedRevision); err != nil {
			switch {
			case errors.Is(err, control.ErrRevisionConflict):
				controlAPIError(w, http.StatusConflict, "revision_conflict", "настройки уже изменились; обновите и повторите")
			case errors.Is(err, control.ErrNotFound):
				controlAPIError(w, http.StatusNotFound, "not_found", "логическая нода не найдена")
			default:
				controlAPIError(w, http.StatusBadRequest, "validation_failed", "не удалось изменить название")
			}
			return
		}
	}
	if adTag != nil {
		mode, value := control.AdTagSet, strings.TrimSpace(*adTag)
		if value == "" {
			mode = control.AdTagClear
		}
		if _, err := r.control.SetNodeAdTag(slot.UserID, nodeID, mode, value, 0); err != nil {
			switch {
			case errors.Is(err, control.ErrNotFound):
				controlAPIError(w, http.StatusNotFound, "not_found", "нода не найдена")
			case errors.Is(err, control.ErrValidation):
				controlAPIError(w, http.StatusBadRequest, "validation_failed", err.Error())
			default:
				r.writeControlUnavailable(w)
			}
			return
		}
	}
	view, _ := r.nodeSettingsView(nodeID)
	writeCabinetJSON(w, http.StatusOK, view)
}
