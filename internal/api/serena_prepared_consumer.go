package api

import (
	"context"
	"errors"
	"mcp-local-hub/internal/process"
	"time"
)

// One retained composition supplies the concrete effects and publication
// observation to every API handle it creates. A cold NewAPI cannot manufacture
// publication authority from phase bytes or an installed executable's name.
func (a *API) BindPreparedSerenaRuntimeOwner(owner *PreparedSerenaRuntimeOwner) error {
	if a == nil || owner == nil {
		return errors.New("SERENA_PREPARED_OWNER_UNAVAILABLE")
	}
	incoming, ok := owner.effects.(*preparedSerenaRuntimeEffects)
	if !ok || incoming == nil || incoming.publication != a.serenaPreparationPublication || incoming.finishObservation == nil || incoming.finishObservation.err != nil || !incoming.finishObservation.view.InOriginalBound || owner.completedRecord == nil || owner.completedBinding == nil {
		return errors.New("SERENA_PREPARED_OWNER_PUBLICATION_UNRECEIVED")
	}
	a.preparedSerenaMu.Lock()
	defer a.preparedSerenaMu.Unlock()
	if a.preparedSerenaOwner != nil && a.preparedSerenaOwner != owner {
		original, ok := a.preparedSerenaOwner.effects.(*preparedSerenaRuntimeEffects)
		if !ok || original == nil || original.publication != incoming.publication || original.Root != incoming.Root || original.IntentPath != incoming.IntentPath || original.sourceSHA256 != incoming.sourceSHA256 || original.packageBinding == nil || incoming.packageBinding == nil || !sameSerenaNamespaceID(original.packageBinding.identity.runtimeRoot, incoming.packageBinding.identity.runtimeRoot) {
			return errors.New("SERENA_PREPARED_OWNER_REBIND_REFUSED")
		}
		return nil
	}
	a.preparedSerenaOwner = owner
	return nil
}
func (a *API) preparedSerenaRuntimeOwner() (*PreparedSerenaRuntimeOwner, error) {
	if a == nil {
		return nil, errors.New("SERENA_RUNTIME_INSTALL_NEEDED")
	}
	a.preparedSerenaMu.Lock()
	defer a.preparedSerenaMu.Unlock()
	if a.preparedSerenaOwner == nil {
		return nil, errors.New("SERENA_PREPARED_COLD_PUBLICATION_UNAVAILABLE")
	}
	return a.preparedSerenaOwner, nil
}

// PreparedSerenaConsumerOwner returns the same injected composition. A cold
// API has no synthetic owner and cannot turn persisted phase bytes into one.
func (a *API) PreparedSerenaConsumerOwner() (*PreparedSerenaRuntimeOwner, error) {
	return a.preparedSerenaRuntimeOwner()
}

func (a *API) PreparedSerenaConsumerOwnerObserved(ctx context.Context, launch *PreparedSerenaLaunchObservation) (*PreparedSerenaRuntimeOwner, error) {
	if a == nil || launch == nil || launch.api != a {
		return nil, errors.New("SERENA_COLD_LAUNCH_SOURCE_MISMATCH")
	}
	if err := launch.validateSource(ctx); err != nil {
		return nil, err
	}
	a.preparedSerenaMu.Lock()
	existing := a.preparedSerenaOwner
	a.preparedSerenaMu.Unlock()
	request := PreparedSerenaConsumerInvocationRequest{OperationID: launch.operationID, Binding: launch.binding, ExpectedDescriptors: []SupervisorDaemon{launch.descriptor}}
	if existing != nil {
		scope, err := existing.AcquireConsumerInvocation(ctx, request)
		if err != nil {
			return nil, err
		}
		proof, closeErr := scope.CloseObserved()
		if err = errors.Join(closeErr, proof.Validate(scope)); err != nil {
			return nil, err
		}
		return existing, nil
	}
	io, err := NewPreparedSerenaRecordIO(launch.root, launch.intentPath, launch.operationID, launch.epoch)
	if err != nil {
		return nil, err
	}
	raw, exists, reader, err := io.readReadyObserved(ctx, launch.binding.RequestIdentitySHA256)
	if err != nil || !exists {
		return nil, errors.Join(err, errors.New("SERENA_COLD_READY_UNAVAILABLE"))
	}
	if reader == nil {
		return nil, errors.New("SERENA_COLD_READY_READER_MISSING")
	}
	record, err := decodePreparedSerenaRecord(raw)
	if err != nil {
		return nil, err
	}
	actual, err := record.Binding(raw)
	if err != nil || actual != launch.binding {
		return nil, errors.Join(err, errors.New("SERENA_COLD_READY_BINDING_DRIFT"))
	}
	policySHA, err := launch.policy.IdentitySHA256()
	if err != nil || record.PolicySHA256 != policySHA {
		return nil, errors.Join(err, errors.New("SERENA_COLD_POLICY_DRIFT"))
	}
	io, err = NewPreparedSerenaRecordIO(launch.root, launch.intentPath, record.OperationID, launch.epoch)
	if err != nil {
		return nil, err
	}
	reads, err := io.observeColdPublicationReads(ctx, record, launch.policy.OwnedTreeLimits)
	if err != nil {
		return nil, err
	}
	projection, err := launch.environment.Projection(ctx)
	if err != nil {
		return nil, err
	}
	projectionSHA, err := projection.SHA256()
	if err != nil {
		return nil, err
	}
	if projection.PackageManifestProjectionSHA256 != reads.operation.PackageManifestProjectionSHA256 || projectionSHA != reads.operation.BuildEnvironmentSHA256 {
		return nil, errors.New("SERENA_COLD_CURRENT_BUILD_PROJECTION_DRIFT")
	}
	if err = launch.environment.Validate(ctx, launch.rawManifestSHA, launch.task, policySHA, projectionSHA); err != nil {
		return nil, err
	}
	publisher := *reads.operation.PublisherIncarnation
	quiescence, err := process.LookupPublisherIncarnation(ctx, publisher, coldPublisherBinding(publisher))
	if err != nil {
		return nil, err
	}
	publication := a.serenaPreparationPublication
	if publication == nil {
		return nil, errors.New("SERENA_COLD_PUBLICATION_OWNER_MISSING")
	}
	recovered, err := publication.RecoverColdPublicationObserved(ctx, record, reads.operation, quiescence, reads)
	if err != nil {
		return nil, err
	}
	owner := &PreparedSerenaRuntimeOwner{consumer: &preparedSerenaConsumerCoordinator{root: launch.root, intentPath: launch.intentPath, sourceEpoch: launch.epoch, publication: publication, runtimeIdentity: reads.identity.runtimeRoot, limits: launch.policy.OwnedTreeLimits, recovered: recovered}}
	a.preparedSerenaMu.Lock()
	if a.preparedSerenaOwner != nil && a.preparedSerenaOwner != owner {
		a.preparedSerenaMu.Unlock()
		return nil, errors.New("SERENA_COLD_CONCURRENT_OWNER_BINDING")
	}
	a.preparedSerenaOwner = owner
	a.preparedSerenaMu.Unlock()
	return owner, nil
}

func (o *PreparedSerenaRuntimeOwner) InspectReady(ctx context.Context, request PreparedSerenaConsumerInvocationRequest) (record PreparedSerenaRuntimeRecord, err error) {
	if ctx == nil {
		return record, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	scope, err := o.AcquireConsumerInvocation(ctx, request)
	if err != nil {
		return record, err
	}
	defer func() {
		proof, closeErr := scope.CloseObserved()
		err = errors.Join(err, closeErr, proof.Validate(scope))
		if err != nil {
			record = PreparedSerenaRuntimeRecord{}
		}
	}()
	err = scope.receiveBody(ctx, nil, func(preparedSerenaIntentConsumerEffects) error { record = scope.record; return nil })
	return record, err
}

// Original deadlines are supplied by the descriptor's existing start policy.
// This helper adds no service timer and never resets a caller deadline.
func PreparedSerenaStartContext(parent context.Context, existingBindLimit time.Duration) (context.Context, context.CancelFunc, error) {
	if parent == nil || existingBindLimit <= 0 {
		return nil, nil, errors.New("SERENA_PREPARED_DEADLINE_MISSING")
	}
	end := time.Now().Add(existingBindLimit)
	if original, ok := parent.Deadline(); ok && original.Before(end) {
		end = original
	}
	ctx, cancel := context.WithDeadline(parent, end)
	return ctx, cancel, nil
}
