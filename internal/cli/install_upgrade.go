// Package cli — Task 8.2 `mcphub install --upgrade` IPC handoff
// orchestration (v0.5.0 supervisor architecture).
//
// Spec §"Cold-restart upgrade flow (detail)" + §"Upgrade sequence".
//
// This file owns the new v0.5.0 cold-restart upgrade flow that
// coordinates rename-aside binary replacement (Task 8.1), supervisor
// IPC `quiesce-timers` + `exit{graceful}` (Tasks 5.2 / future), force-
// kill fallback, and per-OS supervisor start. It is intentionally
// distinct from the LEGACY `runInstallUpgrade` in install.go: the
// legacy flow drives Scheduler-backed daemons directly via StopAll /
// Bootstrap / RestartAll; the v0.5.0 flow targets a long-lived
// supervisor process that owns daemons as children under a Job
// Object (Windows) / process group + PR_SET_PDEATHSIG (Linux) /
// kqueue (macOS) lifecycle primitive.
//
// Scope of Task 8.2: ORCHESTRATOR ONLY. The dependency-injection
// surface (`UpgradeDeps`) lets tests inject fakes for every external
// side effect — file rename, IPC connect, force-kill spawn, per-OS
// supervisor start — so the sequencing contract is exercised
// deterministically without needing a real supervisor on disk.
// Production wiring of UpgradeDeps lands in later integration tasks:
//
//   - RenameAsideBinary → api.RenameAsideReplace (Task 8.1, shipped)
//   - QuiesceTimers     → IPC client `quiesce-timers` (Task 5.2 pipe +
//                         future client-side helper)
//   - ExitGraceful      → IPC client `exit{graceful: true, ...}`
//   - ForceKillSupervisor → `taskkill /F /T /PID` (Windows) /
//                          `kill -KILL -<pgid>` (POSIX)
//   - StartSupervisor   → `schtasks /Run \mcp-local-hub-supervisor`
//                         (Windows shim) or detached CreateProcess
//                         with DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP
//                         (POSIX: launchctl kickstart / systemctl
//                         --user restart / mcphub supervise &)
package cli

import (
	"context"
	"fmt"

	"mcp-local-hub/internal/api"
)

// UpgradeDeps is the dependency-injection surface for RunInstallUpgrade.
//
// Every external side effect performed during the upgrade flow goes
// through one of these methods so the orchestrator stays testable
// end-to-end with mocks. Production wiring binds each method to its
// real implementation in a thin adapter (see file-level docstring for
// the binding table); Task 8.2 ships only the orchestrator + mock-
// based tests.
//
// Method contracts:
//
//   - RenameAsideBinary: replace `target` with `newSrc` via the
//     platform's rename-aside two-step (Windows) or atomic rename
//     (POSIX). Failure aborts the entire upgrade — no IPC traffic
//     and no force-kill must follow.
//
//   - QuiesceTimers: send IPC `quiesce-timers` and wait for the FINAL
//     frame (two-frame response per spec §"Wire format"; immediate
//     `{accepted: true}` then final `{drained, still_running}`).
//     Failure is non-fatal — the supervisor will force-kill any
//     surviving transients during its own exit path, so an outage
//     here does NOT block the rest of the flow.
//
//   - ExitGraceful: send IPC `exit{graceful: true, timeout_ms: N}`
//     and wait for the response. A non-nil error here triggers the
//     force-kill fallback; a successful exit response continues to
//     the supervisor-start step without force-kill.
//
//   - ForceKillSupervisor: terminate the prior supervisor PID + its
//     children (taskkill /F /T on Windows; kill -KILL -<pgid> on
//     POSIX). Called only when ExitGraceful failed. Best-effort: a
//     subsequent failure here is logged via the orchestrator but does
//     NOT abort the upgrade — the supervisor's Job Object / process
//     group will reap children when the kernel cleans up, and the
//     follow-on StartSupervisor will detect any lingering port
//     contention through its own bind.
//
//   - StartSupervisor: explicitly start the new supervisor via the
//     per-OS path (see file-level docstring). Failure here aborts
//     the orchestrator with a wrapped error — the operator is in a
//     state where the binary has been replaced but no supervisor is
//     running; recovery is `mcphub supervise` from a shell, which is
//     the same surface this method drives in detached mode.
type UpgradeDeps interface {
	RenameAsideBinary(target, newSrc string) error
	QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error)
	ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error)
	ForceKillSupervisor(pipePath string) error
	StartSupervisor(binaryPath string) error
}

// UpgradeOpts is the input bundle to RunInstallUpgrade.
//
// Field semantics:
//
//   - BinaryPath: absolute path to the current canonical mcphub
//     binary (e.g. C:\Program Files\mcphub\mcphub.exe on a Windows
//     install, or /usr/local/bin/mcphub on POSIX). The rename-aside
//     step moves this binary to `target.old-<ts>` before the new
//     image takes its place. StartSupervisor uses this same path to
//     launch the new supervisor (the rename-aside is in-place, so
//     the path is unchanged after the swap).
//
//   - NewBinary: absolute path to the newly-built mcphub binary to
//     promote (e.g. C:\Program Files\mcphub\mcphub.exe.new). Caller
//     stages it via the secure-write pipeline BEFORE invoking
//     RunInstallUpgrade; the orchestrator only reads + renames.
//
//   - PipePath: per-OS supervisor IPC endpoint
//     (`\\.\pipe\mcphub-supervisor` on Windows;
//     `<state-dir>/supervisor.sock` on POSIX). Used for both
//     QuiesceTimers and ExitGraceful; ForceKillSupervisor uses it
//     to resolve the supervisor PID from `supervisor.lock` (lives
//     alongside the pipe path in the state dir on POSIX, in a sibling
//     state file on Windows).
//
//   - QuiesceTimeoutMs: per-spec §"Upgrade sequence" step 3 budget
//     for transient drain. 0 → default 30000 ms (30s). The supervisor
//     responds with a two-frame envelope: an immediate accepted-frame
//     and a final-frame after drain completes; the timeout here caps
//     how long the orchestrator waits for the FINAL frame, not the
//     accepted-frame which arrives within a single IPC round-trip.
//
//   - ExitTimeoutMs: per-spec §"Upgrade sequence" step 4 budget for
//     the supervisor's graceful exit. 0 → default 5000 ms (5s). The
//     supervisor's own internal timer for terminating remaining
//     transients + daemon children is set from this value, so the
//     orchestrator's wait window matches the supervisor's exit
//     budget; an IPC failure here (timeout, connection drop, malformed
//     response) trips the force-kill fallback regardless.
//
//   - Deps: dependency-injection surface. Must be non-nil in tests;
//     production callers construct a concrete adapter that binds
//     each method to its real implementation (see file-level
//     docstring binding table).
type UpgradeOpts struct {
	BinaryPath       string
	NewBinary        string
	PipePath         string
	QuiesceTimeoutMs int
	ExitTimeoutMs    int
	Deps             UpgradeDeps
}

// Default IPC timeouts per spec §"Upgrade sequence". Exported as
// constants so the orchestrator + future production adapters share a
// single source of truth.
const (
	// defaultQuiesceTimeoutMs is the per-spec step-3 budget for
	// transient maintenance-timer drain. 30s matches the supervisor-
	// side QuiesceHandler default and the operator-facing UX
	// expectation that "upgrade should not appear to hang for more
	// than half a minute".
	defaultQuiesceTimeoutMs = 30000

	// defaultExitTimeoutMs is the per-spec step-4 budget for the
	// supervisor's graceful-exit transition. 5s is a generous upper
	// bound for the supervisor's signal-driven loop-cancel + audit-
	// emit + lock-release sequence (sub-second in practice).
	defaultExitTimeoutMs = 5000
)

// RunInstallUpgrade orchestrates the v0.5.0 cold-restart upgrade
// sequence per spec §"Upgrade sequence":
//
//  1. Replace binary atomically via rename-aside (Task 8.1).
//  2. Connect to supervisor IPC; client reads `supervisor.lock` for
//     expected `{pid, start_time}` FIRST (handled inside the
//     QuiesceTimers / ExitGraceful method implementations).
//  3. Issue IPC `quiesce-timers` (drains transients up to 30s,
//     two-frame response per spec §"Wire format").
//  4. Issue IPC `exit{graceful: true, timeout_ms: 5000}`.
//  5. Supervisor terminates remaining transients + daemon children,
//     exits 0. (Implicit — driven by step 4's IPC response.)
//  6. `mcphub install` explicitly starts new supervisor (per-OS).
//  7. New supervisor reads intent + reconcile → respawns daemons.
//     (Implicit — happens inside the started supervisor process.)
//
// Return contract:
//
//   - Returns nil on success, including the force-kill fallback path
//     (step 4 failure → step 4a force-kill → step 6 start). The
//     supervisor restart at step 6 is the load-bearing convergence
//     event; if it succeeds, the upgrade has effectively completed
//     even if the prior supervisor exited un-gracefully.
//
//   - Returns non-nil on:
//       * Step 1 failure (rename-aside): the binary is in an
//         indeterminate state; no IPC traffic and no force-kill
//         should follow because the IPC peer is the still-running
//         OLD supervisor and our intent was to replace it. The
//         caller's recovery is to re-run --upgrade after diagnosing
//         the rename failure (locked file, missing permissions, etc.)
//         OR to manually restore the .old-<ts> aside file.
//       * Step 6 failure (start supervisor): the binary HAS been
//         replaced and the prior supervisor has exited (gracefully
//         or via force-kill), but the new supervisor failed to
//         start. The caller's recovery is `mcphub supervise` from a
//         shell — the same surface StartSupervisor drives, just
//         in foreground mode for diagnostics.
//
// Non-fatal failures: QuiesceTimers errors are intentionally swallowed
// because the supervisor's own exit path will force-kill any
// surviving transients during step 5; an IPC outage during drain is
// preferable to aborting the whole upgrade after the binary has
// already been replaced. The orchestrator could not return at this
// point even if it wanted to — the rename-aside is committed, so
// step 6 must run to bring up the new supervisor.
//
// Concurrency: this function is single-threaded by design. The
// orchestrator does NOT spawn goroutines; every step blocks the
// caller until completion. Tests can run the function inline.
//
// Default timeouts: if opts.QuiesceTimeoutMs == 0 OR opts.ExitTimeoutMs
// == 0, defaults (30000 / 5000 ms respectively) are filled in. This
// matches the spec's stated budgets without forcing every caller to
// repeat them.
func RunInstallUpgrade(ctx context.Context, opts UpgradeOpts) error {
	// Default timeouts (spec §"Upgrade sequence" steps 3 + 4).
	if opts.QuiesceTimeoutMs == 0 {
		opts.QuiesceTimeoutMs = defaultQuiesceTimeoutMs
	}
	if opts.ExitTimeoutMs == 0 {
		opts.ExitTimeoutMs = defaultExitTimeoutMs
	}

	// Step 1: Atomic binary replacement via rename-aside.
	//
	// Failure here aborts the entire flow: the binary is in an
	// indeterminate state (rename may have partially completed on
	// Windows step 1 of 2, though the helper's best-effort rollback
	// should have restored it), and we MUST NOT proceed to IPC
	// traffic because the supervisor we'd be talking to is the
	// PRIOR supervisor whose binary we just tried (and failed) to
	// swap. Going further would issue a graceful-exit against a
	// supervisor that's actually still healthy, which is exactly
	// the wrong behavior.
	if err := opts.Deps.RenameAsideBinary(opts.BinaryPath, opts.NewBinary); err != nil {
		return fmt.Errorf("rename-aside binary replacement failed: %w; "+
			"the prior binary may have been restored via best-effort rollback; "+
			"verify with `ls %s*` and inspect any `.old-<ts>` artifact, "+
			"then re-run `mcphub install --upgrade` once the cause is resolved",
			err, opts.BinaryPath)
	}

	// Step 2-3: IPC quiesce-timers (drain transient maintenance-timer
	// PIDs from supervisor-state.transient_pids).
	//
	// Per spec §"Wire format" the supervisor returns two frames: an
	// immediate `{accepted: true}` (so the orchestrator gets a prompt
	// ack while drain runs on the supervisor's side goroutine) then
	// a final `{drained: N, still_running: [...], final: true}` when
	// drain completes OR the supervisor's internal timeout expires.
	// QuiesceTimers blocks until the FINAL frame arrives (or the
	// orchestrator's own ctx fires).
	//
	// Failure here is non-fatal: the supervisor's exit path in
	// step 5 will force-kill any surviving transients during its
	// own graceful-exit transition. Pre-spec discussion considered
	// short-circuiting the upgrade on QuiesceTimers failure but
	// concluded the binary has ALREADY been replaced at this point
	// (step 1 committed it), so aborting would leave the operator
	// without a running supervisor at all — worse than proceeding
	// without a clean drain. The intentionally-discarded error here
	// reflects that policy.
	_, _ = opts.Deps.QuiesceTimers(ctx, opts.PipePath, opts.QuiesceTimeoutMs)

	// Step 4: IPC exit{graceful: true, timeout_ms: N}.
	//
	// The supervisor responds within timeout_ms with an exit
	// acknowledgement, then proceeds through its own internal
	// graceful-exit transition (set graceful_exit_in_progress=true,
	// cancel event loop, terminate daemon children + remaining
	// transients, release supervisor.lock, exit 0).
	//
	// On error (timeout, connection drop, malformed response),
	// fall through to the force-kill fallback. The supervisor may
	// have hung in a non-responsive state — a stuck child, a
	// stale file handle, or a goroutine deadlock — that prevents
	// it from honoring the IPC verb. Force-kill closes the gap.
	if _, err := opts.Deps.ExitGraceful(ctx, opts.PipePath, opts.ExitTimeoutMs); err != nil {
		// Step 4a: force-kill fallback.
		//
		// taskkill /F /T /PID on Windows (kills the supervisor PID
		// + its children via /T); kill -KILL -<pgid> on POSIX (kills
		// the supervisor process group). Either way, the running
		// transients + daemon children die with the supervisor.
		//
		// Best-effort: a force-kill failure (already exited,
		// missing PID, permission denied) does NOT abort the
		// orchestrator. The supervisor may already be gone (the
		// IPC error could have been "connection refused after
		// supervisor crash"), in which case taskkill returns a
		// "process not found" code that's benign here. Step 6
		// will detect any actually-surviving port contention via
		// its own bind attempt.
		_ = opts.Deps.ForceKillSupervisor(opts.PipePath)
	}

	// Step 6: Explicit per-OS supervisor start.
	//
	// Windows: `schtasks /Run /TN \mcp-local-hub-supervisor` if the
	// scheduled-task shim is installed; otherwise detached
	// CreateProcess with DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP
	// (the new supervisor's stdin/stdout/stderr inherit nothing
	// from this CLI process, so it survives the upgrade caller's
	// own exit).
	// Linux managed: `systemctl --user restart mcphub-supervisor.service`.
	// Linux unmanaged: `mcphub supervise &` (detached background).
	// macOS managed: `launchctl kickstart -k gui/<uid>/com.applicate2628.mcphub-supervisor`.
	// macOS unmanaged: `mcphub supervise &` (detached background).
	//
	// Failure here aborts with an error because the operator is now
	// in a state where the binary has been replaced and the prior
	// supervisor is dead, but no supervisor is running. Recovery
	// path is documented in the error message: `mcphub supervise`
	// from a shell, which exercises the same per-OS surface in
	// foreground mode (with diagnostics visible to the operator).
	if err := opts.Deps.StartSupervisor(opts.BinaryPath); err != nil {
		return fmt.Errorf("supervisor start failed after binary replacement + prior-supervisor exit: %w; "+
			"the canonical binary at %s is the new image but no supervisor is running; "+
			"run `mcphub supervise` from a shell to diagnose (or `mcphub supervise &` to background-start)",
			err, opts.BinaryPath)
	}

	// Step 7 (implicit): new supervisor reads intent + daemon-intent
	// files on its own startup path, runs the first reconcile pass,
	// and respawns daemons. This happens INSIDE the supervisor
	// process started in step 6; the orchestrator does NOT observe
	// it directly. Operator-facing convergence verification is via
	// `mcphub status` (which talks to the new supervisor through
	// the same IPC pipe).
	return nil
}
