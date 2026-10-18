package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
)

// This is the acquisition/install/qualification part of the SAME concrete
// package effects owner. Original tree capabilities remain retained here;
// returned path/identity DTOs never become deletion authority.
type preparedSerenaAcquisition struct {
	mu                  sync.Mutex
	inputs              SerenaPreparationInputs
	qualifier           PreparedSerenaQualificationPort
	generation, attempt *SerenaOwnedTree
	roots               PreparedSerenaOwnedRoots
	started, acquiring  bool
	generationAttempted, attemptAttempted bool
}

func (a *preparedSerenaAcquisition) acquireRoots(ctx context.Context, r PreparedSerenaRequest, key string) (roots PreparedSerenaOwnedRoots, retErr error) {
	actual, err := r.Identity()
	if err != nil {
		return roots, err
	}
	if a == nil || a.inputs.Request != r || actual != key {
		return roots, errors.New("SERENA_PREPARED_ACQUISITION_INPUT_DRIFT")
	}
	a.mu.Lock()
	if a.acquiring || a.generation != nil || a.attempt != nil || a.started {
		a.mu.Unlock()
		return roots, errors.New("SERENA_PREPARED_ROOT_ACQUISITION_REENTRY")
	}
	a.acquiring = true
	a.mu.Unlock()
	defer func() {
		// Acquisition has not entered the execution factory. Close every
		// acquired capability on failure; an unproven created tree remains
		// a pending cleanup obligation, never pathname deletion authority.
		if retErr != nil {
			retErr = errors.Join(retErr, a.closeOriginalRoots())
		}
		a.mu.Lock()
		a.acquiring = false
		a.roots = roots
		a.mu.Unlock()
	}()
	a.generationAttempted=true
	a.generation, err = AcquireSerenaOwnedTree(ctx, SerenaOwnedTreeRequest{Namespace: r.Namespace, PackageIdentitySHA256: key, OperationID: r.OperationID, GenerationID: r.GenerationID, Kind: "generation", Limits: r.OwnedTreeLimits})
	if err != nil {
		return roots, err
	}
	gen, err := a.generation.Inventory(ctx)
	if err != nil {
		return roots, err
	}
	roots.Generation, err = preparedSerenaRootFile(filepath.Join(r.Namespace, key, "generations", r.GenerationID), gen.RootIdentity)
	if err != nil {
		return roots, err
	}
	a.attemptAttempted=true
	a.attempt, err = AcquireSerenaOwnedTree(ctx, SerenaOwnedTreeRequest{Namespace: r.Namespace, PackageIdentitySHA256: key, OperationID: r.OperationID, GenerationID: r.GenerationID, Kind: "attempt", Limits: r.OwnedTreeLimits, survivors: []*SerenaOwnedTree{a.generation}})
	if err != nil {
		return roots, err
	}
	attempt, err := a.attempt.Inventory(ctx)
	if err != nil {
		return roots, err
	}
	roots.Attempt, err = preparedSerenaRootFile(filepath.Join(r.Namespace, key, "attempts", r.OperationID), attempt.RootIdentity)
	return roots, err
}

func (a *preparedSerenaAcquisition) closeOriginalRoots() (retErr error) {
	if a == nil {
		return nil
	}
	for _, tree := range []*SerenaOwnedTree{a.attempt, a.generation} {
		if tree == nil {
			continue
		}
		closed, err := tree.Close()
		retErr = errors.Join(retErr, err)
		if !closed.Attempted || !closed.Closed || closed.Error != nil {
			retErr = errors.Join(retErr, errors.New("SERENA_PREPARED_ROOT_CLOSE_PENDING"))
		}
	}
	return retErr
}
func preparedSerenaRootFile(path string, root SerenaTreeObject) (PreparedSerenaFile, error) {
	if !root.Directory || root.Device == "" || root.FileID == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return PreparedSerenaFile{}, errors.New("SERENA_PREPARED_ROOT_IDENTITY_INVALID")
	}
	raw, err := json.Marshal(root)
	if err != nil {
		return PreparedSerenaFile{}, err
	}
	return PreparedSerenaFile{Path: path, SHA256: preparedSerenaHash(raw), Device: root.Device, FileID: root.FileID}, nil
}
func (a *preparedSerenaAcquisition) install(ctx context.Context, r PreparedSerenaRequest, roots PreparedSerenaOwnedRoots) (installed PreparedSerenaInstallation, retErr error) {
	if a == nil || a.inputs.Request != r {
		return installed, errors.New("SERENA_PREPARED_INSTALL_INPUT_DRIFT")
	}
	a.mu.Lock()
	if a.started || a.acquiring || a.generation == nil || a.attempt == nil || a.roots != roots {
		a.mu.Unlock()
		return installed, errors.New("SERENA_PREPARED_INSTALL_ACQUISITION_UNQUALIFIED")
	}
	a.started = true
	a.mu.Unlock()
	installed, retErr = installPreparedSerena(ctx, a.inputs, roots)
	if !installed.Closure.complete(r.OperationID) {
		return installed, retErr
	}
	inventory, err := a.generation.Inventory(ctx)
	retErr = errors.Join(retErr, err)
	if err == nil {
		installed.InventorySHA256 = inventory.SHA256
	}
	return installed, retErr
}
func (a *preparedSerenaAcquisition) cleanupAttempt(ctx context.Context, roots PreparedSerenaOwnedRoots, closure PreparedSerenaExecutionClosure) (cleanup PreparedSerenaCleanup, retErr error) {
	if a == nil || a.attempt == nil || a.roots != roots || !closure.complete(a.inputs.Request.OperationID) {
		return cleanup, errors.New("SERENA_PREPARED_ATTEMPT_CLEANUP_UNQUALIFIED")
	}
	// The original generation capability qualifies surviving immutable links.
	// No pathname or new handle becomes that survivor authority.
	receipt, err := a.attempt.RemoveOwned(ctx, true, closure)
	if err != nil || !receipt.Complete() {
		return cleanup, errors.Join(err, errors.New("SERENA_PREPARED_ATTEMPT_CLEANUP_PENDING"))
	}
	raw, err := json.Marshal(struct {
		Before          string
		Removed, Absent bool
		DeleteAttempts  int
	}{receipt.Before.SHA256, receipt.Removed, receipt.Absent, receipt.DeleteAttempts})
	if err != nil {
		return cleanup, err
	}
	return PreparedSerenaCleanup{ReceiptSHA256: preparedSerenaHash(raw), AttemptAbsent: true, LinksDischarged: true, NoPendingResources: true}, nil
}
func (a *preparedSerenaAcquisition) qualify(ctx context.Context, r PreparedSerenaRequest, roots PreparedSerenaOwnedRoots, installed PreparedSerenaInstallation) (qualification PreparedSerenaQualification, retErr error) {
	if a == nil || a.qualifier == nil || a.inputs.Request != r || a.roots != roots || a.generation == nil {
		return qualification, errors.New("SERENA_UNPUBLISHED_QUALIFIER_UNAVAILABLE")
	}
	id := "qualification-" + preparedSerenaHash([]byte(r.OperationID))
	if !preparedSerenaID(id) {
		return qualification, errors.New("SERENA_QUALIFICATION_ID_INVALID")
	}
	resources, acquireErr := AcquireUnpublishedSerenaQualificationResources(ctx, r, installed, id)
	cleanupCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), r.AbsoluteCleanupDeadline)
	defer cancel()
	if acquireErr != nil {
		if resources != nil {
			_, closeErr := resources.CloseUnstartedObserved(cleanupCtx)
			acquireErr = errors.Join(acquireErr, closeErr)
		}
		return qualification, acquireErr
	}
	request := PreparedSerenaQualificationRequest{QualificationID: id, Request: r, Inputs: a.inputs, Roots: roots, Installation: installed, WorkDeadline: r.WorkDeadline, CleanupDeadline: r.AbsoluteCleanupDeadline, Resources: resources}
	observation, qualifyErr := a.qualifier.QualifyUnpublished(ctx, request)
	if observation == nil {
		_, closeErr := resources.CloseUnstartedObserved(cleanupCtx)
		return qualification, errors.Join(qualifyErr, closeErr, errors.New("SERENA_UNPUBLISHED_OBSERVATION_MISSING"))
	}
	qualification.Closure = observation.closure
	if err := observation.Validate(cleanupCtx, request); err != nil {
		return qualification, errors.Join(qualifyErr, err)
	}
	inventory, err := a.generation.Inventory(cleanupCtx)
	if err != nil {
		return qualification, errors.Join(qualifyErr, err)
	}
	qualification.ReceiptSHA256 = observation.ReceiptSHA256()
	qualification.RuntimeProtocolQualified = true
	qualification.IndependentOfAttemptRoots = observation.resourceClose.tree.Complete()
	qualification.FinalEnvironmentInventorySHA256 = inventory.SHA256
	qualification.observation = observation
	return qualification, qualifyErr
}
