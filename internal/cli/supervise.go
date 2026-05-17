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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
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

// reconcileSpawnFn / reconcileTerminateFn are package-private test
// seams pointing at the production reconcile fan-out closures
// installed by runSupervise. They are nil at package load; runSupervise
// assigns the production closures during startup unless a test has
// already swapped a fake in via setReconcileSpawnFnForTest /
// setReconcileTerminateFnForTest. Production callers MUST NOT reassign
// these vars directly; the accessors below are the only allowed write
// path.
//
// The seams exist because the v0.5.0 reconcile pass (Task 7.1) needs
// to actually spawn daemon children via os/exec + Job-Object assign-
// at-create, and the wiring tests in this package must capture those
// fan-out calls without launching real child processes. The same
// pattern reaperFn / setReaperFnForTest uses (supervise.go:52-62) is
// repeated here for parity.
var (
	reconcileSpawnFn     SpawnFunc
	reconcileTerminateFn TerminateFunc
)

// setReconcileSpawnFnForTest installs a test spawn closure. Returns
// an "uninstall" function tests defer to restore the production
// wiring (nil — runSupervise re-installs its own closure on next
// startup) before the next test runs. Production code paths never
// invoke this accessor.
func setReconcileSpawnFnForTest(fn SpawnFunc) func() {
	prev := reconcileSpawnFn
	reconcileSpawnFn = fn
	return func() { reconcileSpawnFn = prev }
}

// setReconcileTerminateFnForTest is the terminate-side companion of
// setReconcileSpawnFnForTest. Currently unused by tests because the
// startup-only reconcile scope never terminates daemons (no daemon
// has been spawned yet), but the seam is in place so the watcher /
// reload follow-up can swap a fake without further refactoring.
func setReconcileTerminateFnForTest(fn TerminateFunc) func() {
	prev := reconcileTerminateFn
	reconcileTerminateFn = fn
	return func() { reconcileTerminateFn = prev }
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
//     — control IPC plane.
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

// gracefulCounter replaces the historical gracefulInProgress
// atomic.Bool to fix codex-r4-b-p2: two concurrent quiesce-timers
// requests sharing one boolean would have the first goroutine's
// `defer Store(false)` clear the flag while the second drain was
// still running. Lifecycle handlers + transient-timer suppression
// then observed `InProgress() == false` even though a drain was
// still active.
//
// The counter holds an atomic.Int32 incremented by Enter and
// decremented by Exit. InProgress returns true iff the counter is
// strictly positive — i.e. at least one quiesce-drain or supervisor-
// exit handler is still inside its protected region.
//
// Supervisor-exit handlers call Enter without a matching Exit
// (process is about to terminate); the resulting positive counter is
// the "permanent" graceful-exit-in-progress signal and is the
// expected end state for the lifetime of the supervisor process.
type gracefulCounter struct {
	n atomic.Int32
}

// Enter records a graceful-exit-protected region start.
func (g *gracefulCounter) Enter() { g.n.Add(1) }

// Exit records a graceful-exit-protected region end. Must be paired
// 1:1 with Enter; balanced Enter/Exit returns the counter to zero.
// Decrementing below zero is a programming error — go's atomic.Int32
// does not panic on negative values, but a negative counter means
// InProgress() would mis-report false too early.
func (g *gracefulCounter) Exit() { g.n.Add(-1) }

// InProgress reports whether any graceful-exit-protected region is
// active (counter > 0). Concurrent with Enter/Exit; readers see a
// snapshot under release semantics from the underlying atomic.
func (g *gracefulCounter) InProgress() bool { return g.n.Load() > 0 }

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
	stateDir          string
	events            *api.SupervisorEventLog
	reconcileReady    *atomic.Bool
	intentFilesLoaded *atomic.Bool
	// gracefulInProgress is a refcount-style counter (codex-r4-b-p2)
	// — see gracefulCounter docstring. The field name retains its
	// historical spelling so call sites elsewhere in supervise.go
	// stay one rename away from churn-free; the type change is
	// load-bearing for the concurrent-drain correctness contract.
	gracefulInProgress  *gracefulCounter
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
	//
	// Codex round-4 Lane B P2 (codex-r4-b-p2): the historical
	// atomic.Bool implementation let two concurrent quiesce-timers
	// requests share one flag — the first goroutine's `defer
	// Store(false)` would clear the in-progress signal while the
	// second drain was still running. Replaced with a refcount-style
	// gracefulCounter so InProgress() remains true until the LAST
	// active region exits.
	var gracefulInProgress gracefulCounter

	loop.RegisterHandler(func(e api.LoopEvent) {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "debug",
			Source:   "lifecycle",
			Event:    "loop-event",
			TaskName: e.TaskName,
			Body: map[string]any{
				"kind":                      string(e.Kind),
				"graceful_exit_in_progress": gracefulInProgress.InProgress(),
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

	// Read the two intent files. Production wiring (this commit)
	// threads supervisor-intent.json into the reconcile pass below;
	// daemon-intent.json feeds the IsActiveStop decision in the
	// reconciler. Missing files (os.ErrNotExist) are a valid "loaded"
	// outcome — the supervisor must come up cleanly on a fresh host
	// before any intent file exists.
	intent, daemonIntent := loadIntentFiles(stateDir, events, &intentFilesLoaded)

	// Startup reconcile pass. Wires the parsed supervisor-intent.json
	// through NewReconciler with production spawn/terminate closures.
	// This replaces the Task 6.2 stub that flipped reconcileReady
	// immediately after intent reads, leaving every daemon un-spawned.
	//
	// Test seam: if reconcileSpawnFn / reconcileTerminateFn were
	// installed via setReconcileSpawnFnForTest, the test fakes win
	// over the production closures so wiring tests can capture spawn
	// fan-out without launching real child processes.
	//
	// Order: reconcile runs BEFORE reconcileReady.Store(true) so any
	// migration watcher polling the audit log sees daemon-spawned
	// events emitted ahead of the reconcile-ready marker.
	currentRunning := loadSupervisorCurrentRunning(stateDir)
	job, jobErr := process.NewKillOnCloseJob()
	if jobErr != nil {
		// Best-effort: a job-create failure means the supervisor's
		// children survive its exit, but the supervisor itself stays
		// up. Audit row preserves root cause for operators.
		_ = events.Emit(api.SupervisorEvent{
			Severity: "warn",
			Source:   "lifecycle",
			Event:    "reconcile-job-create-failed",
			Body:     map[string]any{"err": jobErr.Error()},
		})
		job = nil
	}
	// Hold the Job handle for the lifetime of the supervisor so the
	// kernel kill-on-close action fires on supervisor exit. defer
	// Close() balances NewKillOnCloseJob's open handle.
	if job != nil {
		defer func() { _ = job.Close() }()
	}

	spawnFn := reconcileSpawnFn
	if spawnFn == nil {
		spawnFn = makeProductionSpawnFn(job, events)
	}
	terminateFn := reconcileTerminateFn
	if terminateFn == nil {
		terminateFn = func(d api.SupervisorDaemon) error {
			// Startup-only scope: no daemon has been spawned yet so
			// nothing to terminate. Watcher / reload (follow-up) will
			// wire a real terminate path that walks supervisor-state's
			// Daemons map and sends the OS-appropriate stop signal.
			return nil
		}
	}

	if intent != nil {
		reconciler := NewReconciler(spawnFn, terminateFn)
		reconciler.Reconcile(intent, daemonIntent, currentRunning, time.Now().UTC())
	}

	// Mark reconcile-ready. The flag transitions false → true exactly
	// once, AFTER the startup reconcile pass has fanned out spawn
	// decisions. Migration / upgrade callers wait on this flag (not
	// all-daemons-healthy) per spec §"Migration step 14".
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
		// Codex round-4 Lane B P2: lifecycle-exit handlers use Enter
		// without a matching Exit — the supervisor process is about
		// to terminate, so the resulting positive counter is the
		// permanent graceful-exit-in-progress signal for whatever
		// short window remains before the process actually exits.
		gracefulInProgress.Enter()
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
		// Codex round-4 Lane B P2: lifecycle-exit handlers use Enter
		// without a matching Exit — the supervisor process is about
		// to terminate, so the resulting positive counter is the
		// permanent graceful-exit-in-progress signal for whatever
		// short window remains before the process actually exits.
		gracefulInProgress.Enter()
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
		// Codex round-4 Lane B P2: lifecycle-exit handlers use Enter
		// without a matching Exit — the supervisor process is about
		// to terminate, so the resulting positive counter is the
		// permanent graceful-exit-in-progress signal for whatever
		// short window remains before the process actually exits.
		gracefulInProgress.Enter()
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
		// Codex round-4 Lane B P2: lifecycle-exit handlers use Enter
		// without a matching Exit — the supervisor process is about
		// to terminate, so the resulting positive counter is the
		// permanent graceful-exit-in-progress signal for whatever
		// short window remains before the process actually exits.
		gracefulInProgress.Enter()
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
	//
	// Codex round-4 Lane B P2: Enter/Exit (refcount) pair replaces
	// the historical Store(true)/Store(false) so two concurrent
	// quiesce-timers requests can't have the first's Exit clear the
	// in-progress flag while the second drain is still running.
	deps.gracefulInProgress.Enter()
	handler := quiesceHandlerFactory(deps.stateDir)
	resultCh := make(chan QuiesceResult, 1)
	go func() {
		defer deps.gracefulInProgress.Exit()
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
// daemon-intent.json files under stateDir and returns the parsed
// values to the caller. Production wiring threads the supervisor
// intent into the reconcile pass; the daemon intent feeds the
// IsActiveStop decision tree inside Reconciler.Reconcile.
//
// File-not-exist is a valid "loaded" outcome: a freshly installed
// supervisor on a clean host has no intent files, and the empty
// reconcile pass that follows will detect zero daemons / zero
// timers in that state. Both return values may be nil — callers
// MUST nil-check before deref.
//
// Errors (parse failure, I/O denied) are logged via the event log
// but do NOT prevent the ready transition — a corrupt intent file
// must not block the supervisor from coming up; the audit row is
// enough for an operator to diagnose. The function still sets
// intentFilesLoaded=true once both reads attempted, so the IPC
// `status` flag transitions out of startup state even when intent
// files are missing or corrupt.
//
// daemon-intent.json read path: the canonical *API.ReadDaemonIntent
// flow lives on *api.API and resolves DaemonStateDir() internally —
// that path does NOT honor the supervise CLI's
// MCPHUB_STATE_DIR_OVERRIDE seam, so the startup reconcile reads the
// file directly here via os.ReadFile + json.Unmarshal. Watcher/
// reload paths (follow-up) will switch to the full flock+quarantine
// flow once the production path-injection variant lands.
func loadIntentFiles(
	stateDir string,
	events *api.SupervisorEventLog,
	intentFilesLoaded *atomic.Bool,
) (*api.SupervisorIntentFile, *api.DaemonIntentFile) {
	var (
		supervisorIntent *api.SupervisorIntentFile
		daemonIntent     *api.DaemonIntentFile
	)

	supervisorIntentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if got, err := api.ReadSupervisorIntent(supervisorIntentPath); err != nil {
		// Only log non-NotExist errors — a missing file on a fresh
		// host is the expected first-boot shape.
		if !errors.Is(err, os.ErrNotExist) {
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
	} else {
		supervisorIntent = got
	}

	daemonIntentPath := filepath.Join(stateDir, "daemon-intent.json")
	if raw, err := os.ReadFile(daemonIntentPath); err != nil {
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
	} else {
		var parsed api.DaemonIntentFile
		if err := json.Unmarshal(raw, &parsed); err != nil {
			// Best-effort parse — same audit-only policy as the
			// supervisor-intent read above. Reconcile will see
			// daemonIntent==nil and treat every task as "no
			// override" (the safe default).
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "daemon-intent-parse-failed",
				Body: map[string]any{
					"path": daemonIntentPath,
					"err":  err.Error(),
				},
			})
		} else {
			daemonIntent = &parsed
		}
	}

	intentFilesLoaded.Store(true)
	return supervisorIntent, daemonIntent
}

// loadSupervisorCurrentRunning builds the currentRunning map the
// Reconciler needs from <state-dir>/supervisor-state.json. The map's
// shape is map[task_name]bool — Reconcile diffs intent.Daemons
// against this set to decide spawn vs no-op.
//
// Cold-start case: a missing supervisor-state.json (fresh install,
// post-quarantine restart, or operator-deleted state) returns the
// empty map. Reconcile then treats every intent daemon as
// "not running" and fans out spawn for each.
//
// Warm-restart case: a parsed supervisor-state.json may list daemons
// in state="running" with CurrentPID > 0. Those names go into the
// map so the second invocation of the supervisor does NOT respawn
// every daemon (the Job Object's kill-on-close would have already
// reaped them on the prior exit, but the safe contract is to trust
// what supervisor-state.json says and let the watcher / reload path
// reconcile any drift).
//
// Errors (read failure, parse failure) are swallowed silently —
// supervisor-events.log already captures the audit row in the
// watcher / reload path; on startup the safer default is to assume
// "nothing was running" and let the reconcile fan-out attempt every
// daemon (idempotent against an already-running peer because the
// production SpawnFn checks the file system port via daemon manifest
// before binding).
func loadSupervisorCurrentRunning(stateDir string) map[string]bool {
	result := map[string]bool{}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	state, err := api.ReadSupervisorState(statePath)
	if err != nil || state == nil {
		return result
	}
	for taskName, ds := range state.Daemons {
		if ds.State == "running" && ds.CurrentPID > 0 {
			result[taskName] = true
		}
	}
	return result
}

// makeProductionSpawnFn returns the SpawnFunc the Reconciler invokes
// for each daemon that needs to be (re)started. Each call:
//
//   - builds an exec.Cmd from the SupervisorDaemon descriptor
//     (command, args, env, workspace)
//   - applies process.NoConsole so no console window pops on Windows
//   - routes through process.StartWithJob when a Job Object is held
//     (Windows: PROC_THREAD_ATTRIBUTE_JOB_LIST assign-at-create;
//     POSIX: thin cmd.Start() shim per start_with_job_other.go)
//   - emits daemon-spawned (success) / daemon-spawn-failed (error)
//     audit events with the child PID + command + workspace
//
// Errors from cmd.Start propagate up via the SpawnFunc return value;
// Reconciler swallows that error (per supervise_reconcile.go:118)
// because the audit row is the canonical operator-visible signal.
//
// Env handling: cmd.Env defaults to nil ("inherit parent's") when the
// descriptor's Env map is empty. A non-empty Env map produces a
// full os.Environ()-derived block with the descriptor's keys appended
// — matching the v0.4.x daemon-host spawn convention so the v0.5.0
// supervisor's environment is a strict superset of what the prior
// daemons used to see.
func makeProductionSpawnFn(job *process.Job, events *api.SupervisorEventLog) SpawnFunc {
	return func(d api.SupervisorDaemon) error {
		cmd := exec.Command(d.Command, d.Args...)
		if d.Workspace != "" {
			cmd.Dir = d.Workspace
		}
		if len(d.Env) > 0 {
			env := os.Environ()
			for k, v := range d.Env {
				env = append(env, k+"="+v)
			}
			cmd.Env = env
		}
		process.NoConsole(cmd)

		var (
			pid      int
			startErr error
		)
		if job != nil {
			pid, startErr = process.StartWithJob(job, cmd)
		} else {
			startErr = cmd.Start()
			if startErr == nil && cmd.Process != nil {
				pid = cmd.Process.Pid
			}
		}
		if startErr != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-spawn-failed",
				TaskName: d.TaskName,
				Body: map[string]any{
					"err":     startErr.Error(),
					"command": d.Command,
				},
			})
			return startErr
		}
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "lifecycle",
			Event:    "daemon-spawned",
			TaskName: d.TaskName,
			Body: map[string]any{
				"pid":       pid,
				"command":   d.Command,
				"workspace": d.Workspace,
				"port":      d.Port,
			},
		})
		return nil
	}
}
