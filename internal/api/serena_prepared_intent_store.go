package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/clients"
	"reflect"
)

func preparedSerenaTerminalDigest(received *PreparedConsumerTerminalReceipt) (string, error) {
	if received == nil || received.ticket == nil || !received.native.Complete() {
		return "", errors.New("SERENA_PREPARED_TERMINAL_UNSETTLED")
	}
	raw, err := json.Marshal(struct {
		Binding PreparedConsumerBindingV1
		Native  any
	}{received.ticket.binding, received.native.Snapshot()})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}
func preparedSerenaNoChildDigest(binding PreparedConsumerBindingV1) (string, error) {
	raw, err := json.Marshal(struct {
		Kind    string
		Binding PreparedConsumerBindingV1
	}{"received-original-host-no-child-release", binding})
	if err != nil {
		return "", err
	}
	return preparedSerenaHash(raw), nil
}

// These mutations use the SAME SupervisorIntent schema and its observed
// exclusion. The whole-file snapshot, generation and writer are one owner;
// there is no second consumer registry or reference count.
func (io PreparedSerenaRecordIO) heldIntent(ctx context.Context) (*StateFileObservedLease, error) {
	if io.intentNamespace != nil {
		if err := io.intentNamespace.ValidateOperation(ctx, io.intentNamespace.request.SourceOperationID, io.intentRequestSHA256); err != nil {
			return nil, err
		}
	}
	if io.intent == nil {
		return nil, errors.New("SERENA_PREPARED_INTENT_OWNER_UNAVAILABLE")
	}
	io.intent.mu.Lock()
	lease := io.intent.lease
	io.intent.mu.Unlock()
	if err := lease.ValidateHeld(ctx, io.IntentPath); err != nil {
		return nil, err
	}
	return lease, nil
}

func (io PreparedSerenaRecordIO) readPreparedIntent(ctx context.Context) (*SupervisorIntentFile, []byte, error) {
	if _, err := io.heldIntent(ctx); err != nil {
		return nil, nil, err
	}
	raw, observed, err := ReadStateFileInodeAnchoredObserved(ctx, io.IntentPath, 64<<20)
	io.intent.mu.Lock()
	io.intent.readers = append(io.intent.readers, observed)
	io.intent.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	if err = observed.Validate(ctx, io.IntentPath, 64<<20, raw, preparedSerenaHash(raw)); err != nil {
		return nil, nil, err
	}
	intent, _, err := decodeSupervisorIntentFile(io.IntentPath, raw)
	return intent, raw, err
}

func (io PreparedSerenaRecordIO) mutatePreparedIntent(ctx context.Context, mutate func(*SupervisorIntentFile) error) (uint64, error) {
	lease, err := io.heldIntent(ctx)
	if err != nil {
		return 0, err
	}
	current, prior, err := io.readPreparedIntent(ctx)
	if err != nil {
		return 0, err
	}
	desired := cloneSupervisorIntentFile(current)
	if err = mutate(desired); err != nil {
		return 0, err
	}
	if desired.IntentGeneration == ^uint64(0) {
		return 0, errors.New("SERENA_PREPARED_INTENT_GENERATION_EXHAUSTED")
	}
	desired.IntentGeneration++
	raw, err := marshalSupervisorIntent(desired)
	if err != nil {
		return 0, err
	}
	transaction, err := clients.CaptureStateFileWriteSnapshotObserved(ctx, io.IntentPath, 64<<20, preparedSerenaHash(prior))
	if err != nil {
		if transaction != nil {
			_, closeErr := transaction.CloseObserved()
			io.intent.mu.Lock()
			io.intent.cleanupErr = errors.Join(io.intent.cleanupErr, closeErr)
			io.intent.mu.Unlock()
			err = errors.Join(err, closeErr)
		}
		return 0, err
	}
	publication, writeErr := WriteStateFileBytesLockHeldObserved(clients.ConfigWriteRequest{Context: ctx, Path: io.IntentPath, Contents: raw, ExpectedSnapshot: transaction.Snapshot()}, lease)
	parentClose, closeErr := transaction.CloseObserved()
	io.intent.mu.Lock()
	io.intent.cleanupErr = errors.Join(io.intent.cleanupErr, closeErr)
	io.intent.mu.Unlock()
	if publication != nil {
		closeErr = errors.Join(closeErr, publication.ReceiveBorrowedParentClose(parentClose))
		io.intent.mu.Lock()
		io.intent.publications = append(io.intent.publications, publication)
		io.intent.mu.Unlock()
	} else {
		closeErr = errors.Join(closeErr, errors.New("SERENA_PREPARED_INTENT_WRITER_UNDEFINED"))
	}
	if err = errors.Join(writeErr, closeErr, ctx.Err()); err != nil {
		return 0, err
	}
	return desired.IntentGeneration, nil
}

func preparedIntentRow(intent *SupervisorIntentFile, ticket *PreparedConsumerTicket) (int, error) {
	if intent == nil || ticket == nil || intent.Version != 2 || intent.SerenaPreparedConsumers == nil || intent.IntentGeneration != ticket.intentGeneration {
		return -1, errors.New("SERENA_PREPARED_INTENT_TICKET_DRIFT")
	}
	for i, row := range intent.SerenaPreparedConsumers.Bindings {
		if row.OperationID == ticket.binding.OperationID && row.TaskName == ticket.binding.TaskName {
			if !reflect.DeepEqual(row, ticket.binding) {
				return -1, errors.New("SERENA_PREPARED_INTENT_BINDING_DRIFT")
			}
			return i, nil
		}
	}
	return -1, errors.New("SERENA_PREPARED_INTENT_BINDING_MISSING")
}

func (io PreparedSerenaRecordIO) registerPreparedConsumer(ctx context.Context, binding PreparedConsumerBindingV1, descriptor SupervisorDaemon) (*preparedConsumerRegistration, error) {
	registration := &preparedConsumerRegistration{priorBindings: []PreparedConsumerBindingV1{}, registeredBindings: []PreparedConsumerBindingV1{}}
	epoch, err := io.mutatePreparedIntent(ctx, func(intent *SupervisorIntentFile) error {
		if err := binding.Validate(); err != nil {
			return err
		}
		if err := validatePreparedSupervisorIntent(intent); err != nil {
			return err
		}
		before := cloneSupervisorIntentFile(intent)
		for _, d := range before.Daemons {
			if d.TaskName == descriptor.TaskName {
				copy := d
				registration.priorDescriptor = &copy
				break
			}
		}
		if before.SerenaPreparedConsumers != nil {
			for _, row := range before.SerenaPreparedConsumers.Bindings {
				if row.TaskName == binding.TaskName {
					registration.priorBindings = append(registration.priorBindings, row)
				}
			}
		}
		if intent.SerenaPreparedConsumers != nil {
			for i, row := range intent.SerenaPreparedConsumers.Bindings {
				if row.OperationID == binding.OperationID && row.TaskName == binding.TaskName {
					return errors.New("SERENA_PREPARED_CONSUMER_REPLAY")
				}
				if row.TaskName == binding.TaskName {
					if row.Disposition == "pending" {
						return errors.New("SERENA_PREPARED_PRIOR_CONSUMER_PENDING")
					}
					intent.SerenaPreparedConsumers.Bindings[i].Disposition = "rollback"
				}
			}
		}
		found := false
		for i, d := range intent.Daemons {
			if d.TaskName == descriptor.TaskName {
				intent.Daemons[i] = descriptor
				found = true
				break
			}
		}
		if !found {
			intent.Daemons = append(intent.Daemons, descriptor)
		}
		if err := normalizePreparedSupervisorIntentMerge(before, intent); err != nil {
			return err
		}
		intent.SerenaPreparedConsumers.Bindings = append(intent.SerenaPreparedConsumers.Bindings, binding)
		intent.SerenaPreparedConsumers.canonicalize()
		copy := cloneSupervisorIntentFile(intent)
		for _, row := range copy.SerenaPreparedConsumers.Bindings {
			if row.TaskName == binding.TaskName {
				registration.registeredBindings = append(registration.registeredBindings, row)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	registration.generation = epoch
	return registration, nil
}

func (io PreparedSerenaRecordIO) validatePreparedConsumerPending(ctx context.Context, ticket *PreparedConsumerTicket) error {
	intent, _, err := io.readPreparedIntent(ctx)
	if err != nil {
		return err
	}
	_, err = preparedIntentRow(intent, ticket)
	if err != nil {
		return err
	}
	if ticket.binding.Disposition != "pending" || ticket.binding.StartDisposition != "not_attempted" {
		return errors.New("SERENA_PREPARED_CREATE_NOT_PENDING")
	}
	return nil
}

func (io PreparedSerenaRecordIO) updatePreparedTicket(ctx context.Context, ticket *PreparedConsumerTicket, update func(*PreparedConsumerBindingV1) error) error {
	var next PreparedConsumerBindingV1
	epoch, err := io.mutatePreparedIntent(ctx, func(intent *SupervisorIntentFile) error {
		i, err := preparedIntentRow(intent, ticket)
		if err != nil {
			return err
		}
		next = intent.SerenaPreparedConsumers.Bindings[i]
		if err = update(&next); err != nil {
			return err
		}
		if err = next.Validate(); err != nil {
			return err
		}
		intent.SerenaPreparedConsumers.Bindings[i] = next
		return nil
	})
	if err == nil {
		ticket.binding = next
		ticket.intentGeneration = epoch
	}
	return err
}

func (io PreparedSerenaRecordIO) bindPreparedConsumerStart(ctx context.Context, ticket *PreparedConsumerTicket, received *PreparedConsumerStartReceipt) error {
	if received == nil || received.ticket != ticket || received.original == nil {
		return errors.New("SERENA_PREPARED_START_RECEIPT_FOREIGN")
	}
	return io.updatePreparedTicket(ctx, ticket, func(row *PreparedConsumerBindingV1) error {
		if row.Disposition != "pending" || row.StartDisposition != "not_attempted" {
			return errors.New("SERENA_PREPARED_START_REPLAY")
		}
		row.StartDisposition = "attempted_unknown"
		identity := received.original.Identity()
		if identity.Known {
			native, err := preparedConsumerNativeBinding(identity, row.OperationID)
			if err != nil {
				return err
			}
			row.NativeBinding = native
			if received.original.Complete() {
				if err = received.original.Validate(ctx, native.PID, native.NativeBirthFILETIME); err != nil {
					return err
				}
				row.StartDisposition = "confirmed"
			}
		}
		return nil
	})
}

func (io PreparedSerenaRecordIO) confirmPreparedConsumer(ctx context.Context, ticket *PreparedConsumerTicket, descriptor SupervisorDaemon) error {
	return io.updatePreparedTicket(ctx, ticket, func(row *PreparedConsumerBindingV1) error {
		if row.Disposition != "pending" || row.StartDisposition != "confirmed" || row.NativeBinding == nil {
			return errors.New("SERENA_PREPARED_START_UNVERIFIED")
		}
		sha, err := preparedSerenaDescriptorSHA(descriptor)
		if err != nil || sha != row.DescriptorSHA256 {
			return errors.Join(err, errors.New("SERENA_PREPARED_DESCRIPTOR_DRIFT"))
		}
		row.Disposition = "current"
		return nil
	})
}

func (io PreparedSerenaRecordIO) finishPreparedConsumer(ctx context.Context, ticket *PreparedConsumerTicket, received *PreparedConsumerTerminalReceipt) error {
	if received == nil || received.ticket != ticket || !received.native.Complete() || received.io == nil {
		return errors.New("SERENA_PREPARED_TERMINAL_UNSETTLED")
	}
	if err := received.io.ValidateOriginalIO(ctx, ticket.binding); err != nil {
		return err
	}
	return io.updatePreparedTicket(ctx, ticket, func(row *PreparedConsumerBindingV1) error {
		if row.StartDisposition == "terminal" || row.StartDisposition == "no_child" {
			return errors.New("SERENA_PREPARED_TERMINAL_REPLAY")
		}
		if row.NativeBinding == nil {
			return errors.New("SERENA_PREPARED_ORIGINAL_NATIVE_UNBOUND")
		}
		view := received.native.Snapshot()
		if !view.RootIdentity.Known || view.RootIdentity.PID <= 0 || uint64(view.RootIdentity.PID) > uint64(^uint32(0)) || uint32(view.RootIdentity.PID) != row.NativeBinding.PID || view.RootIdentity.NativeBirthFILETIME != row.NativeBinding.NativeBirthFILETIME {
			return errors.New("SERENA_PREPARED_TERMINAL_IDENTITY_DRIFT")
		}
		raw, err := preparedSerenaTerminalDigest(received)
		if err != nil {
			return err
		}
		row.StartDisposition = "terminal"
		row.ClosureReceiptSHA256 = &raw
		return nil
	})
}

func (io PreparedSerenaRecordIO) finishPreparedConsumerNoChild(ctx context.Context, ticket *PreparedConsumerTicket, received *PreparedConsumerNoChildReleaseReceipt) error {
	if received == nil || received.ticket != ticket || received.source == nil {
		return errors.New("SERENA_PREPARED_NO_CHILD_FOREIGN")
	}
	if err := received.source.ValidateNoChildRelease(ctx, ticket.binding); err != nil {
		return err
	}
	if ticket.registration == nil {
		return errors.New("SERENA_PREPARED_NO_CHILD_INVERSE_UNAVAILABLE")
	}
	epoch, err := io.mutatePreparedIntent(ctx, func(intent *SupervisorIntentFile) error { return applyPreparedConsumerNoChildInverse(intent, ticket) })
	if err != nil {
		return err
	}
	ticket.intentGeneration = epoch
	digest, err := preparedSerenaNoChildDigest(ticket.binding)
	if err != nil {
		return err
	}
	ticket.binding.StartDisposition = "no_child"
	ticket.binding.ClosureReceiptSHA256 = &digest
	return nil
}

func applyPreparedConsumerNoChildInverse(intent *SupervisorIntentFile, ticket *PreparedConsumerTicket) error {
	i, err := preparedIntentRow(intent, ticket)
	if err != nil {
		return err
	}
	row := intent.SerenaPreparedConsumers.Bindings[i]
	if row.Disposition != "pending" || row.StartDisposition != "not_attempted" || row.NativeBinding != nil || ticket.registration == nil {
		return errors.New("SERENA_PREPARED_NO_CHILD_INELIGIBLE")
	}
	ownedRows := []PreparedConsumerBindingV1{}
	for _, row := range intent.SerenaPreparedConsumers.Bindings {
		if row.TaskName == ticket.binding.TaskName {
			ownedRows = append(ownedRows, row)
		}
	}
	if !reflect.DeepEqual(ownedRows, ticket.registration.registeredBindings) {
		return errors.New("SERENA_PREPARED_NO_CHILD_INVERSE_BINDING_DRIFT")
	}
	found := -1
	for j, d := range intent.Daemons {
		if d.TaskName == ticket.binding.TaskName {
			if found != -1 || !reflect.DeepEqual(d, ticket.descriptor) {
				return errors.New("SERENA_PREPARED_NO_CHILD_DESCRIPTOR_DRIFT")
			}
			found = j
		}
	}
	if found < 0 {
		return errors.New("SERENA_PREPARED_NO_CHILD_DESCRIPTOR_MISSING")
	}
	if ticket.registration.priorDescriptor != nil {
		prior := cloneSupervisorIntentFile(&SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{*ticket.registration.priorDescriptor}})
		intent.Daemons[found] = prior.Daemons[0]
	} else {
		intent.Daemons = append(intent.Daemons[:found], intent.Daemons[found+1:]...)
	}
	retained := make([]PreparedConsumerBindingV1, 0, len(intent.SerenaPreparedConsumers.Bindings))
	for _, row := range intent.SerenaPreparedConsumers.Bindings {
		if row.TaskName != ticket.binding.TaskName {
			retained = append(retained, row)
		}
	}
	// Clone through the SAME owning producer so maps/native/hash pointers cannot
	// become aliases of the retained original inverse.
	old := cloneSupervisorIntentFile(&SupervisorIntentFile{Version: 2, SerenaPreparedConsumers: &PreparedSerenaConsumersV1{Schema: PreparedSerenaConsumersSchema, Bindings: ticket.registration.priorBindings}})
	intent.SerenaPreparedConsumers.Bindings = append(retained, old.SerenaPreparedConsumers.Bindings...)
	return nil
}
