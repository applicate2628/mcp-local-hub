// Package cli owns the single admitted `mcphub upgrade` transaction.
// It stages and admits the candidate, quiesces and reaps the managed
// supervisor, proves lock/port release, promotes by rename-aside, binds
// successor readiness to process and binary identity, and writes the durable
// receipt. Unsupported machine/platform states fail closed before mutation.
// UpgradeDeps keeps every external side effect deterministic in tests:
//
//   - RenameAsideBinary → api.RenameAsideReplaceWithResult
//   - QuiesceTimers     → IPC client `quiesce-timers`
//   - ExitGraceful      → IPC client `exit{graceful: true, ...}`
//   - ForceKillSupervisor → identity-gated `taskkill /F /T /PID` on Windows
//   - StartSupervisor   → spawnSupervisorDetached with no-window policy
//
// Unsupported platforms provide no adapter and fail closed in dispatch.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//   - RenameAsideBinary: promote the already-admitted staged successor only
//     after the prior supervisor lock and daemon ports are proven released.
//
//   - QuiesceTimers: send IPC `quiesce-timers` and wait for the FINAL
//     frame (two-frame response per spec §"Wire format"; immediate
//     `{accepted: true}` then final `{drained, still_running}`).
//     Failure routes the transaction through force-kill before release proof.
//
//   - ExitGraceful: send IPC `exit{graceful: true, timeout_ms: N}`
//     and wait for the response. A non-nil error here triggers the
//     force-kill fallback; a successful response continues to release proof.
//
//   - ForceKillSupervisor: terminate the prior supervisor PID + its
//     children (taskkill /F /T on Windows; kill -KILL -<pgid> on
//     POSIX). Called when graceful exit or quiesce is unproven and during
//     post-promotion rollback. Non-benign failure aborts promotion/rollback.
//
//   - StartSupervisor: explicitly start the new supervisor via the
//     per-OS path. Before promotion it recovers the untouched prior binary;
//     after promotion it starts the successor or restored retained prior.
type UpgradeDeps interface {
	RenameAsideBinary(target, newSrc string) (api.RenameAsideResult, error)
	RestoreRetainedBinary(target, retainedPrior string) error
	QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error)
	ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error)
	ForceKillSupervisor(pipePath string) error
	StartSupervisor(binaryPath string) error
}

const (
	UpgradeReceiptSchemaV1       = "upgrade-receipt-v1"
	UpgradeAdmissionLocalProduct = "local-product-build"
)

// UpgradeCandidateV1 is the immutable identity admitted for one upgrade.
// Build metadata describes the running product build whose exact bytes were
// staged; SHA256 binds those bytes across both admission passes and readback.
type UpgradeCandidateV1 struct {
	Admission string `json:"admission"`
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	SHA256    string `json:"sha256"`
}

// UpgradeReceiptV1 is written atomically only after the successor is ready and
// the canonical file has been re-read and matched to the admitted candidate.
type UpgradeReceiptV1 struct {
	Schema      string `json:"schema"`
	Admission   string `json:"admission"`
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	BuildDate   string `json:"build_date"`
	SHA256      string `json:"sha256"`
	InstalledAt string `json:"installed_at"`
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
	// Used after the prior supervisor exits on both clean graceful and force-kill
	// paths: child daemons may still hold listening sockets briefly. The
	// verification step proves the next supervisor (started in step 6) will be
	// able to re-bind without fighting a zombie listener.
	VerifyPortsUnbound func(ports []int, perPortTimeout time.Duration) error
	// WaitSupervisorLockReleased blocks until the prior supervisor releases
	// supervisor.lock. The callback is wired by production adapters that know the
	// concrete state directory. Nil preserves test/backcompat callers that do not
	// own a real supervisor lock.
	WaitSupervisorLockReleased func(ctx context.Context, timeout time.Duration) error
	// WaitSupervisorReady blocks until the successor supervisor has acquired the
	// lock and answers IPC status. Nil preserves non-production tests and legacy
	// callers that cannot probe a successor.
	WaitSupervisorReady func(ctx context.Context, timeout time.Duration, binaryPath string, candidate UpgradeCandidateV1) error
	// WithRollbackStopSettlementFence is wired only by managed upgrade callers.
	// Automatic rollback runs its successor force-kill inside this callback while
	// the canonical supervisor-state flock is held. A nil callback refuses
	// rollback: separating a read from the destructive operation would allow a
	// pending stop receipt to appear between them.
	WithRollbackStopSettlementFence func(ctx context.Context, critical func() error) error
	// AdmitStaged validates the staged PE, non-placeholder build metadata, and
	// SHA-256. It is invoked before any fleet mutation and again after the prior
	// supervisor releases its lock and ports. Both results must be identical.
	AdmitStaged func(path string) (UpgradeCandidateV1, error)
	// AdmitPrior validates the rollback source before the prior fleet is touched.
	AdmitPrior  func(path string) (sha256 string, err error)
	VerifyPrior func(path, expectedSHA256 string) error
	// VerifyCanonical re-reads the promoted canonical file and proves its PE and
	// SHA-256 still match the admitted staged candidate.
	VerifyCanonical func(path string, candidate UpgradeCandidateV1) error
	// WriteReceipt is the final blocking step. Production wires it to the API's
	// atomic state-file writer; failure triggers exact retained-prior rollback.
	WriteReceipt func(receipt UpgradeReceiptV1) error
	Now          func() time.Time
	Deps         UpgradeDeps
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

	// defaultSupervisorLockReleaseTimeout covers the old supervisor's
	// post-ACK graceful shutdown window: quiesce drain (30s) plus exit
	// teardown (5s). The old process owns supervisor.lock until its defer chain
	// runs, so a successor start before this condition is proven can lose
	// AcquireSupervisorLock and exit while upgrade reports success.
	defaultSupervisorLockReleaseTimeout = time.Duration(defaultQuiesceTimeoutMs+defaultExitTimeoutMs) * time.Millisecond
)

// RunInstallUpgrade owns one cold-restart transaction:
//
//  1. Admit the staged PE, build metadata, and SHA-256 before mutation.
//  2. Quiesce/exit (or force-kill) the prior supervisor.
//  3. Prove supervisor.lock and every expected daemon port are released.
//  4. Re-admit the unchanged staged candidate, then promote via rename-aside.
//  5. Start and verify the successor, re-read canonical bytes, and atomically
//     write upgrade-receipt-v1 as the final blocking step.
//
// Any pre-promotion candidate/promotion failure restarts the untouched prior
// canonical binary. Any post-promotion start/readiness/readback/receipt failure
// restores the exact retained prior binary and verifies prior readiness. Nil is
// returned only after every configured completion proof succeeds.
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
	callerQuiesceTimeoutMs := opts.QuiesceTimeoutMs
	callerExitTimeoutMs := opts.ExitTimeoutMs

	// Default timeouts (spec §"Upgrade sequence" steps 3 + 4).
	if opts.QuiesceTimeoutMs == 0 {
		opts.QuiesceTimeoutMs = defaultQuiesceTimeoutMs
	}
	if opts.ExitTimeoutMs == 0 {
		opts.ExitTimeoutMs = defaultExitTimeoutMs
	}
	if opts.Deps == nil {
		return errors.New("upgrade dependencies are required")
	}

	var admitted UpgradeCandidateV1
	if opts.AdmitStaged != nil {
		var err error
		admitted, err = opts.AdmitStaged(opts.NewBinary)
		if err != nil {
			return fmt.Errorf("pre-mutation staged candidate admission failed: %w", err)
		}
	}
	priorSHA256 := ""
	if opts.AdmitPrior != nil {
		var err error
		priorSHA256, err = opts.AdmitPrior(opts.BinaryPath)
		if err != nil {
			return fmt.Errorf("pre-mutation prior canonical admission failed: %w", err)
		}
		if priorSHA256 == "" {
			return errors.New("pre-mutation prior canonical admission returned an empty SHA-256")
		}
	}

	// Stop phase: IPC quiesce-timers (drain transient maintenance-timer
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

	}

	handoffTimeout := effectiveSupervisorHandoffTimeout(callerQuiesceTimeoutMs, callerExitTimeoutMs, opts.QuiesceTimeoutMs, opts.ExitTimeoutMs)
	if opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, handoffTimeout); err != nil {
			return recoverUnpromotedAfterReleaseFailure(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("prior supervisor did not release supervisor.lock within %s after upgrade exit request: %w", handoffTimeout, err))
		}
	}

	// Verify daemon ports are actually unbound before starting the successor.
	// This runs on BOTH clean graceful-exit and force-kill paths: exit{graceful}
	// ACKs before the supervisor's shutdown path releases children, so the clean
	// path has the same zombie-listener race as the force path.
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return recoverUnpromotedAfterReleaseFailure(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("port-unbound verification failed after prior-supervisor exit: %w", err))
		}
	}

	// Re-admit after the old fleet has released every lock and expected port.
	// This closes the stage-to-promotion mutation window without touching the
	// still-canonical prior binary.
	if opts.AdmitStaged != nil {
		rechecked, err := opts.AdmitStaged(opts.NewBinary)
		if err != nil {
			return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("post-quiesce staged candidate admission failed: %w", err))
		}
		if rechecked != admitted {
			return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("staged candidate identity changed between admission passes: before=%+v after=%+v", admitted, rechecked))
		}
	}
	if opts.VerifyPrior != nil {
		if err := opts.VerifyPrior(opts.BinaryPath, priorSHA256); err != nil {
			return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("prior canonical drifted after fleet release and before promotion: %w", err))
		}
	}

	// Promotion occurs only after candidate admission and complete old-fleet
	// release have both been proven.
	promotion, err := opts.Deps.RenameAsideBinary(opts.BinaryPath, opts.NewBinary)
	if promotion.Promoted && promotion.RetainedPrior == "" {
		return fmt.Errorf("post-promotion failure: rename-aside reports successor promoted but retained prior path is missing; refusing to start the successor because exact rollback is unavailable")
	}
	if err != nil {
		if promotion.Promoted {
			return rollbackInstallUpgrade(ctx, opts, promotion.RetainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("rename-aside promoted the successor but post-promotion verification failed: %w", err))
		}
		if !promotion.PriorCanonical && promotion.RetainedPrior != "" {
			return recoverRetainedPriorBeforePromotion(ctx, opts, promotion.RetainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("rename-aside did not promote the successor and could not restore the retained prior: %w", err))
		}
		return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256,
			fmt.Errorf("rename-aside binary replacement failed before promotion: %w", err))
	}
	if !promotion.Promoted {
		if !promotion.PriorCanonical && promotion.RetainedPrior != "" {
			return recoverRetainedPriorBeforePromotion(ctx, opts, promotion.RetainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("rename-aside returned a retained prior without a promoted successor"))
		}
		return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256,
			fmt.Errorf("rename-aside returned invalid promotion result: promoted=%v retained_prior=%q", promotion.Promoted, promotion.RetainedPrior))
	}
	retainedPrior := promotion.RetainedPrior

	// Step 6: Start the admitted successor through the wired platform adapter.
	//
	// Windows always uses spawnSupervisorDetached: detached CreateProcess with
	// CREATE_BREAKAWAY_FROM_JOB when admitted by the parent job, a flagless
	// detached retry otherwise, and the shared no-window child policy. The new
	// supervisor's
	// stdin/stdout/stderr inherit nothing from this CLI process, so it
	// survives both the upgrade caller's exit and the closing of the
	// terminal the upgrade was typed into. Surviving the caller's
	// EXIT and surviving the caller's CONSOLE are different
	// properties; detached lifetime plus the no-window policy provide both. See
	// spawnSupervisorDetached in install_migration_wiring_windows.go,
	// which is the Deps.StartSupervisor implementation this calls.
	// Other platforms currently have no UpgradeDeps adapter; their dispatcher
	// fails closed before entering this transaction.
	//
	// Failure triggers exact retained-prior rollback. The command returns the
	// successor-start cause plus rollback proof or the precise rollback stage
	// that could not be completed.
	if err := opts.Deps.StartSupervisor(opts.BinaryPath); err != nil {
		return rollbackInstallUpgrade(ctx, opts, retainedPrior, priorSHA256, handoffTimeout,
			fmt.Errorf("supervisor start failed after binary replacement + prior-supervisor exit: %w", err))
	}
	if opts.WaitSupervisorReady != nil {
		if err := opts.WaitSupervisorReady(ctx, handoffTimeout, opts.BinaryPath, admitted); err != nil {
			return rollbackInstallUpgrade(ctx, opts, retainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("supervisor successor did not become IPC-ready within %s after upgrade start: %w", handoffTimeout, err))
		}
	}
	if opts.VerifyCanonical != nil {
		if err := opts.VerifyCanonical(opts.BinaryPath, admitted); err != nil {
			return rollbackInstallUpgrade(ctx, opts, retainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("promoted canonical binary readback failed: %w", err))
		}
	}
	if opts.WriteReceipt != nil {
		now := time.Now
		if opts.Now != nil {
			now = opts.Now
		}
		receipt := UpgradeReceiptV1{
			Schema:      UpgradeReceiptSchemaV1,
			Admission:   admitted.Admission,
			Version:     admitted.Version,
			Commit:      admitted.Commit,
			BuildDate:   admitted.BuildDate,
			SHA256:      admitted.SHA256,
			InstalledAt: now().UTC().Format(time.RFC3339Nano),
		}
		if err := opts.WriteReceipt(receipt); err != nil {
			return rollbackInstallUpgrade(ctx, opts, retainedPrior, priorSHA256, handoffTimeout,
				fmt.Errorf("persist %s failed: %w", UpgradeReceiptSchemaV1, err))
		}
	}
	if err := sweepOldBinariesFn(filepath.Dir(opts.BinaryPath), func(path string, err error) {
		fmt.Fprintf(os.Stderr, "warn: old binary sweep remove %s failed: %v\n", path, err)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "warn: old binary sweep in %s failed: %v\n", filepath.Dir(opts.BinaryPath), err)
	}

	// Step 7: new supervisor reads intent + daemon-intent files on its own startup
	// path, runs the first reconcile pass, and respawns daemons. When the caller
	// wires WaitSupervisorReady, the orchestrator observes this via IPC status
	// before reporting success; operator-facing follow-up remains `mcphub status`.
	return nil
}

func recoverUnpromotedUpgrade(ctx context.Context, opts UpgradeOpts, timeout time.Duration, priorSHA256 string, trigger error) error {
	if err := opts.Deps.StartSupervisor(opts.BinaryPath); err != nil {
		return fmt.Errorf("%w; canonical prior binary was not promoted but prior supervisor recovery start failed: %v", trigger, err)
	}
	if opts.WaitSupervisorReady != nil {
		priorCandidate := UpgradeCandidateV1{Admission: "retained-prior", SHA256: priorSHA256}
		if err := opts.WaitSupervisorReady(ctx, timeout, opts.BinaryPath, priorCandidate); err != nil {
			return fmt.Errorf("%w; canonical prior binary was not promoted and recovery start returned, but prior supervisor did not become ready: %v", trigger, err)
		}
	}
	return fmt.Errorf("%w; canonical prior binary was not promoted and prior supervisor recovery completed", trigger)
}

func recoverRetainedPriorBeforePromotion(ctx context.Context, opts UpgradeOpts, retainedPrior, priorSHA256 string, timeout time.Duration, trigger error) error {
	if opts.VerifyPrior != nil {
		if err := opts.VerifyPrior(retainedPrior, priorSHA256); err != nil {
			return fmt.Errorf("%w; retained prior verification failed before recovery: %v", trigger, err)
		}
	}
	if err := opts.Deps.RestoreRetainedBinary(opts.BinaryPath, retainedPrior); err != nil {
		return fmt.Errorf("%w; restoring retained prior before promotion failed: %v", trigger, err)
	}
	if opts.VerifyPrior != nil {
		if err := opts.VerifyPrior(opts.BinaryPath, priorSHA256); err != nil {
			return fmt.Errorf("%w; restored prior canonical readback failed: %v", trigger, err)
		}
	}
	return recoverUnpromotedUpgrade(ctx, opts, timeout, priorSHA256, trigger)
}

func recoverUnpromotedAfterReleaseFailure(ctx context.Context, opts UpgradeOpts, timeout time.Duration, priorSHA256 string, trigger error) error {
	if opts.WithRollbackStopSettlementFence == nil {
		return fmt.Errorf("%w; canonical prior binary was not promoted, but recovery force-kill was refused because the stop-settlement fence is unavailable", trigger)
	}
	if err := opts.WithRollbackStopSettlementFence(ctx, func() error {
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil && !isAlreadyExitedError(killErr) {
			return fmt.Errorf("force-kill prior supervisor: %w", killErr)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%w; canonical prior binary was not promoted, but recovery could not reap the prior supervisor: %v", trigger, err)
	}
	if opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, timeout); err != nil {
			return fmt.Errorf("%w; canonical prior binary was not promoted, recovery force-killed the prior supervisor, but supervisor.lock remained held: %v", trigger, err)
		}
	}
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return fmt.Errorf("%w; canonical prior binary was not promoted, recovery force-killed the prior supervisor, but expected ports remained bound: %v", trigger, err)
		}
	}
	return recoverUnpromotedUpgrade(ctx, opts, timeout, priorSHA256, trigger)
}

func rollbackInstallUpgrade(ctx context.Context, opts UpgradeOpts, retainedPrior, priorSHA256 string, timeout time.Duration, trigger error) error {
	if retainedPrior == "" {
		return fmt.Errorf("%w; automatic rollback unavailable: rename-aside returned no retained prior binary", trigger)
	}
	if opts.WithRollbackStopSettlementFence == nil {
		return fmt.Errorf("%w; automatic rollback refused before force-kill: stop-settlement fence unavailable", trigger)
	}
	if err := opts.WithRollbackStopSettlementFence(ctx, func() error {
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil && !isAlreadyExitedError(killErr) {
			return fmt.Errorf("force-kill successor: %w", killErr)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%w; automatic rollback refused before force-kill: %v", trigger, err)
	}
	if opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, timeout); err != nil {
			return fmt.Errorf("%w; automatic rollback killed the successor but supervisor.lock remained held: %v", trigger, err)
		}
	}
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return fmt.Errorf("%w; automatic rollback killed the successor but expected ports remained bound: %v", trigger, err)
		}
	}
	if opts.VerifyPrior != nil {
		if err := opts.VerifyPrior(retainedPrior, priorSHA256); err != nil {
			return fmt.Errorf("%w; automatic rollback retained-prior SHA-256 verification failed: %v", trigger, err)
		}
	}
	if err := opts.Deps.RestoreRetainedBinary(opts.BinaryPath, retainedPrior); err != nil {
		return fmt.Errorf("%w; automatic rollback failed restoring retained prior binary %s: %v", trigger, retainedPrior, err)
	}
	if opts.VerifyPrior != nil {
		if err := opts.VerifyPrior(opts.BinaryPath, priorSHA256); err != nil {
			return fmt.Errorf("%w; automatic rollback restored prior bytes but canonical SHA-256 readback failed: %v", trigger, err)
		}
	}
	if err := opts.Deps.StartSupervisor(opts.BinaryPath); err != nil {
		return fmt.Errorf("%w; prior binary restored but prior supervisor restart failed: %v", trigger, err)
	}
	if opts.WaitSupervisorReady != nil {
		priorCandidate := UpgradeCandidateV1{Admission: "retained-prior", SHA256: priorSHA256}
		if err := opts.WaitSupervisorReady(ctx, timeout, opts.BinaryPath, priorCandidate); err != nil {
			return fmt.Errorf("%w; prior binary restored and restarted but did not become ready: %v; run `mcphub supervise` from a shell to inspect startup diagnostics", trigger, err)
		}
	}
	if samePath(retainedPrior, opts.BinaryPath) {
		return fmt.Errorf("%w; automatic rollback restored the prior supervisor and verified it ready, but refusing retained-artifact cleanup because %s aliases the canonical binary", trigger, retainedPrior)
	}
	if err := os.Remove(retainedPrior); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w; automatic rollback restored %s and verified the prior supervisor ready, but retained-artifact cleanup failed: %v", trigger, retainedPrior, err)
	}
	return fmt.Errorf("%w; automatic rollback restored %s and verified the prior supervisor ready", trigger, retainedPrior)
}

func effectiveSupervisorHandoffTimeout(callerQuiesceTimeoutMs, callerExitTimeoutMs, effectiveQuiesceTimeoutMs, effectiveExitTimeoutMs int) time.Duration {
	if callerQuiesceTimeoutMs == 0 && callerExitTimeoutMs == 0 {
		return defaultSupervisorLockReleaseTimeout
	}
	callerShutdownBudget := time.Duration(effectiveQuiesceTimeoutMs+effectiveExitTimeoutMs) * time.Millisecond
	if callerShutdownBudget > defaultSupervisorLockReleaseTimeout {
		return callerShutdownBudget
	}
	return defaultSupervisorLockReleaseTimeout
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
