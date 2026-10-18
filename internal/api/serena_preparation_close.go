package api

import (
	"context"
	"errors"
)

type PreparedSerenaPreparationCloseSnapshot struct {
	OperationID        string `json:"operation_id"`
	PackageKey         string `json:"package_key"`
	Package            string `json:"package"`
	ReadyNamespace     string `json:"ready_namespace"`
	OperationNamespace string `json:"operation_namespace"`
	Generation         string `json:"generation"`
	Attempt            string `json:"attempt"`
	PublishedRead      string `json:"published_read"`
	InOriginalBound    bool   `json:"in_original_bound"`
}

// Only the SAME effects finalizer issues this original local close observation.
// It does not replace installation/protocol/native-child proof.
type PreparedSerenaPreparationCloseObservation struct {
	owner       *preparedSerenaRuntimeEffects
	request     PreparedSerenaRequest
	lastOutcome *StateFilePublicationObservation
	view        PreparedSerenaPreparationCloseSnapshot
	err         error
}

func (o *PreparedSerenaPreparationCloseObservation) Snapshot() PreparedSerenaPreparationCloseSnapshot {
	if o == nil {
		return PreparedSerenaPreparationCloseSnapshot{}
	}
	return o.view
}
func (o *PreparedSerenaPreparationCloseObservation) Validate(ctx context.Context, originalEffects PreparedSerenaRuntimeEffects, r PreparedSerenaRequest) error {
	if o == nil || ctx == nil || o.owner == nil || originalEffects != o.owner || o.request != r || o.err != nil || !o.view.InOriginalBound {
		return errors.New("SERENA_PREPARATION_ORIGINAL_CLOSE_UNQUALIFIED")
	}
	for _, state := range []string{o.view.Package, o.view.ReadyNamespace, o.view.OperationNamespace, o.view.Generation, o.view.Attempt, o.view.PublishedRead} {
		if state != "closed" && state != "not_attempted" {
			return errors.New("SERENA_PREPARATION_RESOURCE_PENDING")
		}
	}
	return ctx.Err()
}

func (e *preparedSerenaRuntimeEffects) FinishPreparation(ctx context.Context, r PreparedSerenaRequest, lastOutcome *StateFilePublicationObservation) (*PreparedSerenaPreparationCloseObservation, error) {
	if e == nil {
		return nil, errors.New("SERENA_PREPARATION_OWNER_MISSING")
	}
	e.finishOnce.Do(func() {
		key := ""
		if e.packageBinding != nil {
			key = e.packageBinding.key
		}
		o := &PreparedSerenaPreparationCloseObservation{owner: e, request: e.acquisition.inputs.Request, lastOutcome: lastOutcome, view: PreparedSerenaPreparationCloseSnapshot{OperationID: e.operationID, PackageKey: key, Package: "not_attempted", ReadyNamespace: "not_attempted", OperationNamespace: "not_attempted", Generation: "not_attempted", Attempt: "not_attempted", PublishedRead: "not_attempted"}}
		if e.packageLeaseAttempted {
			o.view.Package = "unknown"
			if e.packageLease != nil {
				proof, err := e.packageLease.CloseObserved()
				o.err = errors.Join(o.err, err, proof.Validate(e.packageLease))
				if err == nil && proof.Validate(e.packageLease) == nil {
					o.view.Package = "closed"
				}
			}
		}
		o.err = errors.Join(o.err, e.closeLocalResources(ctx))
		if e.priorPublishedReadAttempted {
			o.view.PublishedRead = "unknown"
			if e.priorPublishedRead != nil {
				proof, closeErr := e.priorPublishedRead.CloseObserved()
				o.err = errors.Join(o.err, closeErr, proof.Validate(e.priorPublishedRead))
				if closeErr == nil && proof.Validate(e.priorPublishedRead) == nil {
					o.view.PublishedRead = "closed"
				}
			}
		}
		for _, slot := range []struct {
			attempted bool
			namespace *SerenaRecordNamespaceObservation
			state     *string
		}{{e.readyNamespaceAttempted, e.namespace, &o.view.ReadyNamespace}, {e.operationNamespaceAttempted, e.operationNamespace, &o.view.OperationNamespace}} {
			if !slot.attempted {
				continue
			}
			*slot.state = "unknown"
			if slot.namespace != nil {
				proof, err := slot.namespace.CloseObserved()
				o.err = errors.Join(o.err, err)
				if err == nil && proof.ValidateSource(slot.namespace) == nil {
					*slot.state = "closed"
				}
			}
		}
		for _, slot := range []struct {
			attempted bool
			tree      *SerenaOwnedTree
			state     *string
		}{{e.acquisition.generationAttempted, e.acquisition.generation, &o.view.Generation}, {e.acquisition.attemptAttempted, e.acquisition.attempt, &o.view.Attempt}} {
			if !slot.attempted {
				continue
			}
			*slot.state = "unknown"
			if slot.tree != nil {
				proof, err := slot.tree.Close()
				o.err = errors.Join(o.err, err)
				if err == nil && proof.Attempted && proof.Closed && proof.Error == nil {
					*slot.state = "closed"
				}
			}
		}
		o.view.InOriginalBound = e.localClosedInBound && ctx != nil && ctx.Err() == nil
		if r != o.request {
			o.err = errors.Join(o.err, errors.New("SERENA_PREPARATION_CLOSE_REQUEST_DRIFT"))
		}
		if lastOutcome != nil && !lastOutcome.Complete() {
			o.err = errors.Join(o.err, errors.New("SERENA_PREPARATION_OUTCOME_FINAL_CLOSE_PENDING"))
		}
		if o.err != nil {
			e.publication.invalidate(e.operationID)
		}
		e.finishObservation = o
	})
	return e.finishObservation, e.finishObservation.err
}
