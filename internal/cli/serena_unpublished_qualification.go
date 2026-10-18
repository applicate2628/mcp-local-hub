package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
	"path/filepath"
	"reflect"
)

// The composition receives the actual original-member/listener evidence
// producer. Its absence refuses before child creation, never substitutes HTTP
// health or an available port for original endpoint ownership.
type PreparedSerenaUnpublishedQualifier struct {
	EndpointEvidence func(context.Context, *daemon.HTTPHost, *daemon.HTTPHostStartReceipt) (daemon.MCPOriginalHostEndpointEvidence, error)
}

func preparedQualificationSHA(raw []byte) string {
	v := sha256.Sum256(raw)
	return hex.EncodeToString(v[:])
}

type unpublishedQualificationHost struct {
	request          api.PreparedSerenaQualificationRequest
	inputs           api.PreparedSerenaQualificationHostInputs
	host             *daemon.HTTPHost
	start            *daemon.HTTPHostStartReceipt
	terminal         *daemon.HTTPHostTerminalReceipt
	protocol         *daemon.MCPHTTPQualificationObservation
	protocolBinding  daemon.MCPHTTPQualificationBinding
	endpointProducer func(context.Context, *daemon.HTTPHost, *daemon.HTTPHostStartReceipt) (daemon.MCPOriginalHostEndpointEvidence, error)
}

func (q *PreparedSerenaUnpublishedQualifier) QualifyUnpublished(ctx context.Context, r api.PreparedSerenaQualificationRequest) (observation *api.PreparedSerenaQualificationObservation, retErr error) {
	if err := api.ValidateUnpublishedSerenaQualificationRequest(ctx, r); err != nil {
		return nil, err
	}
	if q == nil || q.EndpointEvidence == nil || r.Resources == nil {
		return nil, errors.New("SERENA_QUALIFICATION_ORIGINAL_ENDPOINT_PRODUCER_UNAVAILABLE")
	}
	cleanup, cancel := context.WithDeadline(context.WithoutCancel(ctx), r.CleanupDeadline)
	defer cancel()
	_, err := r.Resources.ReleaseReservationObserved(ctx)
	if err != nil {
		_, closeErr := r.Resources.CloseUnstartedObserved(cleanup)
		return nil, errors.Join(err, closeErr)
	}
	inputs, err := r.Resources.HostInputs(ctx, r)
	if err != nil {
		_, closeErr := r.Resources.CloseUnstartedObserved(cleanup)
		return nil, errors.Join(err, closeErr)
	}
	original := &unpublishedQualificationHost{request: r, inputs: inputs, endpointProducer: q.EndpointEvidence}
	host, err := daemon.NewHTTPHost(daemon.HTTPHostConfig{Command: inputs.Entrypoint.Path, Args: inputs.Args, WorkingDir: inputs.WorkingDirectory, PreparedClosedEnvironment: inputs.Environment, UpstreamPort: inputs.Port, UpstreamPath: "/mcp", LogPath: filepath.Join(inputs.WorkingDirectory, "qualification.log"), PreparedAdmission: original, PreparedStartupCleanupDeadline: r.CleanupDeadline})
	if err != nil {
		_, closeErr := r.Resources.CloseUnstartedObserved(cleanup)
		return nil, errors.Join(err, closeErr)
	}
	original.host = host
	var relay *daemon.HTTPToStdioRelay
	defer func() {
		if relay != nil {
			_, closeErr := relay.CloseSessionObserved(cleanup)
			retErr = errors.Join(retErr, closeErr)
		}
		_, endpointCloseErr := host.ReleaseOriginalEndpointObserved(cleanup)
		retErr = errors.Join(retErr, endpointCloseErr)
		terminal, stopErr := host.StopObserved(cleanup)
		original.terminal = &terminal
		var received *api.PreparedSerenaQualificationObservation
		var receiveErr error
		if terminal.NativeSettlement == nil {
			received, receiveErr = api.ReceiveUnpublishedSerenaNoChildQualification(cleanup, r, original)
		} else {
			received, receiveErr = api.ReceiveUnpublishedSerenaQualification(cleanup, r, original)
		}
		observation = received
		retErr = errors.Join(retErr, stopErr, receiveErr)
	}()
	started, startErr := host.StartObserved(ctx)
	original.start = &started
	if startErr != nil {
		return nil, startErr
	}
	source, err := started.OriginalEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	native := started.OriginalStart.Identity()
	startBytes, _ := json.Marshal(native)
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/mcp", inputs.Port)
	b := daemon.MCPHTTPQualificationBinding{OperationID: r.Request.OperationID, QualificationID: r.QualificationID, RequestIdentitySHA256: inputs.RequestIdentitySHA256, GenerationID: r.Request.GenerationID, SourceSHA256: r.Installation.SourceSHA256, HostStartSHA256: preparedQualificationSHA(startBytes), EndpointSHA256: preparedQualificationSHA([]byte(endpoint)), EndpointSource: source}
	relay = &daemon.HTTPToStdioRelay{URL: endpoint}
	original.protocolBinding = b
	protocol, err := relay.QualifyMCPObserved(ctx, b)
	original.protocol = protocol
	if err != nil {
		return nil, err
	}
	close, err := relay.CloseSessionObserved(cleanup)
	if err != nil {
		return nil, err
	}
	if err = protocol.ReceiveClose(cleanup, close); err != nil {
		return nil, err
	}
	if err = host.ReceiveQualificationProtocol(ctx, protocol, b); err != nil {
		return nil, err
	}
	return nil, nil
}
func (p *unpublishedQualificationHost) ValidateNoNativeFactoryRelease(ctx context.Context, r api.PreparedSerenaQualificationRequest) error {
	if p == nil || p.host == nil || p.terminal == nil || p.request.Resources != r.Resources || p.request.Request != r.Request || p.request.QualificationID != r.QualificationID {
		return errors.New("SERENA_UNPUBLISHED_NO_CHILD_FOREIGN")
	}
	return p.terminal.ValidateNoNativeFactoryRelease(ctx, p.host)
}
func (p *unpublishedQualificationHost) BeforeCreate(ctx context.Context, input daemon.HTTPHostPreparedInput) (daemon.HTTPHostPreparedTicket, error) {
	if p == nil || p.host == nil || input.Command != p.inputs.Entrypoint.Path || input.WorkingDir != p.inputs.WorkingDirectory || !reflect.DeepEqual(input.Args, p.inputs.Args) || !reflect.DeepEqual(input.Environment, p.inputs.Environment) {
		return nil, errors.New("SERENA_QUALIFICATION_HOST_INPUT_DRIFT")
	}
	if err := p.request.Resources.BeginOriginalHostFactory(ctx, p.request); err != nil {
		return nil, err
	}
	return p, nil
}
func (p *unpublishedQualificationHost) Started(ctx context.Context, r daemon.HTTPHostStartReceipt) error {
	if err := r.ValidateOriginalHost(ctx, p.host); err != nil {
		return err
	}
	if p.start != nil {
		return errors.New("SERENA_QUALIFICATION_START_REPLAY")
	}
	p.start = &r
	return nil
}
func (p *unpublishedQualificationHost) ObserveOriginalListener(ctx context.Context, h *daemon.HTTPHost, start *daemon.HTTPHostStartReceipt) (daemon.MCPOriginalHostEndpointEvidence, error) {
	if p == nil || p.host != h || p.endpointProducer == nil {
		return nil, errors.New("SERENA_QUALIFICATION_ENDPOINT_PRODUCER_UNAVAILABLE")
	}
	return p.endpointProducer(ctx, h, start)
}
func (p *unpublishedQualificationHost) Terminal(ctx context.Context, r daemon.HTTPHostTerminalReceipt) error {
	if err := r.ValidateOriginalHost(ctx, p.host); err != nil {
		return err
	}
	if p.terminal != nil {
		return errors.New("SERENA_QUALIFICATION_TERMINAL_REPLAY")
	}
	p.terminal = &r
	return nil
}
func (p *unpublishedQualificationHost) ValidateOriginalQualification(ctx context.Context, r api.PreparedSerenaQualificationRequest) error {
	if p == nil || p.host == nil || p.request.Request != r.Request || p.request.Resources != r.Resources || p.request.QualificationID != r.QualificationID || p.start == nil || p.terminal == nil {
		return errors.New("SERENA_QUALIFICATION_ORIGINAL_SOURCE_MISMATCH")
	}
	if err := p.start.ValidateOriginalHost(ctx, p.host); err != nil {
		return err
	}
	if err := p.terminal.ValidateOriginalHost(ctx, p.host); err != nil {
		return err
	}
	original := p.start.OriginalIdentity
	terminal := p.terminal.NativeSettlement.Snapshot().RootIdentity
	if !reflect.DeepEqual(original, terminal) {
		return errors.New("SERENA_QUALIFICATION_NATIVE_IDENTITY_DRIFT")
	}
	return nil
}
func (p *unpublishedQualificationHost) NativeSettlement() *process.ContainedProcessSettlement {
	if p == nil || p.terminal == nil {
		return nil
	}
	return p.terminal.NativeSettlement
}
func (p *unpublishedQualificationHost) OriginalIO() api.PreparedSerenaQualificationIOEvidence {
	return &unpublishedQualificationIO{p}
}

type unpublishedQualificationIO struct{ owner *unpublishedQualificationHost }

func (e *unpublishedQualificationIO) ValidateOriginalQualificationIO(ctx context.Context, r api.PreparedSerenaQualificationRequest) error {
	if e == nil || e.owner == nil {
		return errors.New("SERENA_QUALIFICATION_IO_MISSING")
	}
	if err := e.owner.ValidateOriginalQualification(ctx, r); err != nil {
		return err
	}
	return e.owner.terminal.OriginalIOObservation().ValidateOriginalHost(ctx, e.owner.host)
}
func (e *unpublishedQualificationIO) ReceiptSHA256() string {
	if e == nil || e.owner == nil || e.owner.terminal == nil {
		return ""
	}
	return e.owner.terminal.OriginalIOObservation().ReceiptSHA256()
}
func (p *unpublishedQualificationHost) ProtocolEvidence() api.PreparedSerenaProtocolEvidence {
	return &unpublishedQualificationProtocol{p}
}

type unpublishedQualificationProtocol struct{ owner *unpublishedQualificationHost }

func (e *unpublishedQualificationProtocol) ValidateOriginalQualificationProtocol(ctx context.Context, r api.PreparedSerenaQualificationRequest) error {
	if e == nil || e.owner == nil || e.owner.protocol == nil {
		return errors.New("SERENA_QUALIFICATION_PROTOCOL_MISSING")
	}
	if err := e.owner.ValidateOriginalQualification(ctx, r); err != nil {
		return err
	}
	return e.owner.protocol.Validate(ctx, e.owner.protocolBinding)
}
func (e *unpublishedQualificationProtocol) ReceiptSHA256() string {
	if e == nil || e.owner == nil || e.owner.protocol == nil {
		return ""
	}
	return e.owner.protocol.ReceiptSHA256()
}
func (p *unpublishedQualificationHost) TerminationObservation() api.PreparedSerenaQualificationTerminationV1 {
	if p == nil || p.terminal == nil {
		return api.PreparedSerenaQualificationTerminationV1{}
	}
	t := p.terminal
	order, err := t.QualificationEventOrder(p.host)
	if err != nil {
		return api.PreparedSerenaQualificationTerminationV1{}
	}
	v := api.PreparedSerenaQualificationTerminationV1{Disposition: "pending", NativeExitDefined: t.RootWait.Known, NativeExitError: t.RootWait.Error, OwnedActionReceiptSHA256: order.OwnedActionReceiptSHA256, ProtocolReceivedSequence: order.ProtocolReceived, OwnedActionStartedSequence: order.OwnedActionStarted, OwnedActionCompletedSequence: order.OwnedActionCompleted, ResourceClosureSequence: order.ResourcesClosed}
	if t.RootWait.Known {
		exit := t.RootWait.ExitCode
		v.NativeExitCode = &exit
	}
	if t.Complete() && p.protocol != nil && p.protocol.ReceiptSHA256() != "" {
		if order.OwnedActionStarted != 0 && order.OwnedActionCompleted != 0 {
			v.Disposition = "protocol_qualified_owned_stop"
		} else if t.RootWait.Known && t.RootWait.ExitCode == 0 && t.RootWait.Error == nil {
			v.Disposition = "natural_success"
		}
	}
	return v
}
