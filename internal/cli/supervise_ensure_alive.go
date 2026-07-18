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
func setGUIOwnerAliveFnForTest(fn func() (bool, int)) func() {
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
// returns (false, pid) for a LIVE healthy owner, so runEnsureAlive does NOT
// take the standalone-supervisor RECOVERY path (§5 PART 2) and instead falls
// through to the autostart (livenessRelaunchFn) path — which is inert there
// (scheduler.New returns "not implemented" on non-Windows), so the only effect
// is a misleading "liveness-relaunch-failed" warn rather than a real recovery.
// This is Windows-GA-posture consistent: macOS is preview and Windows-non-amd64
// is not a shipped target, so the degraded recovery there is bounded. On
// Windows amd64 (GA) and Linux (beta) PIDAlive is observed correctly and the
// standalone-recovery path engages as designed.
//
// alive=true → the recorded PID is observed alive → the supervisor-down state
// is a dead-CHILD-under-live-OWNER topology; runEnsureAlive recovers it via the
// standalone `mcphub supervise` spawn (§5 PART 2), leaving the GUI untouched.
// alive=false → no observable live owner (DeadPID / Malformed / unresolvable
// path, OR an unobservable-but-healthy owner on the probe-unsupported platforms
// above) → treated as genuine OWNER death; runEnsureAlive re-fires the autostart
// GUI task to re-establish both the GUI owner and its supervisor.
//
// DELIBERATELY the bare PIDAlive bit, NOT Verdict.Class == VerdictHealthy
// (PR #283 review P3-b). Keying on PIDAlive treats an alive-but-unreachable
// owner (VerdictLiveUnreachable — alive PID, ping failing, e.g. a GUI
// mid-restart) as a live owner, so a supervisor-down state there takes the
// standalone-recovery path (recover the supervisor without disturbing the GUI)
// rather than the autostart path. The narrow PID-recycle false-positive (a
// recycled PID makes a dead owner look alive) at worst routes recovery through
// the standalone spawn instead of the autostart task — the supervisor recovers
// either way; only the GUI re-establishment differs — and self-heals on the
// next tick once the recycled PID dies or the pidport is overwritten. Keying on
// VerdictHealthy instead would also fix the macOS gap above (VerdictHealthy is a
// ping-only verdict that holds on macOS), but that polarity change is deferred
// as a separate decision, not folded into this fix.
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
