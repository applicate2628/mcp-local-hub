package api

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"mcp-local-hub/internal/process"
)

type PreparedConsumerRequestV1 struct {
	OperationID, ConsumerKind string
	Binding                   PreparedSerenaRuntimeBinding
	ExpectedDescriptor        SupervisorDaemon
}
type PreparedConsumerStartEvidence interface {
	ValidateOriginalBinding(context.Context, PreparedConsumerBindingV1) error
	OriginalStartObservation() *process.OriginalStartObservation
}
type PreparedConsumerTerminalEvidence interface {
	ValidateOriginalBinding(context.Context, PreparedConsumerBindingV1) error
	NativeSettlement() *process.ContainedProcessSettlement
	OriginalIO() PreparedConsumerIOEvidence
}
type PreparedConsumerIOEvidence interface {
	ValidateOriginalIO(context.Context, PreparedConsumerBindingV1) error
}

type PreparedConsumerNoChildReleaseEvidence interface {
	ValidateNoChildRelease(context.Context, PreparedConsumerBindingV1) error
}
type PreparedConsumerNoChildReleaseReceipt struct {
	ticket *PreparedConsumerTicket
	source PreparedConsumerNoChildReleaseEvidence
}
type PreparedConsumerTicket struct {
	owner             *PreparedSerenaRuntimeOwner
	binding           PreparedConsumerBindingV1
	descriptor        SupervisorDaemon
	entrypoint        PreparedSerenaFile
	intentGeneration  uint64
	source            any
	registration      *preparedConsumerRegistration
	readyBinding      PreparedSerenaRuntimeBinding
	registrationClose *PreparedSerenaConsumerInvocationCloseObservation
}
type preparedConsumerRegistration struct {
	generation         uint64
	priorDescriptor    *SupervisorDaemon
	priorBindings      []PreparedConsumerBindingV1
	registeredBindings []PreparedConsumerBindingV1
}

// Only the received factories below construct these; an intent/native snapshot
// is not a source, native or stream capability.
type PreparedConsumerStartReceipt struct {
	ticket   *PreparedConsumerTicket
	source   PreparedConsumerStartEvidence
	original *process.OriginalStartObservation
}
type PreparedConsumerTerminalReceipt struct {
	ticket *PreparedConsumerTicket
	source PreparedConsumerTerminalEvidence
	native *process.ContainedProcessSettlement
	io     PreparedConsumerIOEvidence
}

// The concrete implementation is the SAME SupervisorIntent mutation owner.
// No optional legacy phase writer or in-memory reference count can satisfy it.
type preparedSerenaIntentConsumerEffects interface {
	registerPreparedConsumer(context.Context, PreparedConsumerBindingV1, SupervisorDaemon) (*preparedConsumerRegistration, error)
	validatePreparedConsumerPending(context.Context, *PreparedConsumerTicket) error
	bindPreparedConsumerStart(context.Context, *PreparedConsumerTicket, *PreparedConsumerStartReceipt) error
	confirmPreparedConsumer(context.Context, *PreparedConsumerTicket, SupervisorDaemon) error
	finishPreparedConsumer(context.Context, *PreparedConsumerTicket, *PreparedConsumerTerminalReceipt) error
	finishPreparedConsumerNoChild(context.Context, *PreparedConsumerTicket, *PreparedConsumerNoChildReleaseReceipt) error
}

func (o *PreparedSerenaRuntimeOwner) RegisterPendingConsumer(ctx context.Context, r PreparedConsumerRequestV1) (ticket *PreparedConsumerTicket, err error) {
	if ctx == nil || !preparedSerenaID(r.OperationID) {
		return nil, errors.New("SERENA_PREPARED_CONSUMER_REQUEST_INVALID")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("SERENA_PREPARED_DEADLINE_MISSING")
	}
	descriptorBinding, err := preparedSerenaDescriptorBinding(r.ExpectedDescriptor)
	if err != nil || descriptorBinding == nil || *descriptorBinding != r.Binding {
		return nil, errors.Join(err, errors.New("SERENA_PREPARED_CONSUMER_DESCRIPTOR_DRIFT"))
	}
	kind := "global"
	if r.ExpectedDescriptor.Workspace != "" {
		kind = "workspace"
	}
	if kind != r.ConsumerKind {
		return nil, errors.New("SERENA_PREPARED_CONSUMER_KIND_MISMATCH")
	}
	descriptorSHA, err := preparedSerenaDescriptorSHA(r.ExpectedDescriptor)
	if err != nil {
		return nil, err
	}
	binding := PreparedConsumerBindingV1{OperationID: r.OperationID, TaskName: r.ExpectedDescriptor.TaskName, ConsumerKind: kind, PackageRequestIdentitySHA256: r.Binding.RequestIdentitySHA256, GenerationID: r.Binding.GenerationID, ReadyRecordSHA256: r.Binding.RecordSHA256, SourceSHA256: r.Binding.SourceSHA256, EntrypointSHA256: r.Binding.EntrypointSHA256, DescriptorSHA256: descriptorSHA, Disposition: "pending", StartDisposition: "not_attempted"}
	if err = binding.Validate(); err != nil {
		return nil, err
	}
	scope, err := o.AcquireConsumerInvocation(ctx, PreparedSerenaConsumerInvocationRequest{OperationID: r.OperationID, Binding: r.Binding, ExpectedDescriptors: []SupervisorDaemon{r.ExpectedDescriptor}})
	if err != nil {
		return nil, err
	}
	defer func() {
		proof, closeErr := scope.CloseObserved()
		closeErr = errors.Join(closeErr, proof.Validate(scope))
		if ticket != nil {
			ticket.registrationClose = proof
		}
		err = errors.Join(err, closeErr)
	}()
	err = scope.receiveBody(ctx, nil, func(store preparedSerenaIntentConsumerEffects) error {
		registration, writeErr := store.registerPreparedConsumer(ctx, binding, r.ExpectedDescriptor)
		if registration != nil {
			ticket = &PreparedConsumerTicket{owner: o, binding: binding, descriptor: r.ExpectedDescriptor, entrypoint: scope.record.EntrypointIdentity, intentGeneration: registration.generation, registration: registration, readyBinding: r.Binding}
		}
		if writeErr != nil {
			return writeErr
		}
		if registration == nil || registration.generation == 0 {
			return errors.New("SERENA_PREPARED_INTENT_EPOCH_UNDEFINED")
		}
		return nil
	})
	return ticket, err
}
func (t *PreparedConsumerTicket) ValidateBeforeCreate(ctx context.Context, command string, environment []string) error {
	if t == nil || t.registrationClose == nil || t.registrationClose.Validate(t.registrationClose.owner) != nil {
		return errors.New("SERENA_PREPARED_REGISTRATION_CLOSE_UNQUALIFIED")
	}
	if t == nil || t.owner == nil || command != t.entrypoint.Path {
		return errors.New("SERENA_PREPARED_CHILD_COMMAND_DRIFT")
	}
	if err := ValidatePreparedSerenaEnvironment(preparedSerenaEnvironmentMap(environment)); err != nil {
		return err
	}
	return t.owner.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
		if err := store.validatePreparedConsumerPending(ctx, t); err != nil {
			return err
		}
		return revalidatePreparedSerenaFile(ctx, t.entrypoint)
	})
}
func (t *PreparedConsumerTicket) Binding() (PreparedConsumerBindingV1, error) {
	if t == nil || t.owner == nil {
		return PreparedConsumerBindingV1{}, errors.New("SERENA_PREPARED_TICKET_UNAVAILABLE")
	}
	copy := t.binding
	if copy.NativeBinding != nil {
		native := *copy.NativeBinding
		copy.NativeBinding = &native
	}
	if copy.ClosureReceiptSHA256 != nil {
		digest := *copy.ClosureReceiptSHA256
		copy.ClosureReceiptSHA256 = &digest
	}
	return copy, nil
}

func (t *PreparedConsumerTicket) SelectedEntrypoint(ctx context.Context) (file PreparedSerenaFile, err error) {
	if t == nil || t.owner == nil || t.registrationClose == nil || t.registrationClose.Validate(t.registrationClose.owner) != nil {
		return file, errors.New("SERENA_PREPARED_ENTRYPOINT_TICKET_UNQUALIFIED")
	}
	err = t.owner.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
		if err := store.validatePreparedConsumerPending(ctx, t); err != nil {
			return err
		}
		if err := revalidatePreparedSerenaFile(ctx, t.entrypoint); err != nil {
			return err
		}
		file = t.entrypoint
		return nil
	})
	if err != nil {
		file = PreparedSerenaFile{}
	}
	return file, err
}

func (t *PreparedConsumerTicket) Descriptor() (SupervisorDaemon, error) {
	if t == nil || t.owner == nil {
		return SupervisorDaemon{}, errors.New("SERENA_PREPARED_TICKET_UNAVAILABLE")
	}
	copy := cloneSupervisorIntentFile(&SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{t.descriptor}})
	return copy.Daemons[0], nil
}
func (t *PreparedConsumerTicket) bindSource(source any) error {
	if source == nil || reflect.TypeOf(source).Kind() != reflect.Pointer || reflect.ValueOf(source).IsNil() {
		return errors.New("SERENA_PREPARED_EVIDENCE_SOURCE_UNAVAILABLE")
	}
	if t.source != nil && t.source != source {
		return errors.New("SERENA_PREPARED_EVIDENCE_SOURCE_DRIFT")
	}
	t.source = source
	return nil
}
func (o *PreparedSerenaRuntimeOwner) ReceivePreparedConsumerStart(ctx context.Context, t *PreparedConsumerTicket, e PreparedConsumerStartEvidence) (*PreparedConsumerStartReceipt, error) {
	if t == nil || t.owner != o || e == nil {
		return nil, errors.New("SERENA_PREPARED_START_SOURCE_UNAVAILABLE")
	}
	if err := e.ValidateOriginalBinding(ctx, t.binding); err != nil {
		return nil, err
	}
	if err := t.bindSource(e); err != nil {
		return nil, err
	}
	original := e.OriginalStartObservation()
	if original == nil {
		return nil, errors.New("SERENA_PREPARED_ORIGINAL_START_UNDEFINED")
	}
	// Partial/unknown original observations remain received evidence, never
	// activation. Bind records their actual identity only when defined.
	return &PreparedConsumerStartReceipt{ticket: t, source: e, original: original}, nil
}
func (o *PreparedSerenaRuntimeOwner) BindOriginalConsumerStart(ctx context.Context, t *PreparedConsumerTicket, e PreparedConsumerStartEvidence) error {
	received, err := o.ReceivePreparedConsumerStart(ctx, t, e)
	if err != nil {
		return err
	}
	return o.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
		return store.bindPreparedConsumerStart(ctx, t, received)
	})
}
func (o *PreparedSerenaRuntimeOwner) ConfirmCurrentConsumer(ctx context.Context, t *PreparedConsumerTicket, expected SupervisorDaemon) error {
	if t == nil || t.owner != o || t.binding.NativeBinding == nil || t.binding.StartDisposition != "confirmed" {
		return errors.New("SERENA_PREPARED_START_UNVERIFIED")
	}
	sha, err := preparedSerenaDescriptorSHA(expected)
	if err != nil || sha != t.binding.DescriptorSHA256 {
		return errors.Join(err, errors.New("SERENA_PREPARED_CONSUMER_DESCRIPTOR_DRIFT"))
	}
	return o.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
		return store.confirmPreparedConsumer(ctx, t, expected)
	})
}
func (o *PreparedSerenaRuntimeOwner) ReceivePreparedConsumerTerminal(ctx context.Context, t *PreparedConsumerTicket, e PreparedConsumerTerminalEvidence) (*PreparedConsumerTerminalReceipt, error) {
	if t == nil || t.owner != o || e == nil {
		return nil, errors.New("SERENA_PREPARED_TERMINAL_SOURCE_UNAVAILABLE")
	}
	if err := e.ValidateOriginalBinding(ctx, t.binding); err != nil {
		return nil, err
	}
	if err := t.bindSource(e); err != nil {
		return nil, err
	}
	io := e.OriginalIO()
	if io == nil {
		return nil, errors.New("SERENA_PREPARED_ORIGINAL_IO_UNDEFINED")
	}
	if err := io.ValidateOriginalIO(ctx, t.binding); err != nil {
		return nil, err
	}
	native := e.NativeSettlement()
	if !native.Complete() {
		return nil, errors.New("SERENA_PREPARED_ORIGINAL_NATIVE_UNSETTLED")
	}
	return &PreparedConsumerTerminalReceipt{ticket: t, source: e, native: native, io: io}, nil
}
func (o *PreparedSerenaRuntimeOwner) FinishPendingConsumer(ctx context.Context, t *PreparedConsumerTicket, e PreparedConsumerTerminalEvidence) error {
	if e != nil && e.NativeSettlement() == nil {
		noChild, ok := e.(PreparedConsumerNoChildReleaseEvidence)
		if !ok {
			return errors.New("SERENA_PREPARED_ORIGINAL_NATIVE_UNSETTLED")
		}
		received, err := o.ReceivePreparedConsumerNoChildRelease(ctx, t, noChild)
		if err != nil {
			return err
		}
		return o.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
			return store.finishPreparedConsumerNoChild(ctx, t, received)
		})
	}
	received, err := o.ReceivePreparedConsumerTerminal(ctx, t, e)
	if err != nil {
		return err
	}
	return o.withPreparedConsumerExclusions(ctx, t, func(store preparedSerenaIntentConsumerEffects) error {
		return store.finishPreparedConsumer(ctx, t, received)
	})
}

func (o *PreparedSerenaRuntimeOwner) ReceivePreparedConsumerNoChildRelease(ctx context.Context, t *PreparedConsumerTicket, e PreparedConsumerNoChildReleaseEvidence) (*PreparedConsumerNoChildReleaseReceipt, error) {
	if t == nil || t.owner != o || e == nil || t.binding.Disposition != "pending" || t.binding.StartDisposition != "not_attempted" || t.binding.NativeBinding != nil {
		return nil, errors.New("SERENA_PREPARED_NO_CHILD_INELIGIBLE")
	}
	if err := e.ValidateNoChildRelease(ctx, t.binding); err != nil {
		return nil, err
	}
	if err := t.bindSource(e); err != nil {
		return nil, err
	}
	return &PreparedConsumerNoChildReleaseReceipt{ticket: t, source: e}, nil
}
func (o *PreparedSerenaRuntimeOwner) withPreparedConsumerExclusions(ctx context.Context, t *PreparedConsumerTicket, run func(preparedSerenaIntentConsumerEffects) error) (err error) {
	if t == nil || t.owner != o || ctx == nil {
		return errors.New("SERENA_PREPARED_TICKET_UNAVAILABLE")
	}
	scope, err := o.AcquireConsumerInvocation(ctx, PreparedSerenaConsumerInvocationRequest{OperationID: t.binding.OperationID, Binding: t.readyBinding, ExpectedDescriptors: []SupervisorDaemon{t.descriptor}})
	if err != nil {
		return err
	}
	defer func() {
		proof, closeErr := scope.CloseObserved()
		err = errors.Join(err, closeErr, proof.Validate(scope))
	}()
	return scope.receiveBody(ctx, nil, run)
}
func preparedConsumerNativeBinding(id process.ContainedRootIdentity, operation string) (*PreparedConsumerNativeBindingV1, error) {
	if !id.Known || id.PID <= 0 || uint64(id.PID) > uint64(^uint32(0)) || id.NativeBirthFILETIME == 0 {
		return nil, errors.New("SERENA_PREPARED_ORIGINAL_IDENTITY_UNDEFINED")
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return nil, err
	}
	return &PreparedConsumerNativeBindingV1{CreatorOperationID: operation, PID: uint32(id.PID), NativeBirthFILETIME: id.NativeBirthFILETIME, NativeIdentitySHA256: preparedSerenaHash(raw)}, nil
}
