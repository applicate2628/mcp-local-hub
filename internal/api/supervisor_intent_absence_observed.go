package api

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"time"
)

type stateFilePublishedObject struct {
	writer *StateFilePublicationObservation
	object SerenaTreeObject
	sha256 string
}
type SupervisorIntentAbsenceRestorationSnapshot struct {
	Disposition                                                                                    string
	ObjectIdentityKnown, MarkAttempted, Marked, CloseAttempted, Closed, AbsenceQualified, Complete bool
	Error                                                                                          error
}
type SupervisorIntentAbsenceRestorationObservation struct {
	mu                       sync.Mutex
	commit                   *SupervisorFileMutationObservation
	input                    SupervisorFileMutationPrecondition
	ctx                      context.Context
	deadline                 time.Time
	path                     string
	lease                    *StateFileObservedLease
	leaseClose               *StateFileLeaseCloseObservation
	namespaceClose           *StateFileFreshNamespaceCloseObservation
	object                   *stateFilePublishedObject
	capture                  *stateFileIOCapture
	reader, localPolicy      *StateFileReadObservation
	finalized, noOwnedObject bool
	err, receivingErr        error
	view                     SupervisorIntentAbsenceRestorationSnapshot
}

func RestoreSupervisorIntentAbsenceLockHeldObserved(ctx context.Context, path string, input SupervisorFileMutationPrecondition, lease *StateFileObservedLease, commit *SupervisorFileMutationObservation) (o *SupervisorIntentAbsenceRestorationObservation, retErr error) {
	if commit == nil {
		return nil, errors.New("original fresh supervisor intent commit required")
	}
	commit.mu.Lock()
	if commit.restoration != nil {
		o = commit.restoration
		commit.mu.Unlock()
		o.mu.Lock()
		finalized, cachedErr := o.finalized, o.err
		o.mu.Unlock()
		if !finalized {
			return nil, errors.New("intent restoration original action in flight")
		}
		if ctx != o.ctx || path != o.path || lease != o.lease || !sameIntentRestorationInput(input, o.input) {
			return o, errors.New("intent restoration original operation differs")
		}
		return o, cachedErr
	}
	o = &SupervisorIntentAbsenceRestorationObservation{commit: commit, input: copySupervisorMutationInput(input), ctx: ctx, path: path, lease: lease, capture: newStateFileIOCapture(), view: SupervisorIntentAbsenceRestorationSnapshot{Disposition: "refused"}}
	commit.restoration = o
	commit.mu.Unlock()
	defer func() {
		o.mu.Lock()
		o.err = retErr
		o.view.Error = retErr
		o.finalized = true
		if retErr != nil {
			if o.view.MarkAttempted || (o.view.CloseAttempted && !o.view.Closed) {
				o.view.Disposition = "pending"
			} else {
				o.view.Disposition = "refused"
			}
		}
		o.mu.Unlock()
	}()
	if err := validateSupervisorFileMutationPrecondition(input); err != nil {
		return o, err
	}
	if ctx == nil || lease == nil || !input.BeforeDefined || commit.input.BeforeDefined || commit.role != SupervisorIntentLeaf || commit.path != path || commit.lease != lease || !sameIntentRestorationSource(input, commit.input) {
		return o, errors.New("intent restoration fresh commit binding differs")
	}
	end, finite := ctx.Deadline()
	if !finite || end.After(commit.deadline) || ctx.Err() != nil {
		return o, errors.New("intent restoration original deadline unavailable")
	}
	o.deadline = end
	if err := commit.ValidateHeldProgress(ctx, commit.input, *input.BeforeSHA256, lease); err != nil {
		return o, err
	}
	if !commit.view.Changed || commit.writer == nil || commit.writer.publishedObject == nil {
		return o, errors.New("intent restoration published object unavailable")
	}
	o.object = commit.writer.publishedObject
	if o.object.writer != commit.writer || o.object.sha256 != *input.BeforeSHA256 || o.object.object.Directory || o.object.object.Links != 1 || o.object.object.Device == "" || o.object.object.FileID == "" {
		return o, errors.New("intent restoration original object unqualified")
	}
	o.view.ObjectIdentityKnown = true
	retErr = input.NamespaceSource.borrowStateFileFreshNamespace(ctx, func() error {
		return lease.Borrow(ctx, path, func() error { return restoreSupervisorIntentAbsencePlatform(ctx, o) })
	})
	return o, retErr
}
func sameIntentRestorationSource(a, b SupervisorFileMutationPrecondition) bool {
	return a.NamespaceSource == b.NamespaceSource && a.SourceOperationID == b.SourceOperationID && a.RequestSHA256 == b.RequestSHA256 && a.NamespaceSHA256 == b.NamespaceSHA256
}
func sameIntentRestorationInput(a, b SupervisorFileMutationPrecondition) bool {
	return sameIntentRestorationSource(a, b) && a.Apply == b.Apply && a.BeforeDefined == b.BeforeDefined && reflect.DeepEqual(a.BeforeSHA256, b.BeforeSHA256)
}
func (o *SupervisorIntentAbsenceRestorationObservation) validateBody(ctx context.Context, input SupervisorFileMutationPrecondition, lease *StateFileObservedLease, commit *SupervisorFileMutationObservation) error {
	if o == nil || ctx == nil || commit == nil || commit != o.commit || commit.restoration != o || lease != o.lease || !o.finalized || o.err != nil || o.receivingErr != nil || !sameIntentRestorationInput(input, o.input) || !o.view.AbsenceQualified || o.reader == nil {
		return errors.New("intent restoration original receiving incomplete")
	}
	end, finite := ctx.Deadline()
	if !finite || end.After(o.deadline) || !time.Now().Before(o.deadline) || ctx.Err() != nil || o.ctx.Err() != nil {
		return errors.Join(ctx.Err(), o.ctx.Err(), errors.New("intent restoration deadline expired or renewed"))
	}
	if !o.noOwnedObject && (!o.capture.complete() || o.localPolicy == nil || !o.localPolicy.finalized || !o.localPolicy.view.OwnedResourcesComplete || !o.localPolicy.view.BorrowedResourcesComplete) {
		return errors.New("intent restoration local close incomplete")
	}
	return o.reader.ValidateAbsence(ctx, o.path, StateFileReadLeafNotFound)
}
func (o *SupervisorIntentAbsenceRestorationObservation) ValidateHeldProgress(ctx context.Context, input SupervisorFileMutationPrecondition, lease *StateFileObservedLease, commit *SupervisorFileMutationObservation) error {
	if err := o.validateBody(ctx, input, lease, commit); err != nil {
		return err
	}
	if err := lease.ValidateHeld(ctx, o.path); err != nil {
		return err
	}
	if err := input.NamespaceSource.ValidateOperation(ctx, input.SourceOperationID, input.RequestSHA256); err != nil {
		return err
	}
	return input.NamespaceSource.ValidateNamespace(ctx, o.path, SupervisorIntentLeaf, input.NamespaceSHA256)
}
func (o *SupervisorIntentAbsenceRestorationObservation) ReceiveLeaseClose(proof *StateFileLeaseCloseObservation) error {
	if o == nil {
		return errors.New("intent restoration missing")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finalized {
		return errors.New("intent restoration body in flight")
	}
	if o.leaseClose != nil && o.leaseClose != proof {
		return errors.New("intent restoration lease close substituted")
	}
	o.leaseClose = proof
	if o.ctx == nil {
		return errors.New("intent restoration original context missing")
	}
	o.receivingErr = errors.Join(o.receivingErr, proof.Validate(o.lease), o.ctx.Err())
	return o.receivingErr
}
func (o *SupervisorIntentAbsenceRestorationObservation) ReceiveNamespaceClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if o == nil {
		return errors.New("intent restoration missing")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.finalized {
		return errors.New("intent restoration body in flight")
	}
	if o.namespaceClose != nil && o.namespaceClose != proof {
		return errors.New("intent restoration namespace close substituted")
	}
	o.namespaceClose = proof
	if o.ctx == nil {
		return errors.New("intent restoration original context missing")
	}
	o.receivingErr = errors.Join(o.receivingErr, proof.ValidateSource(o.input.NamespaceSource), o.ctx.Err())
	return o.receivingErr
}
func (o *SupervisorIntentAbsenceRestorationObservation) Validate(ctx context.Context, input SupervisorFileMutationPrecondition, commit *SupervisorFileMutationObservation) error {
	if o == nil {
		return errors.New("intent restoration missing")
	}
	if err := o.validateBody(ctx, input, o.lease, commit); err != nil {
		return err
	}
	return errors.Join(o.leaseClose.Validate(o.lease), o.namespaceClose.ValidateSource(input.NamespaceSource))
}
func (o *SupervisorIntentAbsenceRestorationObservation) Snapshot() SupervisorIntentAbsenceRestorationSnapshot {
	if o == nil {
		return SupervisorIntentAbsenceRestorationSnapshot{}
	}
	o.mu.Lock()
	v := o.view
	o.mu.Unlock()
	v.Complete = o.Validate(o.ctx, o.input, o.commit) == nil
	return v
}
