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
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/process"
)

// HTTPHost is the stdio-bridge counterpart for MCP servers that speak
// native HTTP (Streamable HTTP or plain JSON-RPC-over-HTTP) instead of
// stdio. It spawns the server subprocess on an internal port and
// proxies client traffic through, applying the same ProtocolBridge
// transforms (capability-gated synthetic tools) that StdioHost applies.
//
// Transport differences vs StdioHost:
//   - No stdin/stdout pipe. Upstream runs its own HTTP listener.
//   - No internal id multiplexing. HTTP is request-scoped; each handler
//     invocation owns its own upstream request and response.
//   - Content-Type may be application/json (single response) or
//     text/event-stream (SSE stream of JSON-RPC frames).
//
// The SSE path transforms only parseable-JSON data: payloads, leaving
// event:, id:, retry:, comments, and multi-line data: continuations
// unchanged. This is sufficient for every MCP server we've seen —
// official clients emit one-shot single-line data: frames for
// request/response traffic.
type HTTPHost struct {
	cfg HTTPHostConfig

	cmd          *exec.Cmd
	httpClient   *http.Client // request-response POSTs (60 s timeout)
	streamClient *http.Client // long-lived GET SSE (no timeout)
	upstreamURL  string
	bridge       *ProtocolBridge

	mu          sync.Mutex
	started     bool
	stopped     bool
	wg          sync.WaitGroup
	done        chan struct{}
	childExited chan struct{}
	logCloser   io.Closer // non-nil when LogPath was opened; closed by Stop
}

var errBodyTooLarge = errors.New("body too large")

const maxHTTPHostBodyBytes int64 = 4 << 20 // 4 MiB

// HTTPHostConfig describes one native-http-host instance.
type HTTPHostConfig struct {
	Command      string            // subprocess executable (e.g. "uvx")
	Args         []string          // subprocess args
	Env          map[string]string // appended to os.Environ()
	WorkingDir   string            // subprocess cwd; empty means inherit
	UpstreamPort int               // port the subprocess listens on (internal)
	UpstreamPath string            // MCP endpoint path; defaults to "/mcp"
	// HealthTimeout bounds how long Start() waits for the upstream
	// server to begin accepting connections. Default 30 s.
	HealthTimeout time.Duration
	// LogPath, when set, receives subprocess stdout+stderr tee'd into
	// a rotated log file (same convention as daemon.Launch). When empty,
	// subprocess output goes only to os.Stderr (the daemon process's own
	// stderr, typically captured by the scheduler task).
	LogPath string
}

// NewHTTPHost validates config and returns a host ready to Start.
func NewHTTPHost(cfg HTTPHostConfig) (*HTTPHost, error) {
	if cfg.Command == "" {
		return nil, errors.New("HTTPHostConfig.Command is required")
	}
	if cfg.UpstreamPort <= 0 {
		return nil, errors.New("HTTPHostConfig.UpstreamPort is required")
	}
	path := cfg.UpstreamPath
	if path == "" {
		path = "/mcp"
	}
	if cfg.HealthTimeout <= 0 {
		cfg.HealthTimeout = 30 * time.Second
	}
	cfg.UpstreamPath = path
	return &HTTPHost{
		cfg: cfg,
		// httpClient is used for request-response POSTs (initialize,
		// tools/list, tools/call). 60 s is a generous upper bound — most
		// MCP method round-trips finish in tens of ms to a few seconds.
		httpClient: &http.Client{Timeout: 60 * time.Second},
		// streamClient is used for the long-lived SSE subscription on
		// GET /mcp. Per Streamable HTTP spec, the server-to-client
		// notifications channel stays open for the lifetime of the
		// client session. A Timeout here would tear the stream down
		// roughly every minute — so the client is configured with no
		// overall deadline; cancellation is driven purely by the
		// per-request context (client disconnect or host Shutdown).
		streamClient: &http.Client{Timeout: 0},
		upstreamURL:  fmt.Sprintf("http://127.0.0.1:%d%s", cfg.UpstreamPort, path),
		bridge:       NewProtocolBridge(),
		done:         make(chan struct{}),
		childExited:  make(chan struct{}),
	}, nil
}

// Start spawns the subprocess and waits for the upstream HTTP server to
// become reachable. Returns an error if the upstream does not respond
// within HealthTimeout.
func (h *HTTPHost) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("already started")
	}

	cmd := exec.CommandContext(ctx, h.cfg.Command, h.cfg.Args...)
	process.NoConsole(cmd) // suppress per-child console pop on windowsgui parents
	cmd.Dir = h.cfg.WorkingDir
	if len(h.cfg.Env) > 0 {
		env := append([]string{}, os.Environ()...)
		for k, v := range h.cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	// Upstream logs to its own stdout+stderr. There is no protocol
	// channel on stdout for native-http servers (JSON-RPC is carried by
	// the HTTP endpoint), so both streams are diagnostic-only.
	//
	// When LogPath is set, tee both into the rotated log file AND
	// os.Stderr — matches daemon.Launch's logging contract for the
	// pre-HTTPHost native-http path so we don't regress observability.
	stdoutWriter, stderrWriter, logCloser := h.openLogWriters()
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter
	h.logCloser = logCloser

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start upstream subprocess: %w", err)
	}
	h.cmd = cmd
	h.started = true

	// Watcher goroutine owns Wait() so Stop() never double-waits.
	go func() {
		_ = cmd.Wait()
		close(h.childExited)
	}()

	if err := h.waitForReady(ctx); err != nil {
		// Full lifecycle cleanup: tree-kill the child, mark stopped,
		// wait for the watcher to observe exit, close the log file,
		// drain the waitgroup. Previously this path only killed the
		// process tree — leaving h.done open, the watcher goroutine
		// orphaned, and the logCloser unflushed. stopLocked is shared
		// with Stop() so the readiness-fail and normal-shutdown
		// branches end in the same state.
		_ = h.stopLocked()
		return fmt.Errorf("upstream not ready: %w", err)
	}
	return nil
}

// waitForReady probes the upstream URL until it accepts connections or
// HealthTimeout elapses. Any HTTP response — including 4xx — is treated
// as "listening"; only transport-level failures (ECONNREFUSED, context
// cancel) count as not-ready.
func (h *HTTPHost) waitForReady(ctx context.Context) error {
	deadline := time.Now().Add(h.cfg.HealthTimeout)
	attempt := 0
	for time.Now().Before(deadline) {
		attempt++
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, h.upstreamURL, nil)
		req.Header.Set("Accept", "text/event-stream")
		resp, err := h.httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.childExited:
			return errors.New("subprocess exited before upstream became ready")
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("upstream not ready after %v (attempts=%d)", h.cfg.HealthTimeout, attempt)
}

// ChildExited returns a channel closed when the subprocess exits.
func (h *HTTPHost) ChildExited() <-chan struct{} { return h.childExited }

// ExitState returns the subprocess's ProcessState after it has exited,
// or nil if the process has not yet been spawned or is still running.
// Callers should select on ChildExited() before reading this — calling
// it before exit is racy and may return a partially-populated state.
//
// The returned state carries the exit code and (on POSIX) signal info,
// useful for diagnosing silent failures: a `ProcessState.ExitCode()` of
// 1 with empty stderr is an obvious "controlled sys.exit()" pattern;
// negative or 137 hints at parent SIGKILL; a Windows status like
// 0xC0000005 points at a native crash. Without this, the caller only
// knows "child exited" — the actual reason was lost.
func (h *HTTPHost) ExitState() *os.ProcessState {
	if h.cmd == nil {
		return nil
	}
	return h.cmd.ProcessState
}

// Stop kills the subprocess and waits briefly for the watcher to
// observe exit. Mirrors StdioHost.Stop semantics.
func (h *HTTPHost) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.stopLocked()
}

// stopLocked is the shared teardown path used by Stop() and by Start()'s
// readiness-fail branch. Assumes the caller already holds h.mu. Runs
// the full lifecycle cleanup (close done, tree-kill child, wait for
// childExited + wg, close log) so every callsite ends in the same
// resource-free state. Previously the readiness-fail branch only
// tree-killed and left h.done open, h.logFile unclosed, and the
// childExited watcher dangling — any subsequent Stop() silently
// returned nil because h.started was already true but setup was
// incomplete.
func (h *HTTPHost) stopLocked() error {
	if !h.started || h.stopped {
		return nil
	}
	h.stopped = true
	close(h.done)
	if h.cmd != nil && h.cmd.Process != nil {
		_ = killProcessTree(h.cmd.Process.Pid)
	}
	select {
	case <-h.childExited:
	case <-time.After(5 * time.Second):
	}
	h.wg.Wait()
	if h.logCloser != nil {
		_ = h.logCloser.Close()
	}
	return nil
}

// openLogWriters returns the stdout and stderr writers to attach to
// the subprocess, plus an io.Closer for the log file (or nil). When
// LogPath is unset both writers are os.Stderr. When set, both are a
// MultiWriter that tees the log file + os.Stderr, using daemon.Launch's
// rotation convention (10 MB, 5 rotations).
func (h *HTTPHost) openLogWriters() (stdout, stderr io.Writer, closer io.Closer) {
	if h.cfg.LogPath == "" {
		return os.Stderr, os.Stderr, nil
	}
	if err := os.MkdirAll(filepath_Dir(h.cfg.LogPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warn: mkdir log dir: %v\n", err)
		return os.Stderr, os.Stderr, nil
	}
	if err := RotateIfLarge(h.cfg.LogPath, 10*1024*1024, 5); err != nil {
		fmt.Fprintf(os.Stderr, "warn: rotate log: %v\n", err)
	}
	logFile, err := os.OpenFile(h.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: open log %q: %v\n", h.cfg.LogPath, err)
		return os.Stderr, os.Stderr, nil
	}
	multi := io.MultiWriter(logFile, os.Stderr)
	return multi, multi, logFile
}

// HTTPHandler returns the http.Handler for /mcp.
func (h *HTTPHost) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handlePOST(w, r)
		case http.MethodGet:
			h.forwardPassthrough(w, r) // SSE subscription — no transforms
		case http.MethodDelete:
			// Session termination: subprocess is shared; just acknowledge.
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (h *HTTPHost) handlePOST(w http.ResponseWriter, r *http.Request) {
	body, err := readAllWithLimit(r.Body, maxHTTPHostBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		// Body not JSON-RPC — forward bytes unchanged.
		h.forwardBytes(w, r, body)
		return
	}

	origIDRaw, hasID := msg["id"]
	var origMethod string
	if m, ok := msg["method"]; ok {
		_ = json.Unmarshal(m, &origMethod)
	}

	// NOTE on initialize caching: unlike StdioHost (whose subprocess
	// expects exactly one initialize per process lifetime), the native-http
	// upstream creates a separate session per initialize and returns a
	// distinct Mcp-Session-Id header. Replaying a cached body would (1)
	// strip upstream's session-id header from the response, breaking every
	// subsequent POST/GET the client issues, and (2) hide per-client state
	// that the upstream server expects to manage itself. Therefore HTTPHost
	// does NOT short-circuit initialize — every initialize is forwarded to
	// upstream so the upstream creates and returns a fresh session. The
	// bridge still CacheInitialize()s the first successful response so the
	// capability probe (hasCapability) has a stable answer for tools/list
	// injection, but the cached body is used for capability inspection
	// only, never replayed as a response.

	// Synthetic-tool request rewrite (shared with StdioHost).
	var action BridgeAction
	if hasID {
		action = h.bridge.TransformRequest(msg)
		if action.SynthError != nil {
			writeToolCallError(w, origIDRaw, action.SynthError.Error())
			return
		}
	}

	// Re-marshal potentially-rewritten msg and forward.
	forwardBody, err := json.Marshal(msg)
	if err != nil {
		http.Error(w, "marshal rewritten: "+err.Error(), http.StatusInternalServerError)
		return
	}
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.upstreamURL, bytes.NewReader(forwardBody))
	if err != nil {
		http.Error(w, "build upstream request: "+err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeadersForUpstream(upstreamReq.Header, r.Header)

	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy upstream response headers but skip hop-by-hop and body-size
	// fields — our writes may change the body length (InjectTools, reshape)
	// so Content-Length from upstream is not valid after transform. The
	// Go http server recomputes Content-Length / Transfer-Encoding based
	// on what we actually write.
	copyResponseHeaders(w.Header(), resp.Header)

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		w.WriteHeader(resp.StatusCode)
		h.streamSSEResponse(w, resp.Body, origMethod, action.Active)
		return
	}

	// Plain JSON response.
	respBody, err := readAllWithLimit(resp.Body, maxHTTPHostBodyBytes)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			http.Error(w, "upstream response too large", http.StatusBadGateway)
			return
		}
		http.Error(w, "read upstream response: "+err.Error(), http.StatusBadGateway)
		return
	}
	if origMethod == "initialize" {
		h.bridge.CacheInitialize(respBody)
	}
	respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func readAllWithLimit(r io.Reader, maxBytes int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, errBodyTooLarge
	}
	return body, nil
}

// copyResponseHeaders propagates selected upstream response headers to
// the client. Skips Content-Length, Transfer-Encoding, Connection, and
// other hop-by-hop or body-size-specific headers that would mismatch
// after the bridge rewrites the response body.
func copyResponseHeaders(dst, src http.Header) {
	skip := map[string]bool{
		"Content-Length":    true,
		"Transfer-Encoding": true,
		"Connection":        true,
		"Keep-Alive":        true,
	}
	for k, vv := range src {
		if skip[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// forwardPassthrough handles GET /mcp (long-lived SSE subscription).
// Uses streamClient (no timeout) because the notifications channel
// stays open for the client session lifetime — the 60 s httpClient
// deadline would tear it down every minute otherwise.
func (h *HTTPHost) forwardPassthrough(w http.ResponseWriter, r *http.Request) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstreamURL, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeadersForUpstream(upstreamReq.Header, r.Header)
	resp, err := h.streamClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *HTTPHost) forwardBytes(w http.ResponseWriter, r *http.Request, body []byte) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstreamURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyHeadersForUpstream(upstreamReq.Header, r.Header)
	resp, err := h.httpClient.Do(upstreamReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// streamSSEResponse forwards an SSE stream frame-by-frame and applies
// the bridge transform to each JSON-bearing data: line. Non-JSON data,
// multi-line data continuations, and non-data fields pass through
// unchanged. Flushes after each frame boundary (blank line).
func (h *HTTPHost) streamSSEResponse(w http.ResponseWriter, upstream io.Reader,
	origMethod string, active *SyntheticTool) {
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(upstream)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			payload := []byte(strings.TrimPrefix(line, "data: "))
			if json.Valid(payload) {
				// Cache initialize response BEFORE transform so the
				// cached body keeps the original shape (capabilities
				// probe reads it as-is).
				if origMethod == "initialize" {
					h.bridge.CacheInitialize(payload)
				}
				transformed := h.bridge.TransformResponse(origMethod, active, payload)
				_, _ = fmt.Fprintf(w, "data: %s\n", transformed)
			} else {
				// Non-JSON data — passthrough unchanged.
				_, _ = fmt.Fprintf(w, "%s\n", line)
			}
		default:
			// event:, id:, retry:, comments, blank line
			_, _ = fmt.Fprintf(w, "%s\n", line)
			if line == "" && flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// copyHeadersForUpstream copies the request headers the upstream needs.
// Explicit allowlist rather than full pass-through to avoid leaking
// hop-by-hop headers (Connection, Keep-Alive, Transfer-Encoding, etc.)
// that would confuse upstream.
func copyHeadersForUpstream(dst, src http.Header) {
	for _, name := range []string{
		"Accept",
		"Content-Type",
		"Mcp-Session-Id",
		"Authorization",
	} {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}
