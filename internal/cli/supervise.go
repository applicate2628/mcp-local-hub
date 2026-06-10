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
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/daemon_env_overlay"
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

// errSpawnPreChild marks a SpawnFunc error that occurred BEFORE any
// child process existed (cmd.Start / StartWithJob returned non-nil).
// The supervisor controller uses errors.Is to distinguish this from
// a post-child error (e.g. persistDaemonRuntimeTracker failing after
// cmd.Start succeeded and the wait goroutine was launched). Only the
// pre-child case is safe to synthesize an EvChildExit for; the
// post-child case has a live child whose wait goroutine will emit
// the real exit event when it actually exits.
//
// Windows StartWithJob post-create-orphan note: when wrapping a
// startErr that errors.Is(startErr, process.ErrSpawnPostCreate) ==
// true, the OS child IS alive at the kernel level but unreachable
// from Go. We still wrap with errSpawnPreChild so the SM routes
// through backoff (the alternative — no synth event — leaves the
// daemon stuck in StSpawning forever, which is the original bug we
// are fixing). The orphan is reaped by Job Object KILL_ON_JOB_CLOSE
// on supervisor exit or by the backoff respawn hitting port-in-use
// (natural duplicate cap). The daemon-spawn-orphan-detected audit
// event makes the case operator-visible. Closes bot finding A on
// PR #236 1c0ea09 (P2 #5).
//
// Closes Codex Cloud bot finding on PR #236 a54cc95 (P1 — original
// stuck-StSpawning bug on commit 2d67031).
var errSpawnPreChild = errors.New("supervise: spawn failed before child created")

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

var (
	productionQueryPIDStateFn            = process.QueryPIDState
	productionVerifyPIDIdentityFn        = process.VerifyPIDIdentity
	productionTerminatePIDWithIdentityFn = process.TerminatePIDWithIdentity
	currentRunningVerifyPIDIdentityFn    = process.VerifyPIDIdentity
	currentRunningIsPIDAliveFn           = process.IsPidAlive
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
		noIPC       bool
		strictMode  bool
		ensureAlive bool
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

--ensure-alive is a SEPARATE, tiny one-shot action (NOT the long-lived
supervisor): the \mcp-local-hub-liveness scheduled task runs it every
~1 min. It probes the flock-authoritative supervisor.lock and relaunches
the supervisor/GUI owner if it died mid-session; if a supervisor is
running (or liveness is undeterminable) it is a no-op. Always exits 0.
`,
		// Test-only flag scoped to the no-ipc test path. Production
		// invocations either pass --no-ipc=false (default) or omit it.
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --ensure-alive short-circuits the long-lived supervisor: it is
			// a probe-only liveness tick that must NOT acquire the supervisor
			// lock or start the event loop (doing so would itself BE a
			// supervisor). It resolves the state dir, probes liveness, and
			// relaunches the owner only when no live lock holder exists.
			if ensureAlive {
				return runEnsureAliveFromState()
			}
			return runSupervise(cmd.Context(), noIPC, strictMode)
		},
	}
	cmd.Flags().BoolVar(&noIPC, "no-ipc", false, "skip IPC listener (test flag)")
	cmd.Flags().BoolVar(&strictMode, "strict-mode", false, "enforce strict parent-dir DACL gate")
	cmd.Flags().BoolVar(&ensureAlive, "ensure-alive", false,
		"one-shot supervisor-liveness tick: relaunch the owner if dead, else no-op (run by the \\mcp-local-hub-liveness task; always exits 0)")
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
	runtimeTracker    *DaemonRuntimeTracker
	reconcileReady    *atomic.Bool
	intentFilesLoaded *atomic.Bool
	// gracefulInProgress is a refcount-style counter (codex-r4-b-p2)
	// — see gracefulCounter docstring. The field name retains its
	// historical spelling so call sites elsewhere in supervise.go
	// stay one rename away from churn-free; the type change is
	// load-bearing for the concurrent-drain correctness contract.
	gracefulInProgress  *gracefulCounter
	triggerGracefulExit func() // closes loop, signals exit
	// overlay is the parsed daemon-env-overrides.yaml file loaded once
	// at supervisor startup. nil means no overlay file or load failed
	// in a non-fatal way (default-relax). Per-spawn lookups go through
	// daemon_env_overlay.LookupOverlay so a nil overlay yields nil env.
	// Task 4.1 `respawn` IPC reads this same field to look up the
	// affected daemon's overlay env before re-spawning.
	overlay *daemon_env_overlay.Overlay
	// intent is the parsed supervisor-intent.json file loaded once at
	// supervisor startup. The `respawn` IPC handler resolves taskName
	// against intent.Daemons — supervisor-intent.json is the canonical
	// truth for which daemons the operator wants running.
	intent *api.SupervisorIntentFile
	// respawnLate carries the spawn/terminate closures the `respawn`
	// IPC handler uses. These closures are constructed AFTER deps
	// (per plan v5 option (b) — preserves IPC accept-loop startup
	// ordering). The respawnLateBindings pointer is non-nil from deps
	// construction; the closures inside it get Set() after spawnFn /
	// terminateFn exist. Handlers must Get() and nil-check before use.
	respawnLate *respawnLateBindings
	// controllerProvider returns the live A.2 supervisorController when
	// constructed. The respawn handler reads from
	// controllerProvider().intentCache.Lookup(taskName) for the freshest
	// daemon descriptor (closes bot PR#222 P2-2: deps.intent is a startup
	// snapshot, never refreshed; controller.intentCache IS refreshed by
	// IntentWatcher on intent-file mtime changes). nil-safe — handler
	// falls back to deps.intent when provider returns nil (e.g. in unit
	// tests that don't construct a full controller, or in the brief
	// startup window before the controller is built).
	//
	// Closure-captured variable pattern (same as triggerGracefulExit
	// above): deps captures a *supervisorController variable declared
	// early; later code assigns the variable; subsequent provider calls
	// observe the live value. This preserves IPC accept-loop startup
	// ordering (deps + accept goroutine launch BEFORE controller exists)
	// without changing the deps struct lifetime.
	controllerProvider func() *supervisorController
}

const ipcErrorSupervisorStarting = "SUPERVISOR_STARTING"

var daemonIntentReadLockTimeout = 5 * time.Second

type runningProcessIdentity struct {
	PID           int
	PIDGeneration int
	StartedAt     string
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
	// Buffer sized for production: max daemons × max events per
	// reconcile cycle + Phase 9 maintenance-timer publishers + IPC
	// quiesce-drain bursts. Per consultant memo on PR #236 r4:
	// "EventLoop capacity = f(producer_count, max_burst_per_cycle)".
	// 1024 gives ~100 daemons × 10 events headroom plus 24 for the
	// per-iteration churn. Memory cost ~32KB. The two-channel design
	// in api/supervisor_event_loop.go (main ch + priority selfCh)
	// separately addresses the handler-self-post deadlock class, so
	// this cap is purely about external-producer absorption.
	loop := api.NewEventLoop(1024)
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
	// reconcileReady intentionally does not wait for child spawn or
	// terminate fan-out. Those outcomes are audit events; readiness
	// means clients may now issue mutating IPC verbs against a supervisor
	// whose first reconcile pass is scheduled.
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

	// Serena crash-repair self-heal: BEFORE loading the intent for the first
	// reconcile pass, re-converge any registry serena row left orphaned by a
	// crash between an auto-register registry Save and its install commit (it
	// APPENDS the missing daemon rows to supervisor-intent.json; it never
	// replace-alls). Running it here means the loadIntentFiles read below picks
	// up the now-complete intent and the first reconcile spawns the recovered
	// daemons. NON-FATAL: a repair error (or a deferred introduce-crash) never
	// blocks supervisor startup — the supervisor must come up regardless.
	if repaired, deferredKeys, rErr := api.NewAPI().RepairSerenaIntentFromRegistry(stateDir); rErr != nil {
		_ = events.TryEmit(api.SupervisorEvent{
			Severity: "warn",
			Source:   "reconcile",
			Event:    "serena-intent-repair-failed",
			Body: map[string]any{
				"err": rErr.Error(),
			},
		})
	} else if repaired > 0 || len(deferredKeys) > 0 {
		severity := "info"
		if len(deferredKeys) > 0 {
			severity = "warn"
		}
		_ = events.TryEmit(api.SupervisorEvent{
			Severity: severity,
			Source:   "reconcile",
			Event:    "serena-intent-repair-result",
			Body: map[string]any{
				"repaired_count":     repaired,
				"deferred_count":     len(deferredKeys),
				"deferred_workspace": deferredKeys,
			},
		})
	}

	// Read the two intent files before exposing IPC. daemon-intent.json
	// parse/schema failures are fail-closed: a corrupt stop/quarantine
	// file must not collapse to daemonIntent==nil, because Reconcile
	// treats nil as "no stops" and would restart suppressed daemons.
	intent, daemonIntent, intentErr := loadIntentFiles(stateDir, events, &intentFilesLoaded)
	if intentErr != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "lifecycle",
			Event:    "supervise-startup-failed",
			Body: map[string]any{
				"err": intentErr.Error(),
			},
		})
		return fmt.Errorf("load intent files: %w", intentErr)
	}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	runtimeTracker, currentRunning, runningPIDs, stateErr := loadSupervisorStartupRuntime(stateDir)
	if stateErr != nil {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "lifecycle",
			Event:    "supervise-startup-failed",
			Body: map[string]any{
				"err": stateErr.Error(),
			},
		})
		return fmt.Errorf("load supervisor-state.json: %w", stateErr)
	}

	// Load daemon-env-overrides.yaml ONCE at startup. Per spec
	// §"Error handling" + v4 I-V4-5: parse failure is fail-LOUD —
	// supervisor refuses to spawn ANY daemon and prints actionable
	// guidance (`mcphub config overlay-quarantine` renames the corrupt
	// file aside under a `.corrupt-<ts>` suffix so the next cold start
	// boots with empty overlay).
	//
	// Missing file is benign: daemon_env_overlay.Load returns an empty
	// Overlay{Daemons: {}} + nil error on ErrNotExist, so a fresh
	// install without operator overlay edits proceeds with manifest env
	// only — exactly the pre-overlay behavior.
	overlay, overlayErr := loadOverlayAtStartup(stateDir, events, intent)
	if overlayErr != nil {
		return overlayErr
	}

	// respawnLate holds the spawn/terminate closures the `respawn` IPC
	// handler uses. Closures are constructed AFTER spawnFn/terminateFn
	// exist (below); the holder is non-nil from here so the IPC accept
	// loop (started inside the `if !noIPC` block below) can safely
	// reference deps.respawnLate. Late-binding pattern preserves the
	// existing accept-loop startup ordering per plan v5 option (b).
	respawnLate := &respawnLateBindings{}

	// ctrl is the A.2 supervisorController, constructed AFTER spawn/
	// terminate factories exist (below, around line 680). The deps
	// struct captures `&ctrl` via a closure provider so the respawn
	// handler can look up live intent through ctrl.intentCache even
	// though the accept goroutine starts before ctrl is built.
	// Closure-capture pattern (same as triggerGracefulExit below):
	// the closure reads the variable's CURRENT value each call.
	var ctrl *supervisorController

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
		listener, lerr := NewSupervisorIPCListener(pipePath, lk.Owner())
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
			runtimeTracker:     runtimeTracker,
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
			overlay:            overlay,
			intent:             intent,
			respawnLate:        respawnLate,
			controllerProvider: func() *supervisorController { return ctrl },
		}

		// Spawn the accept goroutine. It runs until listener.Close()
		// causes Accept to return an error (the deferred Close above
		// is the exit signal during the graceful-exit flow). Each
		// accepted connection gets its own handler goroutine so a slow
		// client never blocks the next Accept.
		go acceptIPCConnections(listener, deps)
	}

	// Startup reconcile pass. Wires the parsed supervisor-intent.json
	// through NewReconciler with production spawn/terminate closures.
	// Reconcile runs asynchronously; reconcileReady below means "scheduled",
	// not "completed".
	//
	// Test seam: if reconcileSpawnFn / reconcileTerminateFn were
	// installed via setReconcileSpawnFnForTest, the test fakes win
	// over the production closures so wiring tests can capture spawn
	// fan-out without launching real child processes.
	//
	// PER-SPAWN Job Object architecture (ADR #239 Option B,
	// 2026-05-28). Each daemon spawn allocates its own Job Object
	// inside the spawn closure, so orphan cleanup is task-scoped
	// (TerminateJobObject kills only that daemon's tree, not the
	// supervisor-wide tree). The pre-ADR shared-Job design that
	// allocated one Job here for the whole supervisor was removed:
	// bot P1 on PR #238 331b0df flagged that calling TerminateAll on
	// the shared Job would terminate every healthy daemon along with
	// the intended orphan target.
	//
	// KILL_ON_JOB_CLOSE on each per-spawn Job still fires on its own
	// child exit (wait goroutine's defer Close), giving the same
	// "no orphan processes after supervisor exit" guarantee as the
	// old shared-Job model, but in a task-scoped way that supports
	// safe orphan-cleanup via daemonJob.TerminateAll.

	// crashCh: buffered channel the spawn fn's wait goroutine posts to
	// when a child exits non-cleanly. The respawn dispatcher reads these
	// events and schedules backoff-gated respawns up to the per-task
	// sliding-window quarantine limit. Capacity 64 absorbs short bursts
	// (e.g., one wrapper crash per daemon at startup when an env var is
	// misconfigured across the whole fleet). Tests that swap in
	// reconcileSpawnFn skip this wiring entirely — they don't need the
	// dispatcher because their fake spawn fn never posts to the channel.
	crashCh := make(chan crashEvent, 64)
	spawnFn := reconcileSpawnFn
	if spawnFn == nil {
		spawnFn = makeProductionSpawnFnWithStatePath(events, runtimeTracker, statePath, overlay, filepath.Join(stateDir, "daemon-env-overrides.yaml"), crashCh)
	}
	terminateFn := reconcileTerminateFn
	if terminateFn == nil {
		terminateFn = makeProductionTerminateFnWithStatePath(events, runningPIDs, runtimeTracker, statePath)
	}

	// Wire the late-bound respawn closures now that spawnFn/terminateFn
	// exist. The IPC accept loop above (in the !noIPC branch) already
	// holds the respawnLate pointer via deps; the Set() makes Get()
	// return non-nil for subsequent `respawn` IPC requests.
	respawnLate.Set(spawnFn, terminateFn)

	// Phase A.2: build the supervisorController from existing
	// primitives (event loop, tracker, graceful flag) plus fresh
	// intent + daemon-intent caches. The controller absorbs the
	// deleted runRespawnDispatcher's responsibilities (sliding-window
	// quarantine, exponential backoff, spawn fire on EvTimerDue) and
	// routes ALL spawn/respawn through the formal api.Transition
	// state machine.
	//
	// The reconciler keeps spawning through spawnFn directly via
	// EvStart (see supervise_reconcile.go:118): the reconciler posts
	// EvStart onto the same loop the controller listens on, so the
	// controller is the sole consumer of policy transitions while the
	// reconciler stays the source of "this daemon should be running".
	ctrl = &supervisorController{
		intentCache:         newIntentCache(),
		eventLoop:           loop,
		tracker:             runtimeTracker,
		events:              events,
		graceful:            &gracefulInProgress,
		daemonIntent:        newDaemonIntentCache(),
		spawn:               spawnFn,
		terminate:           terminateFn,
		statePath:           statePath,
		ctx:                 loopCtx,
		failureWindow:       respawnFailureWindow,
		quarantineThreshold: respawnQuarantineThreshold,
	}
	ctrl.intentCache.Refresh(intent)
	ctrl.daemonIntent.Refresh(daemonIntent)
	hydrateControllerRunningStates(ctrl, currentRunning)
	loop.RegisterHandler(ctrl.handleLoopEvent)

	// Crash-event bridge: the production spawn fn posts crashEvent
	// onto crashCh from its cmd.Wait goroutine. The Phase A.2
	// controller wants EvChildExit on the formal event loop so the
	// SM transition table drives backoff/quarantine/terminate
	// decisions. Bridge crashCh -> eventLoop.Post(EvChildExit) with
	// exit_code in the Body. Started only when reconcileSpawnFn was
	// nil (production wiring); tests with a fake spawn fn don't
	// receive crash events and don't need this goroutine.
	if reconcileSpawnFn == nil {
		go runCrashEventBridge(loopCtx, crashCh, loop, events)
		go startSupervisorLivenessMonitor(loopCtx.Done(), stateDir, intent, runtimeTracker, loop, events)
	}

	// IntentWatcher: poll <state-dir>/{supervisor,daemon}-intent.json
	// for mtime changes. On change, re-read both files, refresh the
	// controller's caches, and post one EvIntentUpdate per task whose
	// DaemonIntent actually changed (delta-only, NOT one-per-task on
	// every mtime bump - closes the v6 sonnet "per-task storm"
	// finding). The 60s poll interval is the spec-mandated upper
	// bound on watch-miss latency; the IPC `reload` command (Task
	// 6.3) drives faster propagation when clients call it.
	previousDaemonIntent := daemonIntent
	watcher := NewIntentWatcher(stateDir, 60*time.Second, func() {
		// Re-read supervisor-intent.json. Errors are warn-only; a
		// transient read failure should NOT clear the cached
		// snapshot (clearing would make every subsequent
		// handleLoopEvent see an empty intent and drop EvStart events
		// as orphans).
		supervisorIntentPath := filepath.Join(stateDir, "supervisor-intent.json")
		if updatedSupervisor, supErr := api.ReadSupervisorIntent(supervisorIntentPath); supErr != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "intent-watcher",
				Event:    "intent-reload-failed",
				Body: map[string]any{
					"file":  "supervisor-intent.json",
					"error": supErr.Error(),
				},
			})
		} else {
			ctrl.refreshSupervisorIntent(updatedSupervisor)
		}

		// Re-read daemon-intent.json via the free-function form
		// (ReadDaemonIntentFile takes statePath + timeout, matching
		// the loadIntentFiles boot path at supervise.go:1501).
		daemonIntentPath := filepath.Join(stateDir, "daemon-intent.json")
		daemonRead := api.ReadDaemonIntentFile(daemonIntentPath, daemonIntentReadLockTimeout)
		var updatedDaemonIntent *api.DaemonIntentFile
		if daemonRead.Err != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "intent-watcher",
				Event:    "intent-reload-failed",
				Body: map[string]any{
					"file":  "daemon-intent.json",
					"error": daemonRead.Err.Error(),
				},
			})
			updatedDaemonIntent = previousDaemonIntent
		} else if daemonRead.State == api.IntentStateValid {
			parsed := daemonRead.File
			updatedDaemonIntent = &parsed
		} else {
			// IntentStateMissing - treat as nil (empty intent file).
			updatedDaemonIntent = nil
		}
		ctrl.daemonIntent.Refresh(updatedDaemonIntent)

		// Delta-only EvIntentUpdate posting. On a typical mtime
		// bump where only one daemon's Desired flips, delta == 1;
		// the rest of the intent file stays unchanged and no
		// events post (closes the v6 sonnet "per-task storm"
		// IMPORTANT finding).
		delta := diffIntentSnapshots(previousDaemonIntent, updatedDaemonIntent)
		for _, taskName := range delta {
			loop.Post(api.LoopEvent{Kind: api.EvIntentUpdate, TaskName: taskName})
		}
		previousDaemonIntent = updatedDaemonIntent
	})
	go watcher.Run(loopCtx)

	if intent != nil {
		reconciler := NewReconciler(spawnFn, terminateFn)
		// Phase A.2: pass the event loop so the reconciler posts
		// EvStart instead of calling spawnFn directly. The
		// controller's handleLoopEvent then routes the spawn
		// through api.Transition for formal SM bookkeeping.
		// Tests that don't wire an event loop still go through the
		// legacy direct-spawn path (Reconciler.Reconcile
		// nil-checks the field and falls back to r.spawn).
		reconciler.EventLoop = loop
		// bot PR #246 r2 P2: pass the audit log so the desired-set
		// exclusion of legacy nil-RuntimeSpec serena-proxy rows emits its
		// operator-actionable warn at the exclusion point.
		reconciler.Events = events
		// Orphaned-LSP-daemon quarantine fix: pass the registry-membership
		// predicate so an LSP workspace-proxy descriptor whose (workspace_key,
		// language) row was removed (e.g. by `mcphub workspace unregister`)
		// without removing the paired intent descriptor is EXCLUDED from the
		// spawn-desired set instead of spawned, failing "not registered", and
		// churning into quarantine.
		reconciler.LSPRegistryHasRow = api.LSPRegistryRowBacksDescriptor
		go reconciler.Reconcile(intent, daemonIntent, currentRunning, time.Now().UTC())
	}

	// Mark reconcile-ready. The flag transitions false → true once the
	// startup reconcile pass has been scheduled, not when child spawn or
	// terminate fan-out completes. Migration / upgrade callers wait on this
	// flag (not all-daemons-healthy) per spec §"Migration step 14".
	reconcileReady.Store(true)
	_ = events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "reconcile-ready",
		Body: map[string]any{
			"intent_files_loaded": intentFilesLoaded.Load(),
		},
	})

	// Phase 9: production maintenance timer scheduler wiring.
	// Constructed locally in runSupervise so v0.5.0 keeps the
	// scheduler out of IPC deps until a future fire-now command needs
	// direct handler access. The timer list is read through the
	// controller's live intent cache on every Tick; IntentWatcher
	// refreshes that cache whenever supervisor-intent.json changes.
	maintenanceState := newMaintenanceStateAdapter(statePath, events)
	maintenanceProcSpawner := newMaintenanceSpawner(events, &gracefulInProgress)
	maintenanceScheduler := NewMaintenanceScheduler(maintenanceState)
	maintenanceScheduler.SetSpawner(maintenanceProcSpawner)
	maintenanceScheduler.SetFireHook(func(t api.MaintenanceTimer) {
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "maintenance",
			Event:    "maintenance-fired",
			TaskName: t.Name,
			Body: map[string]any{
				"kind":    t.Kind,
				"command": t.Command,
			},
		})
	})
	go runMaintenanceScheduler(loopCtx.Done(), &gracefulInProgress, maintenanceScheduler, func() []api.MaintenanceTimer {
		return maintenanceTimersFromController(ctrl)
	}, 60*time.Second)

	shutdownMaintenance := func(reason string) {
		result := maintenanceProcSpawner.Shutdown(30 * time.Second)
		// Synchronously reconcile on-disk transient_pids before this
		// returns (PR #243 bot P2#2). The per-fire drain goroutine
		// removes a transient PID only after Spawner.Wait returns, but
		// the signal/IPC exit path returns right after this closure, so
		// that goroutine may not run before the process exits — leaving
		// a stale entry for the next cold start. Removing every entry
		// whose PID is no longer alive (isPIDAlive reports PID<=0 claim
		// slots as dead too) is authoritative and race-free: it covers
		// processes Shutdown drained/killed AND any that drained in the
		// window between Spawner.Wait's procs delete and the drain
		// goroutine's RemoveTransientPID. Still-running children are
		// retained for the cold-start reaper.
		reconciled := maintenanceState.ReconcileTransientPIDs(isPIDAlive)
		if result.Drained == 0 && len(result.Killed) == 0 && len(result.StillRunning) == 0 && len(reconciled) == 0 {
			return
		}
		_ = events.Emit(api.SupervisorEvent{
			Severity: "info",
			Source:   "maintenance",
			Event:    "maintenance-shutdown-complete",
			Body: map[string]any{
				"reason":           reason,
				"drained":          result.Drained,
				"killed":           result.Killed,
				"still_running":    result.StillRunning,
				"state_reconciled": reconciled,
			},
		})
	}

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
		shutdownMaintenance("signal")
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
		shutdownMaintenance("test-exit")
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
		shutdownMaintenance("ipc-exit-graceful")
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
		shutdownMaintenance("context-cancel")
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
// (deferred in runSupervise) causes Accept to return net.ErrClosed.
//
// Per spec §"Wire format": each accepted connection is a long-lived
// newline-delimited-JSON channel. Multiple connections from the same
// client (`mcphub status`, `mcphub stop`, `mcphub migrate`) are
// supported concurrently — each gets its own per-connection handler
// goroutine. The handlers share access to the supervisor-wide deps
// via ipcDispatchDeps; no per-connection state is mutated by
// handleIPCRequest.
//
// Error handling (post-mortem of 2026-05-19 22:52:17 IPC crash):
// Pre-fix, ANY error from listener.Accept() (including transient
// per-connection failures like the hello-write race where a client
// disconnects mid-handshake) caused the accept loop to RETURN
// permanently. The supervisor process stayed alive, daemons stayed
// alive, but IPC went silent and `mcphub status` started timing
// out. Dashboard appeared "broken" with no actionable diagnostic.
//
// The post-mortem error was specifically:
//
//	ipc-accept-exit body: "write hello: The pipe is being closed."
//
// which comes from the Accept-time hello write in
// supervise_ipc_windows.go:115 (POSIX equivalent at
// supervise_ipc_posix.go:122). The fix: continue the loop on
// non-ErrClosed errors with a small backoff to avoid hot-loop when
// a persistent transport fault is in play; only ErrClosed signals
// "listener was Close()'d via Stop()" and is the right time to exit.
//
// Defense-in-depth: if we somehow accumulate maxConsecutiveAcceptErrs
// (100) transient errors back-to-back, treat the listener as
// genuinely broken and exit the loop. That keeps the supervisor
// from spinning forever on a permanently-poisoned pipe.
const maxConsecutiveAcceptErrs = 100

// ipcAcceptor is the narrow interface acceptIPCConnections needs from
// the listener. Concrete production type is *SupervisorIPCListener;
// tests inject a fake to exercise the error-handling branches without
// binding a real pipe/socket.
type ipcAcceptor interface {
	Accept() (net.Conn, error)
}

func acceptIPCConnections(
	listener ipcAcceptor,
	deps ipcDispatchDeps,
) {
	consecErrs := 0
	for {
		conn, err := listener.Accept()
		if err != nil {
			// listener.Close() returns net.ErrClosed via Accept on
			// graceful exit. That is the ONE error that signals
			// "the supervisor is shutting down" rather than a
			// per-connection transient. Emit info + exit cleanly.
			if errors.Is(err, net.ErrClosed) {
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
			consecErrs++
			// Per-connection transient (hello-write race, client
			// disconnected mid-handshake, kernel pool pressure).
			// Emit warn + continue so the loop survives. Pre-fix
			// this was the regression that caused the 2026-05-19
			// supervisor-IPC-silence Dashboard outage.
			_ = deps.events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "ipc",
				Event:    "ipc-accept-transient-error",
				Body: map[string]any{
					"err":             err.Error(),
					"consecutive_err": consecErrs,
				},
			})
			if consecErrs >= maxConsecutiveAcceptErrs {
				// Listener is genuinely broken — exit the loop so
				// the supervisor's deferred Close + restart paths
				// can take over rather than spinning forever.
				_ = deps.events.Emit(api.SupervisorEvent{
					Severity: "error",
					Source:   "ipc",
					Event:    "ipc-accept-exit",
					Body: map[string]any{
						"err":             err.Error(),
						"reason":          "consecutive-transient-errors-exceeded-budget",
						"consecutive_err": consecErrs,
					},
				})
				return
			}
			// Short backoff so persistent failure modes don't
			// hot-loop the audit log + CPU.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		consecErrs = 0
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
		daemons, err := supervisorStatusDaemons(deps.stateDir, deps.runtimeTracker)
		if err != nil {
			return writeIPCFrame(conn, api.IPCResponse{
				ID: req.ID,
				Error: &api.IPCErr{
					Code:    "STATUS_FAILED",
					Message: err.Error(),
				},
				Final: true,
			})
		}
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"state":               "running",
				"daemons":             daemons,
				"reconcile_ready":     deps.reconcileReady.Load(),
				"intent_files_loaded": deps.intentFilesLoaded.Load(),
			},
		})
	}
	if !deps.reconcileReady.Load() {
		return writeIPCFrame(conn, api.IPCResponse{
			ID: req.ID,
			Error: &api.IPCErr{
				Code:      ipcErrorSupervisorStarting,
				Message:   "supervisor is still starting; retry status until reconcile_ready=true",
				Retryable: true,
			},
			Final: true,
		})
	}
	switch req.Cmd {
	case "quiesce-timers":
		return handleQuiesceTimers(conn, req, deps)
	case "exit":
		return handleExit(conn, req, deps)
	case "respawn":
		return handleRespawn(conn, req, deps)
	case "reconcile":
		// Phase A.3 (plan v10 §A.3, 2026-05-20): operator-triggered
		// in-place drift cleanup. Reads supervisor-intent.json + the
		// scheduler-registered task list + daemon-intent.json, computes
		// a drift report, and (with args.apply=true) posts
		// EvIntentUpdate per drift entry so the SM drives Run/Stop/Delete
		// transitions WITHOUT a supervisor cold-restart. See
		// supervise_reconcile_ipc.go for the handler body.
		return handleReconcile(conn, req, deps)
	case "restart", "reload":
		// Legacy alias surface preserved for v0.4.x clients. Task 4.1
		// adds the canonical `respawn` verb above; restart/reload still
		// return UNKNOWN_COMMAND because their semantics (which daemon,
		// with which intent diff, against which reconcile pass) differ
		// from a single-daemon respawn and would require a separate
		// dispatch contract.
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

// loadOverlayAtStartup reads `daemon-env-overrides.yaml` once at
// supervisor startup, emits the spec-mandated observability events,
// and returns the parsed overlay. Behaviour:
//
//   - Missing file → returns ({Daemons: {}}, nil) per the
//     daemon_env_overlay.Load contract. No event fires.
//   - Parse failure (hardened-read refusal, YAML decode, size-cap
//     exceeded, non-regular file, etc.) → emits
//     `daemon-env-overlay-load-failed` (warn) AND returns a wrapped
//     error pointing the operator at `mcphub config overlay-quarantine`.
//     Caller turns this into a startup-failed audit row + abort. Per
//     spec v4 §"Error handling" + I-V4-5 (fail-LOUD whole-overlay).
//   - Success → emits `daemon-env-overlay-loaded` (info) with the row
//     count, then `daemon-env-overlay-orphan-row` (warn) once per
//     overlay row whose taskName is NOT present in the supervisor
//     intent. Both event names match the spec §"Observability" table.
//
// The orphan check normalizes both sides via NormalizeOverlayKey so a
// hand-edited overlay using bare-form taskNames matches canonical-form
// intent. Orphan rows are NOT removed here — operators run
// `mcphub config prune-orphan-overlay-rows` (Task 5.1) for that.
func loadOverlayAtStartup(stateDir string, events *api.SupervisorEventLog, intent *api.SupervisorIntentFile) (*daemon_env_overlay.Overlay, error) {
	overlayPath := filepath.Join(stateDir, "daemon-env-overrides.yaml")
	ov, err := daemon_env_overlay.Load(overlayPath)
	if err != nil {
		// Classify the error: rejections from the read-side hardening
		// pipeline (symlink/reparse-point refusal, non-regular file,
		// size cap, parent-DACL gate) are operationally distinct from
		// generic YAML parse errors. The spec assigns separate event
		// names so the operator audit log distinguishes "the file is
		// suspicious" from "the file is malformed". Both still trip
		// the supervise-startup-failed fail-LOUD path; the operator's
		// remedy is the same (`mcphub config overlay-quarantine`).
		errMsg := err.Error()
		isHardeningRejection := strings.Contains(errMsg, "reparse") ||
			strings.Contains(errMsg, "symlink") ||
			strings.Contains(errMsg, "not a regular file") ||
			strings.Contains(errMsg, "exceeds") ||
			strings.Contains(errMsg, "non-UTF-8") ||
			strings.Contains(errMsg, "parent gate") ||
			strings.Contains(errMsg, "not single-user safe")
		if isHardeningRejection {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-env-overlay-read-rejected",
				Body: map[string]any{
					"path": overlayPath,
					"err":  errMsg,
				},
			})
		}
		_ = events.Emit(api.SupervisorEvent{
			Severity: "warn",
			Source:   "lifecycle",
			Event:    "daemon-env-overlay-load-failed",
			Body: map[string]any{
				"path": overlayPath,
				"err":  errMsg,
			},
		})
		_ = events.Emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "lifecycle",
			Event:    "supervise-startup-failed",
			Body: map[string]any{
				"err":    errMsg,
				"remedy": "mcphub config overlay-quarantine",
			},
		})
		return nil, fmt.Errorf("daemon-env-overlay-load failed (run `mcphub config overlay-quarantine` to rename the corrupt overlay aside): %w", err)
	}

	rowCount := 0
	if ov != nil {
		rowCount = len(ov.Daemons)
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "daemon-env-overlay-loaded",
		Body: map[string]any{
			"path":      overlayPath,
			"row_count": rowCount,
		},
	})

	// Orphan-row scan: emit one event per overlay row whose canonical
	// taskName is NOT in the current intent. Empty intent (fresh
	// install before any `mcphub install`) makes every row an orphan;
	// the events surface as warn so the operator log records the drift
	// without aborting.
	if ov != nil && len(ov.Daemons) > 0 {
		intentSet := map[string]struct{}{}
		if intent != nil {
			for _, d := range intent.Daemons {
				intentSet[daemon_env_overlay.NormalizeOverlayKey(d.TaskName)] = struct{}{}
			}
		}
		// Stable iteration order so the audit log entries are
		// deterministic across supervisor restarts on the same overlay.
		keys := make([]string, 0, len(ov.Daemons))
		for k := range ov.Daemons {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, present := intentSet[daemon_env_overlay.NormalizeOverlayKey(k)]; present {
				continue
			}
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "daemon-env-overlay-orphan-row",
				Body: map[string]any{
					"task_name": k,
					"remedy":    "mcphub config prune-orphan-overlay-rows",
				},
			})
		}
	}

	return ov, nil
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
// supervisor-intent.json and daemon-intent.json are both fail-closed when a
// present file cannot be read or parsed. Missing files remain valid first-boot
// input. daemon-intent.json goes through api.ReadDaemonIntentFile so startup
// honors the same sibling-flock/quarantine owner as other daemon-intent readers
// while still using the supervise CLI's already-resolved state-dir override.
func loadIntentFiles(
	stateDir string,
	events *api.SupervisorEventLog,
	intentFilesLoaded *atomic.Bool,
) (*api.SupervisorIntentFile, *api.DaemonIntentFile, error) {
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
				Severity: "error",
				Source:   "lifecycle",
				Event:    "supervisor-intent-read-failed",
				Body: map[string]any{
					"path": supervisorIntentPath,
					"err":  err.Error(),
				},
			})
			return nil, nil, fmt.Errorf("read supervisor-intent.json: %w", err)
		}
	} else {
		supervisorIntent = got
	}

	daemonIntentPath := filepath.Join(stateDir, "daemon-intent.json")
	daemonRead := api.ReadDaemonIntentFile(daemonIntentPath, daemonIntentReadLockTimeout)
	if daemonRead.Err != nil {
		eventName := "daemon-intent-read-failed"
		if daemonRead.State == api.IntentStateCorrupt {
			eventName = "daemon-intent-parse-failed"
		}
		body := map[string]any{
			"path": daemonIntentPath,
			"err":  daemonRead.Err.Error(),
		}
		if daemonRead.QuarantinePath != "" {
			body["quarantine_path"] = daemonRead.QuarantinePath
		}
		_ = events.Emit(api.SupervisorEvent{
			Severity: "error",
			Source:   "lifecycle",
			Event:    eventName,
			Body:     body,
		})
		return supervisorIntent, nil, fmt.Errorf("read daemon-intent.json: %w", daemonRead.Err)
	}
	if daemonRead.State == api.IntentStateValid {
		parsed := daemonRead.File
		daemonIntent = &parsed
	}

	intentFilesLoaded.Store(true)
	return supervisorIntent, daemonIntent, nil
}

// loadSupervisorCurrentRunning builds the currentRunning map the
// Reconciler needs from <state-dir>/supervisor-state.json and the
// sibling taskName->PID map the production terminate path needs.
//
// Cold-start case: a missing supervisor-state.json (fresh install,
// post-quarantine restart, or operator-deleted state) returns the
// empty map. Reconcile then treats every intent daemon as
// "not running" and fans out spawn for each.
//
// Warm-restart case: a parsed supervisor-state.json may list daemons
// in state="running" with CurrentPID > 0. Those names go into the map
// only when the PID is still alive and matches the expected mcphub image
// identity plus the recorded pid_generation/started_at generation proof, so
// a stale state file cannot suppress a required startup spawn.
//
// A missing file is valid first boot. Any other read/parse error is fatal:
// corrupt supervisor-state.json is an untrusted warm-start input and must not
// silently collapse to "no daemons running".
func loadSupervisorStartupRuntime(stateDir string) (*DaemonRuntimeTracker, map[string]bool, map[string]runningProcessIdentity, error) {
	currentRunning, runningPIDs, err := loadSupervisorCurrentRunning(stateDir)
	if err != nil {
		return nil, nil, nil, err
	}
	tracker, err := loadDaemonRuntimeTrackerFromStatePath(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		return nil, nil, nil, err
	}
	return tracker, currentRunning, runningPIDs, nil
}

func loadSupervisorCurrentRunning(stateDir string) (map[string]bool, map[string]runningProcessIdentity, error) {
	result := map[string]bool{}
	pids := map[string]runningProcessIdentity{}
	statePath := filepath.Join(stateDir, "supervisor-state.json")
	state, err := api.ReadSupervisorState(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, pids, nil
		}
		return result, pids, fmt.Errorf("read supervisor-state.json: %w", err)
	}
	if state == nil {
		return result, pids, nil
	}
	// supervisor-state.json (api.SupervisorDaemonState) carries only the
	// runtime PID/state — NOT the daemon's exe. The PID-identity proof must
	// compare against the DAEMON's configured Command (its install path), not
	// the supervisor's own canonicalMcphubPath(); a dev-build supervisor
	// supervising release-path daemons would otherwise mark every live daemon
	// stale on a spurious identity mismatch and respawn duplicates (bug
	// 2026-06-09-supervisor-loses-current-pid-false-quarantine.md). Pull the
	// per-task Command from supervisor-intent.json (canonical-keyed); a task
	// absent from intent or with an empty Command falls back to
	// canonicalMcphubPath() via daemonExpectedIdentityExe.
	intentCommands := supervisorIntentCommandMapForStateDir(stateDir)
	intentPorts := supervisorIntentPortMapForStateDir(stateDir)
	stale := map[string]struct{}{}
	markStale := func(taskName string) {
		if taskName != "" {
			stale[taskName] = struct{}{}
		}
	}
	for taskName, ds := range state.Daemons {
		if ds.State != "running" || ds.CurrentPID <= 0 {
			continue
		}
		expectedExe := daemonExpectedIdentityExe(intentCommands[canonicalSupervisorTaskName(taskName)])
		if ds.PIDGeneration <= 0 || ds.StartedAt == "" || expectedExe == "" {
			markStale(taskName)
			continue
		}
		proof := process.PIDIdentityProof{
			PID:            ds.CurrentPID,
			ExecutablePath: expectedExe,
			StartedAt:      ds.StartedAt,
		}
		if err := currentRunningVerifyPIDIdentityFn(proof); err != nil {
			if !errors.Is(err, process.ErrProcessIdentityUnsupported) || !currentRunningIsPIDAliveFn(ds.CurrentPID) {
				markStale(taskName)
				continue
			}
		}
		startedAt := time.Time{}
		if parsed, err := time.Parse(time.RFC3339Nano, ds.StartedAt); err == nil {
			startedAt = parsed.UTC()
		}
		canonicalTask := canonicalSupervisorTaskName(taskName)
		if port := intentPorts[canonicalTask]; port > 0 {
			// Carry the daemon's configured Command so the inner liveness
			// re-check's PID-identity probe (process.VerifyPIDIdentity in
			// production) compares against the daemon's OWN exe, not the
			// supervisor's canonicalMcphubPath(). Without it a port-bearing
			// daemon whose install path differs from the supervisor binary
			// would false-mismatch here and be cleared as stale (the same bug
			// the outer identity check fixes — 2026-06-09 supervisor false
			// pid_identity_mismatch).
			live, reason := supervisorDaemonEntryLive(api.SupervisorDaemon{
				TaskName: canonicalTask,
				Command:  intentCommands[canonicalTask],
				Port:     port,
			}, DaemonRuntimeEntry{
				State:      daemonRuntimeStateRunning,
				CurrentPID: ds.CurrentPID,
				StartedAt:  startedAt,
			}, time.Now().UTC())
			if !live {
				// Reason routing mirrors the r9 liveness sweep
				// (supervise_liveness.go): a port-stale reason whose PID is
				// STILL ALIVE (port_unbound / port_owner_{mismatch,self,
				// unverified}) means a live-but-wedged mcphub wrapper. We
				// must NOT clear its PID here — clearing would (a) leak the
				// live process (the later liveness sweep reads CurrentPID=0
				// from the just-cleaned state and skips it) AND (b) make
				// currentRunning omit the task so the startup reconcile
				// spawns a DUPLICATE alongside the still-running wrapper
				// (Codex bot #268 r10 P1). Instead keep the entry running
				// (state row untouched → tracker hydrates the live PID →
				// the immediate startup liveness sweep terminates it FIRST
				// then respawns exactly once). Reconcile sees it as running
				// and no-ops, so no duplicate is spawned in the meantime.
				// Only a NOT-alive reason (pid_dead / pid_identity_* via a
				// TOCTOU race after the outer identity check) falls through
				// to markStale → cleared → reconcile respawns (no live
				// process to terminate).
				if !supervisorLivenessReasonHasLivePID(reason) {
					markStale(taskName)
					continue
				}
			}
		}
		result[canonicalTask] = true
		pids[canonicalTask] = runningProcessIdentity{
			PID:           ds.CurrentPID,
			PIDGeneration: ds.PIDGeneration,
			StartedAt:     ds.StartedAt,
		}
	}
	if len(stale) > 0 {
		for taskName := range stale {
			ds := state.Daemons[taskName]
			ds.State = "idle"
			ds.CurrentPID = 0
			ds.StartedAt = ""
			ds.JobProtection = nil
			state.Daemons[taskName] = ds
		}
		if err := api.WriteSupervisorState(statePath, state); err != nil {
			return result, pids, fmt.Errorf("persist stale supervisor-state cleanup: %w", err)
		}
	}
	return result, pids, nil
}

// makeProductionTerminateFn returns the TerminateFunc the Reconciler
// invokes for each daemon that is running but currently stopped by
// daemon-intent.json. The Reconciler carries only the daemon descriptor,
// so startup reconcile threads in the PID snapshot captured from
// supervisor-state.json.
func makeProductionTerminateFn(events *api.SupervisorEventLog, runningPIDs map[string]runningProcessIdentity, tracker *DaemonRuntimeTracker) TerminateFunc {
	return makeProductionTerminateFnWithStatePath(events, runningPIDs, tracker, "")
}

func makeProductionTerminateFnWithStatePath(events *api.SupervisorEventLog, runningPIDs map[string]runningProcessIdentity, tracker *DaemonRuntimeTracker, statePath string) TerminateFunc {
	return func(d api.SupervisorDaemon) error {
		target := runningPIDs[d.TaskName]
		pid := target.PID
		// Live tracker lookup overrides the startup runningPIDs snapshot
		// (closes bot PR#222 P1-5: daemons spawned AFTER supervisor cold
		// restart never appear in runningPIDs — that map is loaded once
		// from supervisor-state.json at startup. Only the tracker holds
		// the live PID for these later spawns. Without this lookup, the
		// new A.2 controller's terminate calls returned "no running PID
		// recorded" and silently skipped killing the child, leaving SM
		// transitions to proceed (StExiting → StIdle on phantom child-exit)
		// while the process was still alive — duplicate-process / port
		// collision risk).
		//
		// runningPIDs remains the fallback for cold-restart-recovery
		// terminates: daemons that were running BEFORE the cold restart
		// but never re-spawned after still have only the startup snapshot.
		if entry, ok := tracker.Get(d.TaskName); ok && entry.CurrentPID > 0 {
			pid = entry.CurrentPID
			target.PID = entry.CurrentPID
			if !entry.StartedAt.IsZero() {
				target.StartedAt = entry.StartedAt.UTC().Format(time.RFC3339Nano)
			}
		}
		if pid <= 0 {
			err := fmt.Errorf("no running PID recorded for task %q", d.TaskName)
			emitDaemonTerminateFailed(events, d, pid, err)
			return err
		}
		state, stateErr := productionQueryPIDStateFn(pid)
		if stateErr != nil {
			err := fmt.Errorf("query PID %d state: %w", pid, stateErr)
			emitDaemonTerminateFailed(events, d, pid, err)
			return err
		}
		if state == process.PIDStateDead {
			emitDaemonTerminateAlreadyExited(events, d, pid)
			tracker.MarkExited(d.TaskName)
			_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
			return nil
		}
		// Identity proof for the terminate path: the target daemon runs from
		// its CONFIGURED command (d.Command — the exact exe the supervisor
		// exec'd), which may differ from the supervisor's own binary. Verify
		// (and later terminate) against the daemon's exe, NOT the supervisor's
		// canonicalMcphubPath(); otherwise a dev-build supervisor cannot verify
		// — and therefore cannot kill — release-path daemons, worsening the
		// orphan/port-fight (bug
		// 2026-06-09-supervisor-loses-current-pid-false-quarantine.md). This
		// single proof is threaded through verify → terminate → finish below.
		proof := process.PIDIdentityProof{
			PID:            pid,
			ExecutablePath: daemonExpectedIdentityExe(d.Command),
			StartedAt:      target.StartedAt,
		}
		if err := productionVerifyPIDIdentityFn(proof); err != nil {
			if errors.Is(err, process.ErrProcessAlreadyExited) {
				emitDaemonTerminateAlreadyExited(events, d, pid)
				tracker.MarkExited(d.TaskName)
				_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
				return nil
			}
			if !errors.Is(err, process.ErrProcessIdentityMismatch) {
				emitDaemonTerminateFailed(events, d, pid, err)
				return err
			}
			_ = events.Emit(api.SupervisorEvent{
				Severity: api.SupervisorEventSeverityWarn,
				Source:   "lifecycle",
				Event:    "daemon-terminate-aborted-pid-reuse",
				TaskName: d.TaskName,
				Body: map[string]any{
					"pid":    pid,
					"reason": err.Error(),
				},
			})
			return nil
		}

		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   "lifecycle",
			Event:    "daemon-terminate-requested",
			TaskName: d.TaskName,
			Body: map[string]any{
				"pid": pid,
			},
		})

		if err := productionTerminatePIDWithIdentityFn(proof); err != nil {
			if errors.Is(err, process.ErrProcessAlreadyExited) {
				emitDaemonTerminateAlreadyExited(events, d, pid)
				tracker.MarkExited(d.TaskName)
				_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
				return nil
			}
			if errors.Is(err, process.ErrProcessIdentityMismatch) {
				_ = events.Emit(api.SupervisorEvent{
					Severity: api.SupervisorEventSeverityWarn,
					Source:   "lifecycle",
					Event:    "daemon-terminate-aborted-pid-reuse",
					TaskName: d.TaskName,
					Body: map[string]any{
						"pid":    pid,
						"reason": err.Error(),
					},
				})
				return nil
			}
			emitDaemonTerminateFailed(events, d, pid, err)
			return err
		}
		if err := finishProductionTerminate(proof, d, events); err != nil {
			emitDaemonTerminateFailed(events, d, pid, err)
			return err
		}

		tracker.MarkTerminated(d.TaskName)
		_ = events.Emit(api.SupervisorEvent{
			Severity: api.SupervisorEventSeverityInfo,
			Source:   "lifecycle",
			Event:    "daemon-terminated",
			TaskName: d.TaskName,
			Body: map[string]any{
				"pid": pid,
			},
		})
		if err := persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName); err != nil {
			return err
		}
		return nil
	}
}

func emitDaemonTerminateAlreadyExited(events *api.SupervisorEventLog, d api.SupervisorDaemon, pid int) {
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityInfo,
		Source:   "lifecycle",
		Event:    "daemon-terminate-already-exited",
		TaskName: d.TaskName,
		Body: map[string]any{
			"pid": pid,
		},
	})
}

func emitDaemonTerminateFailed(events *api.SupervisorEventLog, d api.SupervisorDaemon, pid int, err error) {
	body := map[string]any{"err": err.Error()}
	if pid > 0 {
		body["pid"] = pid
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityError,
		Source:   "lifecycle",
		Event:    "daemon-terminate-failed",
		TaskName: d.TaskName,
		Body:     body,
	})
}

// mergeDaemonEnv merges parent env with descriptor manifest and overlay
// env in deterministic key order.
//
// Precedence (low → high): parent < manifest < overlay. The third arg
// is the Task 2.8 overlay scaffold; Phase 2.7 callers pass nil until
// the overlay loader wires in.
//
// Both-empty fast path: when manifest and overlay are both empty (nil
// or zero-length), return nil so the caller can leave cmd.Env=nil and
// let the child inherit os.Environ() directly. This preserves the
// historical env-less-daemon behavior after the spawn gate was
// removed (Task 2.7 acceptance criterion #5).
//
// Windows case-insensitive PATH-family normalize: on Windows the env
// block is case-insensitive (a child seeing two of "PATH"/"Path"/"path"
// reads only one, by undefined kernel selection). The merge folds
// every key under its uppercase form for collision detection so the
// highest-precedence source wins exactly once; the OUTPUT preserves
// the original casing of that winning source. POSIX keeps the
// historical case-sensitive behavior (different cases are different
// keys).
//
// Determinism: keys are emitted sorted by their uppercase normalized
// form. Spec ref:
// docs/superpowers/specs/2026-05-19-servers-matrix-lsp-and-env-revamp-design.md
// §"Spawn-time env merge" + plan Task 2.7 / I1.
func mergeDaemonEnv(parent []string, manifest, overlay map[string]string) []string {
	if len(manifest) == 0 && len(overlay) == 0 {
		return nil
	}

	winCaseFold := runtime.GOOS == "windows"

	// normKey returns the lookup key used for collision detection.
	// On Windows it folds to uppercase so PATH/Path/path collide.
	// On POSIX it returns the key verbatim.
	normKey := func(k string) string {
		if winCaseFold {
			return strings.ToUpper(k)
		}
		return k
	}

	// entries holds the winning "<key>=<value>" for each normalized
	// key. Later writes (manifest after parent, overlay after
	// manifest) overwrite earlier ones, so the highest-precedence
	// source wins.
	entries := make(map[string]string)

	// Parent goes in first. Lines without '=' are skipped (defensive;
	// os.Environ() never emits malformed entries but a test caller
	// might supply hand-crafted input).
	for _, kv := range parent {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		entries[normKey(kv[:eq])] = kv
	}

	// Manifest overrides parent for the same normalized key.
	// Within a single layer, two source keys can collide under
	// Windows case-fold (e.g. "PATH" and "Path" both in the manifest
	// map). Sort the source keys before applying so the
	// last-write-wins outcome is deterministic across map iteration
	// orders. Sort is by original (non-normalized) key so on POSIX
	// the two cases remain distinct buckets; on Windows the sort is
	// still deterministic and the later case (e.g. "Path" > "PATH"
	// in lexicographic order) wins.
	mKeys := make([]string, 0, len(manifest))
	for k := range manifest {
		mKeys = append(mKeys, k)
	}
	sort.Strings(mKeys)
	for _, k := range mKeys {
		entries[normKey(k)] = k + "=" + manifest[k]
	}

	// Overlay overrides both for the same normalized key. Same
	// within-layer sort discipline as above.
	oKeys := make([]string, 0, len(overlay))
	for k := range overlay {
		oKeys = append(oKeys, k)
	}
	sort.Strings(oKeys)
	for _, k := range oKeys {
		entries[normKey(k)] = k + "=" + overlay[k]
	}

	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]string, 0, len(entries))
	for _, k := range keys {
		out = append(out, entries[k])
	}
	return out
}

// overlayKeySet returns the overlay map's keys (original spelling) in
// deterministic sorted order. The supervisor injects this set via
// MCPHUB_DAEMON_ENV_OVERLAY_KEYS at spawn time so the wrapper's
// reload-FAILURE path can reconstruct the overlay map from os.Environ()
// without a successful overlay-file reload. Sorting makes the injected
// value stable across map-iteration order (matters for the audit trail and
// for deterministic tests). Returns nil for an empty/nil map so the caller
// can treat it as "no overlay applied".
func overlayKeySet(overlay map[string]string) []string {
	if len(overlay) == 0 {
		return nil
	}
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// isSerenaProxyDescriptor reports whether a SupervisorDaemon descriptor is a
// serena per-workspace proxy row, identified by its wrapper argv carrying the
// `daemon serena-proxy` subcommand (the shape
// api.BuildSupervisorDaemonsForSerena emits:
// `daemon serena-proxy --server … --workspace … --port … --task-name …`).
// Global/legacy daemon rows carry `daemon --server … --daemon …` and return
// false. This is the single classification both the legacy-skip guard and the
// intent-path env-channel injection share, so they can never diverge on what
// counts as a serena-proxy row.
func isSerenaProxyDescriptor(d api.SupervisorDaemon) bool {
	return len(d.Args) >= 2 && d.Args[0] == "daemon" && d.Args[1] == "serena-proxy"
}

// isLSPWorkspaceProxyDescriptor reports whether a SupervisorDaemon descriptor is
// a workspace-scoped LSP proxy row, identified by Server == "mcp-language-server"
// AND its wrapper argv carrying the `daemon workspace-proxy` subcommand (the
// shape api.BuildSupervisorDaemonForLSP emits:
// `daemon workspace-proxy --port … --workspace … --language …`). Serena-proxy
// rows (`daemon serena-proxy …`) and global/legacy daemon rows
// (`daemon --server … --daemon …`) return false. This is the single
// classification the reconcile orphan-exclusion guard uses, so it can never
// diverge from the descriptor the LSP register/unregister path builds.
func isLSPWorkspaceProxyDescriptor(d api.SupervisorDaemon) bool {
	return d.Server == "mcp-language-server" &&
		len(d.Args) >= 2 && d.Args[0] == "daemon" && d.Args[1] == "workspace-proxy"
}

// lspWorkspaceProxyArgValue returns the value of the named flag (e.g.
// "--workspace", "--language") from an LSP workspace-proxy descriptor's argv,
// or "" if absent. The argv shape is flat `--flag value` pairs, so a linear
// scan for the flag token followed by its value is sufficient.
func lspWorkspaceProxyArgValue(d api.SupervisorDaemon, flag string) string {
	for i := 0; i+1 < len(d.Args); i++ {
		if d.Args[i] == flag {
			return d.Args[i+1]
		}
	}
	return ""
}

// emitOrphanedLSPDescriptorSkipped emits the single operator-actionable warn
// event fired whenever the reconcile excludes an orphaned LSP workspace-proxy
// descriptor (one whose backing workspaces.yaml row is gone) from the
// spawn-desired set. It is the one owner of this event body so the STARTUP
// reconciler guard (supervise_reconcile.go) and the APPLY-MODE IPC drift
// classifier (supervise_reconcile_ipc.go) can never diverge on the message,
// severity, or remediation guidance. Best-effort: a nil log is a no-op.
func emitOrphanedLSPDescriptorSkipped(events *api.SupervisorEventLog, d api.SupervisorDaemon) {
	if events == nil {
		return
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityWarn,
		Source:   "lifecycle",
		Event:    "orphaned-lsp-descriptor-skipped",
		TaskName: d.TaskName,
		Body: map[string]any{
			"server":    d.Server,
			"workspace": d.Workspace,
			"language":  lspWorkspaceProxyArgValue(d, "--language"),
			"reason":    "LSP workspace-proxy descriptor has no backing registry row (workspaces.yaml); the proxy would exit 1 \"not registered\" and churn into quarantine, so it is excluded from the reconcile spawn-desired set",
			"action":    "re-register the workspace language (`mcphub workspace register` / `mcphub install`) to re-materialize the registry row, or remove this stale supervisor-intent descriptor",
		},
	})
}

// appendSupervisorIntentChannel returns cmdEnv with the
// MCPHUB_SUPERVISOR_INTENT_PATH control-channel var appended LAST so it wins
// over any same-key entry the manifest/overlay merge may have produced (Go's
// exec honors the last occurrence of a duplicate key). When cmdEnv is nil — the
// case where the spawned child would otherwise inherit os.Environ() — it
// materializes os.Environ() first so the appended var survives while preserving
// the inherit-parent semantics. Pure (modulo os.Environ() when cmdEnv==nil) so
// the clobber-immunity property is unit-testable without spawning (bot PR #246
// P2).
func appendSupervisorIntentChannel(cmdEnv []string, intentPath string) []string {
	if cmdEnv == nil {
		cmdEnv = os.Environ()
	}
	return append(cmdEnv, api.SupervisorIntentPathEnvVar+"="+intentPath)
}

// resolveSpawnIntentChannelPath returns the supervisor-intent.json path the
// spawn fn injects via MCPHUB_SUPERVISOR_INTENT_PATH for a serena-proxy child.
//
// It derives the path from the supervisor's ALREADY-RESOLVED state dir (the dir
// of statePath, the same dir the supervisor reads its own intent from at
// runSupervise: filepath.Join(stateDir, "supervisor-intent.json")), so the
// channel value is byte-identical to that path. It deliberately does NOT call
// api.DefaultSupervisorIntentPath() in the resolved case: that re-runs
// api.DaemonStateDir, which — unlike the supervisor's own cli stateDirFunc —
// does NOT honor MCPHUB_STATE_DIR_OVERRIDE and instead resolves via
// SetDaemonStateRootForTest / a POSIX HOME, so it can resolve to a DIFFERENT dir
// than the supervisor's actual stateDir and hand the serena-proxy a path where
// its own descriptor does not exist (bot PR #246 r2 P3; mechanism corrected per
// review — the override is read by cli stateDirFunc, not by api.DaemonStateDir).
//
// statePath == "" is the makeProductionSpawnFn test/manual wrapper: there is no
// resolved state dir to derive from, so it falls back to DefaultSupervisorIntentPath
// (the pre-r2 behavior, correct when the supervisor's state dir is not threaded).
func resolveSpawnIntentChannelPath(statePath string) (string, error) {
	if statePath != "" {
		return filepath.Join(filepath.Dir(statePath), "supervisor-intent.json"), nil
	}
	return api.DefaultSupervisorIntentPath()
}

// makeProductionSpawnFn is a thin compat wrapper that calls
// makeProductionSpawnFnWithStatePath with empty overlay/statePath
// inputs. Preserved for callers that don't need the overlay+respawn
// wiring (currently none in production after the per-spawn Job
// refactor; kept for symmetry with the WithStatePath form).
//
// The SpawnFunc returned here is what the Reconciler invokes for
// each daemon that needs to be (re)started. Each call:
//
//   - builds an exec.Cmd from the SupervisorDaemon descriptor
//     (command, args, env, workspace)
//   - applies process.NoConsole so no console window pops on Windows
//   - allocates a per-spawn Job Object (ADR #239 Option B) and
//     routes through process.StartWithJob (Windows:
//     PROC_THREAD_ATTRIBUTE_JOB_LIST assign-at-create; POSIX: thin
//     cmd.Start() shim per start_with_job_other.go)
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
// in sorted order, matching the v0.4.x daemon-host spawn convention
// while keeping duplicate-case Windows keys deterministic.
//
// Closes the orphaned-godoc P3 nit sonnet pr-review-toolkit flagged on
// PR #241 (separator comment between the two doc blocks meant godoc
// rendered only the second).
func makeProductionSpawnFn(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker) SpawnFunc {
	return makeProductionSpawnFnWithStatePath(events, tracker, "", nil, "", nil)
}

// crashEvent is what the spawn fn posts to the respawn dispatcher
// after observing a non-clean child exit. The dispatcher reads these
// events, computes backoff via the per-task sliding window in the
// DaemonRuntimeTracker, and schedules a respawn (or quarantines).
type crashEvent struct {
	Daemon   api.SupervisorDaemon
	ExitCode int
	WaitErr  error
}

// makeProductionSpawnFnWithStatePath constructs the production spawn
// closure used by the reconciler. The overlay parameter wires the
// per-daemon env-overlay file into the spawn pipeline (per the
// servers-matrix LSP + env-overlay revamp). When crashCh is non-nil
// and the spawned child exits non-cleanly (non-zero exit code OR a
// non-nil Wait error), the wait goroutine posts a crashEvent to that
// channel so an auto-respawn dispatcher can react. Production passes
// real values for both; legacy callers (makeProductionSpawnFn, tests)
// pass nil to preserve the existing "no overlay, spawn once, no
// respawn" behavior.
func makeProductionSpawnFnWithStatePath(events *api.SupervisorEventLog, tracker *DaemonRuntimeTracker, statePath string, overlay *daemon_env_overlay.Overlay, overlayPath string, crashCh chan<- crashEvent) SpawnFunc {
	return func(d api.SupervisorDaemon) error {
		// NOTE (bot PR #246 r2 P2): the legacy nil-RuntimeSpec serena-proxy SKIP
		// no longer lives here. r1 expressed it as `return nil` inside this
		// closure, but the controller's executeSideEffect treats a nil spawn
		// error as SUCCESS → posts EvHealthOK → StSpawning → StRunning, leaving a
		// PHANTOM running daemon. The skip is now applied at EVERY path that
		// reaches spawn: Reconciler.Reconcile excludes such rows from its
		// spawn-desired set (supervise_reconcile.go), and the IPC respawn handler
		// refuses them before calling spawnFn (supervise_respawn.go, r2 P2-1) — so
		// a legacy nil-spec row never reaches this closure. The serena-proxy
		// launcher keeps its own nil-spec fail-loud as last-resort defense-in-depth.
		cmd := exec.Command(d.Command, d.Args...)
		if d.Workspace != "" {
			cmd.Dir = d.Workspace
		}
		// Live overlay reload per-spawn (closes bot PR#222 P1-1: spawn
		// closure captured the startup overlay snapshot, so later
		// /api/daemon/env edits written to daemon-env-overrides.yaml
		// never reached respawned processes — the GUI Apply+Restart
		// workflow silently ignored new config). Re-reading the
		// overlay file on each spawn adds ~ms of disk I/O but spawns
		// are infrequent (per-daemon, on backoff/quarantine/manual
		// respawn paths) so the cost is dominated by process creation.
		//
		// Load errors fall back to the cached startup overlay so a
		// transient permission flip or corrupt-mid-write doesn't break
		// spawn entirely; warn audit emit makes the degradation visible.
		current := overlay
		if overlayPath != "" {
			if fresh, ferr := daemon_env_overlay.Load(overlayPath); ferr == nil {
				current = fresh
			} else if events != nil {
				_ = events.Emit(api.SupervisorEvent{
					Severity: "warn", Source: "lifecycle",
					Event:    "daemon-env-overlay-spawn-reload-failed",
					TaskName: d.TaskName,
					Body: map[string]any{
						"path":     overlayPath,
						"err":      ferr.Error(),
						"fallback": "using cached startup overlay snapshot",
					},
				})
			}
		}
		// Per-task overlay lookup. Returns nil when no row matches the
		// daemon's TaskName or when overlay itself is nil — both fold
		// to "no overlay env for this daemon" so the merge degrades to
		// the manifest-only path (which itself may be nil → cmd.Env
		// stays nil → child inherits os.Environ).
		overlayEnv := daemon_env_overlay.LookupOverlay(current, d.TaskName)
		// ${parent_path} expansion runs BEFORE the merge so the
		// substituted value participates in case-insensitive PATH
		// collision resolution (Windows: PATH/Path/path all collide
		// under mergeDaemonEnv's normalizer). Expansion errors are
		// non-fatal: unknown tokens are logged via ExpandParentPath's
		// own emit path; the daemon spawns with the literal token in
		// its env so the operator sees the failure in the child's
		// observed PATH rather than a silent skip.
		overlayApplied := len(overlayEnv) > 0
		if len(overlayEnv) > 0 {
			if expanded, err := daemon_env_overlay.ExpandParentPath(overlayEnv, os.Environ()); err == nil {
				overlayEnv = expanded
			} else {
				// Audit: ${parent_path} expansion failed (unknown token
				// or some other resolution failure). The daemon still
				// spawns with the literal env block; the operator sees
				// the failure mode reflected in the child's PATH so
				// the audit row + the broken behaviour are correlated.
				_ = api.LogHubMcpEvent("warn", "daemon-env-overlay-parent-path-resolve-failed", map[string]any{
					"task_name": d.TaskName,
					"err":       err.Error(),
				})
			}
		}
		// Phase 2.7 spawn-gate removal: previously this site was
		// guarded by `if len(d.Env) > 0` so overlay-only spawns (no
		// manifest env, but operator overlay supplies values) would
		// silently fall through to inherited parent env. mergeDaemonEnv
		// now returns nil when manifest+overlay are both empty, which
		// is the only case where cmd.Env should stay nil (inherit
		// os.Environ directly).
		if merged := mergeDaemonEnv(os.Environ(), d.Env, overlayEnv); merged != nil {
			cmd.Env = merged
		}
		if overlayApplied {
			cmd.Env = appendDaemonOverlayAppliedMarker(cmd.Env)
			// Inject the applied overlay KEY SET alongside the APPLIED
			// marker. The wrapper's marker-present reload-FAILURE path
			// reconstructs the overlay map from these keys (reading each
			// key's already-expanded value back from os.Environ) when the
			// overlay file is unreadable, so a key present in BOTH the
			// manifest and the cached overlay still resolves to the
			// operator override instead of the manifest default in cfg.Env
			// (closes Codex bot #268 daemon.go:380 P2). Keys are spelled as
			// the overlay stored them; appendDaemonOverlayKeys strips any
			// spoofed value first (same discipline as the APPLIED marker).
			cmd.Env = appendDaemonOverlayKeys(cmd.Env, overlayKeySet(overlayEnv))
		}

		// Intent-path control channel (bot PR #246 P2). A serena-proxy resolves
		// its supervisor-intent.json descriptor by --task-name; on POSIX that
		// path resolution honors HOME / XDG_*_HOME. Because the serena CHILD's
		// manifest env (d.Env) may redirect HOME / XDG for the upstream serena
		// data dir AND that env is what we just merged into cmd.Env, the proxy
		// would resolve its CONTROL-PLANE intent path against the child's home —
		// the wrong dir — and never find its own descriptor. Inject the
		// supervisor's already-resolved canonical intent path as a dedicated
		// MCPHUB_SUPERVISOR_INTENT_PATH var the proxy reads first
		// (api.ResolveSupervisorIntentPathForProxy). This MUST run AFTER the
		// merge so the manifest/overlay env can never clobber it (cmd.Env honors
		// the LAST occurrence of a duplicate key), and the materialized
		// EnvRefs/d.Env still apply to the serena CHILD only — the proxy passes
		// them down via daemon.HTTPHost, not via its own process env. Scoped to
		// serena-proxy rows so no other daemon's env is touched.
		if isSerenaProxyDescriptor(d) {
			if intentPath, perr := resolveSpawnIntentChannelPath(statePath); perr == nil {
				// appendSupervisorIntentChannel materializes os.Environ() when
				// cmd.Env is nil (preserving inherit-parent) and appends the
				// channel var LAST so the merged manifest/overlay env cannot
				// clobber it. Extracted as a pure helper so the clobber-immunity
				// property is unit-testable without spawning. intentPath is the
				// sibling of the supervisor's resolved statePath (NOT a fresh
				// DefaultSupervisorIntentPath resolution) so it matches the dir
				// the supervisor reads its own intent from (bot PR #246 r2 P3).
				cmd.Env = appendSupervisorIntentChannel(cmd.Env, intentPath)
			} else if events != nil {
				// Resolution failure only reaches here on the statePath=="" test/
				// manual wrapper (DefaultSupervisorIntentPath fallback could not
				// resolve the state dir) — surface it; the proxy will fall
				// back to its own DefaultSupervisorIntentPath (same failure mode
				// it would hit anyway). Non-fatal: do not block the spawn.
				_ = events.Emit(api.SupervisorEvent{
					Severity: "warn",
					Source:   "lifecycle",
					Event:    "supervisor-intent-path-channel-unresolved",
					TaskName: d.TaskName,
					Body: map[string]any{
						"err":      perr.Error(),
						"fallback": "serena-proxy will resolve its intent path via DefaultSupervisorIntentPath (may be wrong under a child-overlaid HOME)",
					},
				})
			}
		}
		process.NoConsole(cmd)

		// PER-SPAWN Job Object (ADR #239 Option B, decision deadline
		// BEFORE Phase 9). Each spawn allocates its own Job so
		// orphan cleanup (TerminateJobObject) is task-scoped — kills
		// only this daemon's tree, not every healthy daemon. Replaces
		// the previous design where runSupervise allocated one shared
		// Job for the whole supervisor (bot P1 on PR #238 331b0df
		// flagged the shared-Job kill hazard).
		//
		// Constraint (a) per ADR step 1: preserve the existing
		// non-fatal fallback. If per-spawn Job creation fails (rare:
		// SetInformationJobObject denial in restrictive corp-managed
		// hosts), warn + daemonJob=nil + proceed via cmd.Start
		// WITHOUT StartWithJob. The daemon spawns without orphan-
		// protection; this matches the v0.5.x-pre-ADR fallback
		// semantic at runSupervise:670-679.
		daemonJob, jobErr := process.NewKillOnCloseJob()
		// Record per-spawn Job Object allocation state in the runtime
		// tracker so it surfaces through supervisor-state.json + IPC
		// status response + GUI Dashboard. process.JobProtectionStatus
		// owns platform semantics: Windows emits &true on success and
		// &false on the documented non-fatal fallback below; POSIX
		// returns nil because its Job stub is a no-op, not real Job
		// Object orphan protection. The MarkJobProtection call is the
		// canonical writer; MarkSpawned preserves it for the running
		// spawn, and no-current-spawn transitions clear it.
		// Closes consultant strategic concern #1 on PR #241
		// (silent-degradation gap when fallback fires).
		tracker.MarkJobProtection(d.TaskName, process.JobProtectionStatus(jobErr))
		if jobErr != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "lifecycle",
				Event:    "per-spawn-job-create-failed",
				TaskName: d.TaskName,
				Body: map[string]any{
					"err":      jobErr.Error(),
					"fallback": "proceeding via cmd.Start without StartWithJob; daemon spawns without Job Object orphan-protection",
				},
			})
			daemonJob = nil
		}
		// Constraint (b) per ADR step 1: explicit ownership-transfer
		// pattern with handedOff flag. Parent spawn closure owns
		// daemonJob.Close() on EVERY non-success exit path; wait
		// goroutine takes ownership ONLY after handedOff=true is
		// written (before goroutine launches), so the goroutine
		// alone owns the deferred Close after cmd.Wait().
		//
		// handedOff is written ONLY by this parent goroutine (no
		// race), read ONLY by the parent's deferred close after
		// return.
		handedOff := false
		defer func() {
			if !handedOff && daemonJob != nil {
				_ = daemonJob.Close()
			}
		}()

		var (
			pid      int
			startErr error
		)
		if daemonJob != nil {
			pid, startErr = process.StartWithJob(daemonJob, cmd)
		} else {
			startErr = cmd.Start()
			if startErr == nil && cmd.Process != nil {
				pid = cmd.Process.Pid
			}
		}
		if startErr != nil {
			isOrphan := errors.Is(startErr, process.ErrSpawnPostCreate)
			tracker.MarkSpawnFailed(d.TaskName, startErr)
			_ = events.Emit(api.SupervisorEvent{
				Severity: "error",
				Source:   "lifecycle",
				Event:    "daemon-spawn-failed",
				TaskName: d.TaskName,
				Body: map[string]any{
					"err":     startErr.Error(),
					"command": d.Command,
					"orphan":  isOrphan,
				},
			})
			_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
			if isOrphan {
				// Job-level kill of the Windows post-create orphan
				// via daemonJob.TerminateAll. TerminateJobObject
				// kills the WHOLE tree (orphan root + uvx/python or
				// npx/node descendants) by Job handle - no PID
				// argument means no PID-recycling race between
				// StartWithJob return and our kill call. Safe
				// because daemonJob is task-scoped (this spawn
				// attempt only); no risk of nuking healthy daemons.
				// Step 3 of ADR #239 implementation outline.
				//
				// daemonJob.TerminateAll(5000) blocks until
				// ActiveProcesses == 0 OR 5s deadline; nil return =
				// safe to rebind port for backoff respawn,
				// non-nil = orphan tree may still be alive,
				// do NOT respawn.
				//
				// Fallback: if daemonJob is nil (per-spawn Job
				// create failed earlier), there's no Job to
				// TerminateAll - drop back to the PID-based
				// BestEffortKillByPID with the same wait-success/
				// wait-failure semantic.
				var killErr error
				killMethod := "TerminateJobObject"
				if daemonJob != nil {
					killErr = daemonJob.TerminateAll(5000)
				} else {
					killErr = process.BestEffortKillByPID(pid)
					killMethod = "TerminateProcess(pid)+fallback"
				}
				orphanBody := map[string]any{
					"pid":            pid,
					"kill_succeeded": killErr == nil,
					"kill_method":    killMethod,
				}
				// pidForState is the PID we persist into
				// supervisor-state.json as orphan_pid. Default is the
				// root spawn pid, but on TerminateAll timeout the root
				// may have died while a descendant still holds the
				// port — closing bot P2 on PR #241. When that
				// happens, query the surviving Job members and
				// use the FIRST as the operator-visible orphan_pid
				// so `taskkill /F /T /PID <orphan_pid>` lands on
				// a live process. Also surface the full surviving
				// list in the audit body for operator action.
				pidForState := pid
				severity := "warn"
				if killErr != nil {
					orphanBody["kill_error"] = killErr.Error()
					var survivors []uint32
					if daemonJob != nil {
						survivors, _ = daemonJob.MemberPIDs()
					}
					if len(survivors) > 0 {
						orphanBody["surviving_pids"] = survivors
						pidForState = int(survivors[0])
						orphanBody["note"] = "Orphan tree kill failed - root pid (originally returned by StartWithJob) may be dead while one or more descendants are still alive and holding the port. Returning RAW startErr (no errSpawnPreChild wrap) so controller does NOT synth EvChildExit + backoff retry. Daemon stays in StSpawning (preferable to duplicate-daemon risk). orphan_pid in supervisor-state.json is the FIRST surviving member from the Job (NOT the dead root) so the GUI Dashboard and `mcphub status --json` surface a live PID. Operator: run `taskkill /F /T /PID <each>` for EVERY pid in surviving_pids; using only orphan_pid may leave siblings alive."
					} else {
						orphanBody["note"] = "Orphan kill failed - tree may still be alive but Job member enumeration returned empty (either MemberPIDs syscall failed, or the kernel reaped members after TerminateAll timeout fired but before this query). Returning RAW startErr (no errSpawnPreChild wrap) so controller does NOT synth EvChildExit + backoff retry. Daemon stays in StSpawning (preferable to duplicate-daemon risk). orphan_pid in supervisor-state.json is the original root pid (best available); the actual descendant holding the port may differ. Operator: check `mcphub status --json` and supplement with `taskkill /F /T /IM <wrapper-image>` if root pid is dead."
					}
					severity = "error"
				} else {
					orphanBody["note"] = "Orphan tree (root + descendants) terminated cleanly via " + killMethod + ". Wrapping with errSpawnPreChild so the controller synth EvChildExit + backoff timer drives retry. Fresh respawn binds the port cleanly."
				}
				_ = events.Emit(api.SupervisorEvent{
					Severity: severity,
					Source:   "lifecycle",
					Event:    "daemon-spawn-orphan-detected",
					TaskName: d.TaskName,
					Body:     orphanBody,
				})
				if killErr != nil {
					tracker.MarkSpawnFailedPreservePID(d.TaskName, startErr, pidForState)
					_ = persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName)
					return startErr
				}
			}
			// Common pre-child case (and orphan-with-kill-success):
			// wrap with errSpawnPreChild so the supervisor controller
			// synthesizes EvChildExit and routes through
			// StBackoffWaiting + backoff timer. The parent's deferred
			// Close runs because handedOff stays false.
			return fmt.Errorf("%w: %v", errSpawnPreChild, startErr)
		}
		startedAt := time.Now().UTC()
		tracker.MarkSpawned(d.TaskName, pid, startedAt)
		taskName := d.TaskName
		spawnedPID := pid
		// Emit daemon-spawned BEFORE starting the wait goroutine. A
		// fast-failing wrapper (e.g. uvx fetch error) can exit within
		// microseconds of cmd.Start returning; if we started the wait
		// goroutine first, it could emit daemon-exited BEFORE this
		// daemon-spawned line landed in the audit log — inverting the
		// timeline operators rely on to diagnose the exact class of
		// failure this PR exists to surface. The spawnLogged channel
		// then gates the goroutine so daemon-exited never precedes
		// daemon-spawned in the log even if Emit itself is slow.
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
		spawnLogged := make(chan struct{})
		close(spawnLogged) // Emit above completed before this point; goroutine starts unblocked.
		// Transfer Job-handle ownership to the wait goroutine BEFORE
		// launching it. After this assignment, the parent spawn
		// closure's deferred close is a no-op (handedOff==true) and
		// the goroutine alone owns daemonJob.Close() after cmd.Wait()
		// returns. ADR step 1 constraint (b).
		handedOff = true
		jobForWait := daemonJob // capture before goroutine launch
		go func() {
			defer func() {
				if jobForWait != nil {
					_ = jobForWait.Close()
				}
			}()
			<-spawnLogged
			waitErr := cmd.Wait()
			// Diagnostic emit: without this, a wrapper that exits
			// immediately (e.g. uvx fails to fetch package, port
			// already bound, env vars missing) leaves no trace —
			// supervisor-state.json shows state="idle" with bumped
			// pid_generation and the operator has zero data on why.
			// Emitting daemon-exited with pid + exit_code + wait_err
			// closes that diagnostic gap.
			exitCode := 0
			var ee *exec.ExitError
			if errors.As(waitErr, &ee) {
				exitCode = ee.ExitCode()
			}
			severity := "info"
			if waitErr != nil || exitCode != 0 {
				severity = "warn"
			}
			body := map[string]any{
				"pid":       spawnedPID,
				"exit_code": exitCode,
			}
			if waitErr != nil {
				body["wait_err"] = waitErr.Error()
			}
			_ = events.Emit(api.SupervisorEvent{
				Severity: severity,
				Source:   "lifecycle",
				Event:    "daemon-exited",
				TaskName: taskName,
				Body:     body,
			})
			tracker.MarkExited(taskName)
			_ = persistDaemonRuntimeTracker(events, tracker, statePath, taskName)
			// Child-exit notification: post EVERY exit (clean and
			// non-clean) onto crashCh so the controller's single FIFO
			// event loop observes the real exit and can drive the SM
			// transition for the task. Cleanliness (exit_code==0 &&
			// no waitErr) is carried implicitly in the crashEvent fields
			// and re-derived by runCrashEventBridge into the LoopEvent's
			// `clean_exit` body flag.
			//
			// Why clean exits must also post (Codex bot #268 P1,
			// supervisor_controller.go:893): during a controller-driven
			// restart an OWN daemon that handles SIGTERM and exits 0 has
			// NO real EvChildExit posted if clean exits are suppressed
			// here. The StExiting synthesize gate suppresses the
			// synthetic exit for own-spawned tasks (owned==true), so the
			// SM would wedge in StExiting with queued_action=respawn
			// never consumed — daemon killed, never respawned. Posting
			// the clean exit gives the controller the real EvChildExit it
			// needs to complete the queued respawn.
			//
			// The deliberate-shutdown contract (clean exit on a daemon
			// that was NOT being restarted must NOT trigger respawn) is
			// preserved at the CONTROLLER, not here: handleLoopEvent
			// drops a clean_exit EvChildExit when the task is still in
			// StRunning (no controller-driven exit in flight), so a plain
			// `mcphub stop` / external clean kill at steady state never
			// reaches the StRunning->StBackoffWaiting respawn path.
			//
			// The dispatcher channel may be nil for legacy / test callers
			// — guard with nil-check.
			if crashCh != nil {
				select {
				case crashCh <- crashEvent{Daemon: d, ExitCode: exitCode, WaitErr: waitErr}:
				default:
					// Dispatcher backlog full (>64 concurrent exits
					// pending). Drop this respawn signal and audit-log
					// it. Operator must restart supervisor to recover.
					_ = events.Emit(api.SupervisorEvent{
						Severity: "warn",
						Source:   "lifecycle",
						Event:    "respawn-dispatcher-backlog-full",
						TaskName: taskName,
						Body: map[string]any{
							"exit_code": exitCode,
						},
					})
				}
			}
		}()
		if err := persistDaemonRuntimeTracker(events, tracker, statePath, d.TaskName); err != nil {
			return err
		}
		return nil
	}
}
