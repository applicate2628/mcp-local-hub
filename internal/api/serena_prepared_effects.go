package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"

	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

// This is the concrete assembly of the existing record, acquisition and
// qualification owners. It is private: serialized receipts cannot construct
// its original namespace, image, execution or publication capabilities.
type preparedSerenaRuntimeEffects struct {
	PreparedSerenaRecordIO
	acquisition                                          preparedSerenaAcquisition
	publication                                          *PreparedSerenaPublicationOwner
	operationNamespace                                   *SerenaRecordNamespaceObservation
	publisher                                            process.PublisherIncarnationRecordV1
	capture                                              *process.PublisherIncarnation
	image                                                *PublisherImageObservation
	publisherBinding                                     process.PublisherIncarnationBinding
	installation                                         PreparedSerenaInstallation
	qualification                                        PreparedSerenaQualification
	cleanup                                              PreparedSerenaCleanup
	localCloseOnce                                       sync.Once
	localCloseErr                                        error
	localClosedInBound                                   bool
	readyNamespaceAttempted, operationNamespaceAttempted bool
	packageLeaseAttempted                                bool
	packageLease                                         *StateFileObservedLease
	packageClose                                         *StateFileLeaseCloseObservation
	finishOnce                                           sync.Once
	finishObservation                                    *PreparedSerenaPreparationCloseObservation
	packageMu                                            sync.Mutex
	lastReadyWriter                                      *StateFilePublicationObservation
	lastReadyRaw                                         []byte
	readyClose                                           *StateFileFreshNamespaceCloseObservation
	priorPublishedRead                                   *SerenaPublishedGenerationObservation
	priorPublishedReadAttempted                          bool
}

func (e *preparedSerenaRuntimeEffects) LockPackage(ctx context.Context, key string) (func() error, error) {
	if e == nil || e.packageBinding == nil || key != e.packageBinding.key {
		return nil, errors.New("SERENA_PACKAGE_LOCK_BINDING_INVALID")
	}
	if _, err := e.ReadyNamespace(ctx, key); err != nil {
		return nil, err
	}
	// Resolve preflight before the package exclusion. Lock-held validation
	// receives this same read capability; it cannot reacquire a parent there.
	prior, exists, err := e.ReadReady(ctx, key)
	if err != nil {
		return nil, err
	}
	if exists {
		record, decodeErr := decodePreparedSerenaRecord(prior)
		if decodeErr != nil {
			return nil, decodeErr
		}
		e.priorPublishedReadAttempted = true
		e.priorPublishedRead, err = AcquireSerenaPublishedGenerationObserved(ctx, record, e.acquisition.inputs.Request.OwnedTreeLimits)
		if err != nil {
			return nil, err
		}
	}
	e.packageMu.Lock()
	if e.packageLeaseAttempted {
		e.packageMu.Unlock()
		return nil, errors.New("SERENA_PACKAGE_LEASE_REENTRY")
	}
	e.packageLeaseAttempted = true
	e.packageMu.Unlock()
	lease, err := AcquireStateFileObservedLease(ctx, filepath.Join(e.Root, key, "package"))
	e.packageLease = lease
	if err != nil {
		if lease != nil {
			proof, closeErr := lease.CloseObserved()
			e.packageClose = proof
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	return func() error {
		proof, closeErr := lease.CloseObserved()
		e.packageClose = proof
		return errors.Join(closeErr, proof.Validate(lease))
	}, nil
}

// This closes only this composition's local retained tree/namespace objects.
// It neither asserts child/Job settlement nor authorizes generation deletion.
// All close attempts are cached; an uncertain close is never replayed.
func (e *preparedSerenaRuntimeEffects) closeLocalResources(ctx context.Context) error {
	if e == nil {
		return errors.New("SERENA_PREPARED_LOCAL_OWNER_MISSING")
	}
	e.localCloseOnce.Do(func() {
		e.localCloseErr = e.acquisition.closeOriginalRoots()
		if e.priorPublishedRead != nil {
			proof, closeErr := e.priorPublishedRead.CloseObserved()
			e.localCloseErr = errors.Join(e.localCloseErr, closeErr, proof.Validate(e.priorPublishedRead))
		}
		for _, namespace := range []*SerenaRecordNamespaceObservation{e.operationNamespace, e.namespace} {
			if namespace == nil {
				continue
			}
			proof, closeErr := namespace.CloseObserved()
			e.localCloseErr = errors.Join(e.localCloseErr, closeErr, proof.ValidateSource(namespace))
		}
		if ctx == nil {
			e.localCloseErr = errors.Join(e.localCloseErr, errors.New("SERENA_PREPARED_CLOSE_CONTEXT_MISSING"))
			return
		}
		deadline, finite := ctx.Deadline()
		e.localClosedInBound = finite && !deadline.After(e.acquisition.inputs.Request.AbsoluteCleanupDeadline) && ctx.Err() == nil
		if !e.localClosedInBound {
			e.localCloseErr = errors.Join(e.localCloseErr, ctx.Err(), errors.New("SERENA_PREPARED_LOCAL_CLOSE_OUT_OF_BOUND"))
		}
		if e.localCloseErr != nil {
			e.publication.invalidate(e.operationID)
		}
	})
	return e.localCloseErr
}

// One unlocked install assembly supplies actual inputs and the CLI qualifier.
// The default compiled source is received BEFORE any namespace acquisition.
// Unmarked/cold products visibly refuse instead of inventing an epoch.
func NewPreparedSerenaRuntimeComposition(ctx context.Context, inputs SerenaPreparationInputs, intentPath string, qualifier PreparedSerenaQualificationPort, publication *PreparedSerenaPublicationOwner) (owner *PreparedSerenaRuntimeOwner, retErr error) {
	if ctx == nil || qualifier == nil || publication == nil {
		return nil, errors.New("SERENA_PREPARED_COMPOSITION_INPUT_MISSING")
	}
	r := inputs.Request
	key, err := r.Identity()
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		return nil, err
	}
	if err = source.Validate(source.Epoch()); err != nil {
		return nil, err
	}
	recordIO, err := NewPreparedSerenaRecordIO(r.Namespace, intentPath, r.OperationID, source.Epoch())
	if err != nil {
		return nil, err
	}
	resources, cancel := context.WithDeadline(context.WithoutCancel(ctx), r.AbsoluteCleanupDeadline)
	defer cancel()
	effects := &preparedSerenaRuntimeEffects{PreparedSerenaRecordIO: recordIO, publication: publication, acquisition: preparedSerenaAcquisition{inputs: inputs, qualifier: qualifier}}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, effects.closeLocalResources(resources))
		}
	}()
	effects.readyNamespaceAttempted = true
	namespace, err := recordIO.AcquireRecordNamespaceObserved(resources, key)
	if namespace != nil {
		effects.namespace = namespace
	}
	if err != nil {
		return nil, err
	}
	effects.PreparedSerenaRecordIO, err = recordIO.BindRecordNamespace(resources, namespace)
	if err != nil {
		return nil, err
	}
	common, err := namespace.NamespaceSHA256(ctx)
	if err != nil {
		return nil, err
	}
	publisherID, err := newSerenaPublisherOwnerInstanceID(ctx)
	if err != nil {
		return nil, err
	}
	binding := process.PublisherIncarnationBinding{OwnerInstanceID: publisherID, OperationID: r.OperationID, NamespaceSHA256: common, SourceEpoch: source.Epoch(), ResourceScope: process.PublisherExclusiveLocalResources}
	effects.capture, err = process.CapturePublisherIncarnation(ctx, binding)
	if err != nil {
		return nil, err
	}
	effects.image, err = CapturePublisherImageObserved(ctx, effects.capture, source.Epoch())
	if err != nil {
		return nil, err
	}
	effects.publisher, err = BindPublisherIncarnation(ctx, binding, effects.capture, effects.image, namespace)
	if err != nil {
		return nil, err
	}
	effects.publisherBinding = binding
	effects.operationNamespaceAttempted = true
	effects.operationNamespace, err = effects.PreparedSerenaRecordIO.AcquireOperationNamespaceObserved(resources, key, r.OperationID, true)
	if err != nil {
		return nil, err
	}
	return NewPreparedSerenaRuntimeOwner(effects)
}

func (e *preparedSerenaRuntimeEffects) BeginOperation(ctx context.Context, r PreparedSerenaRequest, key string) (PreparedSerenaOperationUpdateV2, error) {
	if e == nil || e.acquisition.inputs.Request != r || e.packageBinding == nil || key != e.packageBinding.key {
		return PreparedSerenaOperationUpdateV2{}, errors.New("SERENA_PREPARED_OPERATION_INPUT_DRIFT")
	}
	if err := e.capture.ValidateCapture(ctx, e.publisherBinding); err != nil {
		return PreparedSerenaOperationUpdateV2{}, err
	}
	if err := e.image.Validate(ctx, e.capture, e.sourceSHA256); err != nil {
		return PreparedSerenaOperationUpdateV2{}, err
	}
	common, err := e.operationNamespace.NamespaceSHA256(ctx)
	if err != nil {
		return PreparedSerenaOperationUpdateV2{}, err
	}
	leaf, err := e.operationNamespace.LeafParentSHA256(ctx)
	if err != nil {
		return PreparedSerenaOperationUpdateV2{}, err
	}
	if common != e.publisher.NamespaceSHA256 {
		return PreparedSerenaOperationUpdateV2{}, errors.New("SERENA_PREPARED_PUBLISHER_NAMESPACE_DRIFT")
	}
	return PreparedSerenaOperationUpdateV2{Record: PreparedSerenaOperationRecordV2{Schema: PreparedSerenaOperationSchema, Version: 2, OperationID: r.OperationID, GenerationID: r.GenerationID, PackageKey: key, RequestIdentitySHA256: key, PackageManifestProjectionSHA256: r.PackageManifestProjectionSHA256, BuildEnvironmentSHA256: r.BuildEnvironmentSHA256, NamespaceSchema: SerenaPackageNamespaceSchema, NamespaceSHA256: common, LeafParentSHA256: leaf, SourceEpoch: e.sourceSHA256, Phase: "prearmed", PublisherIncarnation: &e.publisher, RootDispositions: []PreparedSerenaRootDispositionV1{}, CleanupErrors: []string{}, OutcomePublication: PreparedSerenaOutcomePublicationV1{Phase: "prearmed"}}, NamespaceSource: e.operationNamespace}, nil
}

func (e *preparedSerenaRuntimeEffects) ReadyNamespace(ctx context.Context, key string) (*SerenaRecordNamespaceObservation, error) {
	if e == nil || e.namespace == nil || e.namespace.key != key {
		return nil, errors.New("SERENA_READY_NAMESPACE_UNAVAILABLE")
	}
	if err := e.namespace.validateStateFileFreshNamespace(ctx, e.namespace.path, "prepared-ready"); err != nil {
		return nil, err
	}
	return e.namespace, nil
}
func (e *preparedSerenaRuntimeEffects) AcquireRoots(ctx context.Context, r PreparedSerenaRequest, key string) (PreparedSerenaOwnedRoots, error) {
	return e.acquisition.acquireRoots(ctx, r, key)
}
func (e *preparedSerenaRuntimeEffects) Install(ctx context.Context, r PreparedSerenaRequest, roots PreparedSerenaOwnedRoots) (PreparedSerenaInstallation, error) {
	result, err := e.acquisition.install(ctx, r, roots)
	e.installation = result
	return result, err
}
func (e *preparedSerenaRuntimeEffects) CleanupAttempt(ctx context.Context, roots PreparedSerenaOwnedRoots, closure PreparedSerenaExecutionClosure) (PreparedSerenaCleanup, error) {
	result, err := e.acquisition.cleanupAttempt(ctx, roots, closure)
	e.cleanup = result
	return result, err
}
func (e *preparedSerenaRuntimeEffects) Qualify(ctx context.Context, r PreparedSerenaRequest, roots PreparedSerenaOwnedRoots, installed PreparedSerenaInstallation) (PreparedSerenaQualification, error) {
	result, err := e.acquisition.qualify(ctx, r, roots, installed)
	e.qualification = result
	if err == nil {
		err = e.publication.prepublication(e, r, e.packageBinding.key, installed.Closure, result.Closure, e.cleanup)
	}
	return result, err
}
func (e *preparedSerenaRuntimeEffects) WriteReady(ctx context.Context, key string, raw []byte) error {
	e.publication.invalidate(e.operationID)
	writer, err := e.PreparedSerenaRecordIO.writeReadyObserved(ctx, key, raw)
	e.lastReadyWriter = writer
	e.lastReadyRaw = append([]byte(nil), raw...)
	if err != nil {
		return err
	}
	return nil
}

func (e *preparedSerenaRuntimeEffects) PersistOperation(ctx context.Context, update PreparedSerenaOperationUpdateV2) (*StateFilePublicationObservation, error) {
	e.publication.invalidate(e.operationID)
	if update.Record.Phase == "published" {
		writer := e.lastReadyWriter
		if writer == nil {
			return nil, errors.New("SERENA_PREPARED_READY_WRITE_MISSING")
		}
		leaf, err := e.namespace.LeafParentSHA256(ctx)
		if err != nil {
			return nil, err
		}
		close, err := e.namespace.CloseObserved()
		e.readyClose = close
		if err != nil {
			return nil, err
		}
		if err = close.ValidateSource(e.namespace); err != nil {
			return nil, err
		}
		if err = e.publication.readyWritten(ctx, e.operationID, writer, writer.readback, e.lastReadyRaw); err != nil {
			return nil, err
		}
		writerSHA := writer.ReceiptSHA256()
		readerRaw, err := json.Marshal(writer.readback.Snapshot())
		if err != nil {
			return nil, err
		}
		readerSHA := preparedSerenaHash(readerRaw)
		writer.lease.mu.Lock()
		leaseClose := writer.lease.closeObservation
		writer.lease.mu.Unlock()
		leaseRaw, err := json.Marshal(leaseClose.Snapshot())
		if err != nil {
			return nil, err
		}
		leaseSHA := preparedSerenaHash(leaseRaw)
		namespaceRaw, err := json.Marshal(struct {
			Schema    string `json:"schema"`
			Common    string `json:"namespace_sha256"`
			Leaf      string `json:"leaf_parent_sha256"`
			Operation string `json:"operation_id"`
			Closed    bool   `json:"closed"`
		}{"serena-ready-namespace-close-v1", e.packageBinding.digest, leaf, e.operationID, close.closed})
		if err != nil {
			return nil, err
		}
		namespaceSHA := preparedSerenaHash(namespaceRaw)
		readySHA := writer.SHA256()
		operation := e.operationID
		update.Record.ReadyPublication = PreparedSerenaReadyPublicationV1{Known: true, ReadyRecordSHA256: &readySHA, SourceOperationID: &operation, WriterReceiptSHA256: &writerSHA, ReaderReceiptSHA256: &readerSHA, LeaseReceiptSHA256: &leaseSHA, NamespaceReceiptSHA256: &namespaceSHA, InstallationReceiptSHA256: &e.installation.Closure.ReceiptSHA256, QualificationReceiptSHA256: &e.qualification.ReceiptSHA256, CleanupReceiptSHA256: &e.cleanup.ReceiptSHA256}
	}
	return e.PreparedSerenaRecordIO.PersistOperation(ctx, update)
}
func (e *preparedSerenaRuntimeEffects) PublicationAvailable(ctx context.Context, r PreparedSerenaRuntimeRecord) (bool, error) {
	if err := e.ValidateRecord(ctx, r); err != nil {
		return false, err
	}
	raw, exists, err := e.ReadReady(ctx, r.RequestIdentitySHA256)
	if err != nil || !exists {
		return false, err
	}
	return e.publication.available(ctx, r, preparedSerenaHash(raw))
}
func (e *preparedSerenaRuntimeEffects) ValidateRecord(ctx context.Context, r PreparedSerenaRuntimeRecord) (retErr error) {
	if e == nil || e.packageBinding == nil || r.RequestIdentitySHA256 != e.packageBinding.key || r.NamespaceSHA256 != e.packageBinding.digest {
		return errors.New("SERENA_PREPARED_RECORD_NAMESPACE_DRIFT")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err = r.Binding(raw); err != nil {
		return err
	}
	if err = revalidatePreparedSerenaFile(ctx, r.EntrypointIdentity); err != nil {
		return err
	}
	if e.finishObservation != nil || r.GenerationID != e.acquisition.inputs.Request.GenerationID || e.acquisition.generation == nil {
		if e.finishObservation == nil && e.priorPublishedRead != nil {
			return e.priorPublishedRead.ValidatePublished(ctx, r)
		}
		if e.packageLease != nil && e.packageLease.ValidateHeld(ctx, filepath.Join(e.Root, e.packageBinding.key, "package")) == nil {
			return errors.New("SERENA_PUBLISHED_READ_NOT_PREACQUIRED")
		}
		read, err := AcquireSerenaPublishedGenerationObserved(ctx, r, e.acquisition.inputs.Request.OwnedTreeLimits)
		if read != nil {
			defer func() {
				proof, closeErr := read.CloseObserved()
				retErr = errors.Join(retErr, closeErr, proof.Validate(read), ctx.Err())
			}()
		}
		if err != nil {
			return err
		}
		return read.ValidatePublished(ctx, r)
	}
	inventory, err := e.acquisition.generation.Inventory(ctx)
	if err != nil {
		return err
	}
	if inventory.SHA256 != r.EnvironmentInventorySHA256 || r.EntrypointIdentity.Path != e.installation.Entrypoint.Path || r.ResolutionSHA256 != e.installation.ResolutionSHA256 || r.QualificationReceiptSHA256 != e.qualification.ReceiptSHA256 || r.CleanupReceiptSHA256 != e.cleanup.ReceiptSHA256 {
		return errors.New("SERENA_PREPARED_RECORD_RECEIPT_DRIFT")
	}
	return nil
}

func (e *preparedSerenaRuntimeEffects) References(ctx context.Context, generation string) (PreparedSerenaReferenceSnapshot, error) {
	intent, _, err := e.readPreparedIntent(ctx)
	if err != nil {
		return PreparedSerenaReferenceSnapshot{}, err
	}
	if intent.Version != 2 || intent.SerenaPreparedConsumers == nil {
		return PreparedSerenaReferenceSnapshot{}, errors.New("SERENA_PREPARED_CONSUMER_BLOCK_REQUIRED")
	}
	result := PreparedSerenaReferenceSnapshot{}
	for _, row := range intent.SerenaPreparedConsumers.Bindings {
		if row.GenerationID != generation {
			continue
		}
		if err = row.Validate(); err != nil {
			return result, err
		}
		ref := PreparedSerenaConsumerReference{OperationID: row.OperationID, ConsumerID: row.TaskName, RequestIdentitySHA256: row.PackageRequestIdentitySHA256, GenerationID: row.GenerationID, ReadyRecordSHA256: row.ReadyRecordSHA256, Disposition: row.Disposition}
		switch row.Disposition {
		case "current":
			result.Current = append(result.Current, ref)
		case "rollback":
			result.Rollback = append(result.Rollback, ref)
		case "pending":
			result.Pending = append(result.Pending, ref)
		}
	}
	// A different/cold generation has no original tree or execution capabilities.
	// Empty persisted rows alone never supply their closure.
	result.OriginalConsumersClosed = generation == e.acquisition.inputs.Request.GenerationID && e.acquisition.generation != nil && e.installation.Closure.complete(e.operationID) && e.qualification.Closure.complete(e.operationID) && len(result.Current)+len(result.Rollback)+len(result.Pending) == 0
	return result, nil
}
func (e *preparedSerenaRuntimeEffects) RetireGeneration(ctx context.Context, r PreparedSerenaRuntimeRecord, refs PreparedSerenaReferenceSnapshot) error {
	if !refs.OriginalConsumersClosed || len(refs.Current)+len(refs.Rollback)+len(refs.Pending) != 0 || e.acquisition.generation == nil || r.GenerationID != e.acquisition.inputs.Request.GenerationID {
		return errors.New("SERENA_PREPARED_RETIREMENT_CUSTODY_UNAVAILABLE")
	}
	if err := e.ValidateRecord(ctx, r); err != nil {
		return err
	}
	raw, exists, err := e.ReadReady(ctx, r.RequestIdentitySHA256)
	if err != nil {
		return err
	}
	if exists {
		selected, err := decodePreparedSerenaRecord(raw)
		if err != nil {
			return err
		}
		if selected.GenerationID == r.GenerationID {
			return errors.New("SERENA_PREPARED_READY_REFERENCE_STILL_SELECTS_GENERATION")
		}
	}
	e.publication.invalidate(r.OperationID)
	removed, err := e.acquisition.generation.RemoveOwned(ctx, true, e.qualification.Closure)
	if !removed.Complete() {
		err = errors.Join(err, errors.New("SERENA_PREPARED_GENERATION_REMOVAL_PENDING"))
	}
	return err
}

// Compile-time checks name the actual single storage/acquisition assembly.
var _ PreparedSerenaRuntimeEffects = (*preparedSerenaRuntimeEffects)(nil)
var _ preparedSerenaIntentConsumerEffects = (*preparedSerenaRuntimeEffects)(nil)
