// Package cli — Task 6.1 `mcphub supervise` cobra command body.
//
// Spec §"Q1 Lifecycle owner" + §"Q7 Supervisor invocation" + §"Package
// ownership" + plan Task 6.1.
//
// This file wires the Phase 1-5 primitives into one long-lived process
// owning every MCP daemon as a child under an OS-appropriate lifecycle
// primitive (Windows Job Object; Linux PR_SET_PDEATHSIG; macOS process
// group + kqueue). Phase 6 lays down the skeleton: singleton lock,
// durable audit log, FIFO event loop, IPC control plane, and a
// signal-driven graceful-exit handler. Reconciliation, IPC dispatch,
// and quiesce-drain wiring land in Tasks 6.2 / 7.x / 9.2.
//
// Per spec §"Package ownership", this file imports `internal/api`,
// `internal/process` (later), and `internal/scheduler` (later) — NOT
// `internal/gui`. The dependency direction protects the supervisor
// runtime from inheriting the GUI's window-message pump, single-
// instance pidport convention, or asset-embed compile cost.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
)

// reaperFn is a package-private test seam pointing at the Task 13.1
// cold-start stale-child reaper. Production wiring routes to
// ReapStaleTransients in supervise_reaper_posix.go (POSIX) or the
// no-op stub in supervise_reaper_windows.go (Windows). Tests in this
// package swap the function via newReaperFnOverride() to assert the
// reaper is invoked during startup without needing real /proc / kill
// scaffolding.
//
// The seam is package-private and exported only via the test-only
// setReaperFnForTest accessor below. Production callers MUST NOT
// reassign this var.
var reaperFn = ReapStaleTransients

// setReaperFnForTest installs a test reaper function. Returns an
// "uninstall" function tests defer to restore the production wiring
// before the next test runs. Production code paths never invoke this
// — only the supervise_test.go file in this package does.
func setReaperFnForTest(fn func(context.Context, ReaperDeps) (ReaperResult, error)) func() {
	prev := reaperFn
	reaperFn = fn
	return func() { reaperFn = prev }
}

// stateDirFunc returns the supervisor state directory.
//
// Production lookup: `api.DaemonStateDir()` — the same Known-Folder /
// XDG-resolved root every other state-bearing component uses (plan
// §15-16). Tests can set `MCPHUB_STATE_DIR_OVERRIDE` to bypass the
// platform resolver and direct supervisor state into `t.TempDir()`.
//
// The env-var override lives at the CLI layer instead of inside
// `api.DaemonStateDir()` because production builds compile WITHOUT
// the `test_state_path_env` tag (plan §16 v9: env fallback is
// excluded from shipped binaries via build-tag gating). Putting the
// supervise-side test seam here keeps the production `api` surface
// free of test-only env-var resolution while still letting fast
// integration tests redirect `<state-dir>` without spinning up a
// real Known-Folder probe.
var stateDirFunc = func() (string, error) {
	if override := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); override != "" {
		return override, nil
	}
	return api.DaemonStateDir()
}

// superviseTestExitCh is a package-level test seam: when non-nil, the
// supervise body selects on this channel in parallel with the real
// signal channel. Receipt of any value on the test channel triggers
// the same graceful-exit flow a real SIGINT would have. The seam
// exists because `os.Process.Signal(os.Interrupt)` is documented as
// unsupported on Windows for self-signaling (Go src/os/exec_windows.go);
// the seam lets cross-platform tests exercise the exit path without
// relying on a working self-signal primitive. Production callers
// MUST NOT set this — it is package-private and exported only via
// the test-only setSuperviseTestExitCh accessor below.
var superviseTestExitCh chan struct{}

// setSuperviseTestExitCh installs a test exit channel. Returns a
// "uninstall" function tests defer to clear the seam before the next
// test runs. Production code paths never invoke this — only the
// supervise_test.go file in this package does.
func setSuperviseTestExitCh(ch chan struct{}) func() {
	superviseTestExitCh = ch
	return func() { superviseTestExitCh = nil }
}

// newSuperviseCmd returns the `mcphub supervise` cobra command.
//
// Wires together (per plan Task 6.1):
//   - api.AcquireSupervisorLock  (Task 2.4)  — singleton enforcement.
//   - api.OpenSupervisorEventLog (Task 2.3)  — durable audit channel.
//   - api.NewEventLoop           (Task 4.2)  — FIFO event loop.
//   - cli.NewSupervisorIPCListener (Task 5.1 Windows / 5.2 POSIX)
//                                            — control IPC plane.
//   - signal handler             (this task) — SIGINT/SIGTERM →
//     graceful_exit_in_progress=true → loop cancel → unlock → exit 0.
//
// Phase-6 scope intentionally stops at the signal-driven cancel +
// release: reconcile-on-tick, IPC dispatch, and quiesce-drain wiring
// land in subsequent tasks (6.2, 7.x, 9.2). The handlers registered
// here are no-op audit-only stubs so the supervisor binary is testable
// end-to-end at this phase without depending on those later layers.
func newSuperviseCmd() *cobra.Command {
	var (
		noIPC      bool
		strictMode bool
	)
	cmd := &cobra.Command{
		Use:   "supervise",
		Short: "Run the v0.5.0 supervisor process (long-lived parent owning all MCP daemons)",
		Long: `mcphub supervise runs the long-lived supervisor process that owns
every MCP daemon as a child under an OS-appropriate lifecycle primitive
(Windows Job Object; Linux PR_SET_PDEATHSIG; macOS process group +
kqueue).

The command is idempotent: it refuses to start if another supervisor
instance already holds <state-dir>/supervisor.lock. Use --strict-mode
to opt into the strict parent-dir DACL gate per spec §Q8.

Signals: SIGINT / SIGTERM trigger the graceful-exit flow — set
graceful_exit_in_progress=true, cancel the event-loop context,
release the singleton lock, and exit 0. Quiesce-drain + transient-PID
force-termination land in Task 9.2.
`,
		// Test-only flag scoped to the no-ipc test path. Production
		// invocations either pass --no-ipc=false (default) or omit it.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSupervise(cmd.Context(), noIPC, strictMode)
		},
	}
	cmd.Flags().BoolVar(&noIPC, "no-ipc", false, "skip IPC listener (test flag)")
	cmd.Flags().BoolVar(&strictMode, "strict-mode", false, "enforce strict parent-dir DACL gate")
	return cmd
}

// ipcDispatchDeps bundles the per-supervisor context the IPC handlers
// need beyond the per-request envelope. It is constructed once in
// runSupervise and passed into the accept-loop goroutine so each
// accepted connection's handler can drive the broader supervisor state
// (loop cancellation, graceful-exit flag, state-dir lookup for the
// quiesce handler).
//
// The struct exists because the v0.5.0 IPC dispatcher must service
// commands (quiesce-timers, exit{graceful}) that depend on more than
// a pair of atomic.Bool flags. Passing six bare pointers to
// handleIPCConn would obscure the contract; the bundle keeps the
// dependency surface visible.
type ipcDispatchDeps struct {
	stateDir           string
	events             *api.SupervisorEventLog
	reconcileReady     *atomic.Bool
	intentFilesLoaded  *atomic.Bool
	gracefulInProgress *atomic.Bool
	triggerGracefulExit func() // closes loop, signals exit
}

// runSupervise is the body shared between the cobra command and any
// future programmatic supervisor entry-point (e.g. when the GUI hosts
// a supervisor in-process for the dev-mode `mcphub gui --supervise`
// preview). Splitting RunE from the body keeps test setup small.
//
// The function is structured so each acquired resource is released
// via a deferred call BEFORE the next resource is acquired, so a
// mid-startup failure never leaks the supervisor.lock or IPC handle.
//
// Phase-6 scope: lock → audit log → event loop → IPC listener → wait
// for signal → cancel loop → return. Reconciliation, IPC dispatch,
// and quiesce-drain are wired in later tasks.
func runSupervise(ctx context.Context, noIPC bool, strictMode bool) error {
	if ctx == nil {
		ctx = context.Background()
	}

	stateDir, err := stateDirFunc()
	if err != nil {
		return fmt.Errorf("resolve supervisor state dir: %w", err)
	}

	// Acquire singleton lock — fails fast if another supervisor holds
	// it. The lock+sidecar is the FIRST resource taken so concurrent
	// supervisors never race to open the audit log or bind the IPC
	// listener (both produce noisy "already in use" errors that mask
	// the real "another supervisor is running" condition).
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	lk, err := api.AcquireSupervisorLock(lockPath)
	if err != nil {
		return fmt.Errorf("acquire supervisor.lock: %w", err)
	}
	defer lk.Release()

	// Open audit log. The log handle is process-lifetime; per-Emit
	// flock+mutex serialization happens inside the helper. Close is a
	// no-op today (see supervisor_events.go:219) but kept on the
	// defer chain so future implementations that hold a long-lived
	// fd can add cleanup without breaking callers.
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		return fmt.Errorf("open supervisor-events.log: %w", err)
	}
	defer func() { _ = events.Close() }()

	// Emit start-of-life self-event. Failure here is logged via the
	// emit-error return but does not abort startup — a missing audit
	// row is preferable to a supervisor that refuses to come up because
	// the log file's parent dir hit an antivirus reading lock.
	_ = events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "supervisor-start",
		Body: map[string]any{
			"pid":         os.Getpid(),
			"strict_mode": strictMode,
			"no_ipc":      noIPC,
			"state_dir":   stateDir,
		},
	})

	// Task 13.1 cold-start stale-child reap. POSIX-only in practice:
	// the Windows variant is a no-op stub because the Job Object's
	// KILL_ON_JOB_CLOSE handler already terminates the prior
	// supervisor's children before this function runs. On POSIX a
	// prior crash leaves transient_pids[] entries pointing at orphans
	// reparented to PID 1; the reaper walks the list, applies the
	// 3-gate ownership check (image basename + cmdline tokens + UID),
	// kill(-pgid, SIGKILL)'s the survivors, settles 2s for the kernel
	// to release listening ports, and clears transient_pids[].
	//
	// Best-effort: a failed read/classify/write never blocks supervisor
	// startup — a missing reap is far better than a stuck supervisor.
	// The skip event captures the wrapped error so operators can
	// diagnose post-mortem; reconcileReady proceeds regardless.
	//
	// Order: this runs BEFORE the IPC listener binds and BEFORE the
	// loop is started so the kill burst + settle complete before any
	// client sees `reconcile_ready=true` and before the first
	// reconcile pass would race the kernel's port-release on the
	// just-killed orphan listeners (spec §"Fallback if step 4 IPC
	// fails" + plan §2660).
	reaperRes, reaperErr := reaperFn(ctx, ReaperDeps{StateDir: stateDir})
	if reaperErr != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "warn",
			Source:   "lifecycle",
			Event:    "cold-start-reap-failed",
			Body: map[string]any{
				"err": reaperErr.Error(),
			},
		})
	} else {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "cold-start-reap-complete",
			Body: map[string]any{
				"killed_count":       len(reaperRes.KilledPIDs),
				"skipped_count":      len(reaperRes.SkippedPIDs),
				"dead_count":         len(reaperRes.DeadPIDs),
				"cleared_transients": reaperRes.ClearedTransients,
				"settle_ms":          reaperRes.SettleDuration.Milliseconds(),
			},
		})
	}

	// FIFO event loop. Capacity 64 absorbs quiesce-complete posts from
	// the side-goroutine drain handler without rendezvous deadlock
	// (spec Q4 v13). Phase-6 registers an audit-only handler so loop
	// events are observable in the log; Task 7.1+ wires the
	// reconciler.
	loop := api.NewEventLoop(64)
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()

	// gracefulInProgress mirrors `supervisor-state.graceful_exit_in_progress`
	// from spec Q4 v13. Side-goroutine drain handlers (Task 5.2
	// quiesce, Task 9.2 graceful exit) read this flag to suppress
	// new transient-timer fires while drain is active. Phase-6 sets
	// it once on signal receipt; later tasks will check it from
	// the FIFO event-loop handlers.
	var gracefulInProgress atomic.Bool

	loop.RegisterHandler(func(e api.LoopEvent) {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "debug",
			Source:   "lifecycle",
			Event:    "loop-event",
			TaskName: e.TaskName,
			Body: map[string]any{
				"kind":                       string(e.Kind),
				"graceful_exit_in_progress":  gracefulInProgress.Load(),
			},
		})
	})

	go loop.Run(loopCtx)

	// reconcileReady mirrors the spec §"Migration step 14:
	// reconcile-ready not all-daemons-healthy" flag. Migration / upgrade
	// callers wait for `status.result.reconcile_ready == true` (NOT
	// all-daemons-healthy) before rolling forward. The flag transitions
	// false → true exactly once, after BOTH:
	//   (a) the supervisor has attempted to read its two intent files,
	//   (b) the first reconcile pass has been scheduled.
	// Task 6.2 stubs (b) — the real reconcile loop body lands in
	// Task 7.1; here we mark ready immediately after (a) so the
	// IPC `status` contract is testable end-to-end at this phase.
	//
	// intentFilesLoaded is the inner observability flag (spec §"Wire
	// format" status result), useful when a migrate-side watcher wants
	// to distinguish "intent read attempted, file may be empty" from
	// "still starting up". It flips true after the attempted reads
	// even when the underlying files are missing — `os.ErrNotExist` is
	// a valid "loaded" outcome.
	var reconcileReady atomic.Bool
	var intentFilesLoaded atomic.Bool

	// ipcExitCh is an internal channel the IPC dispatcher uses to drive
	// graceful exit when the supervisor receives `exit{graceful: true}`.
	// Distinct from superviseTestExitCh (test seam) and the OS signal
	// channel; the select-block at the bottom of runSupervise observes
	// all three uniformly.
	ipcExitCh := make(chan struct{}, 1)

	// IPC listener. `--no-ipc` keeps tests fast (lock+events+loop only).
	// Production callers always pass --no-ipc=false; the listener owns
	// the per-OS pipe/socket and the hello-frame handshake from Task
	// 5.1 / 5.2. Phase-6 / Task 6.2 dispatched only `status`; codex
	// round-3 Lane B P1 #1 extends the cmd-switch with `quiesce-timers`
	// (multi-frame) and `exit{graceful: true}` so the migration
	// rollback path + v0.5 upgrade flow no longer fall through to the
	// force-kill fallback on every invocation.
	if !noIPC {
		pipePath := defaultPipePath(stateDir)
		listener, lerr := NewSupervisorIPCListener(pipePath)
		if lerr != nil {
			return fmt.Errorf("ipc listener: %w", lerr)
		}
		defer func() { _ = listener.Close() }()
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "ipc",
			Event:    "ipc-listener-bound",
			Body: map[string]any{
				"path": pipePath,
			},
		})

		deps := ipcDispatchDeps{
			stateDir:           stateDir,
			events:             events,
			reconcileReady:     &reconcileReady,
			intentFilesLoaded:  &intentFilesLoaded,
			gracefulInProgress: &gracefulInProgress,
			triggerGracefulExit: func() {
				// Non-blocking: idempotent against repeated exit verbs.
				select {
				case ipcExitCh <- struct{}{}:
				default:
				}
			},
		}

		// Spawn the accept goroutine. It runs until listener.Close()
		// causes Accept to return an error (the deferred Close above
		// is the exit signal during the graceful-exit flow). Each
		// accepted connection gets its own handler goroutine so a slow
		// client never blocks the next Accept.
		go acceptIPCConnections(listener, deps)
	}

	// Read the two intent files (Task 7.1 will turn these into the
	// authoritative reconcile inputs). For Task 6.2 scope, the read
	// result is intentionally discarded — the goal is only to flip
	// `intentFilesLoaded` true after the attempt so the IPC `status`
	// contract is observable. Missing files (os.ErrNotExist) are a
	// valid "loaded" outcome — the supervisor must come up cleanly
	// on a fresh host before any intent file exists.
	loadIntentFiles(stateDir, events, &intentFilesLoaded)

	// Mark reconcile-ready. Phase-6 scope stops at the first-pass
	// scheduled flag; Task 7.1 replaces this with a real reconcile
	// loop tick that drives reconcileReady after the diff-and-apply
	// pass completes. Emit a dedicated lifecycle event so migration
	// watchers polling the audit log have a positive ready-marker in
	// addition to the IPC status flag.
	reconcileReady.Store(true)
	_ = events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "reconcile-ready",
		Body: map[string]any{
			"intent_files_loaded": intentFilesLoaded.Load(),
		},
	})

	// Signal handler. SIGINT and SIGTERM both trigger the graceful-exit
	// flow. On Windows `os.Interrupt` IS the canonical Ctrl+C / SIGTERM
	// surface (Windows has no real SIGINT/SIGTERM — Go maps both to
	// os.Interrupt internally). The two-name signal.Notify call is
	// portable: on Windows the syscall.SIGTERM entry is a no-op, on
	// POSIX both are delivered.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	// Snapshot the test exit-channel under a local nil-safe variable.
	// If the seam isn't installed (production), the channel stays nil
	// and `select` ignores that case forever — same shape as a
	// disabled signal listener.
	testExitCh := superviseTestExitCh

	select {
	case sig := <-sigCh:
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit-signal",
			Body: map[string]any{
				"signal": sig.String(),
			},
		})
		// Set the graceful flag BEFORE cancelling the loop so any
		// final-tick handler still in flight observes the active
		// graceful-exit state and can suppress new transient-timer
		// fires per spec §"Graceful exit + quiesce drain" step 1.
		gracefulInProgress.Store(true)
		loopCancel()
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit",
			Body: map[string]any{
				"exit_code": 0,
			},
		})
		return nil
	case <-testExitCh:
		// Test-only seam: same semantics as a real signal, but
		// observable via a channel send instead of os.Interrupt
		// delivery. Records signal="test-exit" so the audit row
		// is distinguishable from a real production exit.
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit-signal",
			Body: map[string]any{
				"signal": "test-exit",
			},
		})
		gracefulInProgress.Store(true)
		loopCancel()
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit",
			Body: map[string]any{
				"exit_code": 0,
			},
		})
		return nil
	case <-ipcExitCh:
		// IPC-driven graceful exit: a client issued `exit{graceful:
		// true}` and the dispatcher has already sent its response
		// frame. Mirror the signal-driven exit path: emit the
		// signal+exit lifecycle pair so the audit log is uniform
		// regardless of trigger, set the graceful flag, cancel the
		// loop, return nil.
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit-signal",
			Body: map[string]any{
				"signal": "ipc-exit-graceful",
			},
		})
		gracefulInProgress.Store(true)
		loopCancel()
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit",
			Body: map[string]any{
				"exit_code": 0,
			},
		})
		return nil
	case <-ctx.Done():
		// Parent ctx cancellation (e.g. cmd.Context() cancelled by
		// the root cobra harness during a wider shutdown). Treat as
		// a graceful exit so the audit channel records the cause.
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "supervisor-exit-ctx",
			Body: map[string]any{
				"err": ctx.Err().Error(),
			},
		})
		gracefulInProgress.Store(true)
		loopCancel()
		return ctx.Err()
	}
}

// defaultPipePath returns the per-OS IPC pipe (Windows) or socket
// (POSIX) path. The state-dir argument is used on POSIX (socket is a
// file under the state dir); Windows ignores it since the pipe lives
// in the kernel namespace `\\.\pipe\...`.
//
// Implementation lives in supervise_pipe_windows.go /
// supervise_pipe_posix.go to keep the platform fan-out next to the
// matching listener variant.
func defaultPipePath(stateDir string) string {
	return defaultPipePathOS(stateDir)
}

// acceptIPCConnections is the listener accept loop. Runs in its own
// goroutine spawned from runSupervise; exits when listener.Close()
// (deferred in runSupervise) causes Accept to return an error.
//
// Per spec §"Wire format": each accepted connection is a long-lived
// newline-delimited-JSON channel. Multiple connections from the same
// client (`mcphub status`, `mcphub stop`, `mcphub migrate`) are
// supported concurrently — each gets its own per-connection handler
// goroutine. The handlers share access to the supervisor-wide deps
// via ipcDispatchDeps; no per-connection state is mutated by
// handleIPCRequest.
func acceptIPCConnections(
	listener *SupervisorIPCListener,
	deps ipcDispatchDeps,
) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			// listener.Close() returns net.ErrClosed via Accept on
			// graceful exit. Logging this would noise up the audit
			// channel; emit an info row only so operators can
			// distinguish "listener closed normally" from a bug.
			_ = deps.events.Emit(api.SupervisorEvent{
				Severity: "info",
				Source:   "ipc",
				Event:    "ipc-accept-exit",
				Body: map[string]any{
					"err": err.Error(),
				},
			})
			return
		}
		go handleIPCConn(conn, deps)
	}
}

// handleIPCConn is the per-connection request loop. Reads
// newline-delimited JSON IPCRequest frames, dispatches via
// dispatchIPCRequest, writes the response frames back. Exits on first
// read error (client closed, deadline exceeded, malformed framing).
//
// Malformed JSON lines are skipped silently rather than closing the
// connection — a future client version sending an unknown field
// shouldn't tear down the long-lived channel. Errors at the dispatch
// layer (unknown cmd, unsupported version) are surfaced via the
// IPCResponse.Error envelope, not via connection close.
//
// Audit: each request gets one `ipc-command` audit row capturing the
// cmd + id. The response body is NOT logged (may contain operator-
// visible state that doesn't belong in the long-lived audit channel);
// just the verb + correlation id is.
func handleIPCConn(conn net.Conn, deps ipcDispatchDeps) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var req api.IPCRequest
		if err := json.Unmarshal([]byte(trimmed), &req); err != nil {
			// Malformed frame — skip, do NOT close. A noisy client
			// would otherwise tear down the long-lived channel on
			// every transient JSON glitch.
			_ = deps.events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "ipc",
				Event:    "ipc-malformed-request",
				Body: map[string]any{
					"err": err.Error(),
				},
			})
			continue
		}
		_ = deps.events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "ipc",
			Event:    "ipc-command",
			Body: map[string]any{
				"cmd": req.Cmd,
				"id":  req.ID,
			},
		})
		if err := dispatchIPCRequest(conn, req, deps); err != nil {
			// Write failure on the response side — tear down the
			// connection. Other dispatch errors are surfaced inside
			// the response envelope and don't reach here.
			return
		}
	}
}

// dispatchIPCRequest dispatches one IPC request to the matching command
// handler and writes its response frame(s) to conn. Returns a non-nil
// error only on connection-write failure, in which case the caller
// closes the connection. Per-command errors (unknown verb, unsupported
// version) are surfaced via the IPCResponse.Error envelope written to
// the connection, NOT returned here.
//
// codex round-3 Lane B P1 #1: extends the dispatcher beyond the
// Phase-6 `status`-only stub with `quiesce-timers` (multi-frame
// response per spec §"Wire format" lines 498-501) and `exit{graceful:
// true}` (single response frame, supervisor exits after sending).
// `restart` and `reload` remain deferred to follow-up — they're
// per-daemon operations and the spec defers them past this round.
func dispatchIPCRequest(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	// Version pinning: refuse requests carrying an explicit non-1
	// envelope version. Zero (the JSON-omitted default) is treated as
	// v1 for backward compatibility with clients that predate the
	// explicit-version requirement.
	if err := api.ValidateRequestEnvelope(req); err != nil {
		var ipcErr *api.IPCErr
		if errors.As(err, &ipcErr) {
			return writeIPCFrame(conn, api.IPCResponse{
				ID:    req.ID,
				Error: ipcErr,
				Final: true,
			})
		}
		// Defensive: unknown error shape — still surface a structured
		// response so the client doesn't hang.
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "UNSUPPORTED_PROTOCOL_VERSION",
				Message: err.Error(),
			},
			Final: true,
		})
	}
	switch req.Cmd {
	case "status":
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"state":               "running",
				"daemons":             []any{},
				"reconcile_ready":     deps.reconcileReady.Load(),
				"intent_files_loaded": deps.intentFilesLoaded.Load(),
			},
		})
	case "quiesce-timers":
		return handleQuiesceTimers(conn, req, deps)
	case "exit":
		return handleExit(conn, req, deps)
	case "restart", "reload":
		// Deferred to follow-up: per-daemon operations whose semantics
		// (which daemon, with which intent diff, against which
		// reconcile pass) depend on the Task 7.1 reconcile loop. Until
		// that lands, return UNKNOWN_COMMAND so callers fail fast
		// instead of hanging on a missing response.
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "UNKNOWN_COMMAND",
				Message: "IPC command deferred to follow-up: " + req.Cmd,
			},
			Final: true,
		})
	default:
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "UNKNOWN_COMMAND",
				Message: "unknown IPC command: " + req.Cmd,
			},
			Final: true,
		})
	}
}

// quiesceDrainer is the minimal interface handleQuiesceTimers needs
// from a QuiesceHandler. Defined as an interface so tests can inject
// a fake without spawning real child processes. Production wires
// the real *QuiesceHandler (which satisfies this interface) via
// quiesceHandlerFactory.
type quiesceDrainer interface {
	Drain(ctx context.Context, timeoutMs int) QuiesceResult
}

// quiesceHandlerFactory builds a quiesceDrainer from the on-disk
// supervisor-state.json under the supervisor's state-dir. Package-level
// indirection so tests can inject a fake without driving the real
// state-file load path; production wires it to the real
// NewQuiesceHandler against `<state-dir>/supervisor-state.json`.
//
// A missing state file or unreadable JSON is NOT a hard error — the
// handler is built against an empty SupervisorStateFile so Drain
// immediately reports (drained=0, still_running=[]). This matches
// the spec contract that a freshly-installed supervisor with no
// running transients should still answer quiesce-timers successfully.
var quiesceHandlerFactory = func(stateDir string) quiesceDrainer {
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	state, err := api.ReadSupervisorState(statePath)
	if err != nil || state == nil {
		// Missing / unreadable state → empty handler. NewQuiesceHandler
		// already handles nil state via its defensive guards
		// (Drain returns drained=0).
		return NewQuiesceHandler(nil, statePath)
	}
	return NewQuiesceHandler(state, statePath)
}

// setQuiesceHandlerFactoryForTest installs a test factory. Returns an
// uninstall function tests defer to restore the production wiring.
// Production code paths never invoke this.
func setQuiesceHandlerFactoryForTest(fn func(stateDir string) quiesceDrainer) func() {
	prev := quiesceHandlerFactory
	quiesceHandlerFactory = fn
	return func() { quiesceHandlerFactory = prev }
}

// handleQuiesceTimers implements the multi-frame response shape for
// the `quiesce-timers` IPC verb (spec §"Wire format" lines 498-501).
//
// Wire contract:
//
//	Request:  {"id": N, "cmd": "quiesce-timers", "args": {"timeout_ms": 30000}}
//	Frame 1 (immediate): {"id": N, "ok": true, "result": {"accepted": true}}
//	Frame 2 (final):     {"id": N, "ok": true, "result": {"drained": K,
//	                       "still_running": [...]}, "final": true}
//
// The drain runs on a separate goroutine per spec line 472 so the
// supervisor's FIFO event loop can continue processing while drain is
// in progress. The `graceful_exit_in_progress` flag is set for the
// duration of the drain so the loop's per-timer fire path suppresses
// new transient-timer fires (spec §"Graceful exit + quiesce drain"
// step 1).
//
// timeout_ms parsing: accepts JSON-number `float64` (the default
// shape for `map[string]any` unmarshal) AND `int` (defensive against
// custom marshalers). Missing/zero/negative timeout_ms defaults to
// 30000 ms.
func handleQuiesceTimers(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	timeoutMs := parseTimeoutMs(req.Args, "timeout_ms", 30000)

	// Frame 1: immediate acceptance ack.
	if err := writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"accepted": true,
		},
	}); err != nil {
		return err
	}

	// Run drain on a separate goroutine so the loop continues
	// processing. We block this connection handler waiting for the
	// drain result; that is intentional — the client expects the
	// final frame on the SAME connection.
	deps.gracefulInProgress.Store(true)
	handler := quiesceHandlerFactory(deps.stateDir)
	resultCh := make(chan QuiesceResult, 1)
	go func() {
		defer deps.gracefulInProgress.Store(false)
		// 5s grace window beyond the requested deadline so the drain
		// goroutine's ctx is honored even when the caller is sloppy.
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(timeoutMs+5000)*time.Millisecond)
		defer cancel()
		resultCh <- handler.Drain(ctx, timeoutMs)
	}()
	result := <-resultCh

	_ = deps.events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "quiesce-timers-complete",
		Body: map[string]any{
			"id":            req.ID,
			"timeout_ms":    timeoutMs,
			"drained":       result.Drained,
			"still_running": len(result.StillRunning),
		},
	})

	// Frame 2: final result with drained + still_running.
	stillRunning := make([]any, 0, len(result.StillRunning))
	for _, pid := range result.StillRunning {
		stillRunning = append(stillRunning, map[string]any{"pid": pid})
	}
	return writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"drained":       result.Drained,
			"still_running": stillRunning,
		},
		Final: true,
	})
}

// handleExit implements the `exit` IPC verb (single-frame response).
// When args.graceful is true, the dispatcher posts a graceful-exit
// trigger AFTER writing the response so the client observes the
// acknowledgement before the supervisor tears down. The state-machine
// transition itself (spec §"Restart policy state machine"
// request-graceful-exit) is driven by the loop's existing exit path
// — see the ipcExitCh case in runSupervise.
//
// Wire contract:
//
//	Request:  {"id": N, "cmd": "exit", "args": {"graceful": true, "timeout_ms": 5000}}
//	Response: {"id": N, "ok": true, "result": {"graceful_exit_initiated": true}, "final": true}
//
// graceful=false is currently unsupported (would be an ungraceful
// immediate exit, equivalent to a kill). Returning UNKNOWN_COMMAND
// here defers that semantic to a follow-up: the rollback path always
// requests graceful=true, and there is no current caller for
// graceful=false.
func handleExit(conn net.Conn, req api.IPCRequest, deps ipcDispatchDeps) error {
	graceful, _ := req.Args["graceful"].(bool)
	if !graceful {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:    "UNKNOWN_COMMAND",
				Message: "exit{graceful:false} not supported; pass graceful:true",
			},
			Final: true,
		})
	}

	_ = deps.events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "exit-graceful-requested",
		Body: map[string]any{
			"id": req.ID,
		},
	})

	// Write the response BEFORE triggering exit. If we triggered first,
	// the supervisor's deferred listener.Close() could race the response
	// write and the client would observe a connection drop instead of
	// the acknowledgement frame.
	if err := writeIPCFrame(conn, api.IPCResponse{
		ID: req.ID,
		OK: true,
		Result: map[string]any{
			"graceful_exit_initiated": true,
		},
		Final: true,
	}); err != nil {
		return err
	}

	// Trigger the graceful-exit path. Idempotent: repeated exit verbs
	// just push to a buffered channel; the select in runSupervise
	// observes the first delivery and starts tearing down.
	if deps.triggerGracefulExit != nil {
		deps.triggerGracefulExit()
	}
	return nil
}

// parseTimeoutMs reads a "*_ms" int field from req.Args, accepting
// both JSON-number (float64) and int shapes. Returns def when the key
// is missing, of the wrong type, or non-positive.
func parseTimeoutMs(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	}
	return def
}

// writeIPCFrame marshals resp + appends the trailing newline and
// writes it to conn. Returns the connection-write error so the caller
// can decide whether to tear down the connection (write failures
// typically indicate the client closed prematurely).
func writeIPCFrame(conn net.Conn, resp api.IPCResponse) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}
	body = append(body, '\n')
	if _, err := conn.Write(body); err != nil {
		return err
	}
	return nil
}

// loadIntentFiles attempts to read the supervisor-intent.json and
// daemon-intent.json files under stateDir. Both reads are best-
// effort — Task 7.1 will surface the parsed contents to the
// reconcile loop; here the side effect is flipping intentFilesLoaded
// to true so the IPC `status` flag transitions out of startup state.
//
// File-not-exist is a valid "loaded" outcome: a freshly installed
// supervisor on a clean host has no intent files, and the empty
// reconcile pass that follows will detect zero daemons / zero
// timers in that state.
//
// Errors (parse failure, I/O denied) are logged via the event log
// but do NOT prevent the ready transition — a corrupt intent file
// must not block the supervisor from coming up; the audit row is
// enough for an operator to diagnose. Task 7.1 wires the actual
// fail-closed reconcile policy for genuinely-unreadable intent.
func loadIntentFiles(
	stateDir string,
	events *api.SupervisorEventLog,
	intentFilesLoaded *atomic.Bool,
) {
	supervisorIntentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if _, err := api.ReadSupervisorIntent(supervisorIntentPath); err != nil {
		// Only log non-NotExist errors — a missing file on a fresh
		// host is the expected first-boot shape.
		if !os.IsNotExist(err) {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "supervisor-intent-read-failed",
				Body: map[string]any{
					"path": supervisorIntentPath,
					"err":  err.Error(),
				},
			})
		}
	}

	// daemon-intent.json: best-effort os.ReadFile probe under the
	// override-aware stateDir. The full ReadDaemonIntent flow lives
	// on *api.API and resolves DaemonStateDir() internally — that
	// path doesn't honor the supervise CLI's MCPHUB_STATE_DIR_OVERRIDE
	// seam, so we read directly here for the Task 6.2 stub. Task 7.1
	// will replace this with the canonical flock+quarantine flow once
	// the production path-injection variant lands.
	daemonIntentPath := filepath.Join(stateDir, "daemon-intent.json")
	if _, err := os.ReadFile(daemonIntentPath); err != nil {
		if !os.IsNotExist(err) {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "daemon-intent-read-failed",
				Body: map[string]any{
					"path": daemonIntentPath,
					"err":  err.Error(),
				},
			})
		}
	}

	intentFilesLoaded.Store(true)
}
