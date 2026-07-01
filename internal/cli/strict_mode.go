// Package cli — `mcphub strict-mode {enable|disable|--recover}` cobra
// command (plan §2543-2603, Task 11.2).
//
// Strict-mode is a single-bit policy that influences supervisor
// startup: when enabled, `mcphub supervise --strict-mode` is the shim's
// argv AND `supervisor-intent.json.strict_mode` records the operator's
// intent so the supervisor can read it back at boot. The two resources
// must move together — if intent says strict but the shim doesn't pass
// the flag (or vice versa), the next boot is inconsistent.
//
// The mutation pipeline is therefore an atomic two-resource write:
//
//  1. Acquire `migration.lock` (refuse-if-held → exit 9 STRICT_MODE_BUSY).
//  2. Acquire `--once.lock` (LIFO release).
//  3. Refuse-if-held on the breadcrumb file (operator must run --recover).
//  4. Snapshot the autostart shim's observable Status before mutating it.
//  5. Write supervisor-intent.json with the new strict_mode value.
//  6. Call autostart.Backend.Enable(Options{StrictMode: newValue}).
//  7. On step 6 failure: revert step 5 (re-write intent with original).
//  8. On step 7 failure (both writes failed): write breadcrumb + exit 10.
//  9. On step 7 success: re-probe the shim. The per-OS backends are NOT
//     all-or-nothing — they mutate the shim (delete-before-create on
//     Windows, write-before-enable on Linux, bootout+write-before-bootstrap
//     on macOS) before the OS step can error, so a failed Enable can leave
//     the shim torn or out of sync with the reverted intent. Only when the
//     re-probe PROVES the shim is unchanged from the step-4 snapshot is the
//     breadcrumb deleted; otherwise a torn-shape breadcrumb is KEPT and the
//     command exits 10 so `--recover` can drive both resources back in sync.
//  10. LIFO release.
//
// Invariant: strict-mode never deletes the breadcrumb / returns clean while
// the autostart shim might be torn or out of sync with the intent.
//
// `--recover` reads the breadcrumb, prompts the operator with two
// exhaustive branches (drive both to `intended` or drive both to
// `actual_intent_state`), atomically reconciles, and deletes the
// breadcrumb on success.
//
// All OS-specific shim work — Task Scheduler XML, systemd unit, launchctl
// plist — lives in the autostart package and is reached through the
// Backend interface; this file is portable across all three target OSes.
package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

// Exit codes per spec §Q8 + plan §2603.
const (
	// ExitStrictModeBusy is returned when migration.lock or --once.lock
	// is held by another holder. Operator must wait or release manually.
	ExitStrictModeBusy = 9

	// ExitStrictModeRevertFailed is returned when both step 5 (shim
	// write) AND step 6 (intent revert) fail, leaving the system in a
	// torn state. The breadcrumb records the inconsistency so the
	// operator can run `mcphub strict-mode --recover`.
	ExitStrictModeRevertFailed = 10
)

// Breadcrumb phase markers. Distinguish the two crash/torn shapes the
// breadcrumb file (<state-dir>/strict-mode-mutation-incomplete.json) can hold:
//
//   - strictModeBreadcrumbPhaseTorn ("" — the legacy/default shape) is written
//     on the HANDLED both-writes-failed branch: step 1 (intent) succeeded,
//     step 2 (shim) failed, AND the revert of step 1 also failed.
//   - strictModeBreadcrumbPhaseInProgress is written BEFORE step 1 and deleted
//     AFTER step 2 succeeds. If the process is SIGKILLed / power-lost in the
//     step1→step2 window (intent flipped, shim still original — a strict-mode
//     posture DRIFT with NO handled-failure breadcrumb), this in-progress
//     marker is what survives so `mcphub strict-mode --recover` can detect and
//     reconcile the drift (opus-3 F11).
//
// Back-compat: the marker is `omitempty`, so every pre-existing both-failed
// breadcrumb (which never set Phase) round-trips as the torn shape and the
// refuse-if-held / --recover surface treats it exactly as before.
const (
	strictModeBreadcrumbPhaseTorn       = ""
	strictModeBreadcrumbPhaseInProgress = "in-progress"
)

// strictModeBreadcrumb is the on-disk schema for
// <state-dir>/strict-mode-mutation-incomplete.json. Written when revert
// fails; read by --recover; deleted on successful reconcile.
//
// Fields are ordered to match the spec verbatim. JSON tags use
// snake_case per repo convention; the breadcrumb is operator-facing so
// every field MUST be deterministically populated even when its source
// error is nil (use empty-string sentinel).
type strictModeBreadcrumb struct {
	// Intended is the operator's requested final state (true for
	// enable, false for disable).
	Intended bool `json:"intended"`

	// ActualIntentState is the state supervisor-intent.json was in
	// after step 4 — i.e. AFTER the first write succeeded but BEFORE
	// revert was attempted. On a typical enable flow this equals
	// Intended; the field exists because the dual-failure recovery
	// branch B drives back to this value.
	ActualIntentState bool `json:"actual_intent_state"`

	// ActualShimState is the state the autostart shim was in after
	// step 5 attempt failed — i.e. whatever the shim was BEFORE the
	// failed Enable call, since the call failed atomically. The
	// production fake-shim path treats this as the pre-call shim
	// strict_mode flag.
	ActualShimState bool `json:"actual_shim_state"`

	// Step1Error is the error from step 4 (intent write) if it
	// failed. Empty string when step 4 succeeded (the typical
	// breadcrumb-writing path).
	Step1Error string `json:"step1_error,omitempty"`

	// Step2Error is the error from step 5 (shim Enable) if it
	// failed.
	Step2Error string `json:"step2_error,omitempty"`

	// RevertError is the error from step 6 (intent re-write back to
	// original). Empty only when step 6 succeeded (in which case the
	// breadcrumb would not be written).
	RevertError string `json:"revert_error,omitempty"`

	// TS is the breadcrumb creation timestamp, UTC RFC3339Nano. Used
	// by the operator to correlate with watchdog/intent-audit logs.
	TS string `json:"ts"`

	// Phase distinguishes the in-progress crash-window marker from the
	// legacy both-failed (torn) shape. Empty (omitted) is the torn shape
	// for back-compat with pre-F11 breadcrumbs; "in-progress" is written
	// before step 1 and deleted after step 2 succeeds, so a value that
	// survives means a SIGKILL/power-loss happened in the step1→step2
	// window. See strictModeBreadcrumbPhase* constants.
	Phase string `json:"phase,omitempty"`
}

// StrictModeDeps holds the injected dependencies the strict-mode CLI
// consumes. Production callers fill only StateDir + IntentPath +
// BreadcrumbPath + AutostartBackend; PromptOperator defaults to a
// stdin-reading function.
//
// The deps struct is exported so the test file in the same package can
// build a fully-faked harness without exporting internal helpers.
type StrictModeDeps struct {
	// StateDir is the absolute path to the mcphub state directory,
	// used for migration.lock + --once.lock + breadcrumb resolution.
	StateDir string

	// IntentPath is the absolute path to supervisor-intent.json. The
	// directory must already exist (production: created by Task 1.1
	// state-path resolver; tests: t.TempDir() does this).
	IntentPath string

	// BreadcrumbPath is the absolute path to
	// strict-mode-mutation-incomplete.json.
	BreadcrumbPath string

	// AutostartBackend is the per-OS autostart backend. Tests inject
	// an in-memory fake; production callers pass autostart.New().
	AutostartBackend autostart.Backend

	// PromptOperator is the test seam for the --recover interactive
	// prompt. Returns "A" or "B" (anything else surfaces as a
	// non-fatal re-prompt). Production default reads from stdin.
	PromptOperator func() (string, error)

	// MutateIntentFn overrides the supervisor-intent locked RMW mutator for
	// tests that need to inject write failures. Production deps leave this nil
	// and the implementation falls back to api.MutateSupervisorIntentIfChanged.
	MutateIntentFn func(path string, mutate func(*api.SupervisorIntentFile) (bool, error)) error

	// Stdout/Stderr override the default os.Stdout / os.Stderr writers.
	// Tests may inject capture buffers; production leaves them nil and
	// the helpers below fall back to os.* defaults.
	Stdout io.Writer
	Stderr io.Writer
}

// newStrictModeCmd returns the top-level `mcphub strict-mode` cobra
// command. The single command handles enable/disable/--recover via
// argument + flag inspection so the cobra tree stays flat (matching the
// spec which calls them "strict-mode enable", "strict-mode disable",
// and "strict-mode --recover").
func newStrictModeCmd() *cobra.Command {
	var recover bool
	cmd := &cobra.Command{
		Use:   "strict-mode {enable|disable}",
		Short: "Atomically toggle supervisor strict-mode (intent + autostart shim)",
		Long: `mcphub strict-mode mutates the supervisor's strict-mode policy by
writing supervisor-intent.json AND the autostart shim's argv in a
single atomic operation. If either write fails, the other is reverted
so the two resources never drift.

  enable     — set strict_mode=true; shim launches with --strict-mode.
  disable    — set strict_mode=false; shim launches without the flag.
  --recover  — read the breadcrumb left by a torn previous run and
               reconcile both resources interactively.

Exit codes:
  9   STRICT_MODE_BUSY          — migration.lock or --once.lock held by
                                  another holder; wait and retry.
  10  STRICT_MODE_REVERT_FAILED — both shim write AND intent revert
                                  failed; breadcrumb written to
                                  <state-dir>/strict-mode-mutation-incomplete.json
                                  and operator must run --recover.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			deps, err := defaultStrictModeDeps()
			if err != nil {
				return fmt.Errorf("strict-mode deps: %w", err)
			}
			deps.Stdout = cmd.OutOrStdout()
			deps.Stderr = cmd.ErrOrStderr()
			if recover {
				return RunStrictModeRecover(deps)
			}
			return RunStrictMode(args, deps)
		},
	}
	cmd.Flags().BoolVar(&recover, "recover", false,
		"reconcile a torn previous run from the breadcrumb file")
	return cmd
}

// defaultStrictModeDeps assembles the production StrictModeDeps. State
// dir comes from the api.DaemonStateDir() resolver so the same path the
// supervisor uses is used here; autostart backend is autostart.New().
func defaultStrictModeDeps() (StrictModeDeps, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return StrictModeDeps{}, fmt.Errorf("state dir: %w", err)
	}
	backend, err := autostart.New()
	if err != nil {
		return StrictModeDeps{}, fmt.Errorf("autostart backend: %w", err)
	}
	return StrictModeDeps{
		StateDir:         stateDir,
		IntentPath:       filepath.Join(stateDir, "supervisor-intent.json"),
		BreadcrumbPath:   filepath.Join(stateDir, "strict-mode-mutation-incomplete.json"),
		AutostartBackend: backend,
		PromptOperator:   readStrictModeRecoverChoiceStdin,
	}, nil
}

// RunStrictMode is the testable entry point for `mcphub strict-mode
// enable` and `mcphub strict-mode disable`. Returns nil on success, a
// strict-mode-flavored forceExitError carrying the exit code (9/10) on
// failure paths the spec calls out, or a generic error for misuse.
func RunStrictMode(args []string, deps StrictModeDeps) error {
	if len(args) != 1 {
		return fmt.Errorf("expected one of {enable, disable}, got %d args", len(args))
	}
	var desired bool
	switch args[0] {
	case "enable":
		desired = true
	case "disable":
		desired = false
	default:
		return fmt.Errorf("unknown subcommand %q (expected enable|disable)", args[0])
	}

	// Refuse-if-breadcrumb (the operator must --recover first; the
	// migration locks are taken AFTER this check so a stuck breadcrumb
	// surfaces a precise error rather than a generic lock-busy).
	if _, err := os.Stat(deps.BreadcrumbPath); err == nil {
		return fmt.Errorf("strict-mode: prior mutation incomplete; run `mcphub strict-mode --recover` first (breadcrumb at %s)", deps.BreadcrumbPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("strict-mode: stat breadcrumb: %w", err)
	}

	// Acquire migration.lock + --once.lock in spec order; LIFO release.
	//
	// #301-3: AcquireStateDirLocks writes a lock OWNER SIDECAR
	// (migration.lock.owner.json / --once.lock.owner.json) through the same
	// hardened state-file pipeline as supervisor-intent.json. On a broadened
	// parent with a stale strict_mode=true intent, that sidecar write would be
	// refused by the SEC-F2 intent-derived gate (the FIRST
	// OperatorRequiresSingleUserHome call in a fresh `strict-mode disable`
	// process resolves the cache to strict here) → exit 9 BEFORE the mutation
	// could even start. The lock-sidecar writes are part of the strict-mode
	// mutation, so they run inside the env-only mutation-gate bypass too. The
	// window is the lock-acquire call only.
	locks, err := acquireStrictModeStateLocks(deps.StateDir)
	if err != nil {
		// Both ErrStateDirMigrationLockHeld and ErrStateDirOnceLockHeld
		// surface as STRICT_MODE_BUSY per spec Q8.
		return &forceExitError{code: ExitStrictModeBusy}
	}
	defer locks.Release()

	return runStrictModeUnderLocks(desired, deps)
}

// acquireStrictModeStateLocks acquires the universal state-dir locks for a
// strict-mode mutation with the #301-3 mutation-gate bypass active for the
// duration of the acquire. The lock owner-sidecar write goes through the
// secure state-file pipeline; without the bypass, a stale strict_mode=true
// intent on a broadened parent would refuse the sidecar write and strand the
// operator before the mutation begins. The bypass covers ONLY the acquire and
// is always cleared via WithStrictModeMutationGateBypass's defer.
func acquireStrictModeStateLocks(stateDir string) (*api.StateDirLockSet, error) {
	var locks *api.StateDirLockSet
	err := api.WithStrictModeMutationGateBypass(func() error {
		l, e := api.AcquireStateDirLocks(stateDir)
		if e != nil {
			return e
		}
		locks = l
		return nil
	})
	return locks, err
}

type strictModeShimFingerprintKind string

const (
	strictModeShimFingerprintAbsent          strictModeShimFingerprintKind = "absent"
	strictModeShimFingerprintEnabledMatching strictModeShimFingerprintKind = "enabled-matching"
	strictModeShimFingerprintDrifted         strictModeShimFingerprintKind = "drifted"
	strictModeShimFingerprintStaleResidue    strictModeShimFingerprintKind = "stale-residue"
)

type strictModeShimFingerprint struct {
	kind strictModeShimFingerprintKind
	spec string
}

func (f strictModeShimFingerprint) String() string {
	if f.spec == "" {
		return string(f.kind)
	}
	return string(f.kind) + ":" + f.spec
}

// strictModeShimDriftFingerprint keeps only the shim-shape facts strict-mode
// needs for rollback safety. Status distinguishes enabled-running from
// enabled-stopped for operator liveness reporting, but both mean the same
// installed shim matched the StrictMode probe options. Drifted/stale-residue
// include the backend-owned spec fingerprint so distinct drifted shims do not
// collapse into one coarse bucket.
func strictModeShimDriftFingerprint(snapshot autostart.StatusSnapshot) strictModeShimFingerprint {
	switch snapshot.State {
	case autostart.StateAbsent:
		return strictModeShimFingerprint{kind: strictModeShimFingerprintAbsent}
	case autostart.StateEnabledRunning, autostart.StateEnabledStopped:
		return strictModeShimFingerprint{kind: strictModeShimFingerprintEnabledMatching, spec: snapshot.SpecFingerprint}
	case autostart.StateDrifted:
		return strictModeShimFingerprint{kind: strictModeShimFingerprintDrifted, spec: snapshot.SpecFingerprint}
	case autostart.StateStaleResidue:
		return strictModeShimFingerprint{kind: strictModeShimFingerprintStaleResidue, spec: snapshot.SpecFingerprint}
	default:
		return strictModeShimFingerprint{
			kind: strictModeShimFingerprintKind(fmt.Sprintf("unknown:%d", snapshot.State)),
			spec: snapshot.SpecFingerprint,
		}
	}
}

// runStrictModeUnderLocks performs the two-resource mutation with the
// locks already held. Extracted so RunStrictMode and the test seam can
// share the body without duplicating the lock + breadcrumb scaffolding.
func runStrictModeUnderLocks(desired bool, deps StrictModeDeps) error {
	// Read current intent so we know the original value to revert to
	// on failure. Missing file is treated as default StrictMode=false.
	var originalStrict bool
	original, err := readStrictModeIntentSnapshot(deps)
	if err != nil {
		return fmt.Errorf("strict-mode: read intent: %w", err)
	}
	if original != nil {
		originalStrict = original.StrictMode
	}

	// If already in desired state on BOTH surfaces, this is a no-op,
	// but spec says re-issuing autostart.Enable is idempotent — fall
	// through and let it re-write the shim verbatim so any drift in
	// the recorded args gets reconciled. Intent re-write is also
	// idempotent (atomic rename of identical bytes is a no-op
	// observed by readers).

	// Snapshot the shim BEFORE mutating either owned resource. The Status
	// value still carries liveness-only noise (enabled-running vs
	// enabled-stopped), so the revert branch compares the drift fingerprint
	// derived from this baseline instead of raw State equality.
	shimSnapshot, snapshotErr := deps.AutostartBackend.StatusSnapshot(autostart.Options{StrictMode: originalStrict})
	shimSnapshotFingerprint := strictModeShimDriftFingerprint(shimSnapshot)

	// Forward-progress breadcrumb for the SIGKILL/power-loss window
	// (opus-3 F11). Write an IN-PROGRESS marker BEFORE step 1 and delete
	// it after step 2 succeeds. If the process dies in the step1→step2
	// window, intent ends up at `desired` while the shim stays at
	// `originalStrict` — a posture DRIFT with no handled-failure
	// breadcrumb. This marker is what `mcphub strict-mode --recover`
	// detects so it can reconcile the drift. ActualIntentState records the
	// pre-mutation value so --recover branch B (drive both to
	// actual_intent_state) rolls back; branch A (drive both to intended)
	// rolls forward — both branches drive intent + shim consistently and
	// so overwrite whatever partial state the crash left.
	inProgressBC := strictModeBreadcrumb{
		Intended:          desired,
		ActualIntentState: originalStrict,
		ActualShimState:   originalStrict,
		TS:                time.Now().UTC().Format(time.RFC3339Nano),
		Phase:             strictModeBreadcrumbPhaseInProgress,
	}
	if err := writeStrictModeBreadcrumb(deps.BreadcrumbPath, &inProgressBC); err != nil {
		return fmt.Errorf("strict-mode: write in-progress breadcrumb: %w", err)
	}

	// Step 1: write intent with new strict_mode value.
	if err := writeStrictModeIntent(deps, desired); err != nil {
		// A step-1 write error is only safe to treat as pre-mutation if a
		// best-effort re-read proves strict_mode is still at its original value.
		// The production writer publishes with an atomic rename before late
		// close/re-open/post-rename verification steps, so those late errors can
		// leave intent already changed while the shim has not been updated. In
		// that ambiguous/changed case, keep the in-progress breadcrumb so
		// `strict-mode --recover` can reconcile any drift.
		if current, readErr := readStrictModeIntentSnapshot(deps); readErr == nil && current.StrictMode == originalStrict {
			if rmErr := os.Remove(deps.BreadcrumbPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				return fmt.Errorf("strict-mode: step 1 (intent write): %w (cleanup in-progress breadcrumb failed: %v)", err, rmErr)
			}
		}
		return fmt.Errorf("strict-mode: step 1 (intent write): %w", err)
	}

	// Step 2: install/update shim with new strict_mode flag.
	if err := deps.AutostartBackend.Enable(autostart.Options{StrictMode: desired}); err != nil {
		// Revert step 1.
		if revertErr := writeStrictModeIntent(deps, originalStrict); revertErr != nil {
			// Both writes failed — overwrite the in-progress breadcrumb with
			// the torn shape + exit 10. The Phase reverts to torn ("") so the
			// --recover surface treats it as the handled both-failed case.
			bc := strictModeBreadcrumb{
				Intended:          desired,
				ActualIntentState: desired,        // step 1 succeeded, so intent IS at desired
				ActualShimState:   originalStrict, // step 2 failed, so shim stays at original
				Step1Error:        "",
				Step2Error:        err.Error(),
				RevertError:       revertErr.Error(),
				TS:                time.Now().UTC().Format(time.RFC3339Nano),
				Phase:             strictModeBreadcrumbPhaseTorn,
			}
			if writeBCErr := writeStrictModeBreadcrumb(deps.BreadcrumbPath, &bc); writeBCErr != nil {
				// Truly catastrophic — can't even write the breadcrumb.
				// Emit body to stderr so the operator can recover by
				// hand.
				body, _ := json.MarshalIndent(bc, "", "  ")
				fmt.Fprintln(stderrOrDefault(deps), string(body))
				fmt.Fprintf(stderrOrDefault(deps), "strict-mode: revert failed AND breadcrumb write failed: %v\n", writeBCErr)
				return &forceExitError{code: ExitStrictModeRevertFailed}
			}
			// Emit breadcrumb body to stderr so an operator running the
			// CLI in a terminal can see the state immediately without
			// hunting through the state dir.
			body, _ := json.MarshalIndent(bc, "", "  ")
			fmt.Fprintln(stderrOrDefault(deps), string(body))
			return &forceExitError{code: ExitStrictModeRevertFailed}
		}
		// Revert of step 1 (intent) succeeded — intent is back at
		// originalStrict. But the autostart shim is NOT proven to be back
		// at originalStrict: the per-OS backends mutate the shim before the
		// OS step can error, so `Enable` failing means the shim may be torn
		// (Windows: prior task deleted, new one never created; Linux/macOS:
		// unit/plist already overwritten with the NEW flag, enable/bootstrap
		// failed). Re-probe the shim and only delete the breadcrumb when the
		// shim drift fingerprint is PROVABLY unchanged from the pre-mutation
		// snapshot. Any uncertainty (snapshot unavailable, re-probe error, or
		// a fingerprint that differs from the snapshot) must KEEP a torn-shape
		// breadcrumb so `mcphub strict-mode --recover` can drive intent + shim
		// back into sync. The invariant: strict-mode never reports clean while
		// the shim might be out of sync with the (reverted) intent.
		shimProvenUnchanged := false
		var reprobe autostart.StatusSnapshot
		var reprobeErr error
		if snapshotErr == nil {
			reprobe, reprobeErr = deps.AutostartBackend.StatusSnapshot(autostart.Options{StrictMode: originalStrict})
			if reprobeErr == nil && strictModeShimDriftFingerprint(reprobe) == shimSnapshotFingerprint {
				shimProvenUnchanged = true
			}
		}
		if shimProvenUnchanged {
			// Shim is provably back at (== never left) originalStrict and
			// intent was reverted to originalStrict: no torn state, no drift.
			// Delete the in-progress breadcrumb (best-effort; a stale crumb
			// would otherwise force the operator to --recover a non-existent
			// drift). Surface the step 2 error so the operator can investigate
			// the underlying shim failure.
			if rmErr := os.Remove(deps.BreadcrumbPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
				fmt.Fprintf(stderrOrDefault(deps), "strict-mode: cleanup in-progress breadcrumb after revert: %v\n", rmErr)
			}
			return fmt.Errorf("strict-mode: step 2 (autostart shim): %w (intent reverted to %v, shim unchanged)", err, originalStrict)
		}

		// Shim state could NOT be proven unchanged after the failed Enable.
		// Intent was reverted to originalStrict, but the shim may be torn or
		// out of sync. Overwrite the in-progress breadcrumb with the torn
		// shape so `--recover` reconciles both resources. ActualIntentState
		// records where intent actually is now (originalStrict, since the
		// revert succeeded); --recover branch A drives both to Intended,
		// branch B drives both to ActualIntentState — either re-runs Enable
		// and repairs the shim.
		shimDetail := fmt.Sprintf("shim state could not be proven unchanged after failed Enable (snapshot=%v snapshotFingerprint=%v snapshotErr=%v reprobe=%v reprobeFingerprint=%v reprobeErr=%v)",
			shimSnapshot, shimSnapshotFingerprint, snapshotErr, reprobe, strictModeShimDriftFingerprint(reprobe), reprobeErr)
		bc := strictModeBreadcrumb{
			Intended:          desired,
			ActualIntentState: originalStrict, // revert succeeded, so intent IS back at original
			ActualShimState:   originalStrict, // best-effort: target the shim should hold; recover re-Enables regardless
			Step1Error:        "",
			Step2Error:        err.Error() + "; " + shimDetail,
			RevertError:       "",
			TS:                time.Now().UTC().Format(time.RFC3339Nano),
			Phase:             strictModeBreadcrumbPhaseTorn,
		}
		if writeBCErr := writeStrictModeBreadcrumb(deps.BreadcrumbPath, &bc); writeBCErr != nil {
			// Cannot even overwrite the breadcrumb — the in-progress marker
			// written before step 1 still survives on disk (it was never
			// deleted on this path), so `--recover` still has a marker to act
			// on. Emit the intended torn body + the write error to stderr so
			// the operator sees the full state immediately.
			body, _ := json.MarshalIndent(bc, "", "  ")
			fmt.Fprintln(stderrOrDefault(deps), string(body))
			fmt.Fprintf(stderrOrDefault(deps), "strict-mode: shim possibly torn AND torn-breadcrumb write failed: %v\n", writeBCErr)
			return &forceExitError{code: ExitStrictModeRevertFailed}
		}
		body, _ := json.MarshalIndent(bc, "", "  ")
		fmt.Fprintln(stderrOrDefault(deps), string(body))
		return &forceExitError{code: ExitStrictModeRevertFailed}
	}
	// Clean success: both resources reached `desired`. Delete the
	// in-progress breadcrumb so the next strict-mode invocation does not
	// refuse-if-held on a stale marker. A failed delete is non-fatal (the
	// mutation itself succeeded): the stale in-progress crumb is
	// self-healing — branch A of --recover drives both resources to
	// `intended`, which equals the value they already hold, an idempotent
	// no-op. Match the --recover cleanup-failure precedent: warn, don't
	// return an error that would misreport a successful mutation.
	if rmErr := os.Remove(deps.BreadcrumbPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		fmt.Fprintf(stderrOrDefault(deps), "strict-mode: cleanup in-progress breadcrumb after success: %v\n", rmErr)
	}
	// Deep-review round-2 P4 finding: the clean-success branch previously
	// left no supervisor-events.log row at all — only the torn/failure
	// recovery breadcrumb path was observable. strict_mode governs a
	// fail-closed security posture (Job-Object fallback refusal +
	// DACL-gate strictness), so a successful flip deserves the same
	// timestamped/actor audit trail every other supervisor-events.log
	// mutation gets. Best-effort: mirrors emitLivenessEvent
	// (supervise_ensure_alive.go) — a missing audit row must never fail
	// an otherwise-successful command.
	emitStrictModeChangedEvent(deps.StateDir, originalStrict, desired)
	return nil
}

// emitStrictModeChangedEvent records a best-effort
// "strict-mode-changed" row to supervisor-events.log after a clean
// two-resource strict-mode mutation. Mirrors the open-emit-close idiom
// in emitLivenessEvent (supervise_ensure_alive.go:316): the strict-mode
// CLI is a short-lived process, not the long-lived supervisor, so it
// opens its own handle rather than threading one through StrictModeDeps.
// Source is "autostart" because the observable half of this mutation
// operators care about at a glance is the autostart shim flip; the
// intent-file write is the other half of the same atomic pair recorded
// in the body. A failure to open/emit is silently swallowed — this is
// observability, not a gate, and the command has already succeeded.
func emitStrictModeChangedEvent(stateDir string, from, to bool) {
	logger, err := api.OpenSupervisorEventLog(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		return
	}
	defer func() { _ = logger.Close() }()
	_ = logger.Emit(api.SupervisorEvent{
		Severity: api.SupervisorEventSeverityInfo,
		Source:   api.SupervisorEventSourceAutostart,
		Event:    "strict-mode-changed",
		Body: map[string]any{
			"from": from,
			"to":   to,
		},
	})
}

// readStrictModeIntentSnapshot reads supervisor-intent.json under its
// file-content lock and the env-only mutation-gate bypass (#301-3).
//
// Why the wrap: SEC-F2 made the cached intent.strict_mode=true authoritative
// for ALL secure state-file writes, including this very intent write. On a
// broadened parent dir, the OLD strict_mode=true would refuse the NEW
// (disabling) intent write through the parent-dir gate, stranding the operator
// in strict mode exactly when they run the documented recovery path. The intent
// write is the authoritative gate-controlling value itself, so for the duration
// of the write the gate must consult the env var ONLY. The bypass window is the
// single write call and is always cleared by WithStrictModeMutationGateBypass's
// defer.
//
// The bypass is deliberately scoped to the intent file-content access, NOT the
// whole command: the autostart shim Enable call, the operator prompt, and any
// unrelated secure write outside this wrapper stay governed by the full
// env+intent gate.
func readStrictModeIntentSnapshot(deps StrictModeDeps) (*api.SupervisorIntentFile, error) {
	var snapshot *api.SupervisorIntentFile
	err := api.WithStrictModeMutationGateBypass(func() error {
		return api.MutateSupervisorIntentIfChanged(deps.IntentPath, func(file *api.SupervisorIntentFile) (bool, error) {
			if file == nil {
				snapshot = &api.SupervisorIntentFile{Version: 1}
				return false, nil
			}
			cp := *file
			snapshot = &cp
			return false, nil
		})
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func writeStrictModeIntent(deps StrictModeDeps, strict bool) error {
	mutate := deps.MutateIntentFn
	if mutate == nil {
		mutate = api.MutateSupervisorIntentIfChanged
	}
	return api.WithStrictModeMutationGateBypass(func() error {
		return mutate(deps.IntentPath, func(file *api.SupervisorIntentFile) (bool, error) {
			next := supervisorIntentWithStrictMode(file, strict)
			*file = *next
			return true, nil
		})
	})
}

func supervisorIntentWithStrictMode(existing *api.SupervisorIntentFile, strict bool) *api.SupervisorIntentFile {
	if existing == nil {
		return &api.SupervisorIntentFile{
			Version:    1,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			StrictMode: strict,
		}
	}
	next := *existing
	if next.Version == 0 {
		next.Version = 1
	}
	next.StrictMode = strict
	return &next
}

// writeStrictModeBreadcrumb writes the breadcrumb through
// WriteStateFileAtomic to keep the same fsync + atomic-rename guarantees
// the supervisor-intent.json write uses.
//
// #301-3: the breadcrumb is written to the SAME state dir as
// supervisor-intent.json and so is governed by the SAME parent-dir gate.
// SEC-F2 made the cached intent.strict_mode=true authoritative for ALL
// secure state-file writes; on a broadened parent that would refuse this
// breadcrumb write BEFORE the disabling intent write ever runs, stranding
// the operator. The breadcrumb is part of the strict-mode mutation itself
// (it is only ever written by enable/disable/--recover), so its write runs
// inside the env-only mutation-gate bypass too. The window is the single
// write call and is always cleared by WithStrictModeMutationGateBypass's
// defer.
func writeStrictModeBreadcrumb(path string, bc *strictModeBreadcrumb) error {
	return api.WithStrictModeMutationGateBypass(func() error {
		return api.WriteStateFileAtomic(path, bc)
	})
}

// stderrOrDefault returns deps.Stderr if non-nil, else os.Stderr.
func stderrOrDefault(deps StrictModeDeps) io.Writer {
	if deps.Stderr != nil {
		return deps.Stderr
	}
	return os.Stderr
}

// stdoutOrDefault returns deps.Stdout if non-nil, else os.Stdout.
func stdoutOrDefault(deps StrictModeDeps) io.Writer {
	if deps.Stdout != nil {
		return deps.Stdout
	}
	return os.Stdout
}

// readStrictModeRecoverChoiceStdin is the production PromptOperator
// implementation. Reads one line from stdin and trims whitespace.
func readStrictModeRecoverChoiceStdin() (string, error) {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return strings.TrimSpace(strings.ToUpper(line)), nil
}

// RunStrictModeRecover is the testable entry point for `mcphub
// strict-mode --recover`. Reads the breadcrumb, prompts the operator
// with two exhaustive branches, atomically reconciles, and deletes
// the breadcrumb on success.
func RunStrictModeRecover(deps StrictModeDeps) error {
	// Read breadcrumb.
	raw, err := api.ReadStateFileInodeAnchoredEnvStrictOnly(deps.BreadcrumbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("strict-mode --recover: no breadcrumb at %s (nothing to reconcile)", deps.BreadcrumbPath)
		}
		return fmt.Errorf("strict-mode --recover: read breadcrumb: %w", err)
	}
	var bc strictModeBreadcrumb
	if err := json.Unmarshal(raw, &bc); err != nil {
		return fmt.Errorf("strict-mode --recover: parse breadcrumb: %w", err)
	}

	// Acquire migration.lock + --once.lock (same refuse-if-held →
	// exit 9 behavior). #301-3: under the mutation-gate bypass so the
	// lock-owner-sidecar write is not refused by a stale strict intent on a
	// broadened parent (this is the recovery path; it MUST run on the very
	// hosts whose parent ACL just changed).
	locks, err := acquireStrictModeStateLocks(deps.StateDir)
	if err != nil {
		return &forceExitError{code: ExitStrictModeBusy}
	}
	defer locks.Release()

	// Prompt operator. Two branches:
	//   A — drive both resources to bc.Intended
	//   B — drive both resources to bc.ActualIntentState
	// Anything else is invalid (no third branch per spec); we re-prompt
	// up to 3 times then surface an error so the CLI doesn't loop
	// forever in scripted contexts.
	fmt.Fprintf(stdoutOrDefault(deps),
		"strict-mode --recover: torn mutation detected.\n"+
			"  intended:            %v\n"+
			"  actual_intent_state: %v\n"+
			"  actual_shim_state:   %v\n"+
			"  step2_error:         %s\n"+
			"  revert_error:        %s\n"+
			"\n"+
			"Choose a reconcile branch:\n"+
			"  (A) drive both intent + shim to intended (%v)\n"+
			"  (B) drive both intent + shim to actual_intent_state (%v)\n"+
			"Branch [A/B]: ",
		bc.Intended, bc.ActualIntentState, bc.ActualShimState,
		bc.Step2Error, bc.RevertError, bc.Intended, bc.ActualIntentState)
	if deps.PromptOperator == nil {
		deps.PromptOperator = readStrictModeRecoverChoiceStdin
	}
	var target bool
	for attempt := 0; attempt < 3; attempt++ {
		choice, err := deps.PromptOperator()
		if err != nil {
			return fmt.Errorf("strict-mode --recover: prompt: %w", err)
		}
		choice = strings.ToUpper(strings.TrimSpace(choice))
		switch choice {
		case "A":
			target = bc.Intended
		case "B":
			target = bc.ActualIntentState
		default:
			fmt.Fprintf(stdoutOrDefault(deps), "invalid choice %q — type A or B: ", choice)
			continue
		}
		// Got a valid branch; break out of the prompt loop.
		goto reconcile
	}
	return fmt.Errorf("strict-mode --recover: no valid branch chosen after 3 attempts")

reconcile:
	// Atomic two-resource reconcile, same pipeline as RunStrictMode but
	// without the breadcrumb-refusal pre-check (we're recovering FROM a
	// breadcrumb, so it MUST exist).
	if err := reconcileBothResources(target, deps); err != nil {
		// Re-assert breadcrumb on failure with refreshed TS, so the
		// operator can retry --recover.
		newBC := bc
		newBC.TS = time.Now().UTC().Format(time.RFC3339Nano)
		newBC.RevertError = fmt.Sprintf("recover failed: %v", err)
		_ = writeStrictModeBreadcrumb(deps.BreadcrumbPath, &newBC)
		return &forceExitError{code: ExitStrictModeRevertFailed}
	}

	// Delete breadcrumb on success.
	if err := os.Remove(deps.BreadcrumbPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		// Reconcile succeeded but breadcrumb cleanup failed — surface
		// to stderr but don't return an error; the next --recover
		// invocation will treat it as a torn state of "intent ==
		// actual == intended" and the operator can delete it by hand.
		fmt.Fprintf(stderrOrDefault(deps), "strict-mode --recover: cleanup breadcrumb: %v\n", err)
	}
	fmt.Fprintf(stdoutOrDefault(deps), "strict-mode --recover: reconciled to %v\n", target)
	return nil
}

// reconcileBothResources drives intent + shim to target in one
// transactional unit. Used by --recover; mirrors the step 4 + step 5
// sequence of runStrictModeUnderLocks but without revert (recover is
// the revert path, so a failure here is terminal).
func reconcileBothResources(target bool, deps StrictModeDeps) error {
	if err := writeStrictModeIntent(deps, target); err != nil {
		return fmt.Errorf("intent write: %w", err)
	}
	if err := deps.AutostartBackend.Enable(autostart.Options{StrictMode: target}); err != nil {
		return fmt.Errorf("shim enable: %w", err)
	}
	return nil
}
