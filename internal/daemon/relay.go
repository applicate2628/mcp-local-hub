package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPToStdioRelay forwards JSON-RPC messages between a stdio MCP client
// (reads stdin, writes stdout) and an HTTP MCP endpoint using Streamable
// HTTP transport.
//
// Motivation: Antigravity's Cascade agent (and similar stdio-only MCP
// clients) cannot connect to loopback HTTP daemons directly. Relay runs
// as a lightweight subprocess spawned by the client and translates
// between the two transports so the client transparently benefits from
// a shared HTTP daemon.
//
// Protocol reference: docs/superpowers/plans/2026-04-17-post-phase-1-antigravity-relay.md
// § "Protocol notes".
type HTTPToStdioRelay struct {
	URL    string    // e.g. "http://localhost:9121/mcp"
	Stdin  io.Reader // usually os.Stdin
	Stdout io.Writer // usually os.Stdout
	Stderr io.Writer // usually os.Stderr (logging only; never echoed to client)

	// HTTPClient is optional; a sensible default is used if nil.
	HTTPClient *http.Client

	// sessionID is minted by the server on the first POST response
	// (InitializeResult) and echoed on every subsequent request.
	// Stored as atomic.Value for lock-free reads from both goroutines.
	sessionID atomic.Value // string; "" until minted

	// stdoutMu serializes writes from both stdin-pump and sse-listener
	// goroutines so interleaved JSON lines never corrupt the wire.
	stdoutMu sync.Mutex
}

// jsonrpcMeta captures the three fields relay needs to route/synthesize
// without decoding the full MCP payload. Everything else flows through
// as raw bytes.
type jsonrpcMeta struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`     // may be number, string, or null
	Method  string          `json:"method,omitempty"` // empty for responses
}

const (
	mcpSessionIDHeader = "MCP-Session-Id"
	lastEventIDHeader  = "Last-Event-ID"

	// Default HTTP timeout matches what Claude Code uses against our
	// Serena daemon (60 s total). Individual SSE streams are read
	// without a body-read deadline — only the overall connection
	// inherits the HTTPClient's timeout when set to 0 here.
	defaultHTTPTimeoutSec = 60

	// Max line the relay accepts from stdin. MCP messages are typically
	// KB-sized; 1 MiB is a generous ceiling that catches runaway inputs
	// without rejecting any real MCP payload.
	maxStdinLineBytes = 1 << 20 // 1 MiB
)

// Run starts the relay. It returns when stdin reaches EOF (client detached)
// or ctx is canceled, whichever comes first, after issuing a best-effort
// DELETE /mcp to the server for graceful session termination.
//
// Errors from either goroutine propagate; the first error wins. Context
// cancellation is not an error — clean client detach returns nil.
//
// Lifecycle rules:
//   - stdin pump reads lines and dispatches POSTs in parallel goroutines
//     tracked by postWG.
//   - When stdin returns (EOF or read error), Run first waits for all
//     in-flight POSTs to drain so the last response reaches stdout before
//     the SSE listener is canceled and DELETE /mcp is sent.
//   - If external ctx cancels, SSE listener exits immediately and POST
//     goroutines either complete (if their HTTP response was already
//     in flight) or abort with ctx.Canceled. In-flight responses from
//     concurrent POSTs race with cancel but we prefer clean shutdown
//     over forcing completion.
func (r *HTTPToStdioRelay) Run(ctx context.Context) error {
	if r.URL == "" {
		return errors.New("relay: URL is required")
	}
	if r.Stdin == nil || r.Stdout == nil {
		return errors.New("relay: Stdin and Stdout are required")
	}
	if r.Stderr == nil {
		r.Stderr = io.Discard
	}
	if r.HTTPClient == nil {
		r.HTTPClient = &http.Client{
			Timeout: time.Duration(defaultHTTPTimeoutSec) * time.Second,
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var postWG sync.WaitGroup
	sseErrCh := make(chan error, 1)

	go func() {
		sseErrCh <- r.sseListener(runCtx)
	}()

	stdinErr := r.stdinPump(runCtx, &postWG)

	// Drain in-flight POSTs before canceling SSE — we don't want to
	// drop the last responses the client is waiting for.
	postWG.Wait()

	// Shut down SSE listener by canceling runCtx, then wait for it.
	cancel()
	<-sseErrCh

	// Best-effort session termination — don't block caller on failure.
	r.terminateSession()

	if stdinErr != nil && !errors.Is(stdinErr, io.EOF) && !errors.Is(stdinErr, context.Canceled) {
		return stdinErr
	}
	return nil
}

// stdinPump reads JSON-RPC lines from stdin and POSTs each to /mcp.
//
// Messages sent before the session is established (before the server
// responds to `initialize` with a `MCP-Session-Id` header) are
// processed SYNCHRONOUSLY — one at a time, in stdin order. This
// prevents the race where a goroutine carrying `notifications/initialized`
// wins the CAS slot and arrives at the server before `initialize`.
//
// After the session is established, messages are dispatched in parallel
// goroutines tracked by wg so Run can wait for them to drain.
func (r *HTTPToStdioRelay) stdinPump(ctx context.Context, wg *sync.WaitGroup) error {
	reader := bufio.NewReaderSize(r.Stdin, maxStdinLineBytes)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readJSONRPCLine(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return io.EOF
			}
			return fmt.Errorf("stdin read: %w", err)
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		// Synchronous path until session established — guarantees
		// message ordering (initialize first) and session-id capture
		// before any parallel dispatch.
		if r.getSessionID() == "" {
			r.handlePOST(ctx, line)
			continue
		}

		// Session known — safe to parallelize.
		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			r.handlePOST(ctx, body)
		}(line)
	}
}

// handlePOST sends one JSON-RPC message over HTTP POST and forwards the
// server's response(s) back to stdout. On HTTP failure with a request
// (has id), synthesizes a JSON-RPC error response so the client does
// not hang awaiting a reply.
//
func (r *HTTPToStdioRelay) handlePOST(ctx context.Context, body []byte) {
	meta, metaErr := decodeJSONRPCMeta(body)

	req, err := http.NewRequestWithContext(ctx, "POST", r.URL, bytes.NewReader(body))
	if err != nil {
		r.logErr("build POST request: %v", err)
		r.sendSyntheticError(meta, metaErr, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := r.getSessionID(); sid != "" {
		req.Header.Set(mcpSessionIDHeader, sid)
	}

	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down; do not synthesize a response
		}
		r.logErr("POST %s: %v", r.URL, err)
		r.sendSyntheticError(meta, metaErr, err)
		return
	}
	defer resp.Body.Close()

	// Capture session ID minted by the server on first initialize.
	if sid := resp.Header.Get(mcpSessionIDHeader); sid != "" {
		r.setSessionID(sid)
	}

	switch resp.StatusCode {
	case http.StatusAccepted: // 202 — no body for notifications/responses
		return
	case http.StatusOK:
		contentType := resp.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "text/event-stream") {
			if err := r.pipeSSE(ctx, resp.Body); err != nil {
				r.logErr("POST SSE pipe: %v", err)
			}
			return
		}
		// application/json (or anything else) — single message body.
		msg, err := io.ReadAll(resp.Body)
		if err != nil {
			r.logErr("read POST response body: %v", err)
			r.sendSyntheticError(meta, metaErr, err)
			return
		}
		r.writeLine(msg)
	case http.StatusNotFound:
		// Session dropped by server; relay could re-initialize automatically,
		// but for v1 we surface the error to the client. Antigravity will
		// spawn a fresh relay if it retries.
		r.logErr("session lost (server returned 404)")
		r.sendSyntheticError(meta, metaErr, fmt.Errorf("session lost"))
	default:
		msg, _ := io.ReadAll(resp.Body)
		r.logErr("POST non-2xx: status=%d body=%q", resp.StatusCode, msg)
		r.sendSyntheticError(meta, metaErr, fmt.Errorf("HTTP %d", resp.StatusCode))
	}
}

// sseListener opens a long-lived GET /mcp SSE stream and forwards each
// event's JSON payload to stdout. Reconnects once after a 1 s pause on
// stream drop.
func (r *HTTPToStdioRelay) sseListener(ctx context.Context) error {
	var lastEventID string
	attempts := 0

	for {
		if ctx.Err() != nil {
			return nil
		}
		attempts++

		// Wait for session before first SSE request; server requires
		// MCP-Session-Id on GET.
		if r.getSessionID() == "" {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		req, err := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
		if err != nil {
			return fmt.Errorf("build GET request: %w", err)
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set(mcpSessionIDHeader, r.getSessionID())
		if lastEventID != "" {
			req.Header.Set(lastEventIDHeader, lastEventID)
		}

		resp, err := r.HTTPClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			r.logErr("GET SSE: %v", err)
			if attempts >= 3 {
				return err
			}
			if !sleepOrDone(ctx, time.Second) {
				return nil
			}
			continue
		}

		if resp.StatusCode == http.StatusMethodNotAllowed {
			// Server doesn't support server-initiated SSE (spec §allows).
			resp.Body.Close()
			return nil
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			r.logErr("GET SSE non-2xx: status=%d", resp.StatusCode)
			if !sleepOrDone(ctx, time.Second) {
				return nil
			}
			continue
		}

		lastEventID = r.readSSEStream(ctx, resp.Body, lastEventID)
		resp.Body.Close()

		// Stream ended cleanly (server closed). Reconnect after brief pause.
		if !sleepOrDone(ctx, time.Second) {
			return nil
		}
		attempts = 0 // reset on clean close
	}
}

// readSSEStream reads an SSE response body until EOF or ctx cancel,
// forwarding each `data:` event's JSON payload to stdout. Returns the
// last observed event ID so the caller may resume.
func (r *HTTPToStdioRelay) readSSEStream(ctx context.Context, body io.Reader, startID string) string {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStdinLineBytes)

	var dataBuf strings.Builder
	lastID := startID

	flush := func() {
		if dataBuf.Len() == 0 {
			return
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		// SSE `data:` payloads may span multiple lines (joined with \n per spec).
		// For our use case MCP messages are single JSON objects and never contain
		// raw newlines, so the joined form equals the JSON bytes.
		r.writeLine([]byte(payload))
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return lastID
		}
		line := scanner.Text()
		switch {
		case line == "":
			flush() // event delimiter
		case strings.HasPrefix(line, "data:"):
			// Per SSE spec the value is everything after the first colon,
			// optionally with one leading space stripped.
			val := strings.TrimPrefix(line, "data:")
			val = strings.TrimPrefix(val, " ")
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
		case strings.HasPrefix(line, "id:"):
			lastID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "retry:"), strings.HasPrefix(line, ":"):
			// event type / retry hint / comment — ignore; relay is transport-level.
		}
	}
	flush()
	return lastID
}

// pipeSSE handles the rare case where POST /mcp returns an SSE stream
// (long-running request). The payload framing is identical to the GET
// case, but a separate code path keeps the POST handler focused on
// request-scoped responses.
func (r *HTTPToStdioRelay) pipeSSE(ctx context.Context, body io.Reader) error {
	r.readSSEStream(ctx, body, "")
	return nil
}

// terminateSession issues a best-effort DELETE /mcp with the session
// header. Failures are logged and ignored — relay is shutting down
// regardless.
func (r *HTTPToStdioRelay) terminateSession() {
	sid := r.getSessionID()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "DELETE", r.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set(mcpSessionIDHeader, sid)
	resp, err := r.HTTPClient.Do(req)
	if err != nil {
		r.logErr("DELETE /mcp: %v", err)
		return
	}
	resp.Body.Close()
}

// sendSyntheticError emits a JSON-RPC error response to stdout so a
// client awaiting a response does not hang indefinitely. Called when
// the underlying HTTP POST fails. If the original request had no id
// (notification), no synthetic response is emitted (none is expected).
func (r *HTTPToStdioRelay) sendSyntheticError(meta *jsonrpcMeta, metaErr error, cause error) {
	if metaErr != nil || meta == nil || len(meta.ID) == 0 || string(meta.ID) == "null" {
		return
	}
	// Error code -32603 = JSON-RPC "Internal error" per spec.
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(meta.ID),
		"error": map[string]any{
			"code":    -32603,
			"message": fmt.Sprintf("mcp-local-hub relay: %v", cause),
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		r.logErr("marshal synthetic error: %v", err)
		return
	}
	r.writeLine(raw)
}

// writeLine writes a single JSON-RPC message plus trailing newline under
// the stdout mutex. The message must be a single-line JSON payload —
// line-delimited framing per MCP stdio transport spec.
func (r *HTTPToStdioRelay) writeLine(msg []byte) {
	msg = bytes.TrimRight(msg, "\r\n")
	r.stdoutMu.Lock()
	defer r.stdoutMu.Unlock()
	_, _ = r.Stdout.Write(msg)
	_, _ = r.Stdout.Write([]byte("\n"))
}

func (r *HTTPToStdioRelay) logErr(format string, args ...any) {
	fmt.Fprintf(r.Stderr, "[relay] "+format+"\n", args...)
}

func (r *HTTPToStdioRelay) getSessionID() string {
	if v := r.sessionID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

func (r *HTTPToStdioRelay) setSessionID(sid string) {
	// Only store the first session id; rotation would happen via
	// explicit re-initialize which is out of v1 scope.
	if r.getSessionID() == "" {
		r.sessionID.Store(sid)
	}
}


// readJSONRPCLine reads up to the next newline from r and returns the
// raw bytes (without the terminator). Errors include io.EOF (clean
// detach) and any bufio.Reader error.
func readJSONRPCLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), err
}

// decodeJSONRPCMeta extracts the jsonrpc/id/method triple without
// copying the full message. Returns (nil, err) on malformed input,
// in which case callers should skip synthesizing an error reply
// (we have no id to address it to).
func decodeJSONRPCMeta(body []byte) (*jsonrpcMeta, error) {
	var m jsonrpcMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m.JSONRPC != "2.0" {
		return nil, fmt.Errorf("unexpected jsonrpc version %q", m.JSONRPC)
	}
	return &m, nil
}

// sleepOrDone waits for either the timeout or ctx cancellation. Returns
// true if the timeout fired (caller should continue), false if canceled.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
