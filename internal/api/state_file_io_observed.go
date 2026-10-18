package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/clients"
	"sync"
)

type StateFileResourceClose struct {
	Acquired, Attempted, Closed bool
	Error                       error
}
type stateFilePublicationCloseRow struct {
	Acquired  bool `json:"acquired"`
	Attempted bool `json:"attempted"`
	Closed    bool `json:"closed"`
}

func (o *StateFilePublicationObservation) ReceiptSHA256() string {
	if !o.Complete() {
		return ""
	}
	rows := map[string]stateFilePublicationCloseRow{}
	for name, slot := range o.capture.slots {
		rows[name] = stateFilePublicationCloseRow{slot.Acquired, slot.Attempted, slot.Closed}
	}
	body, err := json.Marshal(struct {
		Schema     string                                  `json:"schema"`
		PathSHA    string                                  `json:"path_sha256"`
		ContentSHA string                                  `json:"content_sha256"`
		Slots      map[string]stateFilePublicationCloseRow `json:"slots"`
	}{"state-file-publication-receipt-v1", preparedSerenaHash([]byte(o.path)), o.sha256, rows})
	if err != nil {
		return ""
	}
	return preparedSerenaHash(body)
}

type stateFileIOCapture struct {
	slots                map[string]StateFileResourceClose
	cleanup              error
	write, flush, rename stateFileNativeCall
}

// Native outcomes belong to the same original writer capture. An unentered or
// unreturned call has no inferred success; these fields do not grant a lease or
// change the existing state-publication receipt/resource predicates.
type stateFileNativeCall struct {
	entered, returned bool
	err               error
}

func newStateFileIOCapture() *stateFileIOCapture {
	return &stateFileIOCapture{slots: map[string]StateFileResourceClose{}}
}
func (c *stateFileIOCapture) acquired(name string) {
	if c != nil {
		c.slots[name] = StateFileResourceClose{Acquired: true}
	}
}
func (c *stateFileIOCapture) closed(name string, err error) {
	if c != nil {
		v := c.slots[name]
		v.Attempted = true
		v.Closed = err == nil
		v.Error = err
		c.slots[name] = v
	}
}
func (c *stateFileIOCapture) complete() bool {
	if c == nil || len(c.slots) == 0 || c.cleanup != nil {
		return false
	}
	for _, v := range c.slots {
		if v.Acquired && (!v.Attempted || !v.Closed || v.Error != nil) {
			return false
		}
	}
	return true
}
func (c *stateFileIOCapture) error() error {
	if c == nil {
		return nil
	}
	err := c.cleanup
	for _, v := range c.slots {
		err = errors.Join(err, v.Error)
	}
	return err
}

// This token is created only by the observed canonical IO owner. Serialized
// phase text and exported snapshots cannot construct an instance observation.
type StateFilePublicationObservation struct {
	completionMu                          sync.Mutex
	completionReceived, completionInBound bool
	capture                               *stateFileIOCapture
	path                                  string
	sha256                                string
	operation                             string
	err                                   error
	request                               clients.ConfigWriteRequest
	lease                                 *StateFileObservedLease
	readback                              *StateFileReadObservation
	readbackAttempted                     bool
	readbackQualified                     bool
	readbackRaw                           []byte
	borrowedParent                        *clients.ConfigWriteParentCloseObservation
	finishedInBound                       bool
	fresh                                 *StateFileFreshCreationEvidence
	freshNamespaceClose                   *StateFileFreshNamespaceCloseObservation
	progressNamespace                     *SerenaRecordNamespaceObservation
	progressNamespaceClose                *StateFileFreshNamespaceCloseObservation
	publishedObject                       *stateFilePublishedObject
}

// Only SAME RecordIO registers a replacement's original retained namespace.
// This does not discharge it or change strict final-resource predicates.
func (o *StateFilePublicationObservation) bindProgressNamespace(namespace *SerenaRecordNamespaceObservation) error {
	if o == nil || namespace == nil || namespace.path != o.path || namespace.role != "prepared-operation" || (o.progressNamespace != nil && o.progressNamespace != namespace) {
		return errors.New("STATE_PROGRESS_NAMESPACE_MISMATCH")
	}
	if o.fresh != nil && o.fresh.source != namespace {
		return errors.New("STATE_PROGRESS_FRESH_NAMESPACE_MISMATCH")
	}
	o.progressNamespace = namespace
	return nil
}

func (o *StateFilePublicationObservation) ValidateProgress(ctx context.Context, namespace *SerenaRecordNamespaceObservation, expectedAfterSHA256 string) error {
	if o == nil || namespace == nil || o.progressNamespace != namespace {
		return errors.New("STATE_PROGRESS_OBSERVATION_UNQUALIFIED")
	}
	if err := o.validatePublicationBody(ctx, o.request, expectedAfterSHA256); err != nil {
		return err
	}
	if err := namespace.validateStateFileFreshNamespace(ctx, o.path, "prepared-operation"); err != nil {
		return err
	}
	o.lease.mu.Lock()
	closed := o.lease.closeObservation
	o.lease.mu.Unlock()
	if err := closed.Validate(o.lease); err != nil {
		return err
	}
	if o.request.ExpectedSnapshot == nil {
		if o.fresh == nil || o.fresh.source != namespace || o.fresh.lease != o.lease || o.fresh.absence == nil {
			return errors.New("STATE_PROGRESS_FRESH_BINDING_UNQUALIFIED")
		}
		if err := o.fresh.absence.ValidateAbsence(ctx, o.path, StateFileReadLeafNotFound); err != nil {
			return err
		}
	}
	return nil
}

// One writer body predicate receives every non-retained owner. Callers cannot
// exclude arbitrary resources: only the exact private held progress seam can
// leave its original lease and namespace intentionally outstanding.
func (o *StateFilePublicationObservation) validatePublicationBody(ctx context.Context, req clients.ConfigWriteRequest, expectedAfterSHA string) error {
	if o == nil || ctx == nil || o.request.Context == nil || req.Context != o.request.Context || req.Path != o.path || req.ExpectedSnapshot != o.request.ExpectedSnapshot || req.FreshCreationEvidence != o.request.FreshCreationEvidence || !preparedSerenaDigest(expectedAfterSHA) || expectedAfterSHA != o.sha256 || preparedSerenaHash(req.Contents) != o.sha256 || o.err != nil || !o.finishedInBound || !o.readbackAttempted || !o.readbackQualified || !o.capture.complete() || o.lease == nil || o.readback == nil {
		return errors.New("STATE_PUBLICATION_BODY_UNQUALIFIED")
	}
	end, finite := ctx.Deadline()
	original, originalFinite := o.request.Context.Deadline()
	if !finite || !originalFinite || end.After(original) || ctx.Err() != nil || o.request.Context.Err() != nil {
		return errors.Join(ctx.Err(), o.request.Context.Err(), errors.New("STATE_PUBLICATION_DEADLINE_UNQUALIFIED"))
	}
	if req.ExpectedSnapshot != nil {
		if err := o.borrowedParent.Validate(req.ExpectedSnapshot); err != nil {
			return err
		}
	}
	return o.readback.Validate(ctx, o.path, int64(len(o.request.Contents)), o.readbackRaw, o.sha256)
}

func (o *StateFilePublicationObservation) validateHeldProgress(ctx context.Context, req clients.ConfigWriteRequest, expectedAfterSHA string, lease *StateFileObservedLease, namespace *SupervisorStateNamespaceObservation, role string) error {
	if err := o.validatePublicationBody(ctx, req, expectedAfterSHA); err != nil {
		return err
	}
	if lease == nil || namespace == nil || lease != o.lease {
		return errors.New("STATE_HELD_PROGRESS_OWNER_MISMATCH")
	}
	if err := lease.ValidateHeld(ctx, o.path); err != nil {
		return err
	}
	if err := namespace.validateStateFileFreshNamespace(ctx, o.path, role); err != nil {
		return err
	}
	if o.progressNamespace != nil {
		return errors.New("STATE_HELD_PROGRESS_RECORD_NAMESPACE_CONFLICT")
	}
	if req.ExpectedSnapshot == nil {
		if o.fresh == nil || o.fresh.source != namespace || o.fresh.lease != lease || o.fresh.absence == nil {
			return errors.New("STATE_HELD_PROGRESS_FRESH_OWNER_MISMATCH")
		}
		if err := o.fresh.absence.ValidateAbsence(ctx, o.path, StateFileReadLeafNotFound); err != nil {
			return err
		}
	}
	return nil
}

func (o *StateFilePublicationObservation) Complete() bool {
	if o == nil {
		return false
	}
	o.completionMu.Lock()
	defer o.completionMu.Unlock()
	if o.err != nil || !o.finishedInBound || !o.readbackQualified || !o.ResourcesClosed() {
		return false
	}
	if !o.completionReceived {
		o.completionReceived = true
		if o.request.Context != nil {
			_, finite := o.request.Context.Deadline()
			o.completionInBound = finite && o.request.Context.Err() == nil
		}
	}
	return o.completionInBound
}
func (o *StateFilePublicationObservation) ResourcesClosed() bool {
	if o == nil || !o.capture.complete() || o.lease == nil {
		return false
	}
	o.lease.mu.Lock()
	closeProof := o.lease.closeObservation
	o.lease.mu.Unlock()
	if closeProof.Validate(o.lease) != nil {
		return false
	}
	if o.request.ExpectedSnapshot != nil && o.borrowedParent.Validate(o.request.ExpectedSnapshot) != nil {
		return false
	}
	if o.fresh != nil && o.freshNamespaceClose.Validate(o.fresh) != nil {
		return false
	}
	if o.progressNamespace != nil && o.progressNamespaceClose.ValidateSource(o.progressNamespace) != nil {
		return false
	}
	if !o.readbackAttempted {
		return true
	}
	if o.readback == nil {
		return false
	}
	// Private same-reader finalization covers cleanup even when its body failed;
	// copied Snapshot complete flags are not consumed as authority.
	return o.readback.finalized && o.readback.issuer != nil && o.readback.view.OwnedResourcesComplete && o.readback.view.BorrowedResourcesComplete
}

func (o *StateFilePublicationObservation) receiveRecordNamespaceClose(namespace *SerenaRecordNamespaceObservation, proof *StateFileFreshNamespaceCloseObservation) error {
	if o == nil || namespace == nil || (o.progressNamespace != nil && o.progressNamespace != namespace) {
		return errors.New("STATE_PROGRESS_NAMESPACE_CLOSE_MISMATCH")
	}
	if err := proof.ValidateSource(namespace); err != nil {
		return err
	}
	if o.progressNamespace != nil {
		o.progressNamespaceClose = proof
	}
	if o.fresh != nil {
		return o.ReceiveFreshNamespaceClose(proof)
	}
	return nil
}
func (o *StateFilePublicationObservation) ReceiveBorrowedParentClose(proof *clients.ConfigWriteParentCloseObservation) error {
	if o == nil || o.request.ExpectedSnapshot == nil {
		return errors.New("STATE_PUBLICATION_BORROWED_PARENT_NOT_BOUND")
	}
	if err := proof.Validate(o.request.ExpectedSnapshot); err != nil {
		return err
	}
	o.borrowedParent = proof
	o.capture.closed("borrowed-parent", nil)
	return nil
}
func (o *StateFilePublicationObservation) ReceiveFreshNamespaceClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if o == nil || o.fresh == nil {
		return errors.New("STATE_FRESH_NAMESPACE_NOT_BOUND")
	}
	if err := proof.Validate(o.fresh); err != nil {
		return err
	}
	o.freshNamespaceClose = proof
	return nil
}

func (o *StateFilePublicationObservation) Validate(ctx context.Context, req clients.ConfigWriteRequest, expectedAfterSHA string) error {
	if err := o.validatePublicationBody(ctx, req, expectedAfterSHA); err != nil {
		return err
	}
	if !o.Complete() {
		return errors.New("STATE_PUBLICATION_OBSERVATION_INELIGIBLE")
	}
	return ctx.Err()
}
func (o *StateFilePublicationObservation) SHA256() string {
	if o == nil {
		return ""
	}
	return o.sha256
}
