package cli

import (
	"context"
	"errors"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
	"reflect"
)

// Global and workspace callers enter the SAME pending registration and host
// combiner. Construction failure leaves its exact ticket pending until actual
// no-child/IO evidence can release it; a nil host is not such evidence.
func newPreparedConsumerHTTPHost(ctx context.Context, owner *api.PreparedSerenaRuntimeOwner, descriptor api.SupervisorDaemon, cfg daemon.HTTPHostConfig) (*daemon.HTTPHost, *preparedConsumerLifecycleAdapter, error) {
	if owner == nil {
		return nil, nil, errors.New("SERENA_PREPARED_LIFECYCLE_UNAVAILABLE")
	}
	binding := descriptor.PreparedRuntime
	kind := "global"
	if descriptor.Workspace != "" {
		kind = "workspace"
		if descriptor.RuntimeSpec == nil {
			return nil, nil, errors.New("SERENA_PREPARED_DESCRIPTOR_MISSING")
		}
		binding = descriptor.RuntimeSpec.PreparedRuntime
	}
	if binding == nil {
		return nil, nil, errors.New("SERENA_PREPARED_BINDING_REQUIRED")
	}
	cleanupEnd, err := daemon.PreparedHTTPHostStartupCleanupDeadline(ctx)
	if err != nil {
		return nil, nil, err
	}
	consumerID, err := api.NewSerenaConsumerOperationID(ctx)
	if err != nil {
		return nil, nil, err
	}
	ticket, err := owner.ResolveReady(ctx, api.PreparedConsumerRequestV1{OperationID: consumerID, ConsumerKind: kind, Binding: *binding, ExpectedDescriptor: descriptor})
	if err != nil {
		return nil, nil, err
	}
	selected, selectedErr := ticket.SelectedEntrypoint(ctx)
	if selectedErr != nil {
		return nil, nil, selectedErr
	}
	cfg.Command = selected.Path
	adapter, err := newPreparedConsumerLifecycleAdapter(owner, ticket, nil)
	if err != nil {
		return nil, nil, err
	}
	cfg.PreparedAdmission = adapter
	cfg.PreparedStartupCleanupDeadline = cleanupEnd
	host, err := daemon.NewHTTPHost(cfg)
	if err != nil {
		return nil, nil, err
	}
	adapter.host = host
	return host, adapter, nil
}

// Existing CLI composition is the sole combiner. It imports both actual
// owners; API never imports daemon. One adapter instance retains the original
// host's opaque observations throughout start and terminal receiving.
type preparedConsumerLifecycleAdapter struct {
	owner              *api.PreparedSerenaRuntimeOwner
	ticket             *api.PreparedConsumerTicket
	host               *daemon.HTTPHost
	identity           api.PreparedConsumerBindingV1
	start              *daemon.HTTPHostStartReceipt
	terminal           *daemon.HTTPHostTerminalReceipt
	expectedDescriptor api.SupervisorDaemon
	wrapper            *preparedWrapperServerScope
	wrapperClose       *preparedWrapperServerCloseObservation
	finishAttempted    bool
	finishError        error
}

func newPreparedConsumerLifecycleAdapter(owner *api.PreparedSerenaRuntimeOwner, ticket *api.PreparedConsumerTicket, host *daemon.HTTPHost) (*preparedConsumerLifecycleAdapter, error) {
	if owner == nil || ticket == nil {
		return nil, errors.New("SERENA_PREPARED_LIFECYCLE_UNAVAILABLE")
	}
	binding, err := ticket.Binding()
	if err != nil {
		return nil, err
	}
	descriptor, err := ticket.Descriptor()
	if err != nil {
		return nil, err
	}
	return &preparedConsumerLifecycleAdapter{owner: owner, ticket: ticket, host: host, identity: binding, expectedDescriptor: descriptor}, nil
}
func (p *preparedConsumerLifecycleAdapter) BeforeCreate(ctx context.Context, input daemon.HTTPHostPreparedInput) (daemon.HTTPHostPreparedTicket, error) {
	if p.host == nil {
		return nil, errors.New("SERENA_PREPARED_HOST_NOT_BOUND")
	}
	return p, p.ticket.ValidateBeforeCreate(ctx, input.Command, input.Environment)
}
func (p *preparedConsumerLifecycleAdapter) Started(ctx context.Context, receipt daemon.HTTPHostStartReceipt) error {
	if p.start != nil {
		return errors.New("SERENA_PREPARED_START_DELIVERED_TWICE")
	}
	p.start = &receipt
	if err := p.owner.BindOriginalConsumerStart(ctx, p.ticket, p); err != nil {
		return err
	}
	if !receipt.OriginalCreated() || !receipt.Ready || receipt.Error != nil {
		return errors.New("SERENA_PREPARED_START_PENDING")
	}
	return p.owner.ConfirmCurrentConsumer(ctx, p.ticket, p.hostDescriptor())
}
func (p *preparedConsumerLifecycleAdapter) ObserveOriginalListener(ctx context.Context, h *daemon.HTTPHost, start *daemon.HTTPHostStartReceipt) (daemon.MCPOriginalHostEndpointEvidence, error) {
	if p == nil || p.host != h {
		return nil, errors.New("SERENA_PREPARED_ENDPOINT_HOST_DRIFT")
	}
	return observeSerenaOriginalEndpoint(ctx, h, start)
}
func (p *preparedConsumerLifecycleAdapter) Terminal(ctx context.Context, receipt daemon.HTTPHostTerminalReceipt) error {
	if p.terminal != nil {
		return errors.New("SERENA_PREPARED_TERMINAL_DELIVERED_TWICE")
	}
	p.terminal = &receipt
	if p.wrapper != nil {
		return nil
	}
	return p.finishPending(ctx)
}
func (p *preparedConsumerLifecycleAdapter) finishPending(ctx context.Context) error {
	if !p.finishAttempted {
		p.finishAttempted = true
		p.finishError = p.owner.FinishPendingConsumer(ctx, p.ticket, p)
	}
	return p.finishError
}
func (p *preparedConsumerLifecycleAdapter) ReceiveWrapperServerClose(ctx context.Context, proof *preparedWrapperServerCloseObservation) error {
	if err := proof.validate(ctx, p); err != nil {
		return err
	}
	if p.wrapperClose != nil && p.wrapperClose != proof {
		return errors.New("SERENA_WRAPPER_SERVER_CLOSE_DRIFT")
	}
	p.wrapperClose = proof
	if p.terminal == nil {
		return errors.New("SERENA_PREPARED_TERMINAL_SOURCE_UNDEFINED")
	}
	return p.finishPending(ctx)
}
func (p *preparedConsumerLifecycleAdapter) hostDescriptor() api.SupervisorDaemon {
	return p.expectedDescriptor
}
func (p *preparedConsumerLifecycleAdapter) ValidateOriginalBinding(ctx context.Context, b api.PreparedConsumerBindingV1) error {
	// Disposition/native/closure change during this same acquisition; the
	// immutable source/descriptor/package tuple must remain exactly unchanged.
	a, c := p.identity, b
	a.Disposition = ""
	c.Disposition = ""
	a.StartDisposition = ""
	c.StartDisposition = ""
	a.NativeBinding = nil
	c.NativeBinding = nil
	a.ClosureReceiptSHA256 = nil
	c.ClosureReceiptSHA256 = nil
	if !reflect.DeepEqual(a, c) || p.host == nil {
		return errors.New("SERENA_PREPARED_LIFECYCLE_BINDING_DRIFT")
	}
	if p.terminal != nil {
		return p.terminal.ValidateOriginalHost(ctx, p.host)
	}
	if p.start == nil {
		return errors.New("SERENA_PREPARED_START_SOURCE_UNDEFINED")
	}
	return p.start.ValidateOriginalHost(ctx, p.host)
}
func (p *preparedConsumerLifecycleAdapter) OriginalStartObservation() *process.OriginalStartObservation {
	if p.start == nil {
		return nil
	}
	return p.start.OriginalStart
}
func (p *preparedConsumerLifecycleAdapter) NativeSettlement() *process.ContainedProcessSettlement {
	if p.terminal == nil {
		return nil
	}
	return p.terminal.NativeSettlement
}
func (p *preparedConsumerLifecycleAdapter) OriginalIO() api.PreparedConsumerIOEvidence { return p }
func (p *preparedConsumerLifecycleAdapter) ValidateOriginalIO(ctx context.Context, b api.PreparedConsumerBindingV1) error {
	if p.wrapper != nil {
		if err := p.wrapperClose.validate(ctx, p); err != nil {
			return err
		}
	}
	if err := p.ValidateOriginalBinding(ctx, b); err != nil {
		return err
	}
	if p.terminal == nil {
		return errors.New("SERENA_PREPARED_IO_UNDEFINED")
	}
	return p.terminal.OriginalIOObservation().ValidateOriginalHost(ctx, p.host)
}
func (p *preparedConsumerLifecycleAdapter) ValidateNoChildRelease(ctx context.Context, b api.PreparedConsumerBindingV1) error {
	if p.wrapper != nil {
		if err := p.wrapperClose.validate(ctx, p); err != nil {
			return err
		}
	}
	if err := p.ValidateOriginalBinding(ctx, b); err != nil {
		return err
	}
	if p.terminal == nil {
		return errors.New("SERENA_PREPARED_TERMINAL_SOURCE_UNDEFINED")
	}
	return p.terminal.ValidateNoNativeFactoryRelease(ctx, p.host)
}
