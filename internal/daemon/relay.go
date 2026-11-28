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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

	// InterruptStdin is an explicit capability granted by the input owner.
	// Nil keeps the existing borrowed-input contract. When set, cancellation
	// may call it once while the input pump is active, concurrently with Read.
	// It must make current/future reads terminate and must itself finish; Run
	// joins both operations. Return an error if interruption/cleanup failed.
	// Run never infers permission from io.Closer and never closes Stdout.
	// Expected interrupted reads should return EOF or context.Canceled;
	// unrelated read errors and this callback's error remain observable.
	InterruptStdin func() error

	// HTTPClient is optional and remains caller-owned. If nil, POST uses
	// the default request timeout; streaming GET follows the Run context.
	// Run never stores its defaults here, including across repeated Runs.
	HTTPClient *http.Client

	// A matching InitializeResult publishes its session and negotiated version
	// together, after stdout accepts the complete response. A GET must not race
	// ahead with the session header alone while initialize is still in flight.
	sessionMu       sync.Mutex
	sessionID       string // "" until minted
	protocolVersion string // "" until a matching InitializeResult is forwarded
	initialized     bool   // negotiation completed, even when Session-Id is absent
	qualificationMu sync.Mutex
	qualification   *mcpHTTPQualificationOwner

	// stdoutMu owns complete frame writes and the first framing/output failure.
	// Once a frame fails, every producer must stop using this stream.
	stdoutMu     sync.Mutex
	stdoutErr    error
	stdoutCancel context.CancelFunc // this Run's network/dispatch context only

	// POST failures have their own exclusion so a borrowed, blocked Stdout
	// cannot prevent response failure from canceling context-aware work.
	// Run joins the bounded POST workers before reading or resetting postErr.
	postMu     sync.Mutex
	postErr    error
	postCancel context.CancelFunc
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
	mcpSessionIDHeader       = "MCP-Session-Id"
	lastEventIDHeader        = "Last-Event-ID"
	mcpProtocolVersionHeader = "MCP-Protocol-Version"

	// Default POST timeout is 60 s total, including its response body.
	// The default server-initiated SSE client has no Client.Timeout;
	// its request follows the Run context. An explicit HTTPClient keeps
	// its own timeout policy for both methods.
	defaultHTTPTimeoutSec = 60

	// Max line the relay accepts from stdin. MCP messages are typically
	// KB-sized; 1 MiB is a generous ceiling that catches runaway inputs
	// without rejecting any real MCP payload.
	maxStdinLineBytes = 1 << 20 // 1 MiB

	// Upper bound on concurrent request POST workers after initialization.
	// Responses and notifications use the input owner's single synchronous
	// POST, so a nested reply/cancellation never needs one of these slots.
	maxConcurrentPOSTs = 64

	// Match the existing HTTP host's finite body envelope. SSE streams may
	// continue across events; each pending event has this aggregate bound.
	maxRelayResponseBytes = maxHTTPHostBodyBytes
)

var errStdinLineTooLong = errors.New("stdin line exceeds maxStdinLineBytes")
var errRelaySessionLost = errors.New("HTTP session expired; restart the transport and initialize a new session; earlier calls may have executed and were not replayed")

// Run starts one relay lifetime and joins its POST/SSE workers before returning.
// Normal stdin EOF drains accepted responses before best-effort DELETE /mcp.
// Run must not overlap another Run on the same instance. Its exported
// configuration must remain unchanged until the lifetime has settled.
//
// Stdin and Stdout are borrowed by default; Run never implicitly closes them.
// Cancellation stops context-aware HTTP work, but cannot interrupt an arbitrary blocking
// Reader/Writer. For input, its owner may grant InterruptStdin; Run joins that
// callback as well as the read. Without it, the owner must terminate input.
// Output still requires its owner's termination mechanism if Write can block.
// No detached reader goroutine or implicit ownership transfer is introduced.
//
// The first output failure is retained even when it causes context cancellation.
// Independently observed response/parser and stdin failures are returned
// alongside it. Failure to deliver an answer does not prove the remote
// operation did not execute. POST redirects and parser-failure reconnects
// are refused. Clean EOF/cancellation alone retains the nil result.
//
// Lifecycle rules:
//   - stdin pump reads lines, sends control messages synchronously, and
//     dispatches request POSTs in parallel goroutines tracked by postWG.
//   - When stdin returns (EOF or read error), Run first waits for all
//     in-flight POSTs to drain so the last response reaches stdout before
//     the SSE listener is canceled and DELETE /mcp is sent.
//   - Cancellation stops context-aware HTTP work; responses already in flight
//     may race with it. Run still joins its workers. Borrowed blocking I/O has
//     the separate termination requirement described above.
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
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.stdoutMu.Lock()
	r.stdoutErr = nil
	r.stdoutCancel = cancel
	r.stdoutMu.Unlock()
	r.postMu.Lock()
	r.postErr = nil
	r.postCancel = cancel
	r.postMu.Unlock()
	defer func() {
		r.stdoutMu.Lock()
		r.stdoutCancel = nil
		r.stdoutMu.Unlock()
		r.postMu.Lock()
		r.postCancel = nil
		r.postMu.Unlock()
	}()

	var postWG sync.WaitGroup
	sseErrCh := make(chan error, 1)

	go func() {
		err := r.sseListener(runCtx)
		if runCtx.Err() != nil && relayCleanDetach(err, false) {
			err = nil
		}
		if err != nil {
			cancel()
		}
		sseErrCh <- err
	}()

	stdinErr, interruptErr := r.pumpInput(runCtx, &postWG)

	// Drain in-flight POSTs before canceling SSE — we don't want to
	// drop the last responses the client is waiting for.
	postWG.Wait()

	// Shut down SSE listener by canceling runCtx, then wait for it.
	cancel()
	sseErr := <-sseErrCh

	// Best-effort session termination — don't block caller on failure.
	r.terminateSession()

	r.postMu.Lock()
	postErr := r.postErr
	r.postMu.Unlock()
	outputErr := errors.Join(r.stdoutFailure(), postErr, sseErr)
	if interruptErr != nil {
		outputErr = errors.Join(outputErr, interruptErr)
	}
	if !relayCleanDetach(stdinErr, true) {
		if outputErr != nil {
			return errors.Join(stdinErr, outputErr)
		}
		return stdinErr
	}
	return outputErr
}

// relayCleanDetach accepts only error trees composed entirely of expected
// cancellation causes, plus EOF for input. errors.Is would match just one
// branch and incorrectly discard independent failures in errors.Join.
// Response callers also require their own context to be canceled.
func relayCleanDetach(err error, allowEOF bool) bool {
	if err == nil || err == context.Canceled || (allowEOF && err == io.EOF) {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		causes := e.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if cause == nil || !relayCleanDetach(cause, allowEOF) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		cause := e.Unwrap()
		return cause != nil && relayCleanDetach(cause, allowEOF)
	default:
		return false
	}
}

// pumpInput attaches only the interruption capability explicitly granted by
// the input owner. Reading stays on the Run goroutine; no reader is abandoned.
// Disarm before draining accepted POSTs so normal EOF does not later interrupt
// settled input when Run cancels its HTTP context. If interruption already
// started, join it and retain its outcome independently of the read outcome.
func (r *HTTPToStdioRelay) pumpInput(ctx context.Context, wg *sync.WaitGroup) (error, error) {
	if err := ctx.Err(); err != nil {
		return err, nil
	}
	interrupt := r.InterruptStdin
	if interrupt == nil {
		return r.stdinPump(ctx, wg), nil
	}
	done := make(chan struct{})
	var interruptErr error
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		interruptErr = interrupt()
	})
	stdinErr := r.stdinPump(ctx, wg)
	if !stop() {
		<-done
		if interruptErr != nil {
			interruptErr = fmt.Errorf("relay stdin interruption: %w", interruptErr)
		}
	}
	// Interruption may end the read with EOF, Canceled or an independent
	// native error. None of those replaces an expired operation deadline.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		stdinErr = errors.Join(ctx.Err(), stdinErr)
	}
	return stdinErr, interruptErr
}

// stdinPump reads JSON-RPC lines from stdin and POSTs each to /mcp.
//
// Messages sent before initialization completes (before a matching
// InitializeResult reaches stdout, with or without a Session-Id) are
// processed SYNCHRONOUSLY — one at a time, in stdin order. This
// preserves initialize ordering and publication of negotiated headers.
//
// Afterwards requests use bounded workers tracked by wg. Responses and
// notifications only need an HTTP acknowledgement; the input owner sends
// them directly, independently of those workers. A full request limit rejects
// a new request before delivery instead of blocking input behind a request
// that may itself be waiting for a response or cancellation from that input.
func (r *HTTPToStdioRelay) stdinPump(ctx context.Context, wg *sync.WaitGroup) error {
	reader := bufio.NewReaderSize(r.Stdin, maxStdinLineBytes)
	postSem := make(chan struct{}, maxConcurrentPOSTs)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := readJSONRPCLine(reader)
		if err != nil {
			return fmt.Errorf("stdin read: %w", err)
		}
		// A blocked read may complete with data concurrently with interruption.
		// Such data is not new admission after cancellation, even if a custom
		// HTTP transport would accept an already-canceled request.
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		meta, metaErr := decodeJSONRPCMeta(line)
		if !r.isInitialized() || (metaErr == nil && (meta.Method == "" || len(meta.ID) == 0)) {
			r.handlePOST(ctx, line)
			continue
		}

		// Admission must not stop input: later messages can release the
		// outstanding requests. No queued goroutine or remote effect exists
		// for the rejected request.
		select {
		case postSem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		default:
			capacityErr := errors.New("request capacity exhausted; message was not forwarded")
			if metaErr != nil {
				r.recordPOSTFailure(ctx, capacityErr)
				return capacityErr // no valid request ID to receive a refusal
			}
			r.sendSyntheticError(meta, metaErr, capacityErr)
			continue
		}

		wg.Add(1)
		go func(body []byte) {
			defer wg.Done()
			defer func() { <-postSem }()
			r.handlePOST(ctx, body)
		}(line)
	}
}

// recordPOSTFailure preserves all failures from the bounded admitted workers.
// Cancellation never authorizes replay of a request with an unknown effect.
func (r *HTTPToStdioRelay) recordPOSTFailure(ctx context.Context, err error) {
	if err == nil || (ctx.Err() != nil && relayCleanDetach(err, false)) {
		return
	}
	r.postMu.Lock()
	r.postErr = errors.Join(r.postErr, err)
	cancel := r.postCancel
	r.postMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// handlePOST sends one JSON-RPC message over HTTP POST and forwards the
// server's response(s) back to stdout. On HTTP failure with a request
// (has method and id), synthesizes a JSON-RPC error response so the client does
// not hang awaiting a reply.
func (r *HTTPToStdioRelay) handlePOST(ctx context.Context, body []byte) {
	meta, metaErr := decodeJSONRPCMeta(body)
	// One deadline owns this operation, including any GET continuation. Copy
	// the caller's client so redirect and timeout handling never mutate it.
	client := http.Client{Timeout: time.Duration(defaultHTTPTimeoutSec) * time.Second}
	if r.HTTPClient != nil {
		client = *r.HTTPClient
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	requestCtx := ctx
	if client.Timeout > 0 {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(ctx, client.Timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(requestCtx, "POST", r.URL, bytes.NewReader(body))
	if err != nil {
		r.recordPOSTFailure(ctx, fmt.Errorf("build POST request: %w", err))
		r.logErr("build POST request: %v", err)
		r.sendSyntheticError(meta, metaErr, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sid := r.getSessionID(); sid != "" {
		req.Header.Set(mcpSessionIDHeader, sid)
	}
	if version := r.getProtocolVersion(); version != "" {
		req.Header.Set(mcpProtocolVersionHeader, version)
	}

	// A redirect must never resend a possibly effectful POST.
	resp, err := client.Do(req)
	if err != nil {
		shuttingDown := ctx.Err() != nil
		r.recordPOSTFailure(ctx, fmt.Errorf("POST request: %w", err))
		if shuttingDown {
			return // shutting down; do not synthesize a response
		}
		r.logErr("POST %s: %v", r.URL, err)
		r.sendSyntheticError(meta, metaErr, err)
		return
	}
	initializing := metaErr == nil && meta.Method == "initialize"
	// Only an InitializeResult may establish a session. A response header on
	// another method must not start GET before negotiation has completed.
	sid := resp.Header.Get(mcpSessionIDHeader)
	pendingSession := initializing && sid != ""
	cleanupVersion := ""
	closeResponse := true
	defer func() {
		if closeResponse {
			if err := resp.Body.Close(); err != nil {
				r.recordPOSTFailure(ctx, fmt.Errorf("close POST response: %w", err))
			}
		}
		if pendingSession {
			// The server may already have created a session even though stdout
			// failed or no matching result arrived. This worker retains its
			// cleanup identity without exposing an unaccepted session to GET.
			r.deleteSession(sid, cleanupVersion)
		}
	}()
	forward := func(payload []byte) (bool, error) {
		version := ""
		if initializing {
			version = relayInitializeVersion(meta.ID, payload)
			if version != "" {
				cleanupVersion = version
			}
		}
		if err := r.writeLine(payload); err != nil {
			return false, err
		}
		if version != "" {
			r.sessionMu.Lock()
			if r.protocolVersion == "" {
				r.protocolVersion = version
				r.initialized = true
				if r.sessionID == "" {
					r.sessionID = sid
				}
			}
			pendingSession = sid != "" && r.sessionID != sid
			r.sessionMu.Unlock()
			initializing = false
		}
		return metaErr == nil && relayResponseMatches(meta.ID, payload), nil
	}

	switch resp.StatusCode {
	case http.StatusAccepted: // 202 — no body for notifications/responses
		return
	case http.StatusOK:
		contentType := resp.Header.Get("Content-Type")
		if strings.HasPrefix(contentType, "text/event-stream") {
			// The request-scoped stream owns every response body from here.
			closeResponse = false
			if err := r.pipePOSTSSE(requestCtx, &client, req, resp, meta, metaErr, forward); err != nil {
				r.recordPOSTFailure(ctx, fmt.Errorf("POST SSE pipe: %w", err))
				r.logErr("POST SSE pipe: %v", err)
			}
			return
		}
		// application/json (or anything else) — single message body.
		msg, err := readAllWithLimit(resp.Body, maxRelayResponseBytes)
		if err != nil {
			r.recordPOSTFailure(ctx, fmt.Errorf("read POST response body: %w", err))
			r.logErr("read POST response body: %v", err)
			r.sendSyntheticError(meta, metaErr, err)
			return
		}
		_, _ = forward(msg) // writeLine retains any stdout failure for Run
	case http.StatusNotFound:
		// An RPC error alone leaves an existing stdio client on the expired
		// transport. End this lifetime so its owner observes the loss, then
		// requires fresh initialization. Never resend a delivered request.
		failure := errRelaySessionLost
		if req.Header.Get(mcpSessionIDHeader) == "" {
			failure = errors.New("POST endpoint returned HTTP 404")
		}
		r.recordPOSTFailure(ctx, failure)
		r.logErr("POST: %v", failure)
		r.sendSyntheticError(meta, metaErr, failure)
	default:
		msg, readErr := readAllWithLimit(resp.Body, maxRelayResponseBytes)
		statusErr := fmt.Errorf("HTTP %d", resp.StatusCode)
		if metaErr == nil && (meta.Method == "" || len(meta.ID) == 0) {
			// A rejected response/cancellation has no JSON-RPC response of
			// its own. Keep its failure observable and stop dependent work.
			r.recordPOSTFailure(ctx, statusErr)
		}
		if readErr != nil {
			r.recordPOSTFailure(ctx, fmt.Errorf("read POST HTTP %d body: %w", resp.StatusCode, readErr))
			r.logErr("POST non-2xx: status=%d body read: %v", resp.StatusCode, readErr)
		} else {
			r.logErr("POST non-2xx: status=%d body=%q", resp.StatusCode, msg)
		}
		r.sendSyntheticError(meta, metaErr, errors.Join(statusErr, readErr))
	}
}

// sseListener opens a long-lived GET /mcp SSE stream and forwards each
// event's JSON payload to stdout. A valid server retry field updates the delay
// for this GET stream, including subsequent clean EOF and transport retries.
// The initial delay remains 1 s. POST streams are never replayed here.
// Parser/output failures terminate the stream; they are not a resume point.
func (r *HTTPToStdioRelay) sseListener(ctx context.Context) error {
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{} // the owned stream lives until Run cancellation
	}
	var lastEventID string
	retryDelay := time.Second
	attempts := 0

	for {
		if ctx.Err() != nil {
			return nil
		}
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

		attempts++
		req, err := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
		if err != nil {
			return fmt.Errorf("build GET request: %w", err)
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set(mcpSessionIDHeader, r.getSessionID())
		if version := r.getProtocolVersion(); version != "" {
			req.Header.Set(mcpProtocolVersionHeader, version)
		}
		if lastEventID != "" {
			req.Header.Set(lastEventIDHeader, lastEventID)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				if relayCleanDetach(err, false) {
					return nil
				}
				return err
			}
			r.logErr("GET SSE: %v", err)
			if attempts >= 3 {
				return err
			}
			if !sleepOrDone(ctx, retryDelay) {
				return nil
			}
			continue
		}

		if resp.StatusCode == http.StatusMethodNotAllowed {
			// Server doesn't support server-initiated SSE (spec §allows).
			resp.Body.Close()
			return nil
		}
		if resp.StatusCode == http.StatusNotFound {
			return errors.Join(errRelaySessionLost, resp.Body.Close())
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			r.logErr("GET SSE non-2xx: status=%d", resp.StatusCode)
			if !sleepOrDone(ctx, retryDelay) {
				return nil
			}
			continue
		}

		lastEventID, err = r.readSSEStream(ctx, resp.Body, lastEventID, &retryDelay)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("GET SSE stream: %w", err)
		}
		if err := r.stdoutFailure(); err != nil {
			return err // a damaged output stream must not reconnect and write again
		}

		// Stream ended cleanly (server closed). Reconnect after brief pause.
		if !sleepOrDone(ctx, retryDelay) {
			return nil
		}
		attempts = 0 // reset on clean close
	}
}

// readSSEStream reads an SSE response body until EOF or ctx cancel,
// forwarding nonempty default/message event JSON payloads to stdout. Returns the
// last observed event ID and any parser/output failure. Only clean EOF may
// resume; an ID observed in a failed event is never used for reconnect.
func (r *HTTPToStdioRelay) readSSEStream(ctx context.Context, body io.Reader, startID string, retryDelay *time.Duration) (string, error) {
	return readRelaySSE(ctx, body, startID, int(maxRelayResponseBytes), func(payload []byte) (bool, error) {
		return false, r.writeLine(payload)
	}, retryDelay)
}

// The legacy relay and finite observed exchange share the same SSE framing.
// A matched response can stop an observed POST without claiming body EOF.
// retryDelay is owned by the GET listener, or nil for callers that cannot
// reconnect. Unlike an event cursor, retry takes effect when its field is read;
// it does not depend on a blank line or successful message delivery.
func readRelaySSE(ctx context.Context, body io.Reader, startID string, maxEventBytes int, receive func([]byte) (bool, error), retryDelay *time.Duration) (string, error) {
	state, err := readRelaySSECursor(ctx, body, relaySSECursor{ID: startID}, maxEventBytes, receive, retryDelay)
	return state.ID, err
}

// The shared parser retains its original cursor-only interface above. POST
// continuation also needs to know whether every delivered message is covered
// by the current cursor; reconstructing that fact from payloads is unsafe.
func readRelaySSECursor(ctx context.Context, body io.Reader, state relaySSECursor, maxEventBytes int, receive func([]byte) (bool, error), retryDelay *time.Duration) (relaySSECursor, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStdinLineBytes)

	var dataBuf strings.Builder
	hasData := false
	eventName := ""
	pendingID := state.ID
	explicitID := false
	delivered := false

	flush := func() (bool, error) {
		payload := dataBuf.String()
		messageEvent := eventName == "" || eventName == "message"
		dataBuf.Reset()
		hasData = false
		eventName = ""
		// MCP priming/control events carry cursors without JSON messages.
		// Reset the type even for an id-only block; it never carries over.
		if len(payload) == 0 || !messageEvent {
			return false, nil
		}
		delivered = true
		// SSE `data:` payloads may span multiple lines (joined with \n per spec).
		// The receiver owns JSON validation and stdio framing.
		return receive([]byte(payload))
	}

	for scanner.Scan() {
		if ctx.Err() != nil {
			return state, errors.Join(ctx.Err(), scanner.Err())
		}
		line := scanner.Text()
		if line == "" {
			stop, err := flush()
			if err != nil {
				return state, errors.Join(err, scanner.Err())
			}
			if explicitID {
				state.ReplaySafe = pendingID != "" && (pendingID != state.ID || (state.ReplaySafe && !delivered))
			} else if delivered {
				state.ReplaySafe = false
			}
			state.ID = pendingID
			explicitID, delivered = false, false
			if stop {
				return state, scanner.Err()
			}
			continue
		}
		// A field without a colon has an empty value. Remove at most one
		// ASCII space after the colon; other whitespace remains meaningful.
		field, val, _ := strings.Cut(line, ":")
		val = strings.TrimPrefix(val, " ")
		switch field {
		case "data":
			additional := len(val)
			if hasData {
				additional++ // separator counts toward the event envelope
			}
			if maxEventBytes > 0 && additional > maxEventBytes-dataBuf.Len() {
				return state, errors.Join(errStdinLineTooLong, scanner.Err())
			}
			if hasData {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(val)
			hasData = true
		case "event":
			eventName = val
		case "id":
			if !strings.ContainsRune(val, '\x00') {
				pendingID = val
				explicitID = true
			}
		case "retry":
			if retryDelay != nil && val != "" && strings.IndexFunc(val, func(r rune) bool { return r < '0' || r > '9' }) == -1 {
				const maxMillis = uint64((1<<63 - 1) / time.Millisecond)
				millis, err := strconv.ParseUint(val, 10, 64)
				if err != nil || millis > maxMillis {
					// Do not wrap or ignore a valid but unrepresentable delay:
					// either could reconnect before the server permits it.
					return state, errors.Join(errors.New("SSE retry exceeds supported duration"), scanner.Err())
				}
				*retryDelay = time.Duration(millis) * time.Millisecond
			}
		default:
			// Comments and unknown fields do not contribute to the payload.
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) || errors.Is(err, bufio.ErrNegativeAdvance) || errors.Is(err, bufio.ErrAdvanceTooFar) || errors.Is(err, bufio.ErrBadReadCount) {
			return state, err
		}
		return state, &relaySSEReadError{err}
	}
	if hasData {
		return state, &relaySSEReadError{io.ErrUnexpectedEOF} // no blank line committed this event
	}
	return state, nil
}

// pipeSSE forwards request-scoped SSE without retaining reconnect state.
// Initialize responses use a response-aware receiver in handlePOST; both
// receivers keep the same bounded framing and stdout failure contract.
func (r *HTTPToStdioRelay) pipeSSE(ctx context.Context, body io.Reader) error {
	_, err := r.readSSEStream(ctx, body, "", nil)
	return err
}

// terminateSession issues a best-effort DELETE /mcp with the session
// header. Failures are logged and ignored — relay is shutting down
// regardless.
func (r *HTTPToStdioRelay) terminateSession() {
	version := r.getProtocolVersion()
	sid := r.takeSessionID()
	r.deleteSession(sid, version)
}

// deleteSession is also used by a POST worker whose initialization response
// could not be published. Both cleanup paths have the same independent bound.
func (r *HTTPToStdioRelay) deleteSession(sid, version string) {
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
	if version != "" {
		req.Header.Set(mcpProtocolVersionHeader, version)
	}
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{} // the cleanup context already supplies its 2 s bound
	}
	resp, err := client.Do(req)
	if err != nil {
		r.logErr("DELETE /mcp: %v", err)
		return
	}
	resp.Body.Close()
}

// sendSyntheticError emits a JSON-RPC error response to stdout so a
// client awaiting a response does not hang indefinitely. Called when
// the underlying HTTP POST fails. Notifications and client responses never
// receive a synthetic response of their own.
func (r *HTTPToStdioRelay) sendSyntheticError(meta *jsonrpcMeta, metaErr error, cause error) {
	if metaErr != nil || meta == nil || meta.Method == "" || len(meta.ID) == 0 || string(meta.ID) == "null" {
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

// writeLine compacts one bounded UTF-8 JSON message before any output write.
// JSON whitespace may span HTTP/SSE lines; escaped newlines inside strings
// remain data. Syntax/framing failure poisons this stream just like a failed
// write, so another producer cannot append a success-looking response.
func (r *HTTPToStdioRelay) writeLine(msg []byte) error {
	r.stdoutMu.Lock()
	defer r.stdoutMu.Unlock()
	if r.stdoutErr != nil {
		return r.stdoutErr
	}
	var frame bytes.Buffer
	var err error
	switch {
	case int64(len(msg)) > maxRelayResponseBytes:
		err = errBodyTooLarge
	case !utf8.Valid(msg):
		err = errors.New("JSON message is not valid UTF-8")
	default:
		err = json.Compact(&frame, msg)
	}
	if err == nil {
		for _, part := range [][]byte{frame.Bytes(), []byte("\n")} {
			var n int
			n, err = r.Stdout.Write(part)
			if err == nil && n != len(part) {
				err = io.ErrShortWrite
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		r.stdoutErr = fmt.Errorf("relay stdout: %w", err)
		if r.stdoutCancel != nil {
			r.stdoutCancel()
		}
		return r.stdoutErr
	}
	return nil
}

func (r *HTTPToStdioRelay) stdoutFailure() error {
	r.stdoutMu.Lock()
	defer r.stdoutMu.Unlock()
	return r.stdoutErr
}

func (r *HTTPToStdioRelay) logErr(format string, args ...any) {
	fmt.Fprintf(r.Stderr, "[relay] "+format+"\n", args...)
}

func (r *HTTPToStdioRelay) getSessionID() string {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return r.sessionID
}

func (r *HTTPToStdioRelay) getProtocolVersion() string {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return r.protocolVersion
}

func (r *HTTPToStdioRelay) isInitialized() bool {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	return r.initialized
}

func (r *HTTPToStdioRelay) setSessionID(sid string) {
	// Only store the first session id; rotation would happen via
	// explicit re-initialize which is out of v1 scope.
	// The finite qualification owner calls this only after validating its
	// InitializeResult; it does not run the stdio negotiation path.
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.sessionID == "" {
		r.sessionID = sid
		r.initialized = sid != ""
	}
}

func (r *HTTPToStdioRelay) takeSessionID() string {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	sid := r.sessionID
	r.sessionID = ""
	r.protocolVersion = ""
	r.initialized = false
	return sid
}

// The relay observes negotiation; endpoint peers still choose compatibility.
// Never infer a version from a request, error, notification, or unrelated ID.
// Date syntax keeps HTTP metadata valid without imposing a version allowlist.
func relayInitializeVersion(requestID json.RawMessage, payload []byte) string {
	var reply struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  *struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(payload, &reply) != nil || reply.JSONRPC != "2.0" || reply.Method != "" ||
		reply.Result == nil || len(reply.Error) != 0 || !relayJSONRPCIDsEqual(requestID, reply.ID) {
		return ""
	}
	version := reply.Result.ProtocolVersion
	if _, err := time.Parse(time.DateOnly, version); err != nil {
		return ""
	}
	return version
}

// Compare parsed JSON IDs by value without rounding numbers through float64.
// MCP numeric IDs are integers. Normalize their coefficients and decimal
// exponents separately; neither a power of ten nor a giant integer exponent
// is materialized. Each digit is visited only a bounded number of times.
func relayJSONRPCIDsEqual(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if a[0] == '"' || b[0] == '"' {
		var left, right string
		return a[0] == '"' && b[0] == '"' && json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && left == right
	}
	if (a[0] != '-' && (a[0] < '0' || a[0] > '9')) || (b[0] != '-' && (b[0] < '0' || b[0] > '9')) {
		return false
	}
	// The adjustment only removes a decimal point or trailing coefficient
	// zeros, so its magnitude is bounded by the admitted message byte limit.
	// For an exponent longer than 18 significant digits that adjustment cannot
	// change its sign. Decimal carry/borrow therefore needs just one scan.
	adjustExponent := func(value string, adjustment int) string {
		negative := strings.HasPrefix(value, "-")
		value = strings.TrimPrefix(strings.TrimPrefix(value, "-"), "+")
		value = strings.TrimLeft(value, "0")
		if value == "" {
			value, negative = "0", false
		}
		if len(value) <= 18 {
			exponent, _ := strconv.ParseInt(value, 10, 64)
			if negative {
				exponent = -exponent
			}
			return strconv.FormatInt(exponent+int64(adjustment), 10)
		}
		if negative {
			adjustment = -adjustment
		}
		digits := []byte(value)
		for i := len(digits) - 1; i >= 0 && adjustment != 0; i-- {
			digit := int(digits[i]-'0') + adjustment
			if digit < 0 {
				borrow := (9 - digit) / 10
				digits[i], adjustment = byte(digit+10*borrow)+'0', -borrow
			} else {
				digits[i], adjustment = byte(digit%10)+'0', digit/10
			}
		}
		value = string(digits)
		if adjustment > 0 {
			value = strconv.Itoa(adjustment) + value
		}
		value = strings.TrimLeft(value, "0")
		if negative {
			value = "-" + value
		}
		return value
	}
	coefficient := func(raw []byte) (string, string) {
		value := string(raw)
		sign := ""
		if strings.HasPrefix(value, "-") {
			sign, value = "-", value[1:]
		}
		exponent := "0"
		if i := strings.IndexAny(value, "eE"); i >= 0 {
			exponent = value[i+1:]
			value = value[:i]
		}
		adjustment := 0
		if i := strings.IndexByte(value, '.'); i >= 0 {
			adjustment = -(len(value) - i - 1)
			value = value[:i] + value[i+1:]
		}
		value = strings.TrimLeft(value, "0")
		if value == "" {
			return "0", "0"
		}
		normalized := strings.TrimRight(value, "0")
		adjustment += len(value) - len(normalized)
		return sign + normalized, adjustExponent(exponent, adjustment)
	}
	left, leftExponent := coefficient(a)
	right, rightExponent := coefficient(b)
	return !strings.HasPrefix(leftExponent, "-") && left == right && leftExponent == rightExponent
}

// readJSONRPCLine reads up to the next newline from r and returns the
// raw bytes (without the terminator). Errors include io.EOF (clean
// detach) and any bufio.Reader error.
func readJSONRPCLine(br *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > maxStdinLineBytes {
			return nil, errStdinLineTooLong
		}
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if len(line) == 0 && err != nil {
			return nil, err
		}
		return bytes.TrimRight(line, "\r\n"), err
	}
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
