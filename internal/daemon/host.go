package daemon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mcp-local-hub/internal/process"
)

// HostConfig describes one stdio-host instance.
type HostConfig struct {
	Command string            // subprocess executable
	Args    []string          // subprocess args
	Env     map[string]string // appended to os.Environ() for the subprocess
	// UnsetEnv lists env keys to REMOVE from the inherited os.Environ() for
	// the subprocess — used for skipped OPTIONAL secrets so the child sees the
	// key as truly ABSENT (not present-but-empty), and does not inherit a
	// same-named ambient parent value (Codex #377).
	UnsetEnv   []string
	WorkingDir string // subprocess cwd; empty means inherit
	// LogPath, when set, receives subprocess stderr tee'd into a rotated
	// log file via rotatingFileWriter (10 MB, 5 rotations). Stdout is
	// the JSON-RPC protocol channel and is never written to the log;
	// only diagnostic stderr is captured. When empty AND mcphub's own
	// stderr is a real terminal, stderr goes to os.Stderr (interactive
	// debug). When empty + non-TTY (scheduler-spawned, stdio-child), the
	// subprocess output is dropped to io.Discard to avoid leaking
	// upstream chatter into the parent's inherited stdio (issue #162).
	LogPath string
}

// composeChildEnv builds a subprocess environment: os.Environ() with every key
// in `unset` REMOVED, then the `env` overrides appended. Removing the unset
// keys (vs setting them empty) makes a skipped OPTIONAL secret truly ABSENT in
// the child, so a server that branches on env-var PRESENCE (os.LookupEnv,
// Node `process.env.KEY !== undefined`, Python membership) does not enter its
// credential path with an empty token, and a same-named ambient parent value
// is never inherited (Codex #377). Shared by StdioHost + HTTPHost.
func composeChildEnv(env map[string]string, unset []string) []string {
	var unsetSet map[string]bool
	if len(unset) > 0 {
		unsetSet = make(map[string]bool, len(unset))
		for _, k := range unset {
			unsetSet[k] = true
		}
	}
	out := make([]string, 0, len(os.Environ())+len(env))
	for _, kv := range os.Environ() {
		if unsetSet != nil {
			if k, _, ok := strings.Cut(kv, "="); ok && unsetSet[k] {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
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

	// SSE subscribers: GET /mcp opens a server-sent-events stream. We do not
	// broadcast unrouted subprocess output because it cannot be attributed to
	// a specific HTTP caller safely.
	sseMu      sync.Mutex
	sseClients []chan []byte
	sseActive  atomic.Int32
	sessionID  string

	done        chan struct{}                   // closed by Stop() to unblock pending handlers
	procExited  chan struct{}                   // closed by the watcher goroutine when cmd.Process.Wait() returns (Phase 1 — OS-exit detected, before pipe drain)
	childExited chan struct{}                   // closed by the watcher goroutine after the bounded pipe-drain wait (Phase 4 — pipes drained or pipeDrainTimeout fired); cmd.Wait runs out-of-line AFTER this close
	exitState   atomic.Pointer[os.ProcessState] // saved by Phase 1 from cmd.Process.Wait's return; the out-of-line cmd.Wait would otherwise rewrite cmd.ProcessState to nil (Codex bot P2 on f2dbea0); read via ExitState()

	// job is a Windows Job Object (no-op on POSIX) configured with
	// KILL_ON_JOB_CLOSE so the kernel reaps any descendant tree the
	// subprocess spawned (e.g. npx → node → mcp server) when our
	// daemon process is force-killed without a chance to run cleanup.
	// Set after cmd.Start(); released in Stop after killProcessTree.
	// See internal/process/jobobject_windows.go.
	job *process.Job
}

const maxMCPPostBodyBytes int64 = 1 << 20 // 1 MiB

// maxPendingRequests caps the number of concurrent in-flight JSON-RPC
// requests routed through one StdioHost subprocess. Beyond this the
// handler returns 429; legitimate MCP usage rarely exceeds a handful of
// concurrent requests per client, so 128 is a generous bound.
const maxPendingRequests = 128

// pipeDrainTimeout caps how long the watcher goroutine waits for
// stdout/stderr scanners to drain to EOF before calling cmd.Wait.
//
//   - Fast-exiting children: scanners reach EOF within milliseconds; the
//     wait is essentially a no-op and Go's StdoutPipe/StderrPipe contract
//     is preserved (no truncation of the final response).
//   - Inherited-stdio descendants (Codex Cloud finding 63b417d2): a
//     descendant that inherited stdout/stderr can keep pipes open after
//     the immediate child exits. Without a deadline, the watcher would
//     never call cmd.Wait, childExited would never close, the supervisor
//     would never trigger scheduler restart, and Stop would wedge.
//   - Five seconds is enough for legitimate slow-flush patterns and
//     short enough to fail fast when descendants are reparenting.
const pipeDrainTimeout = 5 * time.Second

// stdioToolResponseTimeout bounds how long the StdioHost bridge waits for the
// child MCP server's reply to a single request before returning 504. It must
// exceed the longest legitimate tool execution: a tools/call IS the tool's run,
// and a long tool (drmemory_run instrumenting a binary under DynamoRIO — slow
// on first run, default timeout_sec 1200; or a long build/ctest via
// run_in_oneapi_env, default 600) holds the reply for many minutes. The earlier
// 30 s cap returned a spurious 504 ("subprocess response timeout") before the
// tool's own timeout could fire (observed live: drmemory first-run 504'd at
// ~34 s). The real bounds remain the per-request r.Context() (client
// disconnect), h.childExited (child death), h.done (host shutdown), and the
// tool's own timeout_sec. 30 min is a generous backstop above the longest tool
// default that still caps a truly-wedged child; fast methods (initialize,
// tools/list) reply in ms and never approach it.
const stdioToolResponseTimeout = 30 * time.Minute

// requireJSONContentType returns true if Content-Type parses as exactly
// `application/json` (case-insensitive media type, parameters allowed).
// Empty Content-Type is rejected — MCP POST clients are required to set
// it; admitting empty would let CSRF probes bypass the gate via simple
// `text/plain` form posts that browsers can issue cross-origin.
//
// strings.HasPrefix(ct, "application/json") was the prior shape and
// admits `application/jsonx`. mime.ParseMediaType handles params (e.g.
// `application/json; charset=utf-8`) correctly.
func requireJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mt, "application/json")
}

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
		procExited:  make(chan struct{}),
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
	if len(h.cfg.Env) > 0 || len(h.cfg.UnsetEnv) > 0 {
		cmd.Env = composeChildEnv(h.cfg.Env, h.cfg.UnsetEnv)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Stderr is NOT part of the JSON-RPC protocol channel. openStderrSink
	// below routes it through the daemonDiagWriter contract: log file
	// (durable) + os.Stderr (TTY only) / io.Discard (non-TTY).
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
			fmt.Fprintf(daemonDiagWriter(), "warn: assign stdio child to Job Object: %v (orphan protection disabled for this child)\n", err)
		} else {
			h.job = job
		}
	} else {
		fmt.Fprintf(daemonDiagWriter(), "warn: create Job Object: %v (orphan protection disabled)\n", jobErr)
	}
	// Forward stderr through openStderrSink: rotated log file when
	// LogPath is set, plus os.Stderr only when mcphub's stderr is a
	// real terminal. Mirrors HTTPHost behavior so
	// `mcphub logs <server>` shows actual subprocess output instead of
	// "(no output yet)" for stdio-bridge daemons.
	stderrSink := h.openStderrSink()
	var pipesDrained sync.WaitGroup
	pipesDrained.Add(2)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		defer pipesDrained.Done()
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
	go func() {
		defer pipesDrained.Done()
		h.readStdoutLoop()
	}()

	// Watcher goroutine: five phases reconciling six constraints.
	//
	// 1. Codex Cloud finding 63b417d2: childExited must close in
	//    BOUNDED time after OS-exit even when a descendant inherited
	//    stdout/stderr — supervisor restart cannot wait indefinitely.
	//    PR #117's unbounded pipesDrained.Wait() before close was the
	//    bug; bounding it via pipeDrainTimeout below preserves the
	//    fix.
	//
	// 2. Go's StdoutPipe/StderrPipe contract: cmd.Wait closes the
	//    pipes once it returns, so calling Wait before the scanners
	//    have drained to EOF truncates the final buffered output.
	//    Fast-exiting children (a shell that writes one JSON line and
	//    exits) lose their last response otherwise.
	//
	// 3. Codex bot P1 on f2512fe: the bounded pipe-drain timeout must
	//    only arm after the child has exited. Otherwise any daemon
	//    that runs longer than `pipeDrainTimeout` always falls into
	//    the timeout path while it is still healthy, which both
	//    truncates final output AND emits a misleading "child exited"
	//    warning while the child is alive.
	//
	// 4. Codex bot P1 on 34b1a30: childExited must NOT close ahead of
	//    pipe drain. A child can write its final reply, exit, and have
	//    the scanner deliver that reply AFTER childExited closes —
	//    handlePOST's select would non-deterministically pick the
	//    childExited branch and return 502 even though a valid response
	//    arrived. Closing childExited after pipe drain (or the bounded
	//    timeout) ensures handlePOST sees a ready respCh first.
	//
	// 5. Codex bot P2 on f2dbea0: cmd.Process.Wait()'s returned
	//    ProcessState must be persisted somewhere readable so the exit
	//    code / signal info is recoverable for diagnostics. Phase 1
	//    saves it to h.exitState (read via ExitState()) — NOT to
	//    cmd.ProcessState, because Phase 3's cmd.Wait would rewrite
	//    cmd.ProcessState to nil during its ECHILD-returning internal
	//    Process.Wait re-call.
	//
	// 6. Codex bot P1 on 0e903bd: cmd.Wait MUST be called so os/exec
	//    releases its internal resources. exec.CommandContext's
	//    Start() launches a context-watcher goroutine that only
	//    exits when cmd.Wait drains its result channel; never
	//    calling cmd.Wait leaks that goroutine for the host context's
	//    lifetime. Phase 3 calls cmd.Wait — its closeDescriptors does
	//    double duty as the unblock-the-scanners path for inherited
	//    descendants holding the pipes.
	//
	// Phase 1: cmd.Process.Wait() reaps the OS child WITHOUT closing
	// the pipe read-ends. We persist the returned ProcessState onto
	// h.exitState (NOT cmd.ProcessState — Phase 3's cmd.Wait would
	// rewrite cmd.ProcessState to nil because its internal Process.Wait
	// re-call returns ECHILD). Callers read h.exitState via ExitState().
	// Codex bot P2 on f2dbea0: discarding the wait result drops exit
	// diagnostics and turns every exit into the same syscall error
	// shape. childExited stays unsignaled until Phase 4 (Codex bot P1
	// on 34b1a30: closing childExited ahead of pipe drain races with
	// response delivery — handlePOST's select could non-deterministically
	// pick the childExited branch and 502 a valid response that the
	// scanner is about to dispatch to respCh).
	//
	// Phase 2: bounded wait for scanners to drain the buffered output
	// the child wrote before exit. The bound only arms here, after
	// Phase 1 completes — so it cannot fire while the child is
	// healthy. h.done short-circuits this on Stop. The pipeDrainTimeout
	// is the upper bound on Codex Cloud finding 63b417d2 supervisor
	// restart latency for the inherited-stdio descendant case
	// (descendant holds parent's stdout/stderr fds open).
	//
	// Phase 3: close stdout/stderr read-ends manually. This unblocks
	// any scanners blocked on inherited-descendant pipes (read returns
	// "file already closed") quickly — without waiting for cmd.Wait's
	// internal Process.Wait re-call, which under heavy parallel load
	// on Windows has been observed to add seconds of latency to Stop.
	//
	// Phase 4: close(childExited) NOW — after scanners drained or the
	// bounded timeout fired. Any final response the child wrote before
	// exit has reached respCh by this point, so handlePOST's select
	// observes a ready respCh in the same iteration and prefers it
	// over childExited. Closing childExited here keeps Stop fast
	// because Stop's wait on h.childExited returns immediately.
	//
	// Phase 5: cmd.Wait() in a SEPARATE goroutine — REQUIRED so os/exec
	// releases its internal resources. exec.CommandContext's Start()
	// spawns a context-watcher goroutine that only exits when cmd.Wait
	// drains its result channel (Codex bot P1 on 0e903bd: never calling
	// cmd.Wait leaks that goroutine). Running it in a SEPARATE goroutine
	// after the watcher has already signaled childExited means the slow
	// cmd.Wait path (seconds under heavy parallel load on Windows due
	// to its internal Process.Wait re-call) does not extend Stop's wall
	// time. The ProcessState we want is already in h.exitState; pipes
	// are already closed; cmd.Wait's only remaining work is the watchCtx
	// drain plus an idempotent closeDescriptors on the parentIOPipes
	// we already closed manually.
	go func() {
		state, _ := cmd.Process.Wait()
		if state != nil {
			h.exitState.Store(state)
		}
		close(h.procExited)

		drained := make(chan struct{})
		go func() {
			pipesDrained.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-h.done:
		case <-time.After(pipeDrainTimeout):
			fmt.Fprintf(daemonDiagWriter(),
				"warn: stdout/stderr scanners did not reach EOF within %s after stdio child exited; a descendant likely inherited the pipes — proceeding so daemon liveness is reported\n",
				pipeDrainTimeout)
		}
		_ = stdout.Close()
		_ = stderr.Close()
		close(h.childExited)

		go func() {
			_ = cmd.Wait()
			// cmd.Wait's internal Process.Wait re-call returns ECHILD
			// because Phase 1 already reaped the zombie, so cmd.Wait
			// leaves cmd.ProcessState = nil. Under GODEBUG=execwait=1|2
			// Go's exec leak checker panics on nil ProcessState with
			// "Cmd started a Process but leaked without a call to Wait"
			// even though we DID call Wait — Codex Cloud bot P2 on
			// d49cffc (host.go:345) flagged this as a debug/CI hazard.
			// Restore cmd.ProcessState from h.exitState so the leak
			// checker sees a non-nil state when its finalizer runs.
			if state := h.exitState.Load(); state != nil {
				cmd.ProcessState = state
			}
		}()
	}()

	return nil
}

// ChildExited returns a channel that is closed when the subprocess exits
// for any reason. Callers may select on it to react to unexpected death
// (the outer daemon loop) or to confirm clean shutdown (Stop()).
func (h *StdioHost) ChildExited() <-chan struct{} {
	return h.childExited
}

// LogSupervisorEvent appends a structured "supervisor: <event>" line
// to the daemon's LogPath (if configured) AND mirrors to os.Stderr
// when mcphub's own stderr is a real terminal.
//
// Designed for callers in the cobra supervisor (internal/cli/daemon.go)
// to record Stop errors and other late-stage events with end-to-end
// durability — Codex CLI xhigh re-review on 479cbc3 (P2 #4): stderr
// alone is not durable under scheduled paths (Task Scheduler etc. can
// drop daemon stderr), and embedding the error in the cobra exit
// message routes it through the same os.Stderr channel via
// cmd.PrintErrln, providing zero additional durability. The rotated
// LogPath file IS durable because it is the same file `mcphub logs`
// reads from, the same file the scheduler does NOT need to redirect
// stderr to capture, and the same file rotated under the daemon's
// own retention.
//
// Re-opens the log file on each call rather than reusing h.logFile so
// it remains usable AFTER Stop has closed h.logFile (the supervisor
// callers run after Stop returns).
//
// Issue #162 closure: the stderr leg now goes through daemonDiagWriter,
// so the operator sees the event on an interactive console (TTY) but
// pipe/file stderr (scheduler-spawned daemon, OR mcphub running as a
// stdio child of an MCP client) gets io.Discard instead of corrupting
// the parent's inherited stdio with "warn: supervisor: ..." lines.
// The durable LogPath append is unchanged.
//
// Best effort: open / write errors are absorbed.
func (h *StdioHost) LogSupervisorEvent(event string) {
	fmt.Fprintf(daemonDiagWriter(), "warn: supervisor: %s\n", event)
	if h.cfg.LogPath == "" {
		return
	}
	f, err := os.OpenFile(h.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "[%s] supervisor: %s\n", time.Now().UTC().Format(time.RFC3339Nano), event)
}

// ExitState returns the subprocess's ProcessState after it has exited,
// or nil before exit. Mirrors HTTPHost.ExitState() so the supervisor
// can capture exit code / signal info for diagnostics. Codex bot P2
// on f2dbea0 — without this, callers that want to distinguish a
// controlled sys.exit from a native crash from a parent kill see only
// "subprocess died unexpectedly" with no further detail.
//
// Backed by an atomic.Pointer rather than cmd.ProcessState because the
// watcher's Phase 3 cmd.Wait rewrites cmd.ProcessState to nil during
// its ECHILD-returning internal Process.Wait re-call. Reading is safe
// concurrent with the writer in Phase 1.
func (h *StdioHost) ExitState() *os.ProcessState {
	return h.exitState.Load()
}

// Stop terminates the subprocess and closes all pipes.
//
// Stop does NOT call cmd.Process.Wait(); that call is owned exclusively by
// the watcher goroutine spawned in Start() so there is no double-Wait race.
// Instead, Stop signals Kill() (if the child is still alive) and then blocks
// on h.childExited to confirm the watcher saw the exit and closed the pipes.
//
// Returns a non-nil error if the subprocess did not exit within the 1s
// post-kill window. This communicates the leaked-daemon risk to callers
// (Codex bot P2 on 34b1a30) so they can log, alert, or escalate; previously
// Stop returned nil unconditionally and callers believed shutdown succeeded
// while the old child + watcher could still be alive.
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
		// Only tree-kill if the OS has not already exited the immediate
		// child. After cmd.Process.Wait() reaps the zombie (Phase 1), the
		// PID is eligible for reuse on POSIX, so issuing a fresh
		// PID-based taskkill/pkill could kill an unrelated process.
		// procExited (closed in Phase 1) — NOT childExited (closed in
		// Phase 4 after pipe drain) — is the correct gate: childExited
		// can stay open for up to pipeDrainTimeout after OS-exit when an
		// inherited-stdio descendant holds pipes, and during that window
		// the original PID may already have been reused.
		select {
		case <-h.procExited:
			// already exited; no kill needed
		default:
			// Tree-kill so wrappers (npx, uvx, uv, node launchers) and
			// their real child servers all go down together. Plain
			// Process.Kill only kills the wrapper; its child would keep
			// its stdin/stdout pipes open past the Wait-watcher close
			// and the port bound past Stop's return. POSIX uses SIGKILL
			// (not SIGTERM) so TERM-ignoring children cannot survive.
			//
			// Codex CLI xhigh re-review on 479cbc3 (P2): the outer
			// default-arm-then-kill shape is racy — procExited can
			// close BETWEEN the arm and the syscall, freeing the PID
			// for OS reuse, and we'd kill an unrelated reused PID.
			// Re-check inside the inner select to narrow the race
			// window from "any time" to "between the inner default
			// and the actual syscall" — microseconds rather than
			// scheduling-bounded.
			select {
			case <-h.procExited:
				// raced; just exited, skip kill
			default:
				_ = killProcessTree(h.cmd.Process.Pid)
			}
		}
	}
	// Wait for the watcher goroutine to close childExited (Phase 4 —
	// after pipe drain). Capped at 1s: h.done was just closed which
	// unblocks the watcher's pipe-drain select immediately, killProcessTree
	// terminates the child synchronously on both Windows (taskkill /F)
	// and POSIX (SIGKILL via the updated treekill.go), and Phase 4 only
	// needs the watcher's cmd.Wait reap to run. Returning an error on
	// timeout surfaces the leaked-daemon risk per Codex bot P2.
	var stopErr error
	select {
	case <-h.childExited:
	case <-time.After(1 * time.Second):
		pid := -1
		if h.cmd != nil && h.cmd.Process != nil {
			pid = h.cmd.Process.Pid
		}
		stopErr = fmt.Errorf("stdio host stop: subprocess did not exit within 1s after tree-kill (pid=%d) — child or watcher may still be alive, expect port-collision on restart", pid)
	}
	// Release the Job Object handle BEFORE waiting on reader goroutines.
	// On Windows, KILL_ON_JOB_CLOSE then terminates any descendants that
	// inherited stdout/stderr fds, which unblocks the readers and lets
	// h.wg.Wait() return. Doing this AFTER h.wg.Wait() created the
	// deadlock Codex finding 63b417d2 reported: a reparented descendant
	// holding the pipes open made the readers wait forever, and the job
	// object was never closed because we were stuck.
	if h.job != nil {
		_ = h.job.Close()
		h.job = nil
	}
	// Bound the reader-goroutine wait. POSIX has no equivalent of the
	// Windows Job Object kill-on-close path, so an inherited-stdio
	// descendant on Linux/macOS may still block reader EOF after the
	// watcher saw cmd.Wait() return. Cap is short — the outer daemon
	// will exit the host process anyway, and orphaned descendants are
	// reparented to PID 1 / launchd / systemd which will reap them on
	// session teardown.
	wgDone := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(wgDone)
	}()
	select {
	case <-wgDone:
	case <-time.After(1 * time.Second):
	}
	if h.logFile != nil {
		_ = h.logFile.Close()
	}
	return stopErr
}

// openStderrSink returns a writer for child stderr.
//
// Path matrix (issue #162 closure — every branch is non-TTY safe):
//
//	LogPath set    + TTY     → tee(rotatingFileWriter, os.Stderr)
//	LogPath set    + non-TTY → rotatingFileWriter only (log file is the durable record)
//	LogPath empty  + TTY     → os.Stderr (interactive debug — operator wants live output)
//	LogPath empty  + non-TTY → io.Discard (no log file, no terminal — bytes are dropped to
//	                            avoid corrupting the parent's inherited stdio)
//	mkdir/rotate/open error  → daemonDiagWriter() (file-or-discard) for the warn line +
//	                            io.Discard for child output (LogPath is broken; there is no
//	                            durable channel to surface the child to)
//
// The "non-TTY + LogPath empty" path was the residual leak after the 401885b commit gated
// only the happy path; the fallback paths in this function (mkdir fail, file-open fail)
// also leaked because they returned raw os.Stderr.
//
// The file leg is wrapped in a best-effort writer that rotates inline when the on-disk
// size grows past the cap and absorbs all rotation / write / open errors.
func (h *StdioHost) openStderrSink() io.Writer {
	if h.cfg.LogPath == "" {
		return daemonDiagWriter()
	}
	if err := os.MkdirAll(filepath_Dir(h.cfg.LogPath), 0o755); err != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: mkdir log dir: %v\n", err)
		// LogPath is broken — there is no durable sink for child output.
		// Returning daemonDiagWriter() (not raw os.Stderr) preserves the
		// non-TTY silence contract; on a real terminal the operator still
		// sees the subprocess for debug.
		return daemonDiagWriter()
	}
	if err := RotateIfLarge(h.cfg.LogPath, 10*1024*1024, 5); err != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: rotate log: %v\n", err)
	}
	f, err := os.OpenFile(h.cfg.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(daemonDiagWriter(), "warn: open log %q: %v\n", h.cfg.LogPath, err)
		return daemonDiagWriter()
	}
	rfw := &rotatingFileWriter{path: h.cfg.LogPath, file: f, maxBytes: 10 * 1024 * 1024, keep: 5}
	h.logFile = rfw
	// codex deep-sec PR #164 r2 P2: read the probe through the
	// mutex-honoring helper so concurrent SetStderrIsTerminalForTest
	// doesn't race with this hot-path check.
	if isStderrTerminal() {
		return io.MultiWriter(rfw, os.Stderr)
	}
	return rfw
}

// rotatingFileWriter writes to a log file, rotating it inline when its
// on-disk size grows past maxBytes. All errors (rotation, reopen, write)
// are absorbed and surfaced as a single warn line on os.Stderr so that
// the surrounding io.MultiWriter(rfw, os.Stderr) keeps the stderr leg
// alive when the file leg breaks.
//
// Write always returns (len(p), nil) — Codex-finding fix for PR #72.
type rotatingFileWriter struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	maxBytes    int64
	keep        int
	written     int64
	rotateBroke bool // log "leg disabled" once
}

func (r *rotatingFileWriter) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeRotateLocked()
	if r.file != nil {
		if n, werr := r.file.Write(p); werr != nil {
			r.disableLocked(fmt.Errorf("write log: %w", werr))
		} else {
			r.written += int64(n)
		}
	}
	return len(p), nil
}

func (r *rotatingFileWriter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	return err
}

func (r *rotatingFileWriter) maybeRotateLocked() {
	if r.file == nil || r.maxBytes <= 0 {
		return
	}
	if r.written < r.maxBytes {
		// Cheap path — no stat call until our write counter hints we
		// might be near the threshold.
		return
	}
	r.written = 0
	if err := r.file.Close(); err != nil {
		r.disableLocked(fmt.Errorf("close before rotate: %w", err))
		return
	}
	if err := RotateIfLarge(r.path, r.maxBytes, r.keep); err != nil {
		r.disableLocked(fmt.Errorf("rotate: %w", err))
		return
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		r.disableLocked(fmt.Errorf("reopen after rotate: %w", err))
		return
	}
	r.file = f
}

func (r *rotatingFileWriter) disableLocked(cause error) {
	if r.file != nil {
		_ = r.file.Close()
		r.file = nil
	}
	if !r.rotateBroke {
		fmt.Fprintf(daemonDiagWriter(), "warn: stdio log file leg disabled (%v); stderr forwarding continues\n", cause)
		r.rotateBroke = true
	}
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
		// Peek both id and method. The id alone is not enough to
		// classify a message as a stale response: JSON-RPC requests
		// (responses' opposite) carry an id AND a method, and MCP
		// servers can legitimately send server-initiated requests
		// (e.g. `sampling/createMessage`) with numeric ids. Codex
		// finding 6290ce6e: a method-blind id filter would suppress
		// those server-initiated requests as if they were stale
		// responses. Only the (id, no method) shape is unambiguously
		// a response and safe to drop.
		var peek struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
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
				if peek.Method == "" {
					// Untracked response id (e.g. a late reply
					// after caller timeout). Do not propagate
					// further; it may belong to a different
					// canceled client and could leak response
					// contents. testStdout-only retains
					// observability for unit tests.
					select {
					case h.testStdout <- line:
					default:
					}
					continue
				}
				// id + method = server-initiated request. Fall
				// through to the testStdout fallback so future
				// SSE work can pick it up uniformly with
				// notifications. SSE fan-out from this loop was
				// removed in PR #64 because unrouted output
				// cannot be attributed to a specific HTTP caller
				// safely; if/when SSE broadcast is reintroduced
				// (separately threat-modeled), it should
				// branch from here rather than from this filter.
			}
		}
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
		if rejectUnsafeLoopbackRequest(w, r) {
			return
		}
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
	// Content-Type gate runs FIRST — before body read, JSON parse, or
	// pending-slot reservation. A `text/plain` CSRF probe must not
	// consume the body parser, the unmarshal path, or a pending slot.
	// Codex finding: PR #62 placed this AFTER body parse (still
	// readable to CSRF as far as DoS goes); PR #123 used a prefix
	// match that admits `application/jsonx`.
	if !requireJSONContentType(r.Header.Get("Content-Type")) {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
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
	if len(h.pending) >= maxPendingRequests {
		h.pendingMu.Unlock()
		http.Error(w, "too many pending requests", http.StatusTooManyRequests)
		return
	}
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

	// Prefer a ready subprocess response over child-exit so a valid final
	// reply is not dropped when the child exits immediately after writing it.
	select {
	case respBody := <-respCh:
		if origMethod == "initialize" {
			h.bridge.CacheInitialize(respBody)
		}
		respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
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
		return
	default:
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
		// Codex CLI xhigh re-review on 479cbc3 (P2): a race-filled response
		// must beat the failure verdict — re-check respCh non-blockingly
		// before declaring shutdown, so a reply that arrived between the
		// peer-arm select and now is not lost.
		select {
		case respBody := <-respCh:
			respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
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
			return
		default:
		}
		http.Error(w, "host shutting down", http.StatusServiceUnavailable)
	case <-h.childExited:
		// Child died while we were waiting for its reply. Return 502 so the
		// client sees a distinct, immediate failure instead of blocking the
		// full stdioToolResponseTimeout. The outer daemon loop observes the same signal
		// and exits non-zero so the scheduler can restart the whole task.
		// Codex CLI xhigh re-review on 479cbc3 (P2): re-check respCh — a
		// reply that landed between the peer-arm select and here must win.
		select {
		case respBody := <-respCh:
			respBody = h.bridge.TransformResponse(origMethod, action.Active, respBody)
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
			return
		default:
		}
		http.Error(w, "subprocess died unexpectedly", http.StatusBadGateway)
	case <-r.Context().Done():
		http.Error(w, "client canceled", http.StatusRequestTimeout)
	case <-time.After(stdioToolResponseTimeout):
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
