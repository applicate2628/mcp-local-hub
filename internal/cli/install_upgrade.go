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
//   - StartSupervisor   → the creator-retained supervisor launch owner with no-window policy
//
// Unsupported platforms provide no adapter and fail closed in dispatch.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
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
	StartSupervisor(binaryPath string) (SupervisorLaunch, error)
}

const (
	UpgradeReceiptSchemaV1       = "upgrade-receipt-v1"
	UpgradeAdmissionLocalProduct = "local-product-build"
)

// errUpgradeSupervisorAlreadyExited is the typed benign stop outcome used by
// platform adapters when the recorded supervisor is proven absent. It lets the
// transaction distinguish "already gone" from an actual force-kill.
var errUpgradeSupervisorAlreadyExited = errors.New("supervisor already exited")

var ErrRetainedGenerationImageMismatch = errors.New("E_RETAINED_GENERATION_IMAGE_MISMATCH")
var ErrRetainedGenerationParentMismatch = errors.New("E_RETAINED_GENERATION_PARENT_MISMATCH")
var ErrRetainedGenerationArgvMismatch = errors.New("E_RETAINED_GENERATION_ARGV_MISMATCH")
var ErrRetainedGenerationPortPopulationMismatch = errors.New("E_RETAINED_GENERATION_PORT_POPULATION_MISMATCH")
var ErrRetainedGenerationProtocolUnsupported = errors.New("E_RETAINED_GENERATION_PROTOCOL_UNSUPPORTED")
var ErrRetainedGenerationChanged = errors.New("E_RETAINED_GENERATION_CHANGED")
var ErrRetainedGenerationQuiesceUnsettled = errors.New("E_RETAINED_GENERATION_QUIESCE_UNSETTLED")

type retainedGenerationPortObservationV1 struct {
	TaskName          string
	Port              int
	State             windowsProductPortStateV1
	PID               int
	StartedAt         string
	ParentPID         int
	ObservedImagePath string
	ArgvSHA256        string
}

type retainedGenerationObservationV1 struct {
	ReceiptID               string
	CanonicalCLIPath        string
	CanonicalWindowlessPath string
	SupervisorPID           int
	SupervisorStartedAt     string
	SupervisorImagePath     string
	SupervisorCommandSHA256 string
	GUIOwner                string
	CurrentPairReceipt      windowsProductPreimageV1
	Ports                   []retainedGenerationPortObservationV1
	ObservedAtUTC           time.Time
}

type retainedGenerationIPCExchangeV1 struct {
	Request  api.IPCRequest
	Hello    api.IPCHello
	Response api.IPCResponse
}

type retainedGenerationSettledPortEventV1 struct {
	TaskName        string
	Port            int
	PID             int
	StartedAt       string
	ImagePathSHA256 string
	ArgvSHA256      string
}

type retainedGenerationSettledEventV1 struct {
	ReasonID                string
	ObservationSHA256       string
	SupervisorPID           int
	SupervisorStartedAt     string
	SupervisorImageSHA256   string
	SupervisorCommandSHA256 string
	Ports                   []retainedGenerationSettledPortEventV1
	SettledAtUTC            time.Time
}

type retainedGenerationLifecycleHooks struct {
	RequestVersion int
	Observe        func(context.Context, string, string, []windowsProductIntentPortV1) (*retainedGenerationObservationV1, error)
	HoldSupervisor func(int) (process.HeldPIDGeneration, error)
	Send           func(context.Context, api.SupervisorLockOwner, int, string, map[string]any, int) (retainedGenerationIPCExchangeV1, error)
	PublishSettled func(retainedGenerationSettledEventV1) (string, error)
}

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
	// WindowsProduct is the sole one-artifact Product/V3 transaction.
	WindowsProduct *WindowsProductTxn
	// GUIOwnedPending means Prepare already wrote the live GUI-owned V3 preimage
	// before the caller stopped the exact GUI/tray generation.
	GUIOwnedPending bool
	// OnProductPrepared reports a durably read-back V3 prepared journal before
	// any supervisor reap or canonical/task mutation. Nil changes no behavior.
	OnProductPrepared func() error
	// WindowsProductPairLifecycle is the private Windows-only proof producer
	// consumed when the pair transaction would replace either canonical target.
	// Nil leaves legacy single-binary upgrades unchanged and cannot authorize a
	// product-pair replacement.
	WindowsProductPairLifecycle *windowsProductPairLifecycleHooks
	StateDir                    string
	BinaryPath                  string
	NewBinary                   string
	PipePath                    string
	QuiesceTimeoutMs            int
	ExitTimeoutMs               int
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
	// defaultQuiesceTimeoutMs is the upgrade transaction's use of the shared step-3 budget.
	// transient maintenance-timer drain. 30s matches the supervisor-
	// side QuiesceHandler default and the operator-facing UX
	// expectation that "upgrade should not appear to hang for more
	// than half a minute".
	defaultQuiesceTimeoutMs = api.DefaultSupervisorReapQuiesceTimeoutMs

	// defaultExitTimeoutMs is the per-spec step-4 budget for the
	// supervisor's graceful-exit transition. 5s is a generous upper
	// bound for the supervisor's signal-driven loop-cancel + audit-
	// emit + lock-release sequence (sub-second in practice).
	defaultExitTimeoutMs = api.DefaultSupervisorReapExitTimeoutMs

	// defaultPostForceKillPortVerifyTimeout is the per-port deadline
	// for VerifyPortsUnbound after a force-kill fallback (codex-r2-c-
	// p1-8). 10s matches the rollback path's step-3 budget at
	// migration/journal.go:1505 — the operator should not wait longer
	// than that for taskkill /F /T to release the supervisor's
	// child daemon listeners.
	defaultPostForceKillPortVerifyTimeout = api.DefaultSupervisorReapPortReleaseTimeout

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
func RunInstallUpgrade(ctx context.Context, opts UpgradeOpts) (retErr error) {
	productRunEntered := false
	if opts.GUIOwnedPending {
		defer func() {
			if retErr != nil && !productRunEntered && opts.WindowsProduct != nil && opts.WindowsProduct.prepared != nil {
				retErr = opts.WindowsProduct.rollback(retErr)
			}
		}()
	}
	if opts.WindowsProduct != nil {
		defer func() { retErr = errors.Join(retErr, opts.WindowsProduct.Close()) }()
	}
	callerQuiesceTimeoutMs := opts.QuiesceTimeoutMs
	callerExitTimeoutMs := opts.ExitTimeoutMs

	// Default timeouts (spec §"Upgrade sequence" steps 3 + 4).
	if opts.QuiesceTimeoutMs == 0 {
		opts.QuiesceTimeoutMs = defaultQuiesceTimeoutMs
	}
	if opts.ExitTimeoutMs == 0 {
		opts.ExitTimeoutMs = defaultExitTimeoutMs
	}
	handoffTimeout := effectiveSupervisorHandoffTimeout(callerQuiesceTimeoutMs, callerExitTimeoutMs, opts.QuiesceTimeoutMs, opts.ExitTimeoutMs)
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

	var pairPreflight *windowsProductPairPreflightV1
	var pairObservation *windowsProductQuiescenceObservationV1
	var retainedInitial *retainedGenerationObservationV1
	pairKnownAbsent := false
	pairRetainedSettled := false
	if opts.WindowsProduct != nil {
		preflight, err := inspectWindowsProductPreflight(opts.WindowsProduct)
		if err != nil {
			return fmt.Errorf("pre-mutation Windows product inspection failed: %w", err)
		}
		pairPreflight = &preflight
		opts.WindowsProduct.Opts.ReplacementAuthority = windowsProductReplacementAuthority{Disposition: preflight.Disposition}
		lifecycle := opts.WindowsProductPairLifecycle
		if lifecycle == nil {
			return ErrWindowsProductPairSupervisorUnsettled
		}
		if err := validateWindowsProductFrozenPorts(lifecycle.FrozenPorts, opts.ExpectedPorts); err != nil {
			return fmt.Errorf("%w: %v", ErrWindowsProductPairSupervisorUnsettled, err)
		}
		if !opts.GUIOwnedPending && lifecycle.Retained != nil && lifecycle.Retained.Observe != nil {
			paths := opts.WindowsProduct.Opts
			retainedInitial, err = lifecycle.Retained.Observe(ctx, paths.ProductPath, paths.LegacyWindowlessPath, append([]windowsProductIntentPortV1(nil), lifecycle.FrozenPorts...))
			if err != nil {
				return err
			}
			if retainedInitial != nil {
				if err := validateRetainedGenerationObservation(*retainedInitial, lifecycle.FrozenPorts, paths.ProductPath, paths.LegacyWindowlessPath); err != nil {
					return err
				}
				pairObservation = retainedGenerationPreSettlement(*retainedInitial, lifecycle.Retained.RequestVersion)
			}
		}
		if pairObservation == nil {
			if lifecycle.Observe == nil {
				return ErrWindowsProductPairSupervisorUnsettled
			}
			observation, err := lifecycle.Observe(ctx, opts.BinaryPath, append([]windowsProductIntentPortV1(nil), lifecycle.FrozenPorts...))
			if err != nil {
				return fmt.Errorf("%w: %v", ErrWindowsProductPairSupervisorUnsettled, err)
			}
			if err := validateWindowsProductQuiescenceObservation(observation, lifecycle.FrozenPorts); err != nil {
				return fmt.Errorf("%w: %v", ErrWindowsProductPairSupervisorUnsettled, err)
			}
			pairObservation = &observation
			pairKnownAbsent = observation.Supervisor.State == windowsProductSupervisorKnownAbsent
		}
		opts.WindowsProduct.Opts.FrozenPorts = append([]windowsProductIntentPortV1(nil), lifecycle.FrozenPorts...)
		if opts.GUIOwnedPending {
			prepared := opts.WindowsProduct.prepared
			if prepared == nil || prepared.journal.Phase != WindowsProductRecoveryGUIOwnedStopPending || prepared.journal.PreSettlement == nil ||
				prepared.journal.PreSettlement.GUIOwner != windowsProductGUIOwnerConfirmedPresent {
				return fmt.Errorf("%w: GUI-owned V3 preimage was not prepared before exit", ErrWindowsProductRecoveryPending)
			}
			original := prepared.journal.PreSettlement
			if !pairKnownAbsent {
				live := pairObservation.Supervisor.Generation
				recorded := original.Supervisor.Generation
				if live == nil || recorded == nil || live.PID != recorded.PID || live.StartedAt != recorded.StartedAt {
					return fmt.Errorf("%w: post-GUI supervisor is not the held original generation", ErrWindowsProductRecoveryPending)
				}
			}
			pairObservation = &windowsProductQuiescenceObservationV1{
				Supervisor: original.Supervisor, Ports: append([]windowsProductPortSettlementV1(nil), original.Ports...),
				GUIOwner: windowsProductGUIOwnerConfirmedAbsent,
			}
		} else {
			opts.WindowsProduct.Opts.PreSettlement = pairObservation
			if err := opts.WindowsProduct.Prepare(); err != nil {
				return fmt.Errorf("prepare Windows product recovery before lifecycle mutation: %w", err)
			}
		}
		if opts.OnProductPrepared != nil {
			if err := opts.OnProductPrepared(); err != nil {
				return fmt.Errorf("report prepared Product before lifecycle mutation: %w", err)
			}
		}
		if retainedInitial != nil {
			settled, err := settleRetainedGenerationFromObservation(ctx, opts, lifecycle, handoffTimeout, retainedInitial)
			if err != nil {
				return err
			}
			pairObservation = settled
			pairRetainedSettled = settled != nil
		}
	}

	// The shared API owner contains the full quiesce -> graceful exit ->
	// identity-gated force fallback decision. Upgrade retains lock/port
	// settlement and rollback because those are transaction-specific.
	var pairReap *windowsProductPairReapRecorder
	if !pairKnownAbsent && !pairRetainedSettled {
		reapDeps := api.SupervisorReapDeps(opts.Deps)
		if pairObservation != nil {
			pairReap = &windowsProductPairReapRecorder{UpgradeDeps: opts.Deps}
			reapDeps = pairReap
		}
		if err := api.ReapSupervisor(ctx, api.SupervisorReapOpts{
			PipePath:         opts.PipePath,
			QuiesceTimeoutMs: opts.QuiesceTimeoutMs,
			ExitTimeoutMs:    opts.ExitTimeoutMs,
			IsAlreadyExited:  isAlreadyExitedError,
			Deps:             reapDeps,
		}); err != nil {
			if errors.Is(err, api.ErrSupervisorReapForceKill) {
				// Preserve the established upgrade-facing failure contract while the
				// shared reap owner retains its typed lifecycle classification and the
				// platform force-kill cause for callers that need to inspect it.
				return fmt.Errorf("force-kill supervisor failed: %w", err)
			}
			return fmt.Errorf("reap prior supervisor for upgrade: %w", err)
		}
	}

	pairForceFallback := pairReap != nil && pairReap.forceKillCalled
	if pairForceFallback {
		if opts.WaitSupervisorLockReleased == nil {
			return fmt.Errorf("%w: pair force fallback requires supervisor lock release proof", ErrWindowsProductPairSupervisorUnsettled)
		}
		if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound == nil {
			return fmt.Errorf("%w: pair force fallback requires expected port release proof", ErrWindowsProductPairSupervisorUnsettled)
		}
	}
	if !pairKnownAbsent && !pairRetainedSettled && opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, handoffTimeout); err != nil {
			if pairForceFallback {
				return fmt.Errorf("%w: pair force fallback lock release proof failed: %v", ErrWindowsProductPairSupervisorUnsettled, err)
			}
			return recoverUnpromotedAfterReleaseFailure(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("prior supervisor did not release supervisor.lock within %s after upgrade exit request: %w", handoffTimeout, err))
		}
	}

	// Verify daemon ports are actually unbound before starting the successor.
	// This runs on BOTH clean graceful-exit and force-kill paths: exit{graceful}
	// ACKs before the supervisor's shutdown path releases children, so the clean
	// path has the same zombie-listener race as the force path.
	if !pairKnownAbsent && !pairRetainedSettled && len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			if pairForceFallback {
				return fmt.Errorf("%w: pair force fallback expected port release proof failed: %v", ErrWindowsProductPairSupervisorUnsettled, err)
			}
			return recoverUnpromotedAfterReleaseFailure(ctx, opts, handoffTimeout, priorSHA256,
				fmt.Errorf("port-unbound verification failed after prior-supervisor exit: %w", err))
		}
	}
	if pairForceFallback {
		return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256, ErrWindowsProductPairSupervisorUnsettled)
	}
	if pairObservation != nil && (!pairKnownAbsent || opts.GUIOwnedPending) && !pairRetainedSettled {
		pairObservation.SupervisorLockReleased = true
		for i := range pairObservation.Ports {
			pairObservation.Ports[i].PostUnbound = true
		}
	}
	if pairObservation != nil && pairPreflight != nil &&
		(opts.WindowsProduct != nil || windowsProductPairDispositionNeedsReplacement(pairPreflight.Disposition)) {
		receipt, err := mintWindowsProductQuiescenceReceipt(
			opts.WindowsProduct.Opts.ProductPath,
			opts.WindowsProduct.Opts.LegacyWindowlessPath,
			*pairPreflight, *pairObservation, opts.WindowsProductPairLifecycle.Now,
		)
		if err != nil {
			cause := fmt.Errorf("%w: %v", ErrWindowsProductPairSupervisorUnsettled, err)
			if pairKnownAbsent {
				return cause
			}
			return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256, cause)
		}
		if pairRetainedSettled {
			publisher := opts.WindowsProductPairLifecycle.Retained.PublishSettled
			if publisher == nil {
				return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256, errors.New("retained-generation settled event publisher is unavailable"))
			}
			event := retainedGenerationSettledEvent(*pairObservation.Retained, receipt.ObservationSHA256, receipt.SettledAtUTC)
			if _, err := publisher(event); err != nil {
				return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256, fmt.Errorf("publish retained-generation settlement: %w", err))
			}
		}
		if opts.GUIOwnedPending {
			if err := opts.WindowsProduct.SettleGUIOwnedStop(receipt); err != nil {
				return err
			}
		} else {
			opts.WindowsProduct.Opts.ReplacementAuthority.Quiescence = &receipt
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
	if opts.WindowsProduct != nil {
		opts.WindowsProduct.Opts.PriorFleetReaped = pairObservation != nil && !pairKnownAbsent
		productRunEntered = true
		_, err := opts.WindowsProduct.Run(ctx)
		var prePromotion *WindowsProductPairPrePromotionError
		if errors.As(err, &prePromotion) {
			return recoverUnpromotedUpgrade(ctx, opts, handoffTimeout, priorSHA256, err)
		}
		return err
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
	// Windows always uses the creator-retained supervisor launch owner: detached CreateProcess with
	// required breakaway and the shared no-window child policy. A denied
	// breakaway fails typed without a flagless successor. The new
	// supervisor's
	// stdin/stdout/stderr inherit nothing from this CLI process, so it
	// survives both the upgrade caller's exit and the closing of the
	// terminal the upgrade was typed into. Surviving the caller's
	// EXIT and surviving the caller's CONSOLE are different
	// properties; detached lifetime plus the no-window policy provide both. See
	// the creator-retained supervisor launch owner in install_migration_wiring_windows.go,
	// which is the Deps.StartSupervisor implementation this calls.
	// Other platforms currently have no UpgradeDeps adapter; their dispatcher
	// fails closed before entering this transaction.
	//
	// Failure triggers exact retained-prior rollback. The command returns the
	// successor-start cause plus rollback proof or the precise rollback stage
	// that could not be completed.
	launch, startErr := opts.Deps.StartSupervisor(opts.BinaryPath)
	if startErr != nil || launch == nil {
		if settleErr := abortStartedSupervisor(launch); settleErr != nil {
			return errors.Join(startErr, settleErr)
		}
		return rollbackInstallUpgrade(ctx, opts, retainedPrior, priorSHA256, handoffTimeout, fmt.Errorf("supervisor start failed after binary replacement + prior-supervisor exit: %w", errors.Join(startErr, missingSupervisorLaunch(launch))))
	}
	rollbackOwned := func(trigger error) error {
		if err := abortStartedSupervisor(launch); err != nil {
			return errors.Join(trigger, fmt.Errorf("owned successor settlement unconfirmed: %w", err))
		}
		return rollbackInstallUpgradeSettled(ctx, opts, retainedPrior, priorSHA256, handoffTimeout, trigger)
	}
	binding, err := launch.BindControl(ctx, supervisorExpectation(opts.StateDir, opts.BinaryPath, admitted.SHA256))
	if err != nil {
		return rollbackOwned(fmt.Errorf("supervisor control receiving failed: %w", err))
	}
	if opts.WaitSupervisorReady != nil {
		if err := opts.WaitSupervisorReady(ctx, handoffTimeout, opts.BinaryPath, admitted); err != nil {
			return rollbackOwned(
				fmt.Errorf("supervisor successor did not become IPC-ready within %s after upgrade start: %w", handoffTimeout, err))
		}
	}
	if opts.VerifyCanonical != nil {
		if err := opts.VerifyCanonical(opts.BinaryPath, admitted); err != nil {
			return rollbackOwned(
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
			return rollbackOwned(
				fmt.Errorf("persist %s failed: %w", UpgradeReceiptSchemaV1, err))
		}
	}
	if err := launch.Handoff(ctx, binding); err != nil {
		return errors.Join(fmt.Errorf("supervisor managed handoff failed: %w", err), abortStartedSupervisor(launch))
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

type windowsProductPairReapRecorder struct {
	UpgradeDeps
	forceKillCalled bool
}

func (d *windowsProductPairReapRecorder) ForceKillSupervisor(pipePath string) error {
	d.forceKillCalled = true
	return d.UpgradeDeps.ForceKillSupervisor(pipePath)
}

func validateWindowsProductFrozenPorts(frozen []windowsProductIntentPortV1, expected []int) error {
	if len(frozen) != len(expected) {
		return errors.New("frozen intent-port population differs from upgrade port population")
	}
	seen := make(map[string]struct{}, len(frozen))
	wantPorts := make(map[int]int, len(expected))
	for _, port := range expected {
		if port < 1 || port > 65535 {
			return errors.New("invalid expected upgrade port")
		}
		wantPorts[port]++
	}
	for _, row := range frozen {
		if strings.TrimSpace(row.TaskName) == "" || row.Port < 1 || row.Port > 65535 {
			return errors.New("invalid frozen intent-port row")
		}
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := seen[key]; exists {
			return errors.New("duplicate frozen intent-port row")
		}
		seen[key] = struct{}{}
		if wantPorts[row.Port] == 0 {
			return errors.New("frozen intent port is absent from upgrade port population")
		}
		wantPorts[row.Port]--
	}
	for _, remaining := range wantPorts {
		if remaining != 0 {
			return errors.New("upgrade port is absent from frozen intent population")
		}
	}
	return nil
}

func validateWindowsProductQuiescenceObservation(observation windowsProductQuiescenceObservationV1, frozen []windowsProductIntentPortV1) error {
	if observation.GUIOwner != windowsProductGUIOwnerConfirmedAbsent || len(observation.Ports) != len(frozen) {
		return errors.New("incomplete Windows product pair lifecycle observation")
	}
	want := make(map[string]struct{}, len(frozen))
	for _, row := range frozen {
		want[fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)] = struct{}{}
	}
	allUnbound := true
	for _, row := range observation.Ports {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := want[key]; !exists {
			return errors.New("observed port population differs from frozen intent")
		}
		delete(want, key)
		switch row.PreState {
		case windowsProductPortUnbound:
			if row.PrePID != 0 {
				return errors.New("unbound observed port has a PID")
			}
		case windowsProductPortCanonicalOwned:
			allUnbound = false
			if row.PrePID <= 0 {
				return errors.New("owned observed port lacks a PID")
			}
		default:
			return errors.New("normal upgrade observation has unsupported port owner state")
		}
	}
	if len(want) != 0 {
		return errors.New("observed port population is incomplete")
	}
	switch observation.Supervisor.State {
	case windowsProductSupervisorKnownAbsent:
		if observation.Supervisor.Generation != nil || !observation.SupervisorLockReleased || !allUnbound {
			return errors.New("known-absent observation is contradictory")
		}
		for _, row := range observation.Ports {
			if !row.PostUnbound {
				return errors.New("known-absent port lacks positive unbound proof")
			}
		}
	case windowsProductSupervisorGracefullySettled:
		generation := observation.Supervisor.Generation
		if observation.SupervisorLockReleased || generation == nil || generation.PID <= 0 || strings.TrimSpace(generation.StartedAt) == "" || (generation.IPCRequestVersion != 0 && generation.IPCRequestVersion != 1) {
			return errors.New("graceful supervisor generation observation is invalid")
		}
		for _, row := range observation.Ports {
			if row.PostUnbound {
				return errors.New("graceful observation claims settlement before lifecycle execution")
			}
		}
	default:
		return errors.New("supervisor lifecycle state is unknown")
	}
	return nil
}

func mintWindowsProductQuiescenceReceipt(productPath, legacyWindowlessPath string, preflight windowsProductPairPreflightV1, observation windowsProductQuiescenceObservationV1, now func() time.Time) (windowsProductQuiescenceReceiptV1, error) {
	settledAt := time.Now().UTC()
	if now != nil {
		settledAt = now()
	}
	receipt := windowsProductQuiescenceReceiptV1{
		Scope:                   windowsProductQuiescenceScopeV1,
		CanonicalCLIPath:        productPath,
		CanonicalWindowlessPath: legacyWindowlessPath,
		PriorCLI:                preflight.PriorCLI,
		PriorWindowless:         preflight.PriorWindowless,
		Supervisor:              observation.Supervisor,
		Ports:                   append([]windowsProductPortSettlementV1(nil), observation.Ports...),
		GUIOwner:                observation.GUIOwner,
		SupervisorLockReleased:  observation.SupervisorLockReleased,
		SettledAtUTC:            settledAt,
	}
	projection, err := json.Marshal(struct {
		Scope                   string
		CanonicalCLIPath        string
		CanonicalWindowlessPath string
		PriorCLI                windowsProductPreimageV1
		PriorWindowless         windowsProductPreimageV1
		Supervisor              windowsProductSupervisorSettlementV1
		Ports                   []windowsProductPortSettlementV1
		GUIOwner                string
		SupervisorLockReleased  bool
		SettledAtUTC            time.Time
	}{receipt.Scope, receipt.CanonicalCLIPath, receipt.CanonicalWindowlessPath, receipt.PriorCLI, receipt.PriorWindowless, receipt.Supervisor, receipt.Ports, receipt.GUIOwner, receipt.SupervisorLockReleased, receipt.SettledAtUTC})
	if err != nil {
		return windowsProductQuiescenceReceiptV1{}, err
	}
	digest := observation.ObservationSHA256
	if digest == "" {
		digest = fmt.Sprintf("%x", sha256.Sum256(projection))
	}
	receipt.ObservationSHA256 = digest
	receipt.ReceiptID = "q-" + digest
	if err := validateWindowsProductQuiescenceReceipt(receipt); err != nil {
		return windowsProductQuiescenceReceiptV1{}, err
	}
	return receipt, nil
}

func settleRetainedGenerationFromObservation(ctx context.Context, opts UpgradeOpts, lifecycle *windowsProductPairLifecycleHooks, handoffTimeout time.Duration, initial *retainedGenerationObservationV1) (result *windowsProductQuiescenceObservationV1, retErr error) {
	if lifecycle == nil || lifecycle.Retained == nil || lifecycle.Retained.Observe == nil {
		return nil, nil
	}
	hooks := lifecycle.Retained
	frozen := append([]windowsProductIntentPortV1(nil), lifecycle.FrozenPorts...)
	cliPath, windowlessPath := "", ""
	if opts.WindowsProduct != nil {
		cliPath, windowlessPath = opts.WindowsProduct.Opts.ProductPath, opts.WindowsProduct.Opts.LegacyWindowlessPath
	}
	observation := initial
	if observation == nil {
		var err error
		observation, err = hooks.Observe(ctx, cliPath, windowlessPath, frozen)
		if err != nil {
			return nil, err
		}
	}
	if observation == nil {
		return nil, nil
	}
	if err := validateRetainedGenerationObservation(*observation, frozen, cliPath, windowlessPath); err != nil {
		return nil, err
	}
	if hooks.HoldSupervisor == nil || hooks.Send == nil {
		return nil, fmt.Errorf("%w: retained lifecycle dependencies are incomplete", ErrRetainedGenerationChanged)
	}
	held, err := hooks.HoldSupervisor(observation.SupervisorPID)
	if err != nil {
		return nil, fmt.Errorf("%w: hold supervisor generation: %v", ErrRetainedGenerationChanged, err)
	}
	defer func() { retErr = errors.Join(retErr, held.Close()) }()
	if held.PID() != observation.SupervisorPID {
		return nil, fmt.Errorf("%w: held PID differs from observation", ErrRetainedGenerationChanged)
	}
	if err := held.VerifyIdentity(process.PIDIdentityProof{PID: observation.SupervisorPID, ExecutablePath: observation.SupervisorImagePath, StartedAt: observation.SupervisorStartedAt}); err != nil {
		return nil, fmt.Errorf("%w: held supervisor identity: %v", ErrRetainedGenerationChanged, err)
	}
	reobserved, err := hooks.Observe(ctx, cliPath, windowlessPath, frozen)
	if err != nil {
		return nil, fmt.Errorf("%w: re-observe retained generation: %v", ErrRetainedGenerationChanged, err)
	}
	if reobserved == nil {
		return nil, fmt.Errorf("%w: retained generation disappeared during re-probe", ErrRetainedGenerationChanged)
	}
	if err := validateRetainedGenerationObservation(*reobserved, frozen, cliPath, windowlessPath); err != nil {
		return nil, err
	}
	if !sameRetainedGenerationObservation(*observation, *reobserved) {
		if observation.SupervisorPID != reobserved.SupervisorPID || observation.SupervisorStartedAt != reobserved.SupervisorStartedAt || !samePath(observation.SupervisorImagePath, reobserved.SupervisorImagePath) || observation.SupervisorCommandSHA256 != reobserved.SupervisorCommandSHA256 || observation.CurrentPairReceipt != reobserved.CurrentPairReceipt {
			return nil, ErrRetainedGenerationChanged
		}
		return nil, ErrRetainedGenerationPortPopulationMismatch
	}

	owner := api.SupervisorLockOwner{PID: observation.SupervisorPID, StartedAt: observation.SupervisorStartedAt}
	quiesce, err := hooks.Send(ctx, owner, hooks.RequestVersion, "quiesce-timers", map[string]any{"timeout_ms": opts.QuiesceTimeoutMs}, opts.QuiesceTimeoutMs)
	if err != nil {
		return nil, fmt.Errorf("%w: quiesce-timers transport: %v", ErrRetainedGenerationProtocolUnsupported, err)
	}
	if err := validateRetainedGenerationIPCExchange(owner, hooks.RequestVersion, "quiesce-timers", quiesce); err != nil {
		return nil, err
	}
	exit, err := hooks.Send(ctx, owner, hooks.RequestVersion, "exit", map[string]any{"graceful": true, "timeout_ms": opts.ExitTimeoutMs}, opts.ExitTimeoutMs)
	if err != nil {
		return nil, fmt.Errorf("%w: exit transport: %v", ErrRetainedGenerationProtocolUnsupported, err)
	}
	if err := validateRetainedGenerationIPCExchange(owner, hooks.RequestVersion, "exit", exit); err != nil {
		return nil, err
	}
	if opts.WaitSupervisorLockReleased == nil {
		return nil, fmt.Errorf("%w: supervisor lock release verifier is unavailable", ErrRetainedGenerationQuiesceUnsettled)
	}
	if err := opts.WaitSupervisorLockReleased(ctx, handoffTimeout); err != nil {
		return nil, fmt.Errorf("%w: supervisor lock remains held: %v", ErrRetainedGenerationQuiesceUnsettled, err)
	}
	if len(frozen) != 0 {
		if opts.VerifyPortsUnbound == nil {
			return nil, fmt.Errorf("%w: port settlement verifier is unavailable", ErrRetainedGenerationQuiesceUnsettled)
		}
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return nil, fmt.Errorf("%w: frozen intent ports remain bound: %v", ErrRetainedGenerationQuiesceUnsettled, err)
		}
	}
	ports := make([]windowsProductPortSettlementV1, 0, len(observation.Ports))
	for _, row := range observation.Ports {
		ports = append(ports, windowsProductPortSettlementV1{TaskName: row.TaskName, Port: row.Port, PreState: row.State, PrePID: row.PID, PostUnbound: true})
	}
	return &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{
			State:      windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{PID: observation.SupervisorPID, StartedAt: observation.SupervisorStartedAt, IPCRequestVersion: hooks.RequestVersion},
		},
		Ports:                  ports,
		GUIOwner:               observation.GUIOwner,
		SupervisorLockReleased: true,
		ObservationSHA256:      retainedGenerationObservationSHA256(*observation),
		Retained:               observation,
	}, nil
}

func retainedGenerationSettledEvent(observation retainedGenerationObservationV1, observationSHA256 string, settledAt time.Time) retainedGenerationSettledEventV1 {
	ports := make([]retainedGenerationSettledPortEventV1, 0, len(observation.Ports))
	for _, row := range observation.Ports {
		imageDigest := ""
		if row.ObservedImagePath != "" {
			imageDigest = hashWindowsProductPath(row.ObservedImagePath)
		}
		ports = append(ports, retainedGenerationSettledPortEventV1{
			TaskName: row.TaskName, Port: row.Port, PID: row.PID, StartedAt: row.StartedAt,
			ImagePathSHA256: imageDigest, ArgvSHA256: row.ArgvSHA256,
		})
	}
	return retainedGenerationSettledEventV1{
		ReasonID: "retained-generation-gracefully-settled", ObservationSHA256: observationSHA256,
		SupervisorPID: observation.SupervisorPID, SupervisorStartedAt: observation.SupervisorStartedAt,
		SupervisorImageSHA256: hashWindowsProductPath(observation.SupervisorImagePath), SupervisorCommandSHA256: observation.SupervisorCommandSHA256,
		Ports: ports, SettledAtUTC: settledAt,
	}
}

func validateRetainedGenerationObservation(observation retainedGenerationObservationV1, frozen []windowsProductIntentPortV1, cliPath, windowlessPath string) error {
	if strings.TrimSpace(observation.ReceiptID) == "" || observation.CanonicalCLIPath != cliPath || observation.CanonicalWindowlessPath != windowlessPath || !cleanAbsolutePath(observation.CanonicalCLIPath) || !cleanAbsolutePath(observation.CanonicalWindowlessPath) {
		return fmt.Errorf("%w: retained observation target identity is invalid", ErrRetainedGenerationChanged)
	}
	if observation.GUIOwner != windowsProductGUIOwnerConfirmedAbsent {
		return errors.New("E_UPGRADE_GUI_OWNER_PRESENT: retained-generation GUI owner absence is unverified")
	}
	if observation.SupervisorPID <= 0 || strings.TrimSpace(observation.SupervisorStartedAt) == "" || !cleanAbsolutePath(observation.SupervisorImagePath) || samePath(observation.SupervisorImagePath, observation.CanonicalCLIPath) || !validLowerHexSHA256(observation.SupervisorCommandSHA256) || !validWindowsProductPreimage(observation.CurrentPairReceipt) || observation.ObservedAtUTC.IsZero() || observation.ObservedAtUTC.Location() != time.UTC {
		return fmt.Errorf("%w: retained supervisor observation is invalid", ErrRetainedGenerationChanged)
	}
	supervisorStarted, err := time.Parse(time.RFC3339Nano, observation.SupervisorStartedAt)
	if err != nil {
		return fmt.Errorf("%w: supervisor start identity is invalid", ErrRetainedGenerationChanged)
	}
	if len(observation.Ports) != len(frozen) {
		return ErrRetainedGenerationPortPopulationMismatch
	}
	want := make(map[string]windowsProductIntentPortV1, len(frozen))
	for _, row := range frozen {
		if !validLowerHexSHA256(row.ArgvSHA256) {
			return fmt.Errorf("%w: frozen argv digest is invalid", ErrRetainedGenerationArgvMismatch)
		}
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		if _, exists := want[key]; exists {
			return ErrRetainedGenerationPortPopulationMismatch
		}
		want[key] = row
	}
	retainedCount := 0
	for _, row := range observation.Ports {
		key := fmt.Sprintf("%s\x00%d", row.TaskName, row.Port)
		expected, exists := want[key]
		if !exists {
			return ErrRetainedGenerationPortPopulationMismatch
		}
		delete(want, key)
		if row.State == windowsProductPortUnbound {
			if row.PID != 0 || row.ParentPID != 0 || row.StartedAt != "" || row.ObservedImagePath != "" || row.ArgvSHA256 != "" {
				return ErrRetainedGenerationPortPopulationMismatch
			}
			continue
		}
		if row.PID <= 0 || row.ParentPID <= 0 || strings.TrimSpace(row.StartedAt) == "" || !cleanAbsolutePath(row.ObservedImagePath) || !validLowerHexSHA256(row.ArgvSHA256) {
			return ErrRetainedGenerationPortPopulationMismatch
		}
		if row.ParentPID != observation.SupervisorPID {
			return ErrRetainedGenerationParentMismatch
		}
		if row.ArgvSHA256 != expected.ArgvSHA256 {
			return ErrRetainedGenerationArgvMismatch
		}
		started, err := time.Parse(time.RFC3339Nano, row.StartedAt)
		if err != nil || started.Before(supervisorStarted) || started.After(observation.ObservedAtUTC) {
			return fmt.Errorf("%w: port process start identity is outside the supervisor observation", ErrRetainedGenerationChanged)
		}
		switch {
		case samePath(row.ObservedImagePath, observation.CanonicalCLIPath):
			if row.State != windowsProductPortCanonicalOwned {
				return ErrRetainedGenerationImageMismatch
			}
		case samePath(row.ObservedImagePath, observation.SupervisorImagePath):
			if row.State != windowsProductPortRetainedOwned {
				return ErrRetainedGenerationImageMismatch
			}
			retainedCount++
		default:
			return ErrRetainedGenerationImageMismatch
		}
	}
	if len(want) != 0 || retainedCount == 0 {
		return ErrRetainedGenerationPortPopulationMismatch
	}
	return nil
}

func sameRetainedGenerationObservation(left, right retainedGenerationObservationV1) bool {
	return left.CanonicalCLIPath == right.CanonicalCLIPath &&
		left.CanonicalWindowlessPath == right.CanonicalWindowlessPath &&
		left.SupervisorPID == right.SupervisorPID &&
		left.SupervisorStartedAt == right.SupervisorStartedAt &&
		samePath(left.SupervisorImagePath, right.SupervisorImagePath) &&
		left.SupervisorCommandSHA256 == right.SupervisorCommandSHA256 &&
		left.GUIOwner == right.GUIOwner &&
		left.CurrentPairReceipt == right.CurrentPairReceipt &&
		reflect.DeepEqual(left.Ports, right.Ports)
}

func validateRetainedGenerationIPCExchange(owner api.SupervisorLockOwner, version int, command string, exchange retainedGenerationIPCExchangeV1) error {
	invalid := func(detail string) error {
		return fmt.Errorf("%w: %s", ErrRetainedGenerationProtocolUnsupported, detail)
	}
	if version != 0 && version != 1 {
		return invalid("request version is unsupported")
	}
	if command != "quiesce-timers" && command != "exit" {
		return invalid("command is unsupported")
	}
	if exchange.Request.Version != version || exchange.Request.ID <= 0 || exchange.Request.Cmd != command || api.ValidateRequestEnvelope(exchange.Request) != nil {
		return invalid("request envelope differs from the retained lifecycle command")
	}
	timeoutValue, timeoutOK := exchange.Request.Args["timeout_ms"].(int)
	if !timeoutOK || timeoutValue <= 0 {
		return invalid("request timeout is missing or invalid")
	}
	if command == "quiesce-timers" {
		if len(exchange.Request.Args) != 1 {
			return invalid("quiesce-timers arguments differ")
		}
	} else if graceful, ok := exchange.Request.Args["graceful"].(bool); len(exchange.Request.Args) != 2 || !ok || !graceful {
		return invalid("exit arguments differ")
	}
	if exchange.Hello.Version != 1 || !api.ValidateHandshake(exchange.Hello, owner) {
		return invalid("supervisor hello generation differs")
	}
	response := exchange.Response
	if response.ID != exchange.Request.ID || !response.OK || response.Error != nil || !response.Final {
		return invalid("final response envelope is not exact")
	}
	result, ok := response.Result.(map[string]any)
	if !ok {
		return invalid("command result is malformed")
	}
	switch command {
	case "quiesce-timers":
		still, ok := result["still_running"].([]any)
		if !ok || len(still) != 0 {
			return invalid("quiesce-timers did not settle every reported transient")
		}
	case "exit":
		if initiated, ok := result["graceful_exit_initiated"].(bool); !ok || !initiated {
			return invalid("graceful exit was not initiated")
		}
	}
	return nil
}

func retainedGenerationObservationSHA256(observation retainedGenerationObservationV1) string {
	raw, _ := json.Marshal(observation)
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}

func retainedGenerationPreSettlement(observation retainedGenerationObservationV1, requestVersion int) *windowsProductQuiescenceObservationV1 {
	ports := make([]windowsProductPortSettlementV1, 0, len(observation.Ports))
	for _, row := range observation.Ports {
		ports = append(ports, windowsProductPortSettlementV1{
			TaskName: row.TaskName, Port: row.Port, PreState: row.State, PrePID: row.PID,
		})
	}
	return &windowsProductQuiescenceObservationV1{
		Supervisor: windowsProductSupervisorSettlementV1{
			State: windowsProductSupervisorGracefullySettled,
			Generation: &windowsProductSupervisorGenerationV1{
				PID: observation.SupervisorPID, StartedAt: observation.SupervisorStartedAt,
				IPCRequestVersion: requestVersion,
			},
		},
		Ports: ports, GUIOwner: observation.GUIOwner,
	}
}

func recoverUnpromotedUpgrade(ctx context.Context, opts UpgradeOpts, timeout time.Duration, priorSHA256 string, trigger error) error {
	launch, err := opts.Deps.StartSupervisor(opts.BinaryPath)
	if err != nil {
		return errors.Join(trigger, fmt.Errorf("canonical prior binary was not promoted but prior supervisor recovery start failed: %w", err), abortStartedSupervisor(launch))
	}
	receiveErr := receiveStartedSupervisor(ctx, launch, supervisorExpectation(opts.StateDir, opts.BinaryPath, priorSHA256), func() error {
		if opts.WaitSupervisorReady != nil {
			return opts.WaitSupervisorReady(ctx, timeout, opts.BinaryPath, UpgradeCandidateV1{Admission: "retained-prior", SHA256: priorSHA256})
		}
		return nil
	})
	if receiveErr != nil {
		return fmt.Errorf("%w; canonical prior binary was not promoted and recovery start returned, but prior supervisor did not become ready: %w", trigger, receiveErr)
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
	stopOutcome := "was killed"
	if err := opts.WithRollbackStopSettlementFence(ctx, func() error {
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil {
			if isAlreadyExitedError(killErr) {
				stopOutcome = "was already gone"
				return nil
			}
			return fmt.Errorf("force-kill prior supervisor: %w", killErr)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%w; canonical prior binary was not promoted, but recovery could not reap the prior supervisor: %v", trigger, err)
	}
	if opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, timeout); err != nil {
			return fmt.Errorf("%w; canonical prior binary was not promoted, recovery prior supervisor %s, but supervisor.lock remained held: %v", trigger, stopOutcome, err)
		}
	}
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return fmt.Errorf("%w; canonical prior binary was not promoted, recovery prior supervisor %s, but expected ports remained bound: %v", trigger, stopOutcome, err)
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
	stopOutcome := "was killed"
	if err := opts.WithRollbackStopSettlementFence(ctx, func() error {
		if killErr := opts.Deps.ForceKillSupervisor(opts.PipePath); killErr != nil {
			if isAlreadyExitedError(killErr) {
				stopOutcome = "was already gone"
				return nil
			}
			return fmt.Errorf("force-kill successor: %w", killErr)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("%w; automatic rollback refused before force-kill: %v", trigger, err)
	}
	if opts.WaitSupervisorLockReleased != nil {
		if err := opts.WaitSupervisorLockReleased(ctx, timeout); err != nil {
			return fmt.Errorf("%w; automatic rollback successor %s, but supervisor.lock remained held: %v", trigger, stopOutcome, err)
		}
	}
	if len(opts.ExpectedPorts) > 0 && opts.VerifyPortsUnbound != nil {
		if err := opts.VerifyPortsUnbound(opts.ExpectedPorts, defaultPostForceKillPortVerifyTimeout); err != nil {
			return fmt.Errorf("%w; automatic rollback successor %s, but expected ports remained bound: %v", trigger, stopOutcome, err)
		}
	}
	return rollbackInstallUpgradeSettled(ctx, opts, retainedPrior, priorSHA256, timeout, trigger)
}

func rollbackInstallUpgradeSettled(ctx context.Context, opts UpgradeOpts, retainedPrior, priorSHA256 string, timeout time.Duration, trigger error) error {
	if retainedPrior == "" {
		return fmt.Errorf("%w; automatic rollback unavailable: retained prior missing", trigger)
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
	launch, err := opts.Deps.StartSupervisor(opts.BinaryPath)
	if err != nil {
		return errors.Join(trigger, fmt.Errorf("prior binary restored but prior supervisor restart failed: %w", err), abortStartedSupervisor(launch))
	}
	receiveErr := receiveStartedSupervisor(ctx, launch, supervisorExpectation(opts.StateDir, opts.BinaryPath, priorSHA256), func() error {
		if opts.WaitSupervisorReady != nil {
			return opts.WaitSupervisorReady(ctx, timeout, opts.BinaryPath, UpgradeCandidateV1{Admission: "retained-prior", SHA256: priorSHA256})
		}
		return nil
	})
	if receiveErr != nil {
		return fmt.Errorf("%w; prior binary restored and restarted but did not become ready: %w; run `mcphub supervise` from a shell to inspect startup diagnostics", trigger, receiveErr)
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
	Deps api.SupervisorReapDeps
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
	err := api.ReapSupervisor(ctx, api.SupervisorReapOpts{PipePath: opts.PipePath, QuiesceTimeoutMs: opts.QuiesceTimeoutMs, ExitTimeoutMs: opts.ExitTimeoutMs, ExpectedPorts: opts.ExpectedPorts, VerifyPortsUnbound: opts.VerifyPortsUnbound, PortReleaseTimeout: api.DefaultSupervisorReapPortReleaseTimeout, IsAlreadyExited: isAlreadyExitedError, Deps: opts.Deps})
	if errors.Is(err, api.ErrSupervisorReapPortRelease) {
		// Migrate callers historically receive this exact diagnostic. Keep it at
		// the CLI adapter while retaining the lower typed failure and bind cause.
		return fmt.Errorf("port-unbound verification failed after supervisor reap: %w", err)
	}
	return err
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
	if errors.Is(err, errUpgradeSupervisorAlreadyExited) {
		return true
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
