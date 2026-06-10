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
// SCOPING LIMITATION (PR #283 review P2 / P3-a / P3-c — be honest about what
// this tick can and cannot recover):
//
//   - The recovery vector is "re-fire the autostart task that launches the
//     GUI owner". That WORKS when the OWNER process died: no live GUI holds
//     the single-instance lock, so the relaunched `mcphub gui` reaches
//     startGuiServer → ensureSupervisorRunning and re-establishes the
//     supervisor.
//   - It does NOT robustly recover a supervisor CHILD that died while its GUI
//     OWNER is still alive (supervisor panic / OOM of just the supervisor PID /
//     `taskkill /F /PID <supervisor>`). The supervisor is spawned DETACHED
//     (gui_supervisor_owner_windows.go), so it can die independently while the
//     GUI keeps running and keeps holding the GUI single-instance lock. In that
//     state a relaunched `mcphub gui` hits ErrSingleInstanceBusy →
//     TryActivateIncumbent → returns WITHOUT reaching ensureSupervisorRunning,
//     so the dead supervisor is never respawned — and it would also steal the
//     user's GUI window to the foreground (/api/activate-window) once per tick.
//     The GUI's own startExitMonitor (gui_supervisor_owner.go) only LOGS a
//     supervisor-child death today; it does not respawn. A GUI-side
//     supervisor-respawn loop is the proper fix and is deferred to a later
//     phase.
//
// To stay HONEST and avoid the perpetual focus-steal, runEnsureAlive probes
// the GUI single-instance owner BEFORE relaunching (guiOwnerAliveFn): if a live
// GUI owner is present while the supervisor is down, the action does NOT fire
// the relaunch and does NOT print a false "relaunched owner" — it records a
// durable, operator-visible "supervisor down under a live GUI owner; this tick
// cannot recover it" diagnostic instead. Only the genuine OWNER-death case
// (no live GUI owner) takes the relaunch path.
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
//   - running=false AND a live GUI owner is present → DEFER: do NOT relaunch
//     (it would be a no-op focus-steal); record a durable warn; exit 0.
//   - running=false AND no live GUI owner → relaunch the owner via the
//     injectable relaunch seam, which re-fires the autostart task
//     (`schtasks /Run /TN \mcp-local-hub-supervisor`). The GUI/supervisor
//     singleton locks make the relaunch idempotent (no duplicate supervisor).
//
// ALL branches exit 0: this is a best-effort recovery tick, not a gate.
//
// OBSERVABILITY (PR #283 review P3-d): every non-trivial outcome — relaunch
// success, relaunch FAILURE, and the dead-supervisor-under-live-GUI deferral —
// is mirrored to a DURABLE sink (`supervisor-events.log`, severity warn for the
// failure/deferral cases) in addition to the one-line `out` writer. Task
// Scheduler discards the action's stdout/stderr, so without the durable log a
// chronically-failing relaunch (e.g. the autostart task `\mcp-local-hub-supervisor`
// was never installed by `mcphub autostart enable`) would be invisible: exit 0,
// no last-run-result signal, no log. The durable warn restores the fail-loud
// operational contract. The `out` writer's transient resolve-failure line still
// goes to os.Stderr (runEnsureAliveFromState).
//
// LOG-CHURN NOTE (PR #283 review P3-c — accepted as-is for merge): while a
// dead-supervisor-under-live-GUI condition persists, every ~1-min tick writes
// one "liveness-supervisor-down-under-live-gui" warn to supervisor-events.log.
// At ~300-400 bytes/entry that is ~0.5 MB/day, so the 10 MB log (single .log.1
// backfile, 16 KB per-entry cap) rotates in ~2-3 weeks of continuous
// unrecovered state. This is bounded by the existing rotation + size-cap
// discipline and is NOT a volume regression (the pre-fix false-success line
// produced the same cadence on the discarded stdout). De-duping per
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

	// Supervisor is down. Before relaunching, distinguish the two topologies
	// (PR #283 review P2): an OWNER death (no live GUI owner — the relaunch
	// recovers it) vs a dead supervisor CHILD under a live GUI OWNER (the
	// relaunch is a no-op focus-steal that cannot recover it).
	if guiAlive, guiPID := guiOwnerAliveFn(); guiAlive {
		// Dead supervisor under a live GUI owner. Re-firing the autostart
		// task here would short-circuit to activate-window (single-instance
		// busy) WITHOUT respawning the supervisor — a perpetual once-per-tick
		// foreground-window steal with zero recovery. Suppress the relaunch
		// and record an HONEST durable diagnostic instead of a false
		// "relaunched owner". GUI-side supervisor respawn is the proper fix
		// (deferred — see the SCOPING LIMITATION in the file header).
		fmt.Fprintf(out, "ensure-alive: supervisor not running BUT a live GUI owner (pid=%d) holds the single-instance lock; "+
			"this tick cannot recover a supervisor-child death under a live GUI (relaunch suppressed to avoid a no-op focus-steal); no action\n", guiPID)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
			"liveness-supervisor-down-under-live-gui",
			"supervisor down but a live GUI owner holds the single-instance lock; liveness cannot recover a supervisor-child death under a live GUI (relaunch suppressed); GUI-side supervisor respawn is deferred",
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
