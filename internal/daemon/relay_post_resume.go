package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type relaySSECursor struct {
	ID         string
	ReplaySafe bool
}

// Only the parser's input boundary issues this type. A receiver error that
// happens to wrap EOF is still an output failure and must never resume.
type relaySSEReadError struct{ cause error }

func (e *relaySSEReadError) Error() string { return e.cause.Error() }
func (e *relaySSEReadError) Unwrap() error { return e.cause }

func relayLegacyResumption(version string) bool {
	// The 2026-07-28 transport removed session and Last-Event-ID resumption.
	// Unknown versions do not acquire legacy behavior by date comparison.
	switch version {
	case "2025-03-26", "2025-06-18", "2025-11-25":
		return true
	default:
		return false
	}
}

func relayResponseMatches(requestID json.RawMessage, payload []byte) bool {
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  json.RawMessage `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	return json.Unmarshal(payload, &reply) == nil && reply.JSONRPC == "2.0" && len(reply.Method) == 0 &&
		((len(reply.Result) > 0) != (len(reply.Error) > 0)) && relayJSONRPCIDsEqual(requestID, reply.ID)
}

// pipePOSTSSE owns the original response and each GET continuation. It never
// repeats POST, reinitializes a session, or borrows the independent GET cursor.
func (r *HTTPToStdioRelay) pipePOSTSSE(ctx context.Context, client *http.Client, original *http.Request, response *http.Response, meta *jsonrpcMeta, metaErr error, forward func([]byte) (bool, error)) error {
	request := metaErr == nil && meta.Method != "" && relayJSONRPCIDsEqual(meta.ID, meta.ID)
	version := original.Header.Get(mcpProtocolVersionHeader)
	session := original.Header.Get(mcpSessionIDHeader)
	var cursor relaySSECursor
	retryDelay := time.Second
	completed := false
	receive := func(payload []byte) (bool, error) {
		stop, err := forward(payload)
		completed = stop && err == nil
		return completed, err
	}
	for {
		var readErr error
		cursor, readErr = readRelaySSECursor(ctx, response.Body, cursor, int(maxRelayResponseBytes), receive, &retryDelay)
		closeErr := response.Body.Close()
		if closeErr != nil {
			return errors.Join(readErr, fmt.Errorf("close request stream: %w", closeErr))
		}
		if completed {
			return readErr
		}
		if ctx.Err() != nil {
			return errors.Join(ctx.Err(), readErr)
		}
		var interrupted *relaySSEReadError
		if readErr != nil && !errors.As(readErr, &interrupted) {
			return readErr
		}
		if !request {
			return readErr
		}
		if !relayLegacyResumption(version) || cursor.ID == "" || !cursor.ReplaySafe {
			return errors.Join(readErr, errors.New("POST response incomplete; no admitted replay-safe continuation"))
		}
		if !sleepOrDone(ctx, retryDelay) {
			return errors.Join(ctx.Err(), readErr)
		}
		resume, err := http.NewRequestWithContext(ctx, http.MethodGet, original.URL.String(), nil)
		if err != nil {
			return errors.Join(readErr, fmt.Errorf("build POST stream continuation: %w", err))
		}
		resume.Header.Set("Accept", "text/event-stream")
		resume.Header.Set(mcpProtocolVersionHeader, version)
		resume.Header.Set(lastEventIDHeader, cursor.ID)
		if session != "" {
			resume.Header.Set(mcpSessionIDHeader, session)
		}
		response, err = client.Do(resume)
		if err != nil {
			return errors.Join(readErr, fmt.Errorf("GET POST stream continuation: %w", err))
		}
		if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
			return errors.Join(readErr, fmt.Errorf("POST stream continuation refused: HTTP %d or non-SSE media type", response.StatusCode), response.Body.Close())
		}
	}
}
