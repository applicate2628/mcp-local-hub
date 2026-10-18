package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"time"
)

type PreparedSerenaConsumerInvocationRequest struct {
	OperationID         string
	Binding             PreparedSerenaRuntimeBinding
	ExpectedDescriptors []SupervisorDaemon
}
type PreparedSerenaConsumerInvocation struct {
	mu                                                                                        sync.Mutex
	owner                                                                                     *PreparedSerenaRuntimeOwner
	request                                                                                   PreparedSerenaConsumerInvocationRequest
	requestSHA                                                                                string
	deadline                                                                                  time.Time
	io                                                                                        PreparedSerenaRecordIO
	record                                                                                    PreparedSerenaRuntimeRecord
	ready, operation                                                                          *SerenaRecordNamespaceObservation
	intentNamespace                                                                           *SupervisorStateNamespaceObservation
	published                                                                                 *SerenaPublishedGenerationObservation
	publication                                                                               *PreparedSerenaPublicationOwner
	packageLease, borrowedIntent                                                              *StateFileObservedLease
	ownedIntent                                                                               *StateFileObservedLease
	ownedIntentAttempted                                                                      bool
	readers                                                                                   []*StateFileReadObservation
	packageClose, intentClose                                                                 *StateFileLeaseCloseObservation
	intentRelease                                                                             func() error
	readyAttempted, operationAttempted, intentAttempted, publishedAttempted, packageAttempted bool
	entered                                                                                   int
	sealed, finalizing                                                                        bool
	primary, cleanup                                                                          error
	close                                                                                     *PreparedSerenaConsumerInvocationCloseObservation
	heldMutations                                                                             []heldSerenaIntentMutation
	heldRestorations                                                                          []*SupervisorIntentAbsenceRestorationObservation
}
type PreparedSerenaConsumerInvocationCloseSnapshot struct {
	OperationID                                                   string
	Ready, Operation, IntentNamespace, Published, Package, Intent string
	Readers                                                       string
	Pending, Complete                                             bool
	Error                                                         error
}
type PreparedSerenaConsumerInvocationCloseObservation struct {
	owner *PreparedSerenaConsumerInvocation
	view  PreparedSerenaConsumerInvocationCloseSnapshot
}

func consumerInvocationRequestSHA(r PreparedSerenaConsumerInvocationRequest) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}
func (o *PreparedSerenaRuntimeOwner) AcquireConsumerInvocation(ctx context.Context, r PreparedSerenaConsumerInvocationRequest) (scope *PreparedSerenaConsumerInvocation, retErr error) {
	if o == nil || ctx == nil || ctx.Err() != nil || !preparedSerenaID(r.OperationID) || len(r.ExpectedDescriptors) == 0 {
		return nil, errors.New("SERENA_CONSUMER_INVOCATION_INPUT_INVALID")
	}
	end, finite := ctx.Deadline()
	if !finite {
		return nil, errors.New("SERENA_CONSUMER_INVOCATION_DEADLINE_REQUIRED")
	}
	original, coordinatorErr := o.consumerCoordinator()
	if coordinatorErr != nil {
		return nil, coordinatorErr
	}
	if err := validatePreparedSerenaBinding(r.Binding); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, d := range r.ExpectedDescriptors {
		binding, err := preparedSerenaDescriptorBinding(d)
		if err != nil || binding == nil || *binding != r.Binding || d.TaskName == "" || seen[d.TaskName] {
			return nil, errors.Join(err, errors.New("SERENA_CONSUMER_DESCRIPTOR_SCOPE_INVALID"))
		}
		seen[d.TaskName] = true
		if _, err = preparedSerenaDescriptorSHA(d); err != nil {
			return nil, err
		}
	}
	cloned := cloneSupervisorIntentFile(&SupervisorIntentFile{Version: 2, Daemons: r.ExpectedDescriptors})
	r.ExpectedDescriptors = cloned.Daemons
	sha, err := consumerInvocationRequestSHA(r)
	if err != nil {
		return nil, err
	}
	scope = &PreparedSerenaConsumerInvocation{owner: o, request: r, requestSHA: sha, deadline: end, publication: original.publication}
	defer func() {
		if retErr != nil {
			scope.primary = retErr
			_, closeErr := scope.CloseObserved()
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	root, resolver, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return scope, err
	}
	if err = resolver.Validate(ctx, root); err != nil {
		return scope, err
	}
	if root != original.root {
		return scope, errors.New("SERENA_CONSUMER_CANONICAL_ROOT_DRIFT")
	}
	readIO, err := NewPreparedSerenaRecordIO(root, original.intentPath, r.OperationID, original.sourceEpoch)
	if err != nil {
		return scope, err
	}
	raw, exists, reader, err := readIO.readReadyObserved(ctx, r.Binding.RequestIdentitySHA256)
	scope.readers = append(scope.readers, reader)
	if err != nil || !exists {
		return scope, errors.Join(err, errors.New("SERENA_RUNTIME_INSTALL_NEEDED"))
	}
	scope.record, err = decodePreparedSerenaRecord(raw)
	if err != nil {
		return scope, err
	}
	actual, err := scope.record.Binding(raw)
	if err != nil || actual != r.Binding {
		return scope, errors.Join(err, errors.New("SERENA_CONSUMER_READY_BINDING_DRIFT"))
	}
	available, err := scope.publication.available(ctx, scope.record, preparedSerenaHash(raw))
	if err != nil || !available {
		return scope, errors.Join(err, errors.New("SERENA_CONSUMER_PUBLICATION_UNAVAILABLE"))
	}
	// Protected preparation outcome is read under its original operation ID;
	// the consumer's intent mutation still has its own exact request source.
	readIO, err = NewPreparedSerenaRecordIO(root, original.intentPath, scope.record.OperationID, original.sourceEpoch)
	if err != nil {
		return scope, err
	}
	path, err := readIO.recordPath(r.Binding.RequestIdentitySHA256)
	if err != nil {
		return scope, err
	}
	scope.readyAttempted = true
	scope.ready, err = acquireSerenaRecordNamespaceReadPlatform(ctx, readIO, r.Binding.RequestIdentitySHA256, path)
	if err != nil {
		return scope, err
	}
	scope.io, err = readIO.bindReadRecordNamespace(ctx, scope.ready)
	if err != nil {
		return scope, err
	}
	if !sameSerenaNamespaceID(scope.io.packageBinding.identity.runtimeRoot, original.runtimeIdentity) {
		return scope, errors.New("SERENA_CONSUMER_RUNTIME_ROOT_IDENTITY_DRIFT")
	}
	common, err := scope.ready.NamespaceSHA256(ctx)
	if err != nil || common != scope.record.NamespaceSHA256 {
		return scope, errors.Join(err, errors.New("SERENA_CONSUMER_NAMESPACE_DRIFT"))
	}
	scope.operationAttempted = true
	scope.operation, err = scope.io.AcquireOperationNamespaceObserved(ctx, r.Binding.RequestIdentitySHA256, scope.record.OperationID, false)
	if err != nil {
		return scope, err
	}
	if err = scope.validateProtectedOperation(ctx); err != nil {
		return scope, err
	}
	scope.intentAttempted = true
	scope.intentNamespace, err = AcquireSupervisorStateNamespaceObserved(ctx, SupervisorStateNamespaceRequest{SourceOperationID: r.OperationID, RequestSHA256: sha, SourceEpoch: original.sourceEpoch, LeafRoles: []SupervisorStateLeafRole{SupervisorIntentLeaf}, CreateMissing: false})
	if err != nil {
		return scope, err
	}
	scope.io.intentNamespace = scope.intentNamespace
	scope.io.intentRequestSHA256 = sha
	scope.publishedAttempted = true
	scope.published, err = AcquireSerenaPublishedGenerationObserved(ctx, scope.record, original.limits)
	if err != nil {
		return scope, err
	}
	if err = scope.published.ValidatePublished(ctx, scope.record); err != nil {
		return scope, err
	}
	return scope, scope.Validate(ctx, o, r)
}
func (s *PreparedSerenaConsumerInvocation) Validate(ctx context.Context, owner *PreparedSerenaRuntimeOwner, r PreparedSerenaConsumerInvocationRequest) error {
	if err := s.beginCall(ctx, owner, r); err != nil {
		return err
	}
	defer s.leave()
	return s.validateSources(ctx)
}
func (s *PreparedSerenaConsumerInvocation) beginCall(ctx context.Context, owner *PreparedSerenaRuntimeOwner, r PreparedSerenaConsumerInvocationRequest) error {
	if s == nil || ctx == nil || ctx.Err() != nil || owner != s.owner {
		return errors.New("SERENA_CONSUMER_INVOCATION_SOURCE_INVALID")
	}
	sha, err := consumerInvocationRequestSHA(r)
	if err != nil || sha != s.requestSHA {
		return errors.Join(err, errors.New("SERENA_CONSUMER_INVOCATION_REQUEST_DRIFT"))
	}
	if !time.Now().Before(s.deadline) {
		return context.DeadlineExceeded
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.primary != nil || s.entered != 0 {
		return errors.New("SERENA_CONSUMER_INVOCATION_CLOSED_PARTIAL_OR_BUSY")
	}
	s.entered++
	return nil
}
func (s *PreparedSerenaConsumerInvocation) validateSources(ctx context.Context) error {
	var err error
	if err = s.ready.platform.validate(ctx, s.ready.path); err != nil {
		return err
	}
	if err = s.operation.platform.validate(ctx, s.operation.path); err != nil {
		return err
	}
	if err = s.intentNamespace.ValidateOperation(ctx, s.request.OperationID, s.requestSHA); err != nil {
		return err
	}
	return s.published.ValidatePublished(ctx, s.record)
}
func (s *PreparedSerenaConsumerInvocation) enter(ctx context.Context) error {
	if s == nil {
		return errors.New("SERENA_CONSUMER_INVOCATION_MISSING")
	}
	if err := s.beginCall(ctx, s.owner, s.request); err != nil {
		return err
	}
	if err := s.validateSources(ctx); err != nil {
		s.leave()
		return err
	}
	return nil
}
func (s *PreparedSerenaConsumerInvocation) leave() {
	s.mu.Lock()
	s.entered--
	run := s.sealed && s.entered == 0 && !s.finalizing && s.close == nil
	if run {
		s.finalizing = true
	}
	s.mu.Unlock()
	if run {
		s.finalize()
	}
}
func (s *PreparedSerenaConsumerInvocation) receiveBody(ctx context.Context, borrowed *StateFileObservedLease, run func(preparedSerenaIntentConsumerEffects) error) (err error) {
	if err = s.enter(ctx); err != nil {
		return err
	}
	defer s.leave()
	if borrowed != nil {
		if err = borrowed.ValidateHeld(ctx, s.io.IntentPath); err != nil {
			return err
		}
		s.borrowedIntent = borrowed
		s.io.intent.mu.Lock()
		s.io.intent.lease = borrowed
		s.io.intent.mu.Unlock()
	} else {
		s.ownedIntentAttempted = true
		s.intentRelease, err = s.io.LockIntentOperation(ctx)
		s.io.intent.mu.Lock()
		s.ownedIntent = s.io.intent.lease
		s.io.intent.mu.Unlock()
		if err != nil {
			return err
		}
	}
	s.packageAttempted = true
	s.packageLease, err = AcquireStateFileObservedLease(ctx, filepath.Join(s.io.Root, s.request.Binding.RequestIdentitySHA256, "package"))
	if err != nil {
		return err
	}
	if err = s.revalidateExcluded(ctx); err != nil {
		return err
	}
	return run(s.io)
}
func (s *PreparedSerenaConsumerInvocation) WithIntentLockHeld(ctx context.Context, heldIntent *StateFileObservedLease, run func() error) (err error) {
	if s == nil || heldIntent == nil || run == nil {
		return errors.New("SERENA_CONSUMER_HELD_INTENT_INPUT_MISSING")
	}
	if err = s.Validate(ctx, s.owner, s.request); err != nil {
		return err
	}
	if err = heldIntent.ValidateHeld(ctx, s.io.IntentPath); err != nil {
		return err
	}
	err = heldIntent.Borrow(ctx, s.io.IntentPath, func() error {
		return s.receiveBody(ctx, heldIntent, func(preparedSerenaIntentConsumerEffects) error { return run() })
	})
	// Only the fresh package scope belongs to this receiver. The original
	// intent owner and acquiring caller receive their own closes afterward.
	if s.packageLease != nil {
		proof, closeErr := s.packageLease.CloseObserved()
		s.packageClose = proof
		err = errors.Join(err, closeErr, proof.Validate(s.packageLease))
	}
	return err
}
func (s *PreparedSerenaConsumerInvocation) ReceiveIntentClose(proof *StateFileLeaseCloseObservation) error {
	if s == nil || s.borrowedIntent == nil {
		return errors.New("SERENA_CONSUMER_BORROWED_INTENT_MISSING")
	}
	var receivingErr error
	for _, mutation := range s.heldMutations {
		receivingErr = errors.Join(receivingErr, mutation.observation.ReceiveLeaseClose(proof))
	}
	for _, restoration := range s.heldRestorations {
		receivingErr = errors.Join(receivingErr, restoration.ReceiveLeaseClose(proof))
	}
	if err := proof.Validate(s.borrowedIntent); err != nil {
		return errors.Join(receivingErr, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entered != 0 || !time.Now().Before(s.deadline) {
		return errors.New("SERENA_CONSUMER_INTENT_CLOSE_LATE_OR_IN_FLIGHT")
	}
	if s.intentClose != nil && s.intentClose != proof {
		return errors.New("SERENA_CONSUMER_INTENT_CLOSE_FOREIGN_REENTRY")
	}
	s.intentClose = proof
	s.io.intent.mu.Lock()
	publications := append([]*StateFilePublicationObservation(nil), s.io.intent.publications...)
	s.io.intent.mu.Unlock()
	for _, publication := range publications {
		if !publication.Complete() {
			return errors.New("SERENA_CONSUMER_BORROWED_INTENT_PUBLICATION_UNSETTLED")
		}
	}
	if s.close != nil && s.close.view.Intent == "borrowed_pending" {
		view := s.close.view
		view.Intent = "closed"
		view.Complete = view.Error == nil
		for _, state := range []string{view.Ready, view.Operation, view.IntentNamespace, view.Published, view.Package, view.Readers} {
			if state != "closed" && state != "not_attempted" {
				view.Complete = false
			}
		}
		view.Pending = !view.Complete
		s.close.view = view
	}
	return receivingErr
}
func (s *PreparedSerenaConsumerInvocation) revalidateExcluded(ctx context.Context) error {
	raw, exists, reader, err := s.io.readReadyObserved(ctx, s.request.Binding.RequestIdentitySHA256)
	s.readers = append(s.readers, reader)
	if err != nil || !exists {
		return errors.Join(err, errors.New("SERENA_CONSUMER_READY_MISSING"))
	}
	record, err := decodePreparedSerenaRecord(raw)
	if err != nil {
		return err
	}
	actual, err := record.Binding(raw)
	if err != nil || actual != s.request.Binding || !reflect.DeepEqual(record, s.record) {
		return errors.Join(err, errors.New("SERENA_CONSUMER_READY_DRIFT"))
	}
	if err = s.published.ValidatePublished(ctx, record); err != nil {
		return err
	}
	if err = s.validateProtectedOperation(ctx); err != nil {
		return err
	}
	available, err := s.publication.available(ctx, record, preparedSerenaHash(raw))
	if err != nil || !available {
		return errors.Join(err, errors.New("SERENA_CONSUMER_PUBLICATION_UNAVAILABLE"))
	}
	return nil
}
func (s *PreparedSerenaConsumerInvocation) CloseObserved() (*PreparedSerenaConsumerInvocationCloseObservation, error) {
	if s == nil {
		return nil, errors.New("SERENA_CONSUMER_INVOCATION_MISSING")
	}
	s.mu.Lock()
	s.sealed = true
	if s.close != nil {
		p := s.close
		s.mu.Unlock()
		return p, p.view.Error
	}
	run := s.entered == 0 && !s.finalizing
	if run {
		s.finalizing = true
	}
	s.mu.Unlock()
	if run {
		s.finalize()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.close == nil {
		return &PreparedSerenaConsumerInvocationCloseObservation{owner: s, view: PreparedSerenaConsumerInvocationCloseSnapshot{OperationID: s.request.OperationID, Pending: true}}, errors.New("SERENA_CONSUMER_INVOCATION_CLOSE_PENDING")
	}
	return s.close, s.close.view.Error
}
func (s *PreparedSerenaConsumerInvocation) finalize() {
	v := PreparedSerenaConsumerInvocationCloseSnapshot{OperationID: s.request.OperationID, Ready: "not_attempted", Operation: "not_attempted", IntentNamespace: "not_attempted", Published: "not_attempted", Package: "not_attempted", Intent: "not_attempted", Readers: "closed"}
	var err error
	if s.packageAttempted {
		v.Package = "unknown"
		if s.packageLease != nil {
			p, e := s.packageLease.CloseObserved()
			s.packageClose = p
			err = errors.Join(err, e, p.Validate(s.packageLease))
			if e == nil && p.Validate(s.packageLease) == nil {
				v.Package = "closed"
			}
		}
	}
	if s.borrowedIntent != nil {
		v.Intent = "borrowed_pending"
		if s.intentClose != nil && s.intentClose.Validate(s.borrowedIntent) == nil {
			v.Intent = "closed"
		}
	} else if s.ownedIntentAttempted {
		v.Intent = "unknown"
		if s.intentRelease != nil {
			err = errors.Join(err, s.intentRelease())
		}
		if s.ownedIntent != nil {
			p, e := s.ownedIntent.CloseObserved()
			s.intentClose = p
			err = errors.Join(err, e, p.Validate(s.ownedIntent))
			if e == nil && p.Validate(s.ownedIntent) == nil {
				v.Intent = "closed"
			}
		}
	}
	for _, slot := range []struct {
		attempted bool
		n         *SerenaRecordNamespaceObservation
		state     *string
	}{{s.operationAttempted, s.operation, &v.Operation}, {s.readyAttempted, s.ready, &v.Ready}} {
		if slot.attempted {
			*slot.state = "unknown"
			if slot.n != nil {
				p, e := slot.n.CloseObserved()
				err = errors.Join(err, e, p.ValidateSource(slot.n))
				if e == nil && p.ValidateSource(slot.n) == nil {
					*slot.state = "closed"
				}
			}
		}
	}
	if s.intentAttempted {
		v.IntentNamespace = "unknown"
		if s.intentNamespace != nil {
			p, e := s.intentNamespace.CloseObserved()
			err = errors.Join(err, e, p.ValidateSource(s.intentNamespace))
			for _, mutation := range s.heldMutations {
				err = errors.Join(err, mutation.observation.ReceiveNamespaceClose(p), mutation.observation.Validate(mutation.observation.ctx, mutation.input, mutation.afterSHA))
			}
			for _, restoration := range s.heldRestorations {
				err = errors.Join(err, restoration.ReceiveNamespaceClose(p), restoration.Validate(restoration.ctx, restoration.input, restoration.commit))
			}
			if e == nil && p.ValidateSource(s.intentNamespace) == nil {
				v.IntentNamespace = "closed"
			}
		}
	}
	if s.publishedAttempted {
		v.Published = "unknown"
		if s.published != nil {
			p, e := s.published.CloseObserved()
			err = errors.Join(err, e, p.Validate(s.published))
			if e == nil && p.Validate(s.published) == nil {
				v.Published = "closed"
			}
		}
	}
	if !time.Now().Before(s.deadline) {
		err = errors.Join(err, context.DeadlineExceeded)
	}
	if s.io.intent != nil {
		s.io.intent.mu.Lock()
		s.readers = append(s.readers, s.io.intent.readers...)
		err = errors.Join(err, s.io.intent.cleanupErr)
		s.io.intent.mu.Unlock()
	}
	for _, reader := range s.readers {
		if reader == nil || !reader.finalized || !reader.view.OwnedResourcesComplete || !reader.view.BorrowedResourcesComplete {
			v.Readers = "unknown"
			err = errors.Join(err, errors.New("SERENA_CONSUMER_READER_CLOSE_UNQUALIFIED"))
		}
	}
	v.Complete = err == nil
	for _, state := range []string{v.Ready, v.Operation, v.IntentNamespace, v.Published, v.Package, v.Intent, v.Readers} {
		if state != "closed" && state != "not_attempted" {
			v.Complete = false
		}
	}
	v.Error = err
	s.mu.Lock()
	s.close = &PreparedSerenaConsumerInvocationCloseObservation{owner: s, view: v}
	s.cleanup = err
	s.mu.Unlock()
}
func (p *PreparedSerenaConsumerInvocationCloseObservation) Snapshot() PreparedSerenaConsumerInvocationCloseSnapshot {
	if p == nil {
		return PreparedSerenaConsumerInvocationCloseSnapshot{}
	}
	return p.view
}
func (p *PreparedSerenaConsumerInvocationCloseObservation) Validate(original *PreparedSerenaConsumerInvocation) error {
	if p == nil || original == nil || p.owner != original {
		return errors.New("SERENA_CONSUMER_CLOSE_FOREIGN")
	}
	original.mu.Lock()
	defer original.mu.Unlock()
	if original.close != p || !p.view.Complete || p.view.Pending || p.view.Error != nil {
		return errors.Join(errors.New("SERENA_CONSUMER_CLOSE_UNQUALIFIED"), p.view.Error)
	}
	return nil
}

func (s *PreparedSerenaConsumerInvocation) validateProtectedOperation(ctx context.Context) error {
	raw, reader, err := ReadStateFileInodeAnchoredObserved(ctx, s.operation.path, 64<<20)
	s.readers = append(s.readers, reader)
	if err != nil {
		return err
	}
	if err = reader.Validate(ctx, s.operation.path, 64<<20, raw, preparedSerenaHash(raw)); err != nil {
		return err
	}
	op, err := decodePreparedSerenaOperation(raw)
	if err != nil {
		return err
	}
	if op.SourceEpoch != s.io.sourceSHA256 {
		return errors.New("SERENA_CONSUMER_PROTECTED_OPERATION_DRIFT")
	}
	leaf, err := s.operation.LeafParentSHA256(ctx)
	if err != nil || leaf != op.LeafParentSHA256 {
		return errors.Join(err, errors.New("SERENA_CONSUMER_OPERATION_LEAF_DRIFT"))
	}
	return s.publication.validateCurrentOperation(ctx, s.record, s.request.Binding.RecordSHA256, op, raw)
}
