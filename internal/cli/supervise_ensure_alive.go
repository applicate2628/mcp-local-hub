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
//
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
//     HISTORY: this second case was previously a SUPPRESSED no-op ("deferred to
//     the GUI side"). Combined with the GUI only respawning supervisors it
//     itself SPAWNED (never ADOPTED ones), the two recovery mechanisms deferred
//     to each other into a PERMANENT deadlock — the §5 live churn. The GUI's own
//     bounded supervisorManager respawn loop (gui_supervisor_owner.go) still
//     self-heals a GUI-SPAWNED supervisor as a fast path; this tick is the
//     authoritative backstop that covers the adopted-then-died and wedged-GUI
//     cases the GUI loop cannot.
//
// The original supervisor-recovery branch probes the GUI single-instance owner
// (guiOwnerAliveFn) only to CHOOSE the recovery method (standalone `supervise`
// vs the autostart gui task), no longer to SUPPRESS recovery. Independently,
// the restart-v3 Phase-I branch may classify an expired GUI handoff, but it
// never changes this supervisor-recovery policy.
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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

// guiOwnerProbeState classifies the DEFINITIVENESS of a GUI single-instance
// owner probe. Before the P1-1 review fix, guiOwnerAliveFn returned a bare
// bool that collapsed "the recorded owner is confirmed dead" (safe to
// authorize a GUI-owner-killing relaunch) together with "the probe could not
// determine liveness at all" (pidport path unresolvable, pidport
// missing/garbage/out-of-range — gui.VerdictMalformed — or a
// probe-unsupported platform) into the SAME alive=false value. Because the
// autostart task's relaunch (relaunchSupervisorOwner) fires `schtasks /Run`
// against a MultipleInstances=StopExisting task (scheduler_windows.go), that
// collapse AUTHORIZED terminating a possibly-still-alive GUI on every tick a
// probe merely came back ambiguous — the worst finding in the review. Only
// guiOwnerStateConfirmedDead may authorize that relaunch;
// guiOwnerStateUnknown must suppress it exactly like guiOwnerStateAlive does.
type guiOwnerProbeState int

const (
	// guiOwnerStateUnknown: the probe could NOT determine liveness. Covers
	// gui.PidportPath() resolution failure, gui.VerdictMalformed (pidport
	// unreadable/garbage/out-of-range port), and any verdict class this
	// mapping does not otherwise recognize. MUST be treated exactly like
	// guiOwnerStateAlive by every consumer — never authorize a relaunch
	// that could terminate a possibly-live GUI on an ambiguous read.
	guiOwnerStateUnknown guiOwnerProbeState = iota
	// guiOwnerStateAlive: a live PID holds the recorded single-instance
	// lock (gui.VerdictHealthy or gui.VerdictLiveUnreachable — alive but
	// not answering ping).
	guiOwnerStateAlive
	// guiOwnerStateConfirmedDead: the recorded PID is confirmed NOT
	// running (gui.VerdictDeadPID) — the ordinary crash/taskkill/OOM
	// topology this whole recovery feature targets, and the ONLY state
	// that may authorize a GUI-owner-killing relaunch.
	guiOwnerStateConfirmedDead
)

// guiOwnerAliveFn is the injectable GUI-incumbent probe SEAM. It reports
// whether a live `mcphub gui` process currently owns the GUI single-instance
// (pidport) lock — i.e. an OWNER process is still alive even though the
// supervisor child is down. Production resolves the pidport path and runs the
// read-only gui.Probe; the unit test swaps in a recording fake so the real
// %LOCALAPPDATA% pidport is never touched.
//
// Returns (state, pid, port). port is the recorded pidport's Verdict.Port —
// widened from the original (bool, int) shape (zero external callers of this
// var outside this package) so the headless-fleet recovery path
// (runEnsureAliveHeadlessFleet) can dial the GUI's actual configured port for
// its non-gating serving attestation instead of assuming a hardcoded 9125.
// state is guiOwnerStateUnknown when the probe cannot read the pidport (no
// file, parse error, out-of-range port) — see guiOwnerProbeState's doc for
// why this MUST NOT be conflated with a confirmed-dead owner. Production
// callers MUST NOT reassign this var directly — setGUIOwnerAliveFnForTest is
// the only allowed write path.
var guiOwnerAliveFn = probeGUIOwnerAlive

// ensureAliveGUIRecoveryStore is the narrow marker authority Phase I needs:
// validated observation plus the one exact nonterminal CAS. It deliberately
// exposes no Begin/Reserve/Commit/general Interrupt surface.
type ensureAliveGUIRecoveryStore interface {
	gui.HandoffMarkerReader
	InterruptFromOwnedFreeProbe(generation string, expectedSequence uint64, reasonCode, operatorAction string) (*gui.HandoffMarkerRecord, error)
}

// ensureAliveGUIRecoveryDependencies keeps the new degrade-only branch
// independently testable without weakening the existing supervisor-liveness
// seams. The production wiring is immutable outside tests.
type ensureAliveGUIRecoveryDependencies struct {
	restartV3Enabled func() bool
	restartDeadlines func() gui.RestartDeadlines
	markerStore      func(stateDir string, deadlines gui.RestartDeadlines) ensureAliveGUIRecoveryStore
	probeOwnerLease  func(context.Context, gui.GUIOwnerLeaseProbeRequest) gui.GUIOwnerLeaseProbeResult
}

var ensureAliveGUIRecoveryDeps = ensureAliveGUIRecoveryDependencies{
	restartV3Enabled: gui.RestartV3Enabled,
	restartDeadlines: gui.DefaultRestartDeadlines,
	markerStore: func(stateDir string, deadlines gui.RestartDeadlines) ensureAliveGUIRecoveryStore {
		return gui.NewHandoffMarkerStore(stateDir, deadlines)
	},
	probeOwnerLease: gui.ProbeGUIOwnerLease,
}

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
func setGUIOwnerAliveFnForTest(fn func() (guiOwnerProbeState, int, int)) func() {
	prev := guiOwnerAliveFn
	guiOwnerAliveFn = fn
	return func() { guiOwnerAliveFn = prev }
}

// setEnsureAliveGUIRecoveryDependenciesForTest installs the Phase-I fakes as
// one bundle so tests cannot accidentally mix a production marker/probe with a
// fake clock. Only supervise_ensure_alive_test.go invokes it.
func setEnsureAliveGUIRecoveryDependenciesForTest(deps ensureAliveGUIRecoveryDependencies) func() {
	prev := ensureAliveGUIRecoveryDeps
	ensureAliveGUIRecoveryDeps = deps
	return func() { ensureAliveGUIRecoveryDeps = prev }
}

// probeGUIOwnerAlive reports whether a live `mcphub gui` process owns the GUI
// single-instance (pidport) lock. Read-only: it runs gui.Probe (no destructive
// action) and classifies the result into the guiOwnerProbeState tri-state,
// plus Verdict.PID and Verdict.Port so callers never need to hardcode the
// GUI's port (it is operator-configurable via --port / gui_server settings).
//
// P1-1 REVIEW FIX: this function used to return the bare Verdict.PIDAlive
// bool, which collapsed gui.VerdictMalformed (pidport unreadable/garbage/
// out-of-range port) and a path-resolution failure into the SAME value as a
// confirmed-dead owner (gui.VerdictDeadPID) — both read as alive=false. Since
// runEnsureAliveHeadlessFleet (supervisor alive, GUI reported dead) and the
// supervisor-down branch in runEnsureAlive both AUTHORIZE a relaunch on that
// value, and the autostart relaunch fires `schtasks /Run` against a
// MultipleInstances=StopExisting task, a merely-AMBIGUOUS probe result (a
// transient I/O hiccup, a corrupted pidport, or any other Malformed cause)
// could terminate a perfectly healthy GUI EVERY TICK. Classifying on
// Verdict.Class instead lets the caller tell "confirmed dead" apart from
// "we don't know," so only a genuine gui.VerdictDeadPID authorizes that
// relaunch; everything else (including path-resolution failure) reports
// guiOwnerStateUnknown, which every consumer MUST treat like a live owner.
//
// Class mapping:
//   - gui.VerdictDeadPID                                  → guiOwnerStateConfirmedDead
//     (the ordinary crash/taskkill/OOM topology: the pidport still names the
//     LAST daemon PID, but processID(pid).Alive is false — this is the
//     common case the whole headless-fleet recovery feature targets.)
//   - gui.VerdictHealthy, gui.VerdictLiveUnreachable       → guiOwnerStateAlive
//     (LiveUnreachable is an alive-but-not-answering-ping owner, e.g. a GUI
//     mid-restart — still a live process, never safe to kill via the
//     GUI-owner-killing autostart relaunch.)
//   - gui.VerdictMalformed, or gui.PidportPath() error     → guiOwnerStateUnknown
//     (pidport missing/garbage/out-of-range port, or the path itself could
//     not be resolved — the probe genuinely cannot tell if an owner exists.)
//
// PLATFORM NOTE: on platforms where the OS identity probe is unsupported
// (macOS, Windows non-amd64), processIDImpl returns a sentinel error and
// probeOnce classifies the result as VerdictLiveUnreachable (never Malformed
// or DeadPID there — see single_instance.go's macOSUnsupported/
// archUnsupported branches) — so this Class-based mapping resolves those
// platforms to guiOwnerStateAlive for BOTH a ping-matching AND a
// ping-failing owner, correctly treating an unobservable-but-plausibly-alive
// owner as one that must not be killed. This also fixes a pre-existing
// platform gap in the old PIDAlive-keyed version: PIDAlive was force-set
// false on those platforms even for a ping-matching VerdictHealthy owner, so
// a healthy macOS GUI used to read as alive=false; keying on Class instead
// reads it correctly as guiOwnerStateAlive.
func probeGUIOwnerAlive() (guiOwnerProbeState, int, int) {
	pidportPath, err := gui.PidportPath()
	if err != nil {
		// Cannot resolve the pidport path → cannot confirm ANY liveness
		// fact. Unknown, not confirmed-dead — see guiOwnerProbeState's doc.
		return guiOwnerStateUnknown, 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v := gui.Probe(ctx, pidportPath)
	switch v.Class {
	case gui.VerdictDeadPID:
		return guiOwnerStateConfirmedDead, v.PID, v.Port
	case gui.VerdictHealthy, gui.VerdictLiveUnreachable:
		return guiOwnerStateAlive, v.PID, v.Port
	default: // gui.VerdictMalformed, or any class this mapping does not expect.
		return guiOwnerStateUnknown, v.PID, v.Port
	}
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

const (
	ensureAliveGUIFreeMessage              = "GUI restart interrupted; run `mcphub gui`."
	ensureAliveGUIOwnerRecoveringMessage   = "GUI restart interrupted; the supervisor owner is being recovered automatically."
	ensureAliveGUIOwnerRecoveringAction    = "automatic supervisor-owner recovery"
	ensureAliveGUIHeldMessage              = "GUI restart interrupted: a GUI process still holds the single-instance lock; run `mcphub gui --force --kill`."
	ensureAliveGUIUnknownMessage           = "GUI restart owner state is unknown; no action."
	ensureAliveGUIMarkerWriteFailedMessage = "GUI restart interruption could not be recorded durably; run `mcphub gui`."
)

// runEnsureAliveGUIRecovery is Phase I's degrade-only classifier. It can
// terminalize one expired nonterminal marker while owning a proved-free GUI
// flock, or emit an operator diagnostic. It never spawns, kills, binds,
// retries, reacquires, or transfers a GUI lease.
func runEnsureAliveGUIRecovery(stateDir string, out io.Writer) {
	deps := ensureAliveGUIRecoveryDeps
	if deps.restartV3Enabled == nil || !deps.restartV3Enabled() {
		return
	}

	if strings.TrimSpace(stateDir) == "" {
		fmt.Fprintln(out, ensureAliveGUIUnknownMessage)
		return
	}
	canonicalStateDir, err := filepath.Abs(filepath.Clean(stateDir))
	if err != nil {
		fmt.Fprintln(out, ensureAliveGUIUnknownMessage)
		return
	}
	// An absent state directory cannot contain a handoff marker. Return before
	// HandoffMarkerStore.Read acquires its record lock, because that lock owner
	// creates the directory for writers and would otherwise turn the unchanged
	// supervisor-liveness probe's fail-closed "unprobeable" input into a false
	// "not running" result later in this tick.
	if _, statErr := os.Stat(canonicalStateDir); errors.Is(statErr, os.ErrNotExist) {
		return
	}
	if deps.restartDeadlines == nil || deps.markerStore == nil || deps.probeOwnerLease == nil {
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, nil, "phase-i-dependency-missing", errors.New("ensure-alive GUI recovery dependency is nil"))
		return
	}

	deadlines := deps.restartDeadlines()
	if deadlines.Now == nil || deadlines.RecordLock <= 0 {
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, nil, "restart-deadline-invalid", errors.New("restart clock or record-lock deadline is invalid"))
		return
	}
	// One clock sample drives the entire predicate and the Phase-E revalidation.
	// This prevents a wall-clock step from changing an expired reservation back
	// into a protected one (or vice versa) inside one ensure-alive tick.
	now := deadlines.Now().UTC()
	deadlines.Now = func() time.Time { return now }
	classifierCtx, cancelClassifier := context.WithTimeout(context.Background(), deadlines.RecordLock)
	defer cancelClassifier()

	// Marker reads and writes are ordinary filesystem calls and therefore have
	// no cancellable syscall surface. Run the complete classifier behind one
	// wall-clock budget so a wedged read, probe re-read, or CAS can never keep
	// this tick from reaching the pre-existing supervisor-liveness recovery.
	// The scheduled tick is a one-shot process: after runEnsureAlive finishes,
	// process exit abandons a still-wedged worker and the OS releases any flock
	// it owns. A buffered result channel lets a worker that unwedges during
	// shutdown finish without blocking on an abandoned receiver.
	result := make(chan string, 1)
	go func() {
		var buffered bytes.Buffer
		runEnsureAliveGUIRecoveryWithinBudget(classifierCtx, canonicalStateDir, &buffered, deps, deadlines, now)
		result <- buffered.String()
	}()

	select {
	case buffered := <-result:
		_, _ = io.WriteString(out, buffered)
	case <-classifierCtx.Done():
		// Degrade-only timeout: the supervisor-liveness body below must run.
	}
}

func runEnsureAliveGUIRecoveryWithinBudget(
	ctx context.Context,
	canonicalStateDir string,
	out io.Writer,
	deps ensureAliveGUIRecoveryDependencies,
	deadlines gui.RestartDeadlines,
	now time.Time,
) {
	if ctx.Err() != nil {
		return
	}

	store := deps.markerStore(canonicalStateDir, deadlines)
	if store == nil {
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, nil, "marker-store-missing", errors.New("handoff marker store is nil"))
		return
	}
	record, err := store.Read()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, nil, "marker-read-failed", err)
		return
	}
	if record == nil {
		return
	}

	phaseDeadline, reasonCode, eligible := ensureAliveGUIRecoveryPhaseDeadline(record)
	if !eligible {
		return
	}
	if phaseDeadline.IsZero() {
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, record, "phase-deadline-missing", errors.New("eligible handoff marker has no phase deadline"))
		return
	}
	if now.Before(phaseDeadline) {
		return
	}

	probe := deps.probeOwnerLease(ctx, gui.GUIOwnerLeaseProbeRequest{
		PidportPath: filepath.Join(canonicalStateDir, gui.PidportFileLeaf),
		Record:      record,
		MarkerStore: store,
		Deadlines:   deadlines,
	})
	if ctx.Err() != nil {
		if probe.Lease != nil {
			probe.Lease.Release()
		}
		return
	}

	switch probe.State {
	case gui.GUIOwnerLeaseStateFree:
		runEnsureAliveGUIRecoveryFree(ctx, canonicalStateDir, out, store, record, probe, reasonCode)
	case gui.GUIOwnerLeaseStateHeld:
		if probe.Lease != nil {
			probe.Lease.Release()
			emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, record, "probe-contract-invalid", errors.New("Held owner probe returned an owned lease"))
			return
		}
		fmt.Fprintln(out, ensureAliveGUIHeldMessage)
		emitLivenessEvent(canonicalStateDir, api.SupervisorEventSeverityWarn,
			"gui-restart-live-holder-wedged", ensureAliveGUIHeldMessage,
			ensureAliveGUIRecoveryEventFields(record, reasonCode, "mcphub gui --force --kill", probe.Reason))
	case gui.GUIOwnerLeaseStateUnknown:
		if probe.Lease != nil {
			probe.Lease.Release()
		}
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, record, reasonCode, probe.Reason)
	default:
		if probe.Lease != nil {
			probe.Lease.Release()
		}
		emitEnsureAliveGUIRecoveryUnknown(canonicalStateDir, out, record, "probe-state-invalid", fmt.Errorf("unknown GUI owner probe state %d", probe.State))
	}
}

func runEnsureAliveGUIRecoveryFree(
	ctx context.Context,
	stateDir string,
	out io.Writer,
	store ensureAliveGUIRecoveryStore,
	observed *gui.HandoffMarkerRecord,
	probe gui.GUIOwnerLeaseProbeResult,
	reasonCode string,
) {
	if probe.Lease == nil {
		emitEnsureAliveGUIRecoveryUnknown(stateDir, out, observed, "probe-contract-invalid", errors.New("Free owner probe returned no lease"))
		return
	}

	release := sync.OnceFunc(probe.Lease.Release)
	defer release()

	current := probe.Record
	if current == nil || current.Generation != observed.Generation || current.Sequence != observed.Sequence || current.Phase != observed.Phase {
		release()
		emitEnsureAliveGUIRecoveryUnknown(stateDir, out, observed, "probe-record-mismatch", errors.New("Free owner probe did not preserve the observed marker generation, sequence, and phase"))
		return
	}
	if ctx.Err() != nil {
		return
	}

	// Reconcile the degrade message with the same flock-authoritative
	// supervisor probe the unchanged liveness body runs next. A dead supervisor
	// means that body will recover the owner automatically this tick; a live (or
	// undeterminable) supervisor leaves GUI recovery manual.
	supervisorRunning, _, supervisorProbeErr := api.SupervisorRunningUnderStateDir(stateDir)
	if ctx.Err() != nil {
		return
	}
	ownerRecovering := supervisorProbeErr == nil && !supervisorRunning
	operatorAction := "mcphub gui"
	if ownerRecovering {
		operatorAction = ensureAliveGUIOwnerRecoveringAction
	}

	interrupted, err := store.InterruptFromOwnedFreeProbe(current.Generation, current.Sequence, reasonCode, operatorAction)
	// Release before any stdout or durable-log I/O. Neither diagnostic sink is
	// allowed to extend the owned-probe window. There is deliberately NO timeout
	// callback that releases this lease while the non-cancellable CAS is still
	// running: the CAS either returns while this worker still owns the flock, or
	// the one-shot process exits and abandons both the goroutine and its OS lock.
	// A successor can therefore never acquire the flock before a late CAS write.
	release()
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		if errors.Is(err, gui.ErrHandoffMarkerCASMismatch) || errors.Is(err, gui.ErrHandoffMarkerStateMismatch) {
			return
		}
		fmt.Fprintln(out, ensureAliveGUIMarkerWriteFailedMessage)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
			"gui-restart-interrupted-marker-write-failed", ensureAliveGUIMarkerWriteFailedMessage,
			ensureAliveGUIRecoveryEventFields(current, reasonCode, "mcphub gui", err))
		return
	}
	if interrupted == nil || interrupted.Phase != gui.HandoffPhaseInterrupted {
		emitEnsureAliveGUIRecoveryUnknown(stateDir, out, current, "interrupt-result-invalid", errors.New("owned-free interrupt returned no terminal interrupted marker"))
		return
	}

	if ownerRecovering {
		fmt.Fprintln(out, ensureAliveGUIOwnerRecoveringMessage)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
			"gui-restart-interrupted-owner-recovering", ensureAliveGUIOwnerRecoveringMessage,
			ensureAliveGUIRecoveryEventFields(interrupted, reasonCode, ensureAliveGUIOwnerRecoveringAction, nil))
		return
	}

	fmt.Fprintln(out, ensureAliveGUIFreeMessage)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
		"gui-restart-interrupted-free-flock", ensureAliveGUIFreeMessage,
		ensureAliveGUIRecoveryEventFields(interrupted, reasonCode, "mcphub gui", supervisorProbeErr))
}

func ensureAliveGUIRecoveryPhaseDeadline(record *gui.HandoffMarkerRecord) (time.Time, string, bool) {
	switch record.Phase {
	case gui.HandoffPhaseInProgress:
		return record.FreshUntil, "freshness-expired", true
	case gui.HandoffPhaseReserved:
		return record.ReservationExpiresAt, "reservation-expired", true
	default:
		return time.Time{}, "", false
	}
}

func emitEnsureAliveGUIRecoveryUnknown(stateDir string, out io.Writer, record *gui.HandoffMarkerRecord, reasonCode string, cause error) {
	fmt.Fprintln(out, ensureAliveGUIUnknownMessage)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
		"gui-restart-owner-unknown", ensureAliveGUIUnknownMessage,
		ensureAliveGUIRecoveryEventFields(record, reasonCode, "", cause))
}

func ensureAliveGUIRecoveryEventFields(record *gui.HandoffMarkerRecord, reasonCode, operatorAction string, cause error) map[string]any {
	fields := map[string]any{
		"generation":  "",
		"phase":       "unknown",
		"reason_code": reasonCode,
		"old_port":    0,
		"new_port":    0,
		"old_pid":     0,
		"child_pid":   0,
	}
	if record != nil {
		fields["generation"] = record.Generation
		fields["phase"] = string(record.Phase)
		fields["old_port"] = record.OldPort
		fields["new_port"] = record.NewPort
		fields["old_pid"] = record.OldPID
		fields["child_pid"] = record.ChildPID
	}
	if operatorAction != "" {
		fields["operator_action"] = operatorAction
	}
	if cause != nil {
		fields["error"] = cause.Error()
	}
	return fields
}

// ---------------------------------------------------------------------------
// Part B: headless-fleet recovery (supervisor alive, GUI owner dead).
//
// runEnsureAlive's `if running` branch used to be a bare no-op: a live
// supervisor was always treated as "nothing to do", without ever checking
// whether its GUI owner was still alive. The GUI hosts the serena/LSP router
// (and, in hub-aggregate mode, the hub listener) on its own HTTP port — none
// of that comes back on its own if the GUI process dies while its supervisor
// keeps running (a crash, `taskkill /F` of just the GUI PID, an OOM-kill, or
// an unhandled panic). The fleet then looks "up" from the supervisor's point
// of view while every MCP client sees nothing but connection failures /
// "Subprocess initialization did not complete" until an operator notices and
// manually runs `mcphub gui` — the live incident this recovers (~4h headless
// after a reboot until manual relaunch).
// ---------------------------------------------------------------------------

const (
	// ensureAliveHeadlessFleetBootGrace suppresses headless-fleet recovery
	// while the supervisor itself started very recently. It exists so a
	// supervisor that JUST cold-started (autostart racing this very ~1-min
	// tick) is not misdiagnosed as a headless fleet before its own GUI
	// counterpart has had a chance to catch up. Chosen well above typical
	// GUI startup time and comfortably inside one liveness-tick period, so a
	// genuinely dead GUI is still recovered within ~2 ticks of a cold boot.
	ensureAliveHeadlessFleetBootGrace = 45 * time.Second

	// ensureAliveHeadlessFleetServingProbeTimeout bounds the non-gating
	// serving attestation recorded on a successful relaunch event. It is
	// diagnostic only (see probeGUIServingWithinTimeout) and must never let
	// a hung dial/response stall the ensure-alive tick.
	ensureAliveHeadlessFleetServingProbeTimeout = 5 * time.Second
)

// runEnsureAliveHeadlessFleet is invoked from runEnsureAlive's `if running`
// branch once guiOwnerAliveFn has already reported no live GUI owner. It
// applies two independent fail-closed suppressors before relaunching the GUI
// owner via the SAME seam genuine owner-death already uses
// (livenessRelaunchFn / relaunchSupervisorOwner): re-firing the autostart
// task's `mcphub gui` finds the supervisor already alive and ADOPTS it
// (gui_supervisor_owner.go's ensureSupervisorRunning, spawned:false) instead
// of starting a second one — zero daemon churn, exactly the same idempotent
// property the existing owner-death branch below already relies on.
//
// This function has no return value: every exit path here is followed by a
// `return nil` in the caller, matching the best-effort, always-exit-0
// contract the rest of this file's ensure-alive action upholds.
func runEnsureAliveHeadlessFleet(stateDir string, out io.Writer, supervisorPID, guiPID, guiPort int) {
	now := time.Now().UTC()

	uptimeSeconds, uptimeErr := ensureAliveHeadlessFleetSupervisorUptime(stateDir, now)
	detectedBody := map[string]any{
		"supervisor_pid":  supervisorPID,
		"gui_pidport_pid": guiPID,
	}
	if uptimeErr == nil {
		detectedBody["supervisor_uptime_s"] = uptimeSeconds
	}
	fmt.Fprintf(out, "ensure-alive: supervisor running (pid=%d) but no live GUI owner holds the single-instance lock (pidport pid=%d); evaluating headless-fleet recovery\n", supervisorPID, guiPID)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo, "gui-headless-fleet-detected",
		"supervisor is running but no live GUI owner holds the single-instance lock",
		detectedBody)

	// (a) Live-handoff suppressor: an unexpired restart-v3 handoff marker
	// means the GUI is mid-self-restart, not dead.
	if handoffSuppressed, handoffErr := ensureAliveHeadlessFleetLiveHandoffSuppressed(stateDir, now); handoffErr != nil {
		ensureAliveHeadlessFleetSuppress(stateDir, out, "live-handoff", fmt.Sprintf("restart-v3 handoff marker unreadable (will retry next tick): %v", handoffErr))
		return
	} else if handoffSuppressed {
		ensureAliveHeadlessFleetSuppress(stateDir, out, "live-handoff", "an unexpired restart-v3 handoff is in progress")
		return
	}

	// (b) Boot-grace suppressor: the supervisor itself may have just started.
	if uptimeErr != nil {
		ensureAliveHeadlessFleetSuppress(stateDir, out, "boot-grace", fmt.Sprintf("supervisor start time undeterminable (will retry next tick): %v", uptimeErr))
		return
	}
	if uptimeSeconds < ensureAliveHeadlessFleetBootGrace.Seconds() {
		ensureAliveHeadlessFleetSuppress(stateDir, out, "boot-grace", fmt.Sprintf("supervisor uptime %.0fs is within the %s boot-grace window", uptimeSeconds, ensureAliveHeadlessFleetBootGrace))
		return
	}

	// (c) Neither suppressor fired: relaunch via the SAME seam the
	// genuine-owner-death branch below uses.
	if relaunchErr := livenessRelaunchFn(); relaunchErr != nil {
		fmt.Fprintf(out, "ensure-alive: headless fleet (supervisor pid=%d); GUI-owner relaunch FAILED (will retry next tick): %v\n", supervisorPID, relaunchErr)
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn, "liveness-gui-headless-relaunch-failed",
			"supervisor running with no live GUI owner; the GUI-owner relaunch failed; will retry next tick",
			map[string]any{
				"relaunch_target": autostart.WindowsTaskName,
				"supervisor_pid":  supervisorPID,
				"error":           relaunchErr.Error(),
			})
		return
	}

	servingProbeOK := probeGUIServingWithinTimeout(guiPort)
	fmt.Fprintf(out, "ensure-alive: headless fleet (supervisor pid=%d); relaunched GUI owner via %s\n", supervisorPID, autostart.WindowsTaskName)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo, "liveness-relaunched-gui-headless-fleet",
		"supervisor running with no live GUI owner; re-fired the autostart task to relaunch the GUI (adopts the live supervisor, zero daemon churn)",
		map[string]any{
			"relaunch_target":  autostart.WindowsTaskName,
			"supervisor_pid":   supervisorPID,
			"serving_probe_ok": servingProbeOK,
		})
}

// ensureAliveHeadlessFleetSuppress writes the shared suppressed-tick
// diagnostic + durable event. reason is machine-filterable: exactly
// "live-handoff" or "boot-grace".
func ensureAliveHeadlessFleetSuppress(stateDir string, out io.Writer, reason, detail string) {
	fmt.Fprintf(out, "ensure-alive: headless-fleet relaunch suppressed (%s): %s\n", reason, detail)
	emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo, "gui-headless-fleet-relaunch-suppressed",
		"headless-fleet relaunch suppressed",
		map[string]any{"reason": reason, "detail": detail})
}

// ensureAliveHeadlessFleetSupervisorUptime reads the supervisor lock-owner
// sidecar directly. SupervisorRunningUnderStateDir (supervisor_lock.go:248)
// deliberately does not surface StartedAt (it only proves liveness via the
// flock itself), so this is an independent read of the same sidecar
// AcquireSupervisorLock writes. Fail-closed: any read or parse error is
// returned to the caller, which suppresses this tick rather than guessing.
func ensureAliveHeadlessFleetSupervisorUptime(stateDir string, now time.Time) (seconds float64, err error) {
	owner, err := api.ReadSupervisorLockOwner(filepath.Join(stateDir, "supervisor.lock"))
	if err != nil {
		return 0, err
	}
	startedAt, err := time.Parse(time.RFC3339Nano, owner.StartedAt)
	if err != nil {
		return 0, err
	}
	return now.Sub(startedAt).Seconds(), nil
}

// ensureAliveHeadlessFleetLiveHandoffSuppressed reports whether an unexpired
// restart-v3 handoff marker means the headless-fleet state observed this
// tick is actually a legitimate GUI self-restart in flight (the old GUI
// released its single-instance lease and the designated child has not
// re-acquired it yet), not a genuine death.
//
// This is a DELIBERATELY independent second read: it does not thread state
// out of runEnsureAliveGUIRecovery (that function returns void — there is
// nothing to thread) and does not route through the ensureAliveGUIRecoveryDeps
// fakes Phase I's classifier uses, so a test that stubs only Phase I's
// dependencies cannot silently disable this suppressor too.
//
// Fail-closed: a marker-store read error suppresses (treated the same as an
// in-flight handoff) rather than falling through to a relaunch that could
// race a legitimate self-restart.
func ensureAliveHeadlessFleetLiveHandoffSuppressed(stateDir string, now time.Time) (suppressed bool, err error) {
	if !gui.RestartV3Enabled() {
		return false, nil
	}
	canonicalStateDir, absErr := filepath.Abs(filepath.Clean(stateDir))
	if absErr != nil {
		return true, absErr
	}
	// Mirrors runEnsureAliveGUIRecovery's absent-state-dir short-circuit
	// (~:410 above): an absent directory cannot contain a marker, and
	// HandoffMarkerStore.Read's record lock would otherwise create the
	// directory here. Unreachable in practice on this path (the caller only
	// reaches here once SupervisorRunningUnderStateDir already found
	// stateDir's supervisor.lock, which implies the directory exists) but
	// kept for defensive parity with the sibling classifier.
	if _, statErr := os.Stat(canonicalStateDir); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	record, readErr := gui.NewHandoffMarkerStore(canonicalStateDir, gui.DefaultRestartDeadlines()).Read()
	if readErr != nil {
		return true, readErr
	}
	if record == nil {
		return false, nil
	}
	phaseDeadline, _, eligible := ensureAliveGUIRecoveryPhaseDeadline(record)
	if !eligible || phaseDeadline.IsZero() {
		return false, nil
	}
	return now.Before(phaseDeadline), nil
}

// probeGUIServingWithinTimeout is a bounded, non-gating attestation stamped
// on the liveness-relaunched-gui-headless-fleet event as serving_probe_ok.
// It dials the port the PRE-relaunch guiOwnerAliveFn probe observed
// (gui.Verdict.Port) — NEVER a hardcoded port, since the GUI's HTTP port is
// operator-configurable (--port, gui_server settings). A false result is the
// common case: schtasks /Run does not wait for the child to start, so the
// newly-relaunched GUI has typically not bound its port yet by the time this
// runs. The field is diagnostic only and never influences the relaunch
// decision already made by the caller above.
func probeGUIServingWithinTimeout(port int) bool {
	if port <= 0 {
		return false
	}
	client := &http.Client{Timeout: ensureAliveHeadlessFleetServingProbeTimeout}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/version", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// runEnsureAlive is the body of `mcphub supervise --ensure-alive`. It is the
// testable entrypoint: the cobra wrapper resolves the real state dir via
// stateDirFunc() and passes it here. The unit test passes a temp dir directly
// (per the §11.10 fleet-wipe lesson — the action never touches the REAL
// %LOCALAPPDATA% supervisor.lock when given a temp stateDir).
//
// out receives the supervisor outcome (running / relaunched-standalone /
// relaunched-autostart / relaunch-failed / undeterminable), preceded by one
// GUI-degrade line only when the independent gated Phase-I classifier fires,
// so a tick is observable in the scheduled-task last-run record / a manual
// invocation. The failure/defer outcomes ALSO land in the durable
// supervisor-events.log (Task Scheduler discards stdout). Returns nil on EVERY
// branch — this is a best-effort recovery tick (exit 0), not a gate.
func runEnsureAlive(stateDir string, out io.Writer) error {
	runEnsureAliveGUIRecovery(stateDir, out)

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
		// A supervisor holds the lock. That is the fully-healthy no-op case
		// ONLY when a live GUI owner is also present — see Part B above: a
		// supervisor that has outlived its GUI (crash / taskkill / OOM of
		// just the GUI PID) is a HEADLESS FLEET, not a healthy one, and is
		// recovered by runEnsureAliveHeadlessFleet instead of falling
		// through to the plain no-op below.
		//
		// P1-1 review fix: only guiOwnerStateConfirmedDead may reach the
		// headless-fleet relaunch (which re-fires the autostart task
		// against a MultipleInstances=StopExisting scheduled task).
		// guiOwnerStateUnknown (pidport malformed/unresolvable) MUST be
		// treated like guiOwnerStateAlive — suppress, never authorize a
		// relaunch that could terminate a possibly-live GUI on an
		// ambiguous read.
		switch guiState, guiPID, guiPort := guiOwnerAliveFn(); guiState {
		case guiOwnerStateConfirmedDead:
			runEnsureAliveHeadlessFleet(stateDir, out, pid, guiPID, guiPort)
			return nil
		case guiOwnerStateUnknown:
			fmt.Fprintf(out, "ensure-alive: supervisor running (pid=%d); GUI owner state undeterminable (pidport malformed/unresolvable); no action (will retry next tick)\n", pid)
			emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
				"gui-owner-probe-undeterminable",
				"supervisor running; GUI owner probe returned an ambiguous/malformed result; suppressing headless-fleet recovery this tick rather than risk killing a possibly-live GUI",
				map[string]any{"supervisor_pid": pid})
			return nil
		}
		// Common case (guiOwnerStateAlive): a supervisor holds the lock and
		// a live GUI owner is present. No-op.
		fmt.Fprintf(out, "ensure-alive: supervisor running (pid=%d); no action\n", pid)
		return nil
	}

	// Supervisor is down. Distinguish the topologies and recover
	// appropriately (§5 permanent fix PART 2 — the dead-supervisor-under-
	// live-GUI case is no longer a suppressed deadlock):
	//   - live GUI owner present, OR owner state undeterminable → the
	//     supervisor CHILD died but the GUI owner may still be fine (or we
	//     cannot prove otherwise). Recover the supervisor DIRECTLY via a
	//     detached standalone `mcphub supervise` spawn — never the
	//     GUI-owner-killing autostart relaunch, since P1-1's ambiguous-read
	//     case applies here identically to the headless-fleet branch above.
	//   - confirmed no live GUI owner → genuine OWNER death. Re-fire the
	//     autostart GUI task to re-establish BOTH the GUI owner and its
	//     supervisor.
	if guiState, guiPID, _ := guiOwnerAliveFn(); guiState != guiOwnerStateConfirmedDead {
		unknown := guiState == guiOwnerStateUnknown
		// Recover the supervisor WITHOUT touching the GUI. Re-firing the
		// autostart task here would either short-circuit to activate-window
		// (single-instance busy, the old gap-B deadlock) or — on an
		// ambiguous probe — risk killing a possibly-live GUI via the
		// task's StopExisting policy. A direct standalone `mcphub
		// supervise` spawn takes the singleton flock (idempotent against a
		// racing duplicate) and the GUI's poller reconnects to it via IPC.
		if relaunchErr := standaloneRelaunchFn(); relaunchErr != nil {
			if unknown {
				fmt.Fprintf(out, "ensure-alive: supervisor down and GUI owner state undeterminable (pidport malformed/unresolvable); standalone supervisor relaunch FAILED (will retry next tick): %v\n", relaunchErr)
			} else {
				fmt.Fprintf(out, "ensure-alive: supervisor down under a live GUI owner (pid=%d); standalone supervisor relaunch FAILED (will retry next tick): %v\n", guiPID, relaunchErr)
			}
			emitLivenessEvent(stateDir, api.SupervisorEventSeverityWarn,
				"liveness-standalone-relaunch-failed",
				"supervisor down under a live-or-undeterminable GUI owner; the GUI-independent standalone supervisor relaunch failed; will retry next tick",
				map[string]any{"gui_owner_pid": guiPID, "gui_owner_state_unknown": unknown, "error": relaunchErr.Error()})
			return nil
		}
		if unknown {
			fmt.Fprintf(out, "ensure-alive: supervisor down and GUI owner state undeterminable (pidport malformed/unresolvable); relaunched a detached standalone supervisor (GUI-independent recovery, will not risk killing a possibly-live GUI)\n")
		} else {
			fmt.Fprintf(out, "ensure-alive: supervisor down under a live GUI owner (pid=%d); relaunched a detached standalone supervisor (GUI-independent recovery)\n", guiPID)
		}
		emitLivenessEvent(stateDir, api.SupervisorEventSeverityInfo,
			"liveness-relaunched-supervisor-under-gui",
			"supervisor down while a live-or-undeterminable GUI owner held the single-instance lock; spawned a detached standalone supervisor to recover it without disturbing the GUI",
			map[string]any{"gui_owner_pid": guiPID, "gui_owner_state_unknown": unknown})
		return nil
	}

	// Confirmed no live GUI owner → genuine OWNER death. Relaunch via the seam.
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
