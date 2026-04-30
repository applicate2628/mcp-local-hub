package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/process"
)

// HostConfig describes one stdio-host instance.
type HostConfig struct {
	Command    string            // subprocess executable
	Args       []string          // subprocess args
	Env        map[string]string // appended to os.Environ() for the subprocess
	WorkingDir string            // subprocess cwd; empty means inherit
	// LogPath, when set, receives subprocess stderr tee'd into a rotated
	// log file (same convention as daemon.Launch — 10 MB, 5 rotations).
	// Stdout is the JSON-RPC protocol channel and is never written to
	// the log; only diagnostic stderr is captured. When empty, stderr
	// goes to os.Stderr only (the daemon process's own stderr).
	LogPath string
}

// StdioHost hosts a long-lived stdio subprocess and (in later tasks) exposes
// an HTTP endpoint that multiplexes concurrent MCP clients onto it.
type StdioHost struct {
	cfg HostConfig

	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdoutScan *bufio.Scanner

	// Test-only unbuffered channel for readStdoutTest.
	testStdout chan []byte

	mu      sync.Mutex
	stdinMu sync.Mutex     // serializes writeStdin so concurrent callers cannot interleave on the wire
	wg      sync.WaitGroup // tracks reader goroutines so Stop() can drain them
	started bool
	stopped bool

	// HTTP-side multiplexing: each incoming JSON-RPC request id is rewritten
	// to a monotonic internal id; readStdoutLoop dispatches the response back
	// to the waiting handler via the matching channel in `pending`.
	nextInternalID atomic.Int64
	pendingMu      sync.Mutex
	pending        map[int64]chan json.RawMessage

	// logFile is the optional rotated log file for child stderr.
	// nil when HostConfig.LogPath is empty.
	logFile io.Closer

	// bridge owns the shared initialize cache, capability probe, and
	// SyntheticTool-driven request/response transforms. Initialize-cache
	// was previously a pair of fields (initMu, initCached) on StdioHost;
	// moving them into ProtocolBridge lets HTTPHost reuse the same
	// machinery without duplication.
	bridge *ProtocolBridge

	// SSE subscribers: GET /mcp opens a server-sent-events stream that
	// receives subprocess-originated notifications (JSON-RPC messages with
	// no `id` or unrouted ids). Each subscriber holds one buffered channel.
	// Subscriptions are session-scoped via validSession; sseActive caps the
	// number of concurrent streams so a client cannot exhaust goroutines.
	sseMu      sync.Mutex
	sseClients []chan []byte
	sseActive  atomic.Int32
	sessionID  string

	done        chan struct{} // closed by Stop() to unblock pending handlers
	childExited chan struct{} // closed by the watcher goroutine when cmd.Wait() returns

	// job is a Windows Job Object (no-op on POSIX) configured with
	// KILL_ON_JOB_CLOSE so the kernel reaps any descendant tree the
	// subprocess spawned (e.g. npx → node → mcp server) when our
	// daemon process is force-killed without a chance to run cleanup.
	// Set after cmd.Start(); released in Stop after killProcessTree.
	// See internal/process/jobobject_windows.go.
	job *process.Job
}

const maxMCPPostBodyBytes int64 = 1 << 20 // 1 MiB

func NewStdioHost(cfg HostConfig) (*StdioHost, error) {
	if cfg.Command == "" {
		return nil, errors.New("HostConfig.Command is required")
	}
	sid, err := randomSessionID()
	if err != nil {
		return nil, fmt.Errorf("generate session id: %w", err)
	}
	return &StdioHost{
		cfg:         cfg,
		testStdout:  make(chan []byte, 16),
		pending:     make(map[int64]chan json.RawMessage),
		bridge:      NewProtocolBridge(),
		done:        make(chan struct{}),
		childExited: make(chan struct{}),
		sessionID:   sid,
	}, nil
}

// Start spawns the subprocess and begins the stdout reader goroutine.
// Returns an error if the subprocess fails to start.
func (h *StdioHost) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return errors.New("already started")
	}

	cmd := exec.CommandContext(ctx, h.cfg.Command, h.cfg.Args...)
	process.NoConsole(cmd) // Windows: suppress console pop (windowsgui parent)
	// Linux: PR_SET_PDEATHSIG=SIGKILL — best-effort direct-child
	// orphan mitigation if mcphub is force-killed. Strictly weaker
	// than Windows Job Object: does NOT cascade through wrappers
	// like uvx/npx that fork-and-stay. See pdeathsig_linux.go.
	process.SetParentDeathSignal(cmd)
	cmd.Dir = h.cfg.WorkingDir
	if len(h.cfg.Env) > 0 {
		env := append([]string{}, os.Environ()...)
		for k, v := range h.cfg.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Stderr is NOT part of the JSON-RPC protocol channel. Forward it to
	// os.Stderr (and thus the scheduler log file via Launch() tee in Task 5).
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start subprocess: %w", err)
	}
	// Place subprocess into a Windows Job Object with KILL_ON_JOB_CLOSE
	// so descendant processes (npx → node → mcp server, etc.) cannot
	// outlive our parent on force-kill. POSIX is a no-op stub for now.
	// See internal/process/jobobject_windows.go.
	if job, jobErr := process.NewKillOnCloseJob(); jobErr == nil {
		if err := job.Assign(cmd); err != nil {
			_ = job.Close()
			fmt.Fprintf(os.Stderr, "warn: assign stdio child to Job Object: %v (orphan protection disabled for this child)\n", err)
		} else {
			h.job = job
		}
	} else {
		fmt.Fprintf(os.Stderr, "warn: create Job Object: %v (orphan protection disabled)\n", jobErr)
	}
	// Forward stderr to os.Stderr AND (if LogPath set) to a rotated
	// log file. Mirrors daemon.Launch / HTTPHost behavior so
	// `mcphub logs <server>` shows actual subprocess output instead of
	// "(no output yet)" for stdio-bridge daemons.
	stderrSink := h.openStderrSink()
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		s := bufio.NewScanner(stderr)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			fmt.Fprintf(stderrSink, "[subproc stderr] %s\n", s.Bytes())
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // up to 1 MB lines

	h.cmd = cmd
	h.stdin = stdin
	h.stdoutScan = scanner
	h.started = true

	// Reader goroutine: pipes every stdout line to testStdout (and, in later
	// tasks, to the ID-routing map for HTTP response delivery).
	h.wg.Add(1)
	go h.readStdoutLoop()

	// Watcher goroutine: owns cmd.Wait() exclusively so no other call site
	// double-waits. When the child exits for any reason (normal exit, crash,
	// signal, Stop-initiated Kill), we close h.childExited so:
	//   - in-flight HTTP handlers can return 502 instead of blocking 30s
	//   - Stop() can observe the termination without calling Wait() itself
	//   - the outer daemon loop (internal/cli/daemon.go) can observe
	//     unexpected death and exit non-zero so the scheduler restarts us
	go func() {
		_ = cmd.Wait()
		close(h.childExited)
	}()

	return nil
}

// ChildExited returns a channel that is closed when the subprocess exits
// for any reason. Callers may select on it to react to unexpected death
// (the outer daemon loop) or to confirm clean shutdown (Stop()).
func (h *StdioHost) ChildExited() <-chan struct{} {
	return h.childExited
}

// Stop terminates the subprocess and closes all pipes.
//
// Stop does NOT call cmd.Process.Wait(); that call is owned exclusively by
// the watcher goroutine spawned in Start() so there is no double-Wait race.
// Instead, Stop signals Kill() (if the child is still alive) and then blocks
// on h.childExited to confirm the watcher saw the exit and closed the pipes.
func (h *StdioHost) Stop() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.started || h.stopped {
		return nil
	}
	h.stopped = true
	close(h.done) // unblock any pending HTTP handlers waiting on the subprocess
	_ = h.stdin.Close()
	if h.cmd != nil && h.cmd.Process != nil {
		// Only tree-kill if the watcher has not already observed process
		// exit. This avoids PID-reuse hazards from issuing a fresh
		// PID-based taskkill/pkill after the original process is gone.
		select {
		case <-h.childExited:
			// already exited; no kill needed
		default:
			// Tree-kill so wrappers (npx, uvx, uv, node launchers) and
			// their real child servers all go down together. Plain
			// Process.Kill only kills the wrapper; its child would keep
			// its stdin/stdout pipes open past the Wait-watcher close
			// and the port bound past Stop's return.
			_ = killProcessTree(h.cmd.Process.Pid)
		}
	}
	// Wait for the watcher goroutine to observe cmd.Wait() returning. This
	// is bounded: either the child was already dead (immediate) or Kill()
	// just terminated it (milliseconds). We still cap the wait so a truly
	// wedged process (e.g. unkillable kernel state on Windows) cannot hang
	// Stop forever — the outer daemon will exit the host process anyway.
	select {
	case <-h.childExited:
	case <-time.After(5 * time.Second):
	}
	h.wg.Wait()
	if h.logFile != nil {
		_ = h.logFile.Close()
	}
	if h.job != nil {
		// Final handle release. killProcessTree above already terminated
		// in-job processes; closing here lets the kernel reclaim the job
		// object resource. No-op on POSIX.
		_ = h.job.Close()
	}
	return nil
}

// openStderrSink returns a writer for child stderr. When LogPath is set
// the writer tees to both the rotated log file (so `mcphub logs` works
// for stdio-bridge daemons) and os.Stderr (so the operator's terminal
// keeps showing live diagnostics). When LogPath is empty, falls back to
// os.Stderr only — matches the pre-LogPath behavior.
func (h *StdioHost) openStderrSink() io.Writer {
	if h.cfg.LogPath == "" {
		return os.Stderr
	}
	if err := os.MkdirAll(filepath_Dir(h.cfg.LogPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warn: mkdir log dir: %v\n", err)
		return os.Stderr
	}
	if err := RotateIfLarge(h.cfg.LogPath, 10*1024*1024, 5); err != nil {
		fmt.Fprintf(os.Stderr, "warn: rotate log: %v\n", err)
	}
	f, err := os.OpenFile(h.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: open log %q: %v\n", h.cfg.LogPath, err)
		return os.Stderr
	}
	h.logFile = f
	return io.MultiWriter(f, os.Stderr)
}

// writeStdin sends a line (terminated with '\n') to the subprocess stdin.
// Safe for concurrent callers: the buffer+newline are concatenated into a
// single slice and sent under stdinMu so two callers cannot interleave on
// the JSON-RPC wire to the subprocess.
func (h *StdioHost) writeStdin(line []byte) error {
	h.stdinMu.Lock()
	defer h.stdinMu.Unlock()
	buf := line
	if len(line) == 0 || line[len(line)-1] != '\n' {
		b := make([]byte, 0, len(line)+1)
		b = append(b, line...)
		b = append(b, '\n')
		buf = b
	}
	_, err := h.stdin.Write(buf)
	return err
}

// readStdoutLoop is the subprocess stdout reader. It peeks at each line's
// JSON-RPC id and dispatches it to the corresponding waiting HTTP handler
// via the `pending` map. Lines without a matching pending entry (e.g.
// notifications, server-initiated messages, or unrouted ids) fall through
// to testStdout so unit tests can still observe raw subprocess output.
func (h *StdioHost) readStdoutLoop() {
	defer h.wg.Done()
	for h.stdoutScan.Scan() {
		line := append([]byte(nil), h.stdoutScan.Bytes()...)
		// Try to parse id and route to pending request.
		var peek struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &peek); err == nil && len(peek.ID) > 0 {
			var id int64
			if err := json.Unmarshal(peek.ID, &id); err == nil {
				h.pendingMu.Lock()
				ch, ok := h.pending[id]
				h.pendingMu.Unlock()
				if ok {
					select {
					case ch <- line:
					default:
					}
					continue
				}
			}
		}
		// Unrouted line = notification or untracked id → fan out to SSE
		// subscribers so Streamable HTTP clients receive server-initiated
		// traffic. Non-blocking send: a slow subscriber cannot stall the
		// reader loop for other clients.
		h.sseMu.Lock()
		for _, c := range h.sseClients {
			select {
			case c <- line:
			default:
			}
		}
		h.sseMu.Unlock()
		// Also keep the testStdout path for tests that watch raw output.
		select {
		case h.testStdout <- line:
		default:
		}
	}
}

// HTTPHandler returns the http.Handler for /mcp implementing the
// Streamable HTTP MCP transport: POST for JSON-RPC requests, GET for SSE
// subscription, DELETE for client-side session termination.
func (h *StdioHost) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			h.handlePOST(w, r)
		case http.MethodDelete:
			if !h.validSession(r) {
				http.Error(w, "missing or invalid session id", http.StatusUnauthorized)
				return
			}
			// Session termination: subprocess stays alive (shared across clients),
			// but we acknowledge the client's request. Nothing to clean up on our side
			// since pending requests are per-request scoped.
			w.WriteHeader(http.StatusNoContent)
		case http.MethodGet:
			h.handleSSE(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func (h *StdioHost) handlePOST(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMCPPostBodyBytes)
	defer r.Body.Close()
	w.Header().Set("Mcp-Session-Id", h.sessionID)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}

	origIDRaw, hasID := msg["id"]

	// Snapshot the original method once. Used by the initialize cache,
	// the __read_resource__ rewrite, and the post-response injection hook.
	var origMethod string
	if m, ok := msg["method"]; ok {
		_ = json.Unmarshal(m, &origMethod)
	}

	// Initialize-cache short-circuit. Stdio MCP servers expect `initialize`
	// once per process lifetime; on a cache hit we replay the prior response
	// with the caller's id substituted, without touching the subprocess.
	if hasID && origMethod == "initialize" {
		if cached := h.bridge.InitCached(); cached != nil {
			var respMsg map[string]json.RawMessage
			_ = json.Unmarshal(cached, &respMsg)
			respMsg["id"] = origIDRaw
			out, _ := json.Marshal(respMsg)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(out)
			return
		}
	}

	// Capability bridge: rewrite tools/call targeting a synthetic tool
	// (__read_resource__ today, __get_prompt__/__list_prompts__ once
	// Phase 2 lands) into the underlying MCP method. action.Active
	// stays non-nil through the response path so TransformResponse can
	// reshape the body.
	var action BridgeAction
	if hasID {
		action = h.bridge.TransformRequest(msg)
		if action.SynthError != nil {
			writeToolCallError(w, origIDRaw, action.SynthError.Error())
			return
		}
	}

	// Notifications have no id; we forward-and-forget.
	if !hasID {
		if err := h.writeStdin(body); err != nil {
			http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Rewrite id to an internal counter to avoid collisions across HTTP clients.
	internalID := h.nextInternalID.Add(1)
	msg["id"] = json.RawMessage(strconv.FormatInt(internalID, 10))
	rewritten, _ := json.Marshal(msg)

	respCh := make(chan json.RawMessage, 1)
	h.pendingMu.Lock()
	h.pending[internalID] = respCh
	h.pendingMu.Unlock()
	defer func() {
		h.pendingMu.Lock()
		delete(h.pending, internalID)
		h.pendingMu.Unlock()
	}()

	if err := h.writeStdin(rewritten); err != nil {
		http.Error(w, "write stdin: "+err.Error(), http.StatusInternalServerError)
		return
	}

	select {
	case respBody := <-respCh:
		// Cache initialize responses so subsequent clients can short-circuit.
		// First responder wins; concurrent first-callers still get a correct
		// answer (they each forwarded once before the cache existed).
		if origMethod == "initialize" {
			h.bridge.CacheInitialize(respBody)
		}
		// Bridge response transforms. Inside TransformResponse:
		//   - if action.Active != nil (synthetic tool call), respBody is
		//     reshaped via Active.MapResult (e.g. resources/read → tools/call)
		//   - else if origMethod == "tools/list", synthetic tools whose
		//     capability is declared get injected into result.tools[]
		//   - otherwise passthrough
		respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
		// Restore the original id before returning to the HTTP client.
		var respMsg map[string]json.RawMessage
		if err := json.Unmarshal(respBody, &respMsg); err != nil {
			http.Error(w, "subprocess returned invalid JSON", http.StatusBadGateway)
			return
		}
		respMsg["id"] = origIDRaw
		out, _ := json.Marshal(respMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	case <-h.done:
		http.Error(w, "host shutting down", http.StatusServiceUnavailable)
	case <-h.childExited:
		// Child died while we were waiting for its reply. Return 502 so the
		// client sees a distinct, immediate failure instead of blocking the
		// full 30s timeout. The outer daemon loop observes the same signal
		// and exits non-zero so the scheduler can restart the whole task.
		http.Error(w, "subprocess died unexpectedly", http.StatusBadGateway)
	case <-r.Context().Done():
		http.Error(w, "client canceled", http.StatusRequestTimeout)
	case <-time.After(30 * time.Second):
		http.Error(w, "subprocess response timeout", http.StatusGatewayTimeout)
	}
}

const maxSSESubscribers = 32

// handleSSE keeps a scoped SSE stream open for the active session until the
// client disconnects, host stops, or the child exits.
func (h *StdioHost) handleSSE(w http.ResponseWriter, r *http.Request) {
	if !h.validSession(r) {
		http.Error(w, "missing or invalid session id", http.StatusUnauthorized)
		return
	}
	if h.sseActive.Add(1) > maxSSESubscribers {
		h.sseActive.Add(-1)
		http.Error(w, "too many SSE subscribers", http.StatusTooManyRequests)
		return
	}
	defer h.sseActive.Add(-1)

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Mcp-Session-Id", h.sessionID)
	_, _ = fmt.Fprint(w, ": keepalive\n\n")
	flusher.Flush()

	ch := make(chan []byte, 32)
	h.sseMu.Lock()
	h.sseClients = append(h.sseClients, ch)
	h.sseMu.Unlock()
	defer func() {
		h.sseMu.Lock()
		for i, c := range h.sseClients {
			if c == ch {
				h.sseClients = append(h.sseClients[:i], h.sseClients[i+1:]...)
				break
			}
		}
		h.sseMu.Unlock()
	}()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-h.childExited:
			// Child died — nothing more will be emitted on this stream.
			// Return cleanly so the HTTP server can close the connection.
			return
		case line := <-ch:
			_, _ = fmt.Fprintf(w, "data: %s\n\n", line)
			flusher.Flush()
		}
	}
}

func (h *StdioHost) validSession(r *http.Request) bool {
	return r.Header.Get("Mcp-Session-Id") == h.sessionID
}

func randomSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b[:]), nil
}

// readStdoutTest exposes the raw stdout stream for unit tests only.
func (h *StdioHost) readStdoutTest(timeout time.Duration) ([]byte, error) {
	select {
	case line := <-h.testStdout:
		return line, nil
	case <-time.After(timeout):
		return nil, errors.New("timeout waiting for stdout line")
	}
}
