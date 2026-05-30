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
//     future client-side helper)
//   - ExitGraceful      → IPC client `exit{graceful: true, ...}`
//   - ForceKillSupervisor → `taskkill /F /T /PID` (Windows) /
//     `kill -KILL -<pgid>` (POSIX)
//   - StartSupervisor   → `schtasks /Run \mcp-local-hub-supervisor`
//     (Windows shim) or detached CreateProcess
//     with DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP
//     (POSIX: launchctl kickstart / systemctl
//     --user restart / mcphub supervise &)
package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

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
	// ExpectedPorts is the list of listening ports the prior
	// supervisor's daemon children were bound to (read from
	// supervisor-intent.json before invoking RunInstallUpgrade).
	// After a force-kill fallback, the orchestrator iterates this
	// list through VerifyPortsUnbound (below) to prove no zombie
	// listeners survived the kill. Empty list → verification skipped
	// (e.g., zero-daemon installs).
	ExpectedPorts []int
	// VerifyPortsUnbound blocks until every port in ports is observed
	// unbound, or until perPortTimeout elapses for any single port.
	// Returns nil when all ports are confirmed unbound. Returns a
	// non-nil error if any port is still bound after the timeout
	// (the implementation is expected to wrap the offending port and
	// the underlying listen error for diagnostics).
	//
	// Modeled as an UpgradeOpts callback rather than a UpgradeDeps
	// interface method so that production wiring in
	// install_migration_wiring_windows.go (the v5UpgradeDeps adapter)
	// is unaffected by the codex-r2-c-p1-8 closure — Group A wires
	// the closure to portBindWaitForRelease in the same change set.
	// When nil AND ExpectedPorts is non-empty, the orchestrator
	// skips verification silently (same semantic as old behavior),
	// preserving backward compatibility for callers that have not
	// yet adopted the closure.
	//
	// Used after the ForceKillSupervisor fallback: taskkill /F /T
	// closes the supervisor PID + its child daemons, but the kernel
	// may still report the listening sockets as bound for a brief
	// window. The verification step proves the next supervisor
	// (started in step 6) will be able to re-bind without fighting
	// a zombie listener.
	VerifyPortsUnbound func(ports []int, perPortTimeout time.Duration) error
	Deps               UpgradeDeps
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

	// defaultPostForceKillPortVerifyTimeout is the per-port deadline
	// for VerifyPortsUnbound after a force-kill fallback (codex-r2-c-
	// p1-8). 10s matches the rollback path's step-3 budget at
	// migration/journal.go:1505 — the operator should not wait longer
	// than that for taskkill /F /T to release the supervisor's
	// child daemon listeners.
	defaultPostForceKillPortVerifyTimeout = 10 * time.Second
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
//
//   - Step 1 failure (rename-aside): the binary is in an
//     indeterminate state; no IPC traffic and no force-kill
//     should follow because the IPC peer is the still-running
//     OLD supervisor and our intent was to replace it. The
//     caller's recovery is to re-run --upgrade after diagnosing
//     the rename failure (locked file, missing permissions, etc.)
//     OR to manually restore the .old-<ts> aside file.
//
//   - Step 6 failure (start supervisor): the binary HAS been
//     replaced and the prior supervisor has exited (gracefully
//     or via force-kill), but the new supervisor failed to
//     start. The caller's recovery is `mcphub supervise` from a
//     shell — the same surface StartSupervisor drives, just
//     in foreground mode for diagnostics.
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
	// Codex round-4 Lane B P1 (codex-r4-b-p1): the QuiesceTimers
	// result envelope MUST be consumed. If `still_running` is non-empty
	// OR if QuiesceTimers itself returned an error, transients did not
	// drain — the supervisor's exit{graceful} merely SCHEDULES exit and
	// does not guarantee the un-drained children get reaped. Route the
	// orchestrator through the force-kill + verifyPortsUnbound path EVEN
	// IF the subsequent ExitGraceful ACKs. The historical bug let a
	// successful ExitGraceful short-circuit force-kill, turning every
	// un-drained transient into an orphan that held daemon ports.
	quiesceResp, qErr := opts.Deps.QuiesceTimers(ctx, opts.PipePath, opts.QuiesceTimeoutMs)
	quiesceUnclean := false
	if qErr != nil {
		// Quiesce error → provenance of drain is unproven; force-kill.
		quiesceUnclean = true
	} else if body, ok := quiesceResp.Result.(map[string]any); ok {
		if stillRun, ok := body["still_running"].([]any); ok && len(stillRun) > 0 {
			// Transients did not drain within the supervisor's
			// internal timeout. ExitGraceful cannot reliably reap
			// them; route through force-kill.
			quiesceUnclean = true
		}
	}

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
	_, exitErr := opts.Deps.ExitGraceful(ctx, opts.PipePath, opts.ExitTimeoutMs)
	if exitErr != nil || quiesceUnclean {
		// Step 4a: force-kill fallback.
		//
		// taskkill /F /T /PID on Windows (kills the supervisor PID
		// + its children via /T); kill -KILL -<pgid> on POSIX (kills
		// the supervisor process group). Either way, the running
		// transients + daemon children die with the supervisor.
		//
		// Codex r2 Lane C P1 #8 closure: capture the error and
		// classify. "Already exited" is benign — the supervisor was
		// already gone before we issued the kill (often the case
		// when ExitGraceful failed with "connection refused after
		// supervisor crash"), so taskkill / kill returns a
		// process-not-found exit code that does NOT block the
		// orchestrator. Any OTHER error (permission denied, missing
		// taskkill binary, malformed PID) propagates up because the
		// supervisor may still be running and the new supervisor
		// will fight it for the IPC pipe + the daemon ports.
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil {
			if !isAlreadyExitedError(killErr) {
				return fmt.Errorf("force-kill supervisor failed after graceful-exit timeout: %w; "+
					"the prior supervisor may still be running and will fight the new one for the IPC pipe and daemon ports; "+
					"resolve the underlying error (often permission denied) and re-run `mcphub install --upgrade`",
					killErr)
			}
			// already-exited path: continue to the port verification
			// step below — taskkill thinks the PID is gone, but a
			// lingering child listener is still possible if the prior
			// supervisor died without its Job Object reaping children.
		}

		// Codex r2 Lane C P1 #8 closure: verify daemon ports are
		// actually unbound after the force-kill. Without this check
		// the rest of the upgrade flow would race a zombie listener:
		// step 6 starts the new supervisor, the new supervisor tries
		// to launch a daemon, the daemon's net.Listen fails with
		// EADDRINUSE because the prior daemon's child socket is still
		// in the kernel's TIME_WAIT/CLOSE_WAIT cleanup window. A 10s
		// deadline matches the rollback step-3 budget. When the
		// caller has not wired VerifyPortsUnbound (production adapter
		// adoption is staged separately), the verification is silently
		// skipped — same semantic as pre-fix behavior.
		if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
			if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
				return fmt.Errorf("port-unbound verification failed after force-kill supervisor: %w; "+
					"one or more daemon ports are still bound; "+
					"identify the offending listener via `netstat -ano | findstr :<port>` and stop it manually before re-running `mcphub install --upgrade`",
					err)
			}
		}
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

// ReapOpts is the input bundle to ReapSupervisorForRestart. It is a
// subset of UpgradeOpts: the reap path does NOT replace the binary
// (BinaryPath/NewBinary are absent) and does NOT start a new supervisor
// (the caller owns the start step explicitly, after its own intent
// write commits).
type ReapOpts struct {
	// PipePath is the per-OS supervisor IPC endpoint (same semantics as
	// UpgradeOpts.PipePath). Used for QuiesceTimers, ExitGraceful, and
	// ForceKillSupervisor (PID resolution from the lock sidecar).
	PipePath string
	// QuiesceTimeoutMs / ExitTimeoutMs default to the same per-spec
	// budgets RunInstallUpgrade uses (30000 / 5000) when 0.
	QuiesceTimeoutMs int
	ExitTimeoutMs    int
	// ExpectedPorts + VerifyPortsUnbound mirror UpgradeOpts: after the reap
	// (on BOTH the clean graceful-exit path and the force-kill path — PR
	// #250 deeper-review BLOCKER) the reap proves the prior supervisor's
	// daemon listeners are released so the caller's later StartSupervisor
	// can re-bind without fighting a still-releasing or job_protection:false
	// orphan child. Empty/nil → skipped.
	ExpectedPorts      []int
	VerifyPortsUnbound func(ports []int, perPortTimeout time.Duration) error
	// Deps provides the IPC + force-kill sub-methods. Only QuiesceTimers,
	// ExitGraceful, and ForceKillSupervisor are invoked; RenameAsideBinary
	// and StartSupervisor are NEVER called by the reap path.
	Deps UpgradeDeps
}

// ReapSupervisorForRestart reaps a currently-running supervisor WITHOUT
// replacing the binary and WITHOUT starting a successor. It runs the
// same IPC quiesce → graceful-exit → force-kill-fallback →
// verify-ports-unbound sub-sequence as RunInstallUpgrade steps 2-4a, but
// omits the rename-aside (step 1) and the supervisor start (step 6).
//
// This is the §7.1 "reap-first" primitive for the serena dynamic-pool
// migrate (design §7.1, Phase 4): the migrate must remove the OLD
// supervisor BEFORE it writes the spec-bearing runtime_spec intent —
// (a) the InstallParsedManifest §7.1 gate refuses a spec-bearing write
// while a supervisor is running (so the write can only succeed AFTER the
// reap), and (b) the migrate is a SAME-binary restart, so the
// rename-aside step RunInstallUpgrade performs would abort (replace a
// binary with itself). The caller writes the new intent after this
// returns nil, then calls Deps.StartSupervisor itself so the fresh
// supervisor cold-reconciles the just-written intent.
//
// Return contract:
//
//   - nil  — the prior supervisor was quiesced + exited (gracefully or
//     via a clean force-kill) and, if ExpectedPorts/VerifyPortsUnbound
//     were supplied, its daemon ports are confirmed unbound. The caller
//     may now write the spec-bearing intent (the §7.1 gate will pass —
//     no supervisor is running) and start the successor.
//
//   - non-nil — the prior supervisor could NOT be reaped (force-kill
//     failed with a non-already-exited cause, or a daemon port stayed
//     bound past the verify deadline). Per §7.1 acceptance criterion 2
//     the migrate MUST fail loud here and NOT write a new intent that a
//     stuck old supervisor would silently ignore (DisallowUnknownFields).
//
// QuiesceTimers errors / non-empty still_running are NON-fatal to the
// reap decision on their own — exactly as in RunInstallUpgrade — but they
// ROUTE the flow through the force-kill + verify path so an un-drained
// transient cannot survive as a port-holding orphan.
//
// Port-verify on BOTH exit paths (PR #250 deeper-review BLOCKER). The
// verify-ports-unbound step runs UNCONDITIONALLY after the reap — on the
// CLEAN graceful-exit path as well as the force-kill path — not only after
// a force-kill. The supervisor's handleExit writes the graceful-exit
// success ACK BEFORE the teardown completes (supervise.go: the response
// frame is written first so a deferred listener.Close cannot race it,
// THEN triggerGracefulExit fires the actual child teardown). So ExitGraceful
// can return success while the daemon children are still being torn down,
// and a job_protection:false child (PR #242 surface — the per-spawn Job
// Object allocation fell back to a plain cmd.Start without
// KILL_ON_JOB_CLOSE) can outlive the supervisor exit holding its TCP port.
// If the verify were force-kill-only, the caller would then start a fresh
// supervisor whose first spawn hits EADDRINUSE on a port a reaped-but-not-
// yet-released child still owns. Running VerifyPortsUnbound on the clean
// path closes that window: the reap returns nil only once every
// ExpectedPort is confirmed unbound, so the caller's StartSupervisor never
// fights a lingering listener. A still-bound port past the deadline on
// EITHER path makes the reap fail loud (the migrate aborts before the
// spec-bearing intent write — legacy stays recoverable).
func ReapSupervisorForRestart(ctx context.Context, opts ReapOpts) error {
	if opts.QuiesceTimeoutMs == 0 {
		opts.QuiesceTimeoutMs = defaultQuiesceTimeoutMs
	}
	if opts.ExitTimeoutMs == 0 {
		opts.ExitTimeoutMs = defaultExitTimeoutMs
	}

	// Step A: IPC quiesce-timers (drain transients). Consume the result
	// envelope: a quiesce error OR a non-empty still_running means drain
	// is unproven, so route through force-kill even if ExitGraceful ACKs
	// (the same codex-r4-b-p1 invariant RunInstallUpgrade enforces).
	quiesceResp, qErr := opts.Deps.QuiesceTimers(ctx, opts.PipePath, opts.QuiesceTimeoutMs)
	quiesceUnclean := false
	if qErr != nil {
		quiesceUnclean = true
	} else if body, ok := quiesceResp.Result.(map[string]any); ok {
		if stillRun, ok := body["still_running"].([]any); ok && len(stillRun) > 0 {
			quiesceUnclean = true
		}
	}

	// Step B: IPC exit{graceful}. On error (timeout / drop / malformed) OR
	// an unclean quiesce, fall through to the force-kill fallback.
	_, exitErr := opts.Deps.ExitGraceful(ctx, opts.PipePath, opts.ExitTimeoutMs)
	if exitErr != nil || quiesceUnclean {
		// Step B-a: force-kill fallback. "Already exited" is benign (the
		// supervisor was already gone); any other error (permission denied,
		// missing taskkill, corrupt PID) propagates because the prior
		// supervisor may still be alive and would fight the successor for
		// the IPC pipe + daemon ports.
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil {
			if !isAlreadyExitedError(killErr) {
				return fmt.Errorf("force-kill supervisor failed during reap-before-restart: %w; "+
					"the prior supervisor may still be running and would fight the new one for the IPC pipe and daemon ports; "+
					"resolve the underlying error (often permission denied) and re-run the migrate",
					killErr)
			}
		}
	}

	// Step B-b: prove the prior supervisor's daemon ports are unbound before
	// the caller starts the successor (no zombie-listener race). This runs
	// UNCONDITIONALLY — on the CLEAN graceful-exit path as well as the
	// force-kill path (PR #250 deeper-review BLOCKER). ExitGraceful returns
	// success as soon as the supervisor writes its ACK frame, which
	// handleExit emits BEFORE triggering the child teardown, so a graceful
	// exit can return with daemon children still releasing their ports — and
	// a job_protection:false child (PR #242 fallback) can outlive the
	// supervisor exit entirely. Verifying here on both paths guarantees the
	// caller's StartSupervisor re-binds cleanly instead of hitting EADDRINUSE.
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return fmt.Errorf("port-unbound verification failed after supervisor reap: %w; "+
				"one or more daemon ports are still bound; "+
				"identify the offending listener via `netstat -ano | findstr :<port>` and stop it manually before re-running the migrate",
				err)
		}
	}

	// Reaped. The caller now writes the spec-bearing intent (the §7.1 gate
	// passes — no supervisor running) and starts the successor.
	return nil
}

// isAlreadyExitedError reports whether err originated from a kill
// invocation against a process that was already gone. taskkill on
// Windows exits with code 128 ("ERROR_WAIT_NO_CHILDREN") for that
// case and embeds the literal phrase "process not found" or similar
// in its stdout; kill on POSIX exits with code 1 and prints
// "no such process". Production force-kill helpers
// (killPIDViaTaskkill in install_migration_wiring_windows.go) wrap
// the underlying exec.ExitError with the combined output via fmt.Errorf,
// so both the exit-code path and the textual-substring path are checked
// for robustness against future helper refactors.
//
// The function MUST stay narrow: any non-already-exited failure
// (permission denied, missing binary, malformed PID) must propagate so
// the orchestrator can refuse to continue when the prior supervisor
// might still be alive.
func isAlreadyExitedError(err error) bool {
	if err == nil {
		return false
	}
	// Exit-code path: taskkill / kill wrapped via os/exec.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch runtime.GOOS {
		case "windows":
			// taskkill /F /T /PID returns 128 when the target PID
			// has already exited (no child processes to wait on).
			if exitErr.ExitCode() == 128 {
				return true
			}
		default:
			// POSIX `kill -KILL` against a vanished PID exits 1
			// with stderr "No such process". We can't rely on
			// exit-code alone (1 is overloaded) — fall through to
			// the textual check.
		}
	}
	// Textual fallback for callers that wrap the exec output into
	// fmt.Errorf (the production killPIDViaTaskkill helper does this).
	// Substrings here MUST be tight enough to avoid catching unrelated
	// "not found" failure modes — most importantly
	//   `exec: "taskkill": executable file not found in %PATH%`
	// which is a missing-binary error, NOT a vanished-process error,
	// and must propagate so the operator fixes their install.
	//
	// taskkill on Windows (English locale) emits:
	//   "ERROR: The process \"<pid>\" not found."
	//   "ERROR: The process with PID <pid> could not be terminated."
	// POSIX `kill -KILL` emits:
	//   "kill: <pid>: no such process"
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "process") && strings.Contains(msg, "not found"):
		// taskkill's "The process \"<pid>\" not found." path —
		// requires BOTH "process" and "not found" to filter out the
		// "executable file not found" missing-binary case.
		return true
	case strings.Contains(msg, "no such process"):
		// POSIX kill against a vanished PID.
		return true
	case strings.Contains(msg, "no running instance"):
		// PowerShell Stop-Process variant when invoked by name.
		return true
	case strings.Contains(msg, "could not find") && strings.Contains(msg, "process"):
		// taskkill localized variant: "ERROR: Could not find the process".
		return true
	}
	return false
}
