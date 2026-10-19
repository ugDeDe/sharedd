package server

// Shared acceptance path for installing an agent on a ready cloud resource.
// The HTTP route and the fleet worker both call startCloudInstall, so money
// and identity rules (claim-on-first-register, pending secret reuse, exact
// profile version) cannot drift between them.

import (
	"context"
	"errors"

	"sharedd/registry/internal/control"
	"sharedd/registry/internal/provision"
	"sharedd/registry/internal/sshx"
)

var errCloudResourceBusy = errors.New("resource busy")

// startCloudInstall validates the target, serializes acceptance with
// nodeClaimMu, takes the per-resource mutation guard, persists the operation
// and launches the install worker. It returns the accepted operation.
func (r *Registry) startCloudInstall(u control.User, res control.CloudResourceV2, pv control.ProfileVersion, userVars map[string]any) (control.Operation, error) {
	if res.Provider != control.ProviderNodehost {
		return control.Operation{}, errCloudNotImplemented
	}
	if res.State != control.CloudResReady {
		return control.Operation{}, control.ErrCreateInProgress
	}
	if validateNodeID(res.Label+"-00000") != nil {
		return control.Operation{}, control.ErrValidation
	}
	if pv.Format != "" && pv.Format != control.FormatText {
		return control.Operation{}, control.ErrValidation
	}

	r.nodeClaimMu.Lock()
	defer r.nodeClaimMu.Unlock()

	var existing control.Operation
	if op, err := r.control.OperationByIdemKey(control.CloudInstallIdem(res.ID)); err == nil {
		if op.UserID != u.ID {
			return control.Operation{}, control.ErrNotFound
		}
		existing = op
		if op.Kind != control.OpKindCloudInstall {
			return control.Operation{}, control.ErrOwnerMismatch
		}
		if r.provisionWorkers().runningOp(op.ID) {
			return control.Operation{}, control.ErrCreateInProgress
		}
		switch op.State {
		case control.OpFailed, control.OpInstalling, control.OpDone:
		default:
			return control.Operation{}, control.ErrCreateInProgress
		}
	} else if !errors.Is(err, control.ErrNotFound) {
		return control.Operation{}, err
	}

	// One active mutation per resource: guarded through the whole worker.
	if !r.cloudGuardAcquire(res.ID) {
		return control.Operation{}, errCloudResourceBusy
	}
	guarded := true
	defer func() {
		if guarded {
			r.cloudGuardRelease(res.ID)
		}
	}()

	vars, err := control.MergeProfileVars(pv.VarsSchema, userVars)
	if err != nil {
		return control.Operation{}, err
	}
	vars, err = r.provisionBaseVars(vars)
	if err != nil {
		return control.Operation{}, err
	}
	password, err := r.cloudInstallPassword(res)
	if err != nil || password == "" {
		return control.Operation{}, errCloudNoPassword
	}
	secret, enc, err := r.manualPendingSecret(existing)
	if err != nil {
		return control.Operation{}, err
	}
	vars["label"], vars["node_secret"] = res.Label, secret
	if err := control.ValidateExtraVars(vars); err != nil {
		return control.Operation{}, err
	}
	op, err := r.control.AuthorizeCloudInstall(res.ID, u, pv.ProfileID, secret, enc)
	if err != nil {
		return control.Operation{}, err
	}
	expectedID, err := control.CloudInstallExpectedNodeID(op)
	if err != nil || expectedID == "" {
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "expected_node_id_unavailable")
		return control.Operation{}, control.ErrValidation
	}
	vars["node_id"] = expectedID
	if err := control.ValidateExtraVars(vars); err != nil {
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "validation_failed")
		return control.Operation{}, err
	}
	prov := provision.New(r.control, nil, r.provisionConfig())
	steps := len(control.ParseProfileSteps(pv.Steps))
	auth := sshx.Auth{Type: "password", Password: password}
	resourceID := res.ID
	if !r.provisionWorkers().run(op.ID, prov.ProvisionBudget(steps), func(wctx context.Context) {
		defer r.cloudRecover(op.ID)
		defer r.cloudGuardRelease(resourceID)
		_, _, _ = prov.InstallProfile(wctx, provision.InstallRequest{
			OperationID: op.ID, ProfileID: pv.ProfileID, ProfileVersionID: pv.ID,
			Host: res.IP, Port: 0, User: "root", Auth: auth,
			Vars: vars, Secrets: []string{password},
			// done only after the real /register claim sets operation.node_id.
			Confirm: func() error {
				got, err := r.control.Operation(op.ID)
				if err != nil {
					return err
				}
				if got.NodeID == "" {
					return errors.New("node did not register with the registry")
				}
				return r.control.SetCloudResourceNode(resourceID, got.NodeID, control.CloudResReady)
			},
		})
	}) {
		_ = r.control.UpdateOperationState(op.ID, control.OpFailed, "", "worker_busy")
		return control.Operation{}, control.ErrCreateInProgress
	}
	guarded = false // worker owns the guard now
	return op, nil
}

var errCloudNoPassword = errors.New("no sealed root password")
