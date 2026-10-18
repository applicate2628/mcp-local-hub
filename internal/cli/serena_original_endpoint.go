package cli

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemon"
	"mcp-local-hub/internal/process"
	"net/url"
	"strconv"
	"sync"
)

// This combiner owns only observational custody. The host owns its original
// process/Job; the TCP scanner and member observer retain their own receipts.
type serenaOriginalEndpoint struct {
	mu                    sync.Mutex
	closing               bool
	scans                 []*api.LoopbackTCPRowsObservation
	host                  *daemon.HTTPHost
	start                 *daemon.HTTPHostStartReceipt
	source                process.OriginalJobMemberSource
	members               *process.OriginalJobMemberObservation
	scan                  *api.LoopbackTCPRowsObservation
	port                  uint16
	startSHA, endpointSHA string
	primary               error
	closeOnce             sync.Once
	closeProof            *process.OriginalJobMemberCloseObservation
	closeErr              error
	notListening          bool
}
type serenaOriginalNotListening struct{ owner *serenaOriginalEndpoint }

func (e *serenaOriginalNotListening) Error() string { return "SERENA_ORIGINAL_NOT_YET_LISTENING" }
func (e *serenaOriginalNotListening) ValidateOriginalNotListening(ctx context.Context, h *daemon.HTTPHost, start *daemon.HTTPHostStartReceipt) error {
	if e == nil || e.owner == nil || !e.owner.notListening || e.owner.host != h || start == nil || e.owner.start.OriginalStart != start.OriginalStart {
		return errors.New("SERENA_NOT_LISTENING_SOURCE_INVALID")
	}
	return e.owner.ValidateOriginalClose(ctx, e.owner.closeProof, e.owner.startSHA)
}
func observeSerenaOriginalEndpoint(ctx context.Context, h *daemon.HTTPHost, start *daemon.HTTPHostStartReceipt) (daemon.MCPOriginalHostEndpointEvidence, error) {
	if h == nil || start == nil || !start.OriginalCreated() {
		return nil, errors.New("SERENA_ORIGINAL_HOST_NOT_CREATED")
	}
	if err := start.ValidateOriginalHost(ctx, h); err != nil {
		return nil, err
	}
	endpoint, err := start.ExpectedEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	address, err := url.Parse(endpoint)
	if err != nil || address.Scheme != "http" || address.Hostname() != "127.0.0.1" || address.User != nil {
		return nil, errors.New("SERENA_ORIGINAL_ENDPOINT_INTENT_INVALID")
	}
	port, err := strconv.Atoi(address.Port())
	if err != nil {
		return nil, err
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("SERENA_ORIGINAL_ENDPOINT_PORT_INVALID")
	}
	raw, err := json.Marshal(start.OriginalStart.Identity())
	if err != nil {
		return nil, err
	}
	e := &serenaOriginalEndpoint{host: h, start: start, port: uint16(port), startSHA: preparedQualificationSHA(raw), endpointSHA: preparedQualificationSHA([]byte(endpoint))}
	e.source, err = h.OriginalMemberSource(ctx)
	if err != nil {
		e.primary = err
		return e, err
	}
	e.members, err = h.ObserveOriginalMembers(ctx)
	if err != nil {
		e.primary = err
		return e, err
	}
	e.scan, err = api.ObserveOriginalLoopbackTCPRowsObserved(ctx)
	if err != nil {
		e.primary = err
		return e, err
	}
	rows, err := e.scan.Rows(ctx)
	if err != nil {
		e.primary = err
		return e, err
	}
	found := false
	for _, row := range rows {
		if row.State == "LISTENING" && row.LocalPort == e.port && (row.LocalHost == "0.0.0.0" || row.LocalHost == "::") {
			e.primary = errors.New("SERENA_ORIGINAL_ENDPOINT_WILDCARD_LISTENER")
			return e, e.primary
		}
		if row.State == "LISTENING" && row.LocalPort == e.port && row.LocalHost == "127.0.0.1" {
			if err = e.members.MatchOwnerPID(ctx, row.PID); err != nil {
				e.primary = err
				return e, err
			}
			found = true
		}
	}
	if !found {
		e.notListening = true
		_, err = e.CloseObserved(ctx)
		if err != nil {
			return e, err
		}
		return e, &serenaOriginalNotListening{e}
	}
	return e, e.members.ValidateSource(ctx, e.source)
}
func (e *serenaOriginalEndpoint) ValidateOriginalEndpoint(ctx context.Context, endpointSHA, startSHA string) error {
	if e == nil {
		return errors.New("SERENA_ORIGINAL_ENDPOINT_BINDING_INVALID")
	}
	e.mu.Lock()
	closing := e.closing
	e.mu.Unlock()
	if e.primary != nil || e.notListening || e.endpointSHA != endpointSHA || e.startSHA != startSHA || closing {
		return errors.New("SERENA_ORIGINAL_ENDPOINT_BINDING_INVALID")
	}
	if err := e.scan.Validate(ctx); err != nil {
		return err
	}
	return e.members.ValidateSource(ctx, e.source)
}
func (e *serenaOriginalEndpoint) ValidateOriginalConnection(ctx context.Context, c *daemon.MCPHTTPOriginalConnection) error {
	if e == nil {
		return errors.New("SERENA_ENDPOINT_OWNER_MISSING")
	}
	if err := e.ValidateOriginalEndpoint(ctx, e.endpointSHA, e.startSHA); err != nil {
		return err
	}
	if err := c.ValidateEndpointSource(ctx, e, e.endpointSHA, e.startSHA); err != nil {
		return err
	}
	tuple := c.Snapshot()
	if tuple.RemoteHost != "127.0.0.1" || tuple.LocalHost != "127.0.0.1" || tuple.RemotePort != int(e.port) || tuple.LocalPort < 1 || tuple.LocalPort > 65535 {
		return errors.New("SERENA_ORIGINAL_CONNECTION_TUPLE_INVALID")
	}
	scan, err := api.ObserveOriginalLoopbackTCPRowsObserved(ctx)
	e.mu.Lock()
	e.scans = append(e.scans, scan)
	e.mu.Unlock()
	if err != nil {
		return err
	}
	rows, err := scan.Rows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.State == "ESTABLISHED" && row.LocalHost == tuple.RemoteHost && int(row.LocalPort) == tuple.RemotePort && row.RemoteHost == tuple.LocalHost && int(row.RemotePort) == tuple.LocalPort {
			return e.members.MatchOwnerPID(ctx, row.PID)
		}
	}
	return errors.New("SERENA_ORIGINAL_SERVER_CONNECTION_NOT_OBSERVED")
}
func (e *serenaOriginalEndpoint) CloseObserved(ctx context.Context) (*process.OriginalJobMemberCloseObservation, error) {
	if e == nil {
		return nil, errors.New("SERENA_ENDPOINT_OWNER_MISSING")
	}
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closing = true
		e.mu.Unlock()
		var proof *process.OriginalJobMemberCloseObservation
		var err error
		if e.members == nil {
			err = errors.New("SERENA_ENDPOINT_MEMBER_ACQUISITION_UNDEFINED")
		} else {
			proof, err = e.members.CloseObserved()
		}
		e.mu.Lock()
		e.closeProof, e.closeErr = proof, err
		e.mu.Unlock()
	})
	e.mu.Lock()
	proof, err := e.closeProof, e.closeErr
	e.mu.Unlock()
	if ctx == nil {
		return proof, errors.Join(err, errors.New("SERENA_ENDPOINT_CLOSE_CONTEXT_MISSING"))
	}
	return proof, errors.Join(err, ctx.Err())
}
func (e *serenaOriginalEndpoint) ValidateOriginalClose(ctx context.Context, p *process.OriginalJobMemberCloseObservation, startSHA string) error {
	if e == nil || ctx == nil || ctx.Err() != nil {
		return errors.New("SERENA_ORIGINAL_ENDPOINT_CLOSE_UNQUALIFIED")
	}
	e.mu.Lock()
	proof, closeErr := e.closeProof, e.closeErr
	scans := append([]*api.LoopbackTCPRowsObservation(nil), e.scans...)
	e.mu.Unlock()
	if e.startSHA != startSHA || proof != p || closeErr != nil || e.primary != nil {
		return errors.New("SERENA_ORIGINAL_ENDPOINT_CLOSE_UNQUALIFIED")
	}
	if err := e.scan.Validate(ctx); err != nil {
		return err
	}
	for _, scan := range scans {
		if err := scan.Validate(ctx); err != nil {
			return err
		}
	}
	return p.Validate(e.source)
}
