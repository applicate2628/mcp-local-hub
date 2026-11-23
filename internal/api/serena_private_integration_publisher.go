package api

import (
	"context"
	"errors"
	"path/filepath"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

// This owner is issued only from the running, source-qualified cli.test.
// Its original context and root getter remain live; it cannot be reconstructed
// from a source epoch, path, native snapshot or diagnostic image observation.
type privateSerenaPublisherOwner struct {
	api             *API
	publication     *PreparedSerenaPublicationOwner
	original        context.Context
	deadline        time.Time
	native          *process.PublisherNativeObservation
	image           *publisherImageQualification
	root            *StateDirResolutionObservation
	rootPath        string
	admitted        bool
	cacheSeedConfig *PrivateSerenaCacheSeed
	cacheSeed       *privateSerenaCacheSeed

	activityMu           sync.Mutex
	activity             privateSerenaPublisherActivity
	namespaceObservation *PrivateSerenaPublisherNamespaceObservation
	effects              *preparedSerenaRuntimeEffects
	failure              error
}

type privateSerenaPublisherActivity uint8

const (
	privateSerenaPreparationActivity privateSerenaPublisherActivity = iota + 1
	privateSerenaNamespaceActivity
)

// One opportunity is consumed before either effect path starts. It is never
// reset after failure or success; retained unknown custody cannot authorize a
// second probe/acquisition. A claim alone does not issue publisher authority.
func (o *privateSerenaPublisherOwner) claimActivity(a *API, activity privateSerenaPublisherActivity) error {
	if o == nil || a == nil || o.api != a {
		return errors.New("SERENA_PRIVATE_PUBLISHER_API_MISMATCH")
	}
	if activity != privateSerenaPreparationActivity && activity != privateSerenaNamespaceActivity {
		return errors.New("SERENA_PRIVATE_PUBLISHER_ACTIVITY_INVALID")
	}
	o.activityMu.Lock()
	defer o.activityMu.Unlock()
	if o.activity != 0 {
		return errors.New("SERENA_PRIVATE_PUBLISHER_ACTIVITY_ALREADY_CLAIMED")
	}
	o.activity = activity
	return nil
}

// NewAPIWithPrivateSerenaIntegrationPublisher admits one private integration
// publisher before its preparation assembly can run UV/Python probes. Ordinary
// Product admission and CapturePublisherImageObserved remain Product-only.
// The caller must retain the supplied finite case context through all consumers
// and cleanup. A nonnil API returned with an error retains partial observations;
// it is never admitted for preparation.
func NewAPIWithPrivateSerenaIntegrationPublisher(ctx context.Context, assembly SerenaPreparationAssembly, seeds ...PrivateSerenaCacheSeed) (*API, error) {
	if ctx == nil {
		return nil, errors.New("SERENA_PRIVATE_PUBLISHER_CONTEXT_MISSING")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return nil, errors.New("SERENA_PRIVATE_PUBLISHER_DEADLINE_MISSING")
	}
	if assembly == nil {
		return nil, errors.New("SERENA_PREPARATION_ASSEMBLY_UNAVAILABLE")
	}
	if len(seeds) > 1 {
		return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_CONFIG_COUNT")
	}
	var seedConfig *PrivateSerenaCacheSeed
	if len(seeds) == 1 {
		copy := seeds[0]
		if !filepath.IsAbs(copy.DescriptorPath) || filepath.Clean(copy.DescriptorPath) != copy.DescriptorPath || !preparedSerenaDigest(copy.DescriptorSHA256) {
			return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_CONFIG_INVALID")
		}
		seedConfig = &copy
	}
	current, ok := debug.ReadBuildInfo()
	if !testing.Testing() || !ok || current.Path != publisherTestEntrypoint {
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_ENTRYPOINT_UNQUALIFIED")
	}
	a := NewAPIWithSerenaPreparation(assembly)
	owner := &privateSerenaPublisherOwner{api: a, publication: a.serenaPreparationPublication, original: ctx, deadline: deadline, cacheSeedConfig: seedConfig}
	a.serenaPrivatePublisher = owner
	var err error
	owner.native, err = process.ObserveCurrentPublisherNative(ctx)
	if err != nil {
		return a, err
	}
	if err = owner.native.Validate(ctx); err != nil {
		return a, err
	}
	owner.image, err = qualifyPublisherImageFile(ctx, owner.native.NativeImagePathForQualification(), owner.native.NativeSnapshot(), "")
	if err != nil {
		return a, err
	}
	if owner.image.role != binaryadmission.WindowsArtifactRolePublisherTest {
		return a, errors.New("SERENA_PRIVATE_PUBLISHER_ROLE_INVALID")
	}
	if err = requireCurrentPublisherTestEntrypoint(owner.image.goEntrypoint); err != nil {
		return a, err
	}
	owner.rootPath, owner.root, err = ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return a, err
	}
	if err = owner.validateTestRoot(ctx); err != nil {
		return a, err
	}
	if err = ctx.Err(); err != nil {
		return a, err
	}
	owner.admitted = true
	return a, nil
}

func (o *privateSerenaPublisherOwner) validateBounds(ctx context.Context) error {
	if ctx == nil {
		return errors.New("SERENA_PRIVATE_PUBLISHER_CONTEXT_MISSING")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if o == nil || o.original == nil || o.deadline.IsZero() {
		return errors.New("SERENA_PRIVATE_PUBLISHER_OWNER_UNAVAILABLE")
	}
	if err := o.original.Err(); err != nil {
		return err
	}
	deadline, finite := ctx.Deadline()
	if !finite || deadline.After(o.deadline) {
		return errors.New("SERENA_PRIVATE_PUBLISHER_CONTEXT_OUT_OF_BOUND")
	}
	if !time.Now().Before(o.deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func (o *privateSerenaPublisherOwner) validateTestRoot(ctx context.Context) error {
	if o == nil || o.root == nil || o.root.testPathSource == nil || o.rootPath == "" {
		return errors.New("SERENA_PRIVATE_PUBLISHER_TEST_ROOT_REQUIRED")
	}
	if err := o.root.Validate(ctx, o.rootPath); err != nil {
		return err
	}
	if o.native == nil || preparedSerenaHash([]byte(o.root.principal)) != o.native.NativeSnapshot().PrincipalSIDSHA256 {
		return errors.New("SERENA_PRIVATE_PUBLISHER_ROOT_PRINCIPAL_DRIFT")
	}
	return nil
}

func (o *privateSerenaPublisherOwner) validateOrigin(ctx context.Context) error {
	if err := o.validateBounds(ctx); err != nil {
		return err
	}
	if !o.admitted || o.api == nil || o.api.serenaPrivatePublisher != o ||
		o.publication == nil || o.api.serenaPreparationPublication != o.publication ||
		o.image == nil || o.image.source == nil || !o.image.finishedInBound ||
		!o.image.view.ResourcesClosed || o.image.role != binaryadmission.WindowsArtifactRolePublisherTest {
		return errors.New("SERENA_PRIVATE_PUBLISHER_OWNER_UNAVAILABLE")
	}
	if err := requireCurrentPublisherTestEntrypoint(o.image.goEntrypoint); err != nil {
		return err
	}
	if err := o.native.Validate(ctx); err != nil {
		return err
	}
	if err := o.image.source.Validate(o.image.view.SourceEpoch); err != nil {
		return err
	}
	return o.validateTestRoot(ctx)
}

// A new caller context is not a new grant: it must already be finite and no
// later than the original case. Cancellation from either original or caller is
// retained. Stop removes the AfterFunc registration; no worker polls or retries.
func (o *privateSerenaPublisherOwner) boundedContext(ctx context.Context, a *API) (context.Context, context.CancelFunc, error) {
	if o == nil || a == nil || o.api != a {
		return nil, nil, errors.New("SERENA_PRIVATE_PUBLISHER_API_MISMATCH")
	}
	if err := o.validateOrigin(ctx); err != nil {
		return nil, nil, err
	}
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.original, cancel)
	done := func() {
		stop()
		cancel()
	}
	if err := o.validateOrigin(bounded); err != nil {
		done()
		return nil, nil, err
	}
	return bounded, done, nil
}

func (o *privateSerenaPublisherOwner) preparationContext(ctx context.Context, a *API, override *SerenaPreparationPolicyV2) (context.Context, context.CancelFunc, error) {
	bounded, cancel, err := o.boundedContext(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	policy, err := ResolveSerenaPreparationPolicy(override)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	end, _ := bounded.Deadline()
	// Reserve the original policy's cleanup interval inside the fixed case,
	// rather than allowing end+CleanupLimit to outlive its publisher proof.
	end = end.Add(-policy.CleanupLimit)
	if !time.Now().Before(end) {
		cancel()
		return nil, nil, context.DeadlineExceeded
	}
	work, workCancel := context.WithDeadline(bounded, end)
	done := func() {
		workCancel()
		cancel()
	}
	if err = validatePublisherImageQualification(work, o.image.source, o.image.file, o.image.view, o.image.finishedInBound, o.native.NativeImagePathForQualification(), o.image.view.SourceEpoch); err != nil {
		done()
		return nil, nil, err
	}
	return work, done, nil
}

func (o *privateSerenaPublisherOwner) validateInputs(ctx context.Context, inputs SerenaPreparationInputs) error {
	if err := o.validateOrigin(ctx); err != nil {
		return err
	}
	if err := inputs.Policy.Validate(); err != nil {
		return err
	}
	r := inputs.Request
	end, finite := ctx.Deadline()
	if !finite || r.Namespace != o.rootPath || r.WorkDeadline.IsZero() || r.AbsoluteCleanupDeadline.IsZero() ||
		r.WorkDeadline.After(end) || r.CleanupLimit != inputs.Policy.CleanupLimit ||
		r.AbsoluteCleanupDeadline != r.WorkDeadline.Add(r.CleanupLimit) ||
		r.AbsoluteCleanupDeadline.After(end.Add(inputs.Policy.CleanupLimit)) ||
		r.AbsoluteCleanupDeadline.After(o.deadline) || inputs.CleanupDeadline != r.AbsoluteCleanupDeadline {
		return errors.New("SERENA_PRIVATE_PUBLISHER_PREPARATION_SCOPE_DRIFT")
	}
	// Keep the original getter object; a matching Request.Namespace string
	// neither replaces it nor repairs a getter changed during probes.
	if err := o.root.Validate(ctx, r.Namespace); err != nil {
		return err
	}
	return validatePrivateSerenaCacheSeedBinding(inputs, o)
}

func (o *privateSerenaPublisherOwner) validateCapture(ctx context.Context, capture *process.PublisherIncarnation, epoch string) error {
	if err := o.validateOrigin(ctx); err != nil {
		return err
	}
	if capture == nil || epoch != o.image.view.SourceEpoch ||
		capture.NativeSnapshot() != o.native.NativeSnapshot() ||
		capture.NativeImagePathForQualification() != o.native.NativeImagePathForQualification() {
		return errors.New("SERENA_PRIVATE_PUBLISHER_NATIVE_DRIFT")
	}
	return nil
}

func (o *privateSerenaPublisherOwner) captureImage(ctx context.Context, binding process.PublisherIncarnationBinding, capture *process.PublisherIncarnation, namespace *SerenaRecordNamespaceObservation) (*PublisherImageObservation, error) {
	if err := o.validateCapture(ctx, capture, binding.SourceEpoch); err != nil {
		return nil, err
	}
	if err := capture.ValidateCapture(ctx, binding); err != nil {
		return nil, err
	}
	if namespace == nil || namespace.io.Root != o.rootPath || namespace.io.operationID != binding.OperationID ||
		namespace.io.sourceSHA256 != binding.SourceEpoch {
		return nil, errors.New("SERENA_PRIVATE_PUBLISHER_NAMESPACE_DRIFT")
	}
	common, err := namespace.NamespaceSHA256(ctx)
	if err != nil || common != binding.NamespaceSHA256 {
		return nil, errors.Join(err, errors.New("SERENA_PRIVATE_PUBLISHER_NAMESPACE_DRIFT"))
	}
	qualified, err := qualifyPublisherImageFile(ctx, capture.NativeImagePathForQualification(), capture.NativeSnapshot(), binding.SourceEpoch)
	if err != nil {
		return nil, err
	}
	if qualified.role != binaryadmission.WindowsArtifactRolePublisherTest ||
		qualified.file != o.image.file || qualified.view != o.image.view {
		return nil, errors.New("SERENA_PRIVATE_PUBLISHER_IMAGE_DRIFT")
	}
	if err = requireCurrentPublisherTestEntrypoint(qualified.goEntrypoint); err != nil {
		return nil, err
	}
	return &PublisherImageObservation{
		capture: capture, source: qualified.source, file: qualified.file,
		view: qualified.view, finishedInBound: qualified.finishedInBound, privatePublisher: o,
	}, nil
}

// New acquisitions retain the original cleanup horizon as their observed
// namespace bound, while work/case cancellation still stops new effects.
// The separate cleanup context remains usable for already-owned resources.
// This context alone is not publisher or namespace authority.
func (o *privateSerenaPublisherOwner) acquisitionContext(work, cleanup context.Context) (context.Context, context.CancelFunc) {
	acquisition, cancel := context.WithCancel(cleanup)
	stopWork := context.AfterFunc(work, cancel)
	stopOriginal := context.AfterFunc(o.original, cancel)
	return acquisition, func() {
		stopWork()
		stopOriginal()
		cancel()
	}
}

// A failed composition may still own its original namespace/roots. Keep those
// originals and input-probe errors on the API owner even when the public method
// returns no prepared owner. No retry or cleanup is initiated by this retention.
func (o *privateSerenaPublisherOwner) retainEffects(effects *preparedSerenaRuntimeEffects) {
	o.activityMu.Lock()
	o.effects = effects
	o.activityMu.Unlock()
}

func (o *privateSerenaPublisherOwner) retainFailure(err error) {
	o.activityMu.Lock()
	o.failure = err
	o.activityMu.Unlock()
}

// Both warm and recovered consumers keep the same closed private issuer.
// Ordinary owners have no private issuer and keep their existing behavior.
func (o *PreparedSerenaRuntimeOwner) privatePublisherOwner() *privateSerenaPublisherOwner {
	if o == nil {
		return nil
	}
	if o.consumer != nil {
		return o.consumer.privatePublisher
	}
	effects, ok := o.effects.(*preparedSerenaRuntimeEffects)
	if !ok || effects == nil || effects.image == nil {
		return nil
	}
	return effects.image.privatePublisher
}
