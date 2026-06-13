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
//  4. Write supervisor-intent.json with the new strict_mode value.
//  5. Call autostart.Backend.Enable(Options{StrictMode: newValue}).
//  6. On step 5 failure: revert step 4 (re-write intent with original).
//  7. On step 6 failure: write breadcrumb + emit to stderr + exit 10.
//  8. LIFO release.
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
// stdin-reading function, and WriteIntentFn defaults to
// api.WriteSupervisorIntent.
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

	// WriteIntentFn overrides api.WriteSupervisorIntent for the
	// revert-failure injection seam. Production deps leave this nil
	// and the implementation falls back to api.WriteSupervisorIntent.
	WriteIntentFn func(path string, intent *api.SupervisorIntentFile) error

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
	locks, err := api.AcquireStateDirLocks(deps.StateDir)
	if err != nil {
		// Both ErrStateDirMigrationLockHeld and ErrStateDirOnceLockHeld
		// surface as STRICT_MODE_BUSY per spec Q8.
		return &forceExitError{code: ExitStrictModeBusy}
	}
	defer locks.Release()

	return runStrictModeUnderLocks(desired, deps)
}

// runStrictModeUnderLocks performs the two-resource mutation with the
// locks already held. Extracted so RunStrictMode and the test seam can
// share the body without duplicating the lock + breadcrumb scaffolding.
func runStrictModeUnderLocks(desired bool, deps StrictModeDeps) error {
	// Read current intent so we know the original value to revert to
	// on failure. Missing file is treated as default StrictMode=false.
	var originalStrict bool
	original, err := api.ReadSupervisorIntent(deps.IntentPath)
	if err == nil {
		originalStrict = original.StrictMode
	} else if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") && !strings.Contains(err.Error(), "cannot find the file") {
		// Treat ENOENT as default-false; surface every other read
		// error so we don't silently overwrite a corrupted intent.
		return fmt.Errorf("strict-mode: read intent: %w", err)
	}

	// If already in desired state on BOTH surfaces, this is a no-op,
	// but spec says re-issuing autostart.Enable is idempotent — fall
	// through and let it re-write the shim verbatim so any drift in
	// the recorded args gets reconciled. Intent re-write is also
	// idempotent (atomic rename of identical bytes is a no-op
	// observed by readers).

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
	newIntent := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		StrictMode: desired,
	}
	if original != nil {
		// Preserve fields we don't own — daemons, maintenance timers AND
		// the E2 stops sub-block (the sole per-daemon stop source; dropping
		// it here would wipe every operator stop on a strict-mode flip).
		// We only flip the strict_mode bit.
		newIntent.Version = original.Version
		if newIntent.Version == 0 {
			newIntent.Version = 1
		}
		newIntent.Daemons = original.Daemons
		newIntent.MaintenanceTimers = original.MaintenanceTimers
		newIntent.Stops = original.Stops
	}
	writeFn := deps.WriteIntentFn
	if writeFn == nil {
		writeFn = api.WriteSupervisorIntent
	}
	if err := writeFn(deps.IntentPath, newIntent); err != nil {
		// Step 1 failed before any strict-mode state was changed. The
		// in-progress breadcrumb only exists to cover a partial mutation
		// window, so remove it here; otherwise a transient initial intent
		// write failure would wedge future invocations behind --recover even
		// though there is no drift to reconcile.
		if rmErr := os.Remove(deps.BreadcrumbPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return fmt.Errorf("strict-mode: step 1 (intent write): %w (cleanup in-progress breadcrumb failed: %v)", err, rmErr)
		}
		return fmt.Errorf("strict-mode: step 1 (intent write): %w", err)
	}

	// Step 2: install/update shim with new strict_mode flag.
	if err := deps.AutostartBackend.Enable(autostart.Options{StrictMode: desired}); err != nil {
		// Revert step 1.
		revertIntent := &api.SupervisorIntentFile{
			Version:    1,
			UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
			StrictMode: originalStrict,
		}
		if original != nil {
			revertIntent.Version = original.Version
			if revertIntent.Version == 0 {
				revertIntent.Version = 1
			}
			revertIntent.Daemons = original.Daemons
			revertIntent.MaintenanceTimers = original.MaintenanceTimers
			revertIntent.Stops = original.Stops
		}
		if revertErr := writeFn(deps.IntentPath, revertIntent); revertErr != nil {
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
		// Revert succeeded — both resources are back at their original
		// strict-mode value, so there is no torn state and no drift: delete
		// the in-progress breadcrumb (best-effort; a stale crumb would
		// otherwise force the operator to --recover a non-existent drift).
		// Surface the step 2 error so the operator can investigate the
		// underlying shim failure.
		if rmErr := os.Remove(deps.BreadcrumbPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			fmt.Fprintf(stderrOrDefault(deps), "strict-mode: cleanup in-progress breadcrumb after revert: %v\n", rmErr)
		}
		return fmt.Errorf("strict-mode: step 2 (autostart shim): %w (intent reverted to %v)", err, originalStrict)
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
	return nil
}

// writeStrictModeBreadcrumb writes the breadcrumb through
// WriteStateFileAtomic to keep the same fsync + atomic-rename guarantees
// the supervisor-intent.json write uses.
func writeStrictModeBreadcrumb(path string, bc *strictModeBreadcrumb) error {
	return api.WriteStateFileAtomic(path, bc)
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
	raw, err := os.ReadFile(deps.BreadcrumbPath)
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
	// exit 9 behavior).
	locks, err := api.AcquireStateDirLocks(deps.StateDir)
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
	// Preserve daemons + maintenance timers from existing intent.
	var preserved *api.SupervisorIntentFile
	if existing, err := api.ReadSupervisorIntent(deps.IntentPath); err == nil {
		preserved = existing
	}
	newIntent := &api.SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
		StrictMode: target,
	}
	if preserved != nil {
		newIntent.Version = preserved.Version
		if newIntent.Version == 0 {
			newIntent.Version = 1
		}
		newIntent.Daemons = preserved.Daemons
		newIntent.MaintenanceTimers = preserved.MaintenanceTimers
		// E2 stops sub-block — preserve, same as the step-1 writer above.
		newIntent.Stops = preserved.Stops
	}
	writeFn := deps.WriteIntentFn
	if writeFn == nil {
		writeFn = api.WriteSupervisorIntent
	}
	if err := writeFn(deps.IntentPath, newIntent); err != nil {
		return fmt.Errorf("intent write: %w", err)
	}
	if err := deps.AutostartBackend.Enable(autostart.Options{StrictMode: target}); err != nil {
		return fmt.Errorf("shim enable: %w", err)
	}
	return nil
}
