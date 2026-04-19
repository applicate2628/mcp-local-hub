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

	cmd         *exec.Cmd
	httpClient  *http.Client
	upstreamURL string
	bridge      *ProtocolBridge

	mu          sync.Mutex
	started     bool
	stopped     bool
	wg          sync.WaitGroup
	done        chan struct{}
	childExited chan struct{}
}

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
		cfg:         cfg,
		httpClient:  &http.Client{Timeout: 60 * time.Second},
		upstreamURL: fmt.Sprintf("http://127.0.0.1:%d%s", cfg.UpstreamPort, path),
		bridge:      NewProtocolBridge(),
		done:        make(chan struct{}),
		childExited: make(chan struct{}),
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
	cmd.Dir = h.cfg.WorkingDir
	if len(h.cfg.Env) > 0 {
		env := append([]string{}, os.Environ()...)
		for k, v := range h.cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	// Upstream logs to its own stdout/stderr — forward both to our stderr
	// so operators can see startup issues without polluting any protocol
	// channel (there is no protocol channel on stdout for native-http).
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

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
		_ = cmd.Process.Kill()
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

// Stop kills the subprocess and waits briefly for the watcher to
// observe exit. Mirrors StdioHost.Stop semantics.
func (h *HTTPHost) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started || h.stopped {
		return nil
	}
	h.stopped = true
	close(h.done)
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	select {
	case <-h.childExited:
	case <-time.After(5 * time.Second):
	}
	h.wg.Wait()
	return nil
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
	body, err := io.ReadAll(r.Body)
	if err != nil {
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

	// Initialize-cache short-circuit. Responds in the client's preferred
	// content-type (application/json or text/event-stream).
	if hasID && origMethod == "initialize" {
		if cached := h.bridge.InitCached(); cached != nil {
			var respMsg map[string]json.RawMessage
			_ = json.Unmarshal(cached, &respMsg)
			respMsg["id"] = origIDRaw
			out, _ := json.Marshal(respMsg)
			writeInAcceptedFormat(w, r, out)
			return
		}
	}

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
	respBody, _ := io.ReadAll(resp.Body)
	if origMethod == "initialize" {
		h.bridge.CacheInitialize(respBody)
	}
	respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
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

// forwardPassthrough is used for GET /mcp (SSE subscription) and for
// non-JSON POST bodies. No transforms — just relay bytes both directions.
func (h *HTTPHost) forwardPassthrough(w http.ResponseWriter, r *http.Request) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, h.upstreamURL, r.Body)
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

// writeInAcceptedFormat writes a JSON-RPC response body either as raw
// JSON or wrapped in a single SSE message frame, based on the client's
// Accept header. Used for initialize-cache short-circuits where we
// must produce the response without consulting upstream.
func writeInAcceptedFormat(w http.ResponseWriter, r *http.Request, body []byte) {
	accept := r.Header.Get("Accept")
	// Prefer text/event-stream only when the client accepts SSE but
	// does NOT also accept plain JSON — that's the MCP-spec convention
	// for "I can only parse SSE frames".
	if strings.Contains(accept, "text/event-stream") && !strings.Contains(accept, "application/json") {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
