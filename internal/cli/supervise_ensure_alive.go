// Package cli — `mcphub supervise --ensure-alive` supervisor-liveness action
// (v0.6 redesign spec §15 P1-b / §5.x Phase 3a).
//
// This is the tiny, ADDITIVE owner-death recovery added BEFORE Phase C/D
// delete the watchdog. The `\mcp-local-hub-liveness` scheduled task (~1-min
// cadence + LogonTrigger, installed by `mcphub setup`) runs this action every
// minute. Its job: if the supervisor/GUI owner died mid-session
// (`taskkill /F mcphub.exe`, OOM, panic, parent Job-Object close), relaunch
// it; otherwise do nothing.
//
// It is DELIBERATELY NOT the full watchdog: no recovery state machine, no new
// state files, no daemon-revival. The supervisor's OWN sweeper
// (supervise_liveness.go) already revives crashed child daemons; this action
// only covers the gap the watchdog never did — the OWNER process itself dying
// (architect TOPOLOGY CORRECTION, §15 P1-b).
//
// RECOVERY TOPOLOGY (§5 permanent fix PART 2 — BOTH death cases recover):
//
//   - GENUINE OWNER death (no live GUI holds the single-instance lock):
//     re-fire the autostart task, so the relaunched `mcphub gui` reaches
//     startGuiServer → ensureSupervisorRunning and re-establishes BOTH the
//     GUI owner and its supervisor.
//   - SUPERVISOR-CHILD death under a LIVE GUI owner (supervisor panic / OOM of
//     just the supervisor PID / `taskkill /F /PID <supervisor>` / an
//     inherited-Job cascade): the supervisor is spawned DETACHED
//     (gui_supervisor_owner_windows.go), so it can die independently while the
//     GUI keeps running (and keeps serving its :9125 hub router). This tick
//     recovers it DIRECTLY — spawn a detached standalone `mcphub supervise`
//     (standaloneRelaunchFn), NOT the autostart GUI task (which under a live
//     GUI would hit ErrSingleInstanceBusy → TryActivateIncumbent and recover
//     nothing while stealing the GUI window). The supervisor singleton flock
//     makes the spawn idempotent; the GUI's poller reconnects to the new
//     supervisor via IPC.
//
//   HISTORY: this second case was previously a SUPPRESSED no-op ("deferred to
//   the GUI side"). Combined with the GUI only respawning supervisors it
//   itself SPAWNED (never ADOPTED ones), the two recovery mechanisms deferred
//   to each other into a PERMANENT deadlock — the §5 live churn. The GUI's own
//   bounded supervisorManager respawn loop (gui_supervisor_owner.go) still
//   self-heals a GUI-SPAWNED supervisor as a fast path; this tick is the
//   authoritative backstop that covers the adopted-then-died and wedged-GUI
//   cases the GUI loop cannot.
//
// runEnsureAlive probes the GUI single-instance owner (guiOwnerAliveFn) only to
// CHOOSE the recovery method (standalone `supervise` vs the autostart gui task),
// no longer to SUPPRESS recovery.
//
// Mechanism (architect-verified; NOT RestartOnFailure — that is the Win11
// 24H2 force-kill bug the watchdog was built around, §15 P1-b):
//
//   - SupervisorRunningUnderStateDir (supervisor_lock.go:265) is the EXISTING
//     flock-authoritative liveness probe. It fails CLOSED: a probe error means
//     liveness is UNDETERMINABLE, which is NOT the same as dead.
//   - probe error  → no-op, exit 0 (undeterminable ≠ dead; the next tick
//     retries).
//   - running=true → no-op, exit 0 (the common case).
//   - running=false AND a live GUI owner is present → recover the supervisor
//     DIRECTLY (§5 permanent fix PART 2): spawn a detached standalone
//     `mcphub supervise` via standaloneRelaunchFn. The supervisor singleton
//     flock makes it idempotent; the GUI is left untouched and its poller
//     reconnects to the new supervisor via IPC. (This REPLACES the old
//     defer-to-GUI suppression, which was a permanent deadlock: liveness
//     deferred to the GUI while the GUI never respawned an adopted supervisor.)
//   - running=false AND no live GUI owner → genuine OWNER death; relaunch via
//     the injectable relaunch seam, which re-fires the autostart task
//     (`schtasks /Run /TN \mcp-local-hub-supervisor`) to re-establish the GUI
//     owner + its supervisor. The GUI/supervisor singleton locks make the
//     relaunch idempotent (no duplicate supervisor).
//
// ALL branches exit 0: this is a best-effort recovery tick, not a gate.
//
// OBSERVABILITY (PR #283 review P3-d): every non-trivial outcome — relaunch
// success and relaunch FAILURE on BOTH the autostart-task and the standalone
// `supervise` paths — is mirrored to a DURABLE sink (`supervisor-events.log`,
// severity warn for the failure cases) in addition to the one-line `out`
// writer. Task
// Scheduler discards the action's stdout/stderr, so without the durable log a
// chronically-failing relaunch (e.g. the autostart task `\mcp-local-hub-supervisor`
// was never installed by `mcphub autostart enable`) would be invisible: exit 0,
// no last-run-result signal, no log. The durable warn restores the fail-loud
// operational contract. The `out` writer's transient resolve-failure line still
// goes to os.Stderr (runEnsureAliveFromState).
//
// LOG-CHURN NOTE (PR #283 review P3-c): a dead supervisor is now RECOVERED on
// the first tick (standalone relaunch when a GUI owner is alive, autostart task
// otherwise), so the steady state emits no recurring warn. Only a PERSISTENTLY-
// FAILING relaunch keeps writing one "liveness-standalone-relaunch-failed" (or
// "liveness-relaunch-failed") warn per ~1-min tick to supervisor-events.log.
// At ~300-400 bytes/entry that is ~0.5 MB/day, so the 10 MB log (single .log.1
// backfile, 16 KB per-entry cap) rotates in ~2-3 weeks of continuous
// relaunch-failure state. This is bounded by the existing rotation + size-cap
// discipline. De-duping per
// gui_owner_pid is intentionally NOT done here: every tick is a SEPARATE
// one-shot process (supervise.go:226), so there is no in-process state that
// survives across ticks to key the de-dup on, and persisting last-emitted
// state would require a NEW state file — explicitly out of scope for this
// additive action ("no new state files", header above). The unrecovered state
// is itself the real problem the operator should fix.
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/scheduler"
)

// livenessRelaunchFn is the injectable relaunch SEAM. Production relaunches
// the supervisor/GUI owner by re-firing the autostart scheduled task; the
// unit test swaps in a recording fake so nothing real spawns. Production
// callers MUST NOT reassign this var directly — setLivenessRelaunchFnForTest
// below is the only allowed write path.
//
// The default relaunches via `schtasks /Run /TN \mcp-local-hub-supervisor`
// (autostart.WindowsTaskName) — the autostart task runs `mcphub gui`, whose
// ensureSupervisorRunning (gui_supervisor_owner.go:88) does the idempotent
// adopt-or-spawn. Re-firing the autostart task is the architect-specified
// relaunch (§15 P1-b), NOT a bare `mcphub supervise` spawn (which would not
// re-establish the GUI owner the autostart task is responsible for).
var livenessRelaunchFn = relaunchSupervisorOwner

// standaloneRelaunchFn is the GUI-INDEPENDENT relaunch SEAM (§5 permanent
// fix PART 2). It is used when the supervisor is down BUT a live GUI owner
// is present: instead of re-firing the autostart GUI task (a no-op
// focus-steal under a live GUI — the old gap-B deadlock), it spawns a
// detached standalone `mcphub supervise` DIRECTLY. The supervisor singleton
// flock makes it idempotent (a racing duplicate exits cleanly via
// supervise.go's singleton path); the GUI keeps serving its window + :9125
// hub router, and its poller reconnects to the new supervisor via IPC.
// Production callers MUST NOT reassign directly — setStandaloneRelaunchFnForTest
// is the only allowed write path.
var standaloneRelaunchFn = spawnStandaloneSupervisor

// spawnStandaloneSupervisor starts `<this-binary> supervise` detached via
// the PART 1 breakaway-tolerant helper and does NOT Wait — the supervisor
// owns its own lifetime and the flock enforces the singleton. Plain
// `supervise` (no --strict-mode): the supervisor seeds strict_mode from the
// canonical supervisor-intent.json itself, so a recovery relaunch needs no
// flag.
func spawnStandaloneSupervisor() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("standalone supervisor relaunch: resolve executable: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(exe); lerr == nil {
		exe = resolved
	}
	build := func() *exec.Cmd {
		c := exec.Command(exe, "supervise")
		c.Stdin = nil
		c.Stdout = nil
		c.Stderr = nil
		configureSupervisorDetach(c)
		return c
	}
	started, err := startSupervisorDetachedBreakaway(build(), build, func(degradeErr error) {
		fmt.Fprintf(os.Stderr, "ensure-alive: standalone supervisor CREATE_BREAKAWAY_FROM_JOB rejected (parent job without BREAKAWAY_OK); spawned flagless: %v\n", degradeErr)
	})
	if err != nil {
		return fmt.Errorf("standalone supervisor relaunch: spawn: %w", err)
	}
	if started.Process != nil {
		_ = started.Process.Release()
	}
	return nil
}

// guiOwnerAliveFn is the injectable GUI-incumbent probe SEAM. It reports
// whether a live `mcphub gui` process currently owns the GUI single-instance
// (pidport) lock — i.e. an OWNER process is still alive even though the
// supervisor child is down. Production resolves the pidport path and runs the
// read-only gui.Probe; the unit test swaps in a recording fake so the real
// %LOCALAPPDATA% pidport is never touched.
//
// Returns (alive, pid). A probe that cannot read the pidport (no file, parse
// error) reports alive=false — the absence of a recorded live owner is the
// genuine OWNER-death case the relaunch path is for. Production callers MUST
// NOT reassign this var directly — setGUIOwnerAliveFnForTest is the only
// allowed write path.
var guiOwnerAliveFn = probeGUIOwnerAlive

// setLivenessRelaunchFnForTest installs a test relaunch function. Returns an
// "uninstall" function tests defer to restore the production wiring before the
// next test runs. Only supervise_ensure_alive_test.go invokes this.
func setLivenessRelaunchFnForTest(fn func() error) func() {
	prev := livenessRelaunchFn
	livenessRelaunchFn = fn
	return func() { livenessRelaunchFn = prev }
}

// setStandaloneRelaunchFnForTest installs a test standalone-relaunch function
// (the GUI-independent recovery path). Returns an "uninstall" function tests
// defer to restore production wiring. Only supervise_ensure_alive_test.go
// invokes this; the default spawns a real detached `mcphub supervise`.
func setStandaloneRelaunchFnForTest(fn func() error) func() {
	prev := standaloneRelaunchFn
	standaloneRelaunchFn = fn
	return func() { standaloneRelaunchFn = prev }
}

// setGUIOwnerAliveFnForTest installs a test GUI-incumbent probe. Returns an
// "uninstall" function tests defer to restore the production wiring. Only
// supervise_ensure_alive_test.go invokes this. The default production probe
// reads the real pidport, so tests MUST install a fake to exercise the
// live-GUI-owner branch without touching the developer's running GUI.
func setGUIOwnerAliveFnForTest(fn func() (bool, int)) func() {
	prev := guiOwnerAliveFn
	guiOwnerAliveFn = fn
	return func() { guiOwnerAliveFn = prev }
}

// probeGUIOwnerAlive reports whether a live `mcphub gui` process owns the GUI
// single-instance (pidport) lock. Read-only: it runs gui.Probe (no destructive
// action) and returns the BARE Verdict.PIDAlive bit, NOT a Verdict-class
// semantic.
//
// PLATFORM CONTRACT (PR #283 review P3-a — the doc must not over-claim):
// Verdict.PIDAlive is set from the OS identity probe (id.Alive,
// single_instance.go:483). It is true only on platforms where that probe can
// OBSERVE the process — Windows amd64 (GA) and Linux (beta, processID
// implemented). On platforms where the identity probe is unsupported (macOS
// and Windows non-amd64), probeOnce force-sets PIDAlive=false
// (single_instance.go:515-517,534-536), AND the VerdictHealthy early-return
// (single_instance.go:499-504) does NOT re-set PIDAlive — so even a perfectly
// healthy, ping-responding live GUI yields Verdict{Class:Healthy,
// PIDAlive:false} there. Consequence: on macOS / Windows-non-amd64 this probe
// returns (false, pid) for a LIVE healthy owner, so the live-GUI-owner
// suppression in runEnsureAlive DEGRADES to the no-op relaunch path — the
// relaunch fires but is inert (scheduler.New returns "not implemented" on
// non-Windows), so the only effect is a misleading "liveness-relaunch-failed"
// warn rather than the accurate live-GUI deferral. This is Windows-GA-posture
// consistent: macOS is preview and Windows-non-amd64 is not a shipped target,
// so suppression silently not engaging there is bounded (no focus-steal, no
// false "relaunched owner"). On Windows amd64 (GA) and Linux (beta) PIDAlive
// is observed correctly and suppression engages as designed.
//
// alive=true → the recorded PID is observed alive → the supervisor-down state
// is a dead-CHILD-under-live-OWNER topology the relaunch cannot recover.
// alive=false → no observable live owner (DeadPID / Malformed / unresolvable
// path, OR an unobservable-but-healthy owner on the probe-unsupported
// platforms above) → treated as genuine OWNER death the relaunch handles.
//
// DELIBERATELY the bare PIDAlive bit, NOT Verdict.Class == VerdictHealthy
// (PR #283 review P3-b, deferred). Keying on PIDAlive treats an
// alive-but-unreachable owner (VerdictLiveUnreachable — alive PID, ping
// failing, e.g. a GUI mid-restart) as a live owner, which preserves the P2
// intent of suppressing the once-per-tick focus-steal whenever an owner
// process exists at all. The alternative (key on VerdictHealthy) would
// eliminate the narrow PID-recycle false-positive — where a recycled PID makes
// a dead owner look alive and suppresses a needed relaunch — and would also
// fix the macOS gap above (VerdictHealthy is a ping-only verdict that holds on
// macOS), but at the cost of relaunching against a LiveUnreachable owner and
// reintroducing the focus-steal the P2 fix removed. That polarity tradeoff is
// a behavior change against the round-1 P2 design, not a clean nit, so it is
// left as-is for this merge; the recycle window is narrow and self-heals on
// the next tick once the recycled PID dies or the pidport is overwritten.
func probeGUIOwnerAlive() (bool, int) {
	pidportPath, err := gui.PidportPath()
	if err != nil {
		// Cannot resolve the pidport path → cannot confirm a live owner.
		// Treat as "no live owner" so the genuine OWNER-death relaunch path
		// is not suppressed by a path-resolution hiccup.
		return false, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v := gui.Probe(ctx, pidportPath)
	return v.PIDAlive, v.PID
}

// relaunchSupervisorOwner re-fires the autostart scheduled task
// (`schtasks /Run /TN \mcp-local-hub-supervisor`), which launches the GUI
// owner whose ensureSupervisorRunning adopt-or-spawns the supervisor. The
// GUI/supervisor singleton locks make this idempotent.
//
// This is the OWNER-death recovery vector — it only fires when no live GUI
// owner holds the single-instance lock (runEnsureAlive gates it on
// guiOwnerAliveFn), so the relaunched `mcphub gui` reaches startGuiServer
// rather than short-circuiting to activate-window.
//
// On Linux/macOS scheduler.New() returns "not implemented"; the relaunch then
// fails loud (caught by the caller and recorded, exit 0). The liveness task is
// a Windows-GA capability in v0.6 (same posture as the watchdog it precedes).
func relaunchSupervisorOwner() error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("liveness relaunch: scheduler: %w", err)
	}
	if err := sch.Run(autostart.WindowsTaskName); err != nil {
		return fmt.Errorf("liveness relaunch: schtasks /Run %s: %w", autostart.WindowsTaskName, err)
	}
	return nil
}

// emitLivenessEvent records a best-effort structured event to
// supervisor-events.log under the given state dir. Mirrors the api-package
// emit idiom (serena_intent_repair.go:431): open the canonical log, emit,
// close. A failure to open/emit is silently non-fatal — the durable log is
// observability, not a gate, and the action must still exit 0.
//
// Uses the BLOCKING Emit (not TryEmit): for the liveness action the event log
// is the ONLY durable record of the relaunch/defer outcome (there is no
// independently-durable state mutation backing it), so the lossy-on-contention
// TryEmit would defeat the diagnosability fix. The ~1-min cadence means
// event-log lock contention against the supervisor's own writers is rare and a
// brief block is acceptable on this non-hot path.
func emitLivenessEvent(stateDir, severity, event, message string, body map[string]any) {
	logger, openErr := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if openErr != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	if body == nil {
		body = map[string]any{}
	}
	body["message"] = message
	_ = logger.Emit(api.SupervisorEvent{
		Severity: severity,
		Source:   api.SupervisorEventSourceLifecycle,
		Event:    event,
		Body:     body,
	})
}

// runEnsureAlive is the body of `mcphub supervise --ensure-alive`. It is the
// testable entrypoint: the cobra wrapper resolves the real state dir via
// stateDirFunc() and passes it here. The unit test passes a temp dir directly
// (per the §11.10 fleet-wipe lesson — the action never touches the REAL
// %LOCALAPPDATA% supervisor.lock when given a temp stateDir).
//
// out receives the one-line outcome (running / relaunched / deferred /
// undeterminable) so a tick is observable in the scheduled-task last-run
// record / a manual invocation. The failure/defer outcomes ALSO land in the
// durable supervisor-events.log (Task Scheduler discards stdout). Returns nil
// on EVERY branch — this is a best-effort recovery tick (exit 0), not a gate.
func runEnsureAlive(stateDir string, out io.Writer) error {
	running, pid, err := api.SupervisorRunningUnderStateDir(stateDir)
	if err != nil {
		// Probe error → liveness UNDETERMINABLE. Fail closed: do NOT
		// relaunch (a relaunch on an undeterminable probe could stack a
		// second owner against a live-but-unprobeable one). Exit 0; the
		// next tick retries.
		fmt.Fprintf(out, "ensure-alive: supervisor liveness undeterminable (probe error); no action: %v\n", err)
		return nil
	}
	if running {
		// Common case: a supervisor holds the lock. No-op.
		fmt.Fprintf(out, "ensure-alive: supervisor running (pid=%d); no action\n", pid)
		return nil
	}

	// Supervisor is down. Distinguish the two topologies and recover BOTH
	// (§5 permanent fix PART 2 — the dead-supervisor-under-live-GUI case is
	// no longer a suppressed deadlock):
	//   - live GUI owner present → the supervisor CHILD died but the GUI
	//     owner (and its :9125 hub router) is fine. Recover the supervisor
	//     DIRECTLY via a detached standalone `mcphub supervise` spawn.
	//   - no live GUI owner → genuine OWNER death. Re-fire the autostart GUI
	//     task to re-establish BOTH the GUI owner and its supervisor.
	if guiAlive, guiPID := guiOwnerAliveFn(); guiAlive {
		// Recover the supervisor WITHOUT touching the GUI. Re-firing the
		// autostart task here would short-circuit to activate-window
		// (single-instance busy) and recover nothing — the old gap-B
		// deadlock. A direct standalone `mcphub supervise` spawn takes the
		// singleton flock (idempotent against a racing duplicate) and the
		// GUI's poller reconnects to it via IPC.
		if relaunchErr := standaloneRelaunchFn(); relaunchErr != nil {
			fmt.Fprintf(out, "ensure-alive: supervisor down under a live GUI owner (pid=%d); standalone supervisor relaunch FAILED (will retry next tick): %v\n", guiPID, relaunchErr)
			emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
				"liveness-standalone-relaunch-failed",
				"supervisor down under a live GUI owner; the GUI-independent standalone supervisor relaunch failed; will retry next tick",
				map[string]any{"gui_owner_pid": guiPID, "error": relaunchErr.Error()})
			return nil
		}
		fmt.Fprintf(out, "ensure-alive: supervisor down under a live GUI owner (pid=%d); relaunched a detached standalone supervisor (GUI-independent recovery)\n", guiPID)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo,
			"liveness-relaunched-supervisor-under-gui",
			"supervisor down while a live GUI owner held the single-instance lock; spawned a detached standalone supervisor to recover it without disturbing the GUI",
			map[string]any{"gui_owner_pid": guiPID})
		return nil
	}

	// No live GUI owner → genuine OWNER death. Relaunch via the seam.
	if relaunchErr := livenessRelaunchFn(); relaunchErr != nil {
		// Best-effort: record + exit 0. The next ~1-min tick retries. Mirror
		// to the durable log so a chronically-failing relaunch (e.g. the
		// autostart task was never installed) is operator-visible despite
		// Task Scheduler discarding stdout.
		fmt.Fprintf(out, "ensure-alive: supervisor not running; relaunch FAILED (will retry next tick): %v\n", relaunchErr)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
			"liveness-relaunch-failed",
			"supervisor down (no live GUI owner) and the owner relaunch failed; will retry next tick",
			map[string]any{
				"relaunch_target": autostart.WindowsTaskName,
				"error":           relaunchErr.Error(),
			})
		return nil
	}
	fmt.Fprintf(out, "ensure-alive: supervisor not running; relaunched owner via %s\n", autostart.WindowsTaskName)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo,
		"liveness-relaunched-owner",
		"supervisor down (no live GUI owner); re-fired the autostart task to relaunch the owner",
		map[string]any{"relaunch_target": autostart.WindowsTaskName})
	return nil
}

// runEnsureAliveFromState is the cobra-facing wrapper: it resolves the
// production state dir, then delegates to runEnsureAlive. Kept separate so the
// state-dir resolution (the one real-fleet-touching call) is isolated from the
// pure action body the unit test drives with a temp dir.
//
// Resolves via the cli-layer stateDirFunc() (supervise.go:159) — NOT
// api.DaemonStateDir() — so it honors MCPHUB_STATE_DIR_OVERRIDE EXACTLY like
// the long-lived supervisor (runSupervise, supervise.go:379) and the serena
// migrate SupervisorRunningUnderStateDir probe (migrate_serena.go:354-359),
// all of which resolve the SAME `supervisor.lock` dir. api.DaemonStateDir()
// does NOT read the override (it is excluded from production builds via the
// `test_state_path_env` build-tag gate), so calling it directly here would
// resolve a DIFFERENT dir than the supervisor's actual lock under any
// override — the lone divergence this fix removes.
func runEnsureAliveFromState() error {
	stateDir, err := stateDirFunc()
	if err != nil {
		// Cannot resolve the state dir → cannot probe. Fail closed (no
		// relaunch) + exit 0; the next tick retries once the resolver
		// recovers. Matches the probe-error polarity in runEnsureAlive.
		// This is the one line that genuinely goes to stderr (the durable
		// supervisor-events.log path is unreachable here because the state
		// dir — and thus the log path — could not be resolved).
		fmt.Fprintf(os.Stderr, "ensure-alive: resolve state dir failed; no action: %v\n", err)
		return nil
	}
	return runEnsureAlive(stateDir, os.Stdout)
}
