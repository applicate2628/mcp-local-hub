package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// mcpProbeSessionCleanupDisposition states the exact outcome of one
// probe-owned MCP session DELETE. Callers own the policy for a protocol-level
// 405: the generic health probe may accept it, whereas a repository router
// that advertised DELETE must reject it.
type mcpProbeSessionCleanupDisposition string

const (
	mcpProbeSessionTerminated             mcpProbeSessionCleanupDisposition = "terminated"
	mcpProbeSessionAlreadyAbsent          mcpProbeSessionCleanupDisposition = "already-absent"
	mcpProbeSessionTerminationUnsupported mcpProbeSessionCleanupDisposition = "termination-unsupported"
)

type mcpProbeSessionCleanupResult struct {
	Disposition mcpProbeSessionCleanupDisposition
	StatusCode  int
}

// mcpProbeSessionCleanupResponseError preserves an unsafe HTTP response so a
// caller can distinguish it from a transport or cancellation failure.
type mcpProbeSessionCleanupResponseError struct{ StatusCode int }

func (e *mcpProbeSessionCleanupResponseError) Error() string {
	return fmt.Sprintf("session cleanup: HTTP %d", e.StatusCode)
}

// mcpProbeSessionCleanupBodyError preserves every response-body settlement
// failure without exposing the response body or probe session id.
type mcpProbeSessionCleanupBodyError struct {
	StatusCode int
	DrainErr   error
	CloseErr   error
}

func (e *mcpProbeSessionCleanupBodyError) Error() string {
	switch {
	case e.DrainErr != nil && e.CloseErr != nil:
		return fmt.Sprintf("session cleanup: settle HTTP %d response body: drain: %v; close: %v", e.StatusCode, e.DrainErr, e.CloseErr)
	case e.DrainErr != nil:
		return fmt.Sprintf("session cleanup: settle HTTP %d response body: drain: %v", e.StatusCode, e.DrainErr)
	default:
		return fmt.Sprintf("session cleanup: settle HTTP %d response body: close: %v", e.StatusCode, e.CloseErr)
	}
}

func (e *mcpProbeSessionCleanupBodyError) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.DrainErr != nil {
		causes = append(causes, e.DrainErr)
	}
	if e.CloseErr != nil {
		causes = append(causes, e.CloseErr)
	}
	return causes
}

// terminateMCPProbeSession performs the one bounded, loopback-only DELETE for
// a session created by a probe. It never logs or persists the session id.
// A caller supplies its own HTTP client; this function owns request context,
// response-body closure, status classification, and no retry (DELETE is an
// externally observable lifecycle operation and the caller has no idempotency
// proof beyond the server's response).
func terminateMCPProbeSession(client *http.Client, endpoint, sessionID string, timeout time.Duration) (mcpProbeSessionCleanupResult, error) {
	if client == nil {
		return mcpProbeSessionCleanupResult{}, fmt.Errorf("session cleanup client is nil")
	}
	if sessionID == "" {
		return mcpProbeSessionCleanupResult{}, fmt.Errorf("refusing to delete empty probe session id")
	}
	if timeout <= 0 {
		return mcpProbeSessionCleanupResult{}, fmt.Errorf("session cleanup timeout must be positive")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		return mcpProbeSessionCleanupResult{}, fmt.Errorf("refusing non-loopback MCP probe endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return mcpProbeSessionCleanupResult{}, err
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		return mcpProbeSessionCleanupResult{}, err
	}
	result := mcpProbeSessionCleanupResult{StatusCode: resp.StatusCode}
	var drainErr error
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		drainErr = err
	}
	closeErr := resp.Body.Close()
	var bodyErr error
	if drainErr != nil || closeErr != nil {
		bodyErr = &mcpProbeSessionCleanupBodyError{StatusCode: resp.StatusCode, DrainErr: drainErr, CloseErr: closeErr}
	}
	var responseErr error
	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		result.Disposition = mcpProbeSessionTerminated
	case resp.StatusCode == http.StatusNotFound:
		result.Disposition = mcpProbeSessionAlreadyAbsent
	case resp.StatusCode == http.StatusMethodNotAllowed:
		result.Disposition = mcpProbeSessionTerminationUnsupported
	default:
		responseErr = &mcpProbeSessionCleanupResponseError{StatusCode: resp.StatusCode}
	}
	if responseErr != nil && bodyErr != nil {
		return result, errors.Join(responseErr, bodyErr)
	}
	if responseErr != nil {
		return result, responseErr
	}
	return result, bodyErr
}
