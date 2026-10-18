package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mcp-local-hub/internal/process"
	"time"
)

type HTTPHostEndpointReleaseSnapshot struct {
	Attempted, NoEndpointAcquisition, Complete bool
	Error                                      error
}
type HTTPHostEndpointReleaseObservation struct {
	owner    *HTTPHost
	state    *httpHostPreparedState
	start    *HTTPHostStartReceipt
	evidence MCPOriginalHostEndpointEvidence
	proof    *process.OriginalJobMemberCloseObservation
	view     HTTPHostEndpointReleaseSnapshot
}

func (o *HTTPHostEndpointReleaseObservation) Snapshot() HTTPHostEndpointReleaseSnapshot {
	if o == nil {
		return HTTPHostEndpointReleaseSnapshot{}
	}
	return o.view
}
func (o *HTTPHostEndpointReleaseObservation) complete() bool {
	return o != nil && o.owner != nil && o.state != nil && o.state.endpointRelease == o && o.view.Complete && o.view.Error == nil
}
func (o *HTTPHostEndpointReleaseObservation) Validate(ctx context.Context, h *HTTPHost, start *HTTPHostStartReceipt) error {
	if ctx == nil || ctx.Err() != nil || o == nil || o.owner != h || start == nil || o.start == nil || start.OriginalStart != o.start.OriginalStart || !o.complete() {
		return errors.New("HTTPHOST_ENDPOINT_RELEASE_UNQUALIFIED")
	}
	return nil
}
func (p *httpHostPreparedState) finishEndpointAcquisition() {
	p.endpointDoneOnce.Do(func() { close(p.endpointDone) })
}
func originalHostStartSHA(start *HTTPHostStartReceipt) (string, error) {
	if start == nil || start.OriginalStart == nil {
		return "", errors.New("HTTPHOST_ENDPOINT_ORIGINAL_START_MISSING")
	}
	raw, err := json.Marshal(start.OriginalStart.Identity())
	if err != nil {
		return "", err
	}
	return qualificationSHA(raw), nil
}
func (h *HTTPHost) ReleaseOriginalEndpointObserved(ctx context.Context) (*HTTPHostEndpointReleaseObservation, error) {
	if h == nil || ctx == nil {
		return nil, errors.New("HTTPHOST_ENDPOINT_RELEASE_CONTEXT_MISSING")
	}
	h.mu.Lock()
	p := h.prepared
	h.mu.Unlock()
	if p == nil {
		return nil, errors.New("HTTPHOST_ENDPOINT_RELEASE_OWNER_MISSING")
	}
	select {
	case <-p.acquisitionDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case <-p.endpointDone:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p.endpointReleaseOnce.Do(func() {
		o := &HTTPHostEndpointReleaseObservation{owner: h, state: p, start: p.endpointStart, evidence: p.endpoint, view: HTTPHostEndpointReleaseSnapshot{Attempted: true}}
		if p.endpoint == nil {
			o.view.NoEndpointAcquisition = !p.endpointAttempted
			if p.endpointAttempted {
				o.view.Error = errors.New("HTTPHOST_PARTIAL_ENDPOINT_OWNER_UNAVAILABLE")
			}
		} else {
			var closeErr error
			o.proof, closeErr = p.endpoint.CloseObserved(ctx)
			startSHA, bindErr := originalHostStartSHA(p.endpointStart)
			o.view.Error = errors.Join(closeErr, bindErr, p.endpoint.ValidateOriginalClose(ctx, o.proof, startSHA))
		}
		o.view.Error = errors.Join(o.view.Error, ctx.Err())
		o.view.Complete = o.view.Error == nil
		p.endpointRelease = o
	})
	return p.endpointRelease, p.endpointRelease.view.Error
}
func (h *HTTPHost) waitForPreparedOriginalListener(ctx context.Context, p *httpHostPreparedState, start *HTTPHostStartReceipt) error {
	end := time.Now().Add(h.cfg.HealthTimeout)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(end) {
		end = deadline
	}
	work, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	defer p.finishEndpointAcquisition()
	for {
		if err := work.Err(); err != nil {
			return err
		}
		h.mu.Lock()
		p.endpointAttempted = true
		p.endpointStart = start
		h.mu.Unlock()
		evidence, err := p.ticket.ObserveOriginalListener(work, h, start)
		h.mu.Lock()
		p.endpoint = evidence
		h.mu.Unlock()
		if err == nil {
			if evidence == nil {
				return errors.New("HTTPHOST_ORIGINAL_LISTENER_EVIDENCE_MISSING")
			}
			start.endpoint = evidence
			return nil
		}
		var notListening OriginalListenerNotListeningEvidence
		if !errors.As(err, &notListening) || notListening.ValidateOriginalNotListening(work, h, start) != nil {
			return err
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-work.Done():
			timer.Stop()
			return errors.Join(err, work.Err())
		case <-h.childExited:
			timer.Stop()
			return errors.Join(err, errors.New("subprocess exited before original listener ready"))
		case <-timer.C:
		}
		if !time.Now().Before(end) {
			return fmt.Errorf("original listener not ready after %v: %w", h.cfg.HealthTimeout, err)
		}
	}
}
