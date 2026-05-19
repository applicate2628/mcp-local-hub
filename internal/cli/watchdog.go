// Package cli — Task 9 watchdog command surface (watchdog plan v13 §10,
// §11, §12, §14, §16, §17, §28, §29, §30, §31, §33, §34, §38, §44, §45,
// §53, §57, §58, §63, §64).
//
// `mcphub watchdog ...` exposes the run-once driver and the operator
// commands the scheduled task and `mcphub setup` rely on. Subcommands:
//
//   - `mcphub watchdog --once`     — one-shot recovery tick (this is the
//                                     scheduled-task entry point).
//   - `mcphub watchdog enable`     — clear stop intent (per daemon or all).
//   - `mcphub watchdog disable`    — write desired=stopped intent.
//   - `mcphub watchdog install`    — install the scheduled task (idempotent).
//   - `mcphub watchdog uninstall`  — remove the scheduled task (interactive
//                                     confirm + --yes per §64).
//   - `mcphub watchdog status`     — rich observability output (text/JSON).
//
// The `--once` driver implements the full plan §10 v8 flow:
//
//  1. Singleton flock (§33) on `<state-dir>/--once.lock`. Owner-info
//     sidecar `*.owner.json` for stale-detection on the next tick.
//  2. ctx.WithTimeout(ctx, 4*time.Minute) — OS-level ExecutionTimeLimit
//     of PT5M is the hard ceiling (§14).
//  3. ReadDaemonIntent + ReadWatchdogState (Tasks 2 + 4).
//  4. Wall-clock jump check (§29) — including missing-baseline-after-corrupt.
//  5. Corrupt-strike accumulation (§28) → self-quarantine on ≥4/30min.
//  6. LoadOwnershipSnapshot + LoadDaemonRegistry (Task 0).
//  7. NewOwnedXMLValidatorFromSnapshot (Task 6).
//  8. StatusContext (Task 0).
//  9. RecoverStoppedDaemons (Task 7) — strict-pure decision tree.
//  10. Healthy-Running cooldown reset (§6).
//  11. Apply decisions: restart-budget guard, IntentStillRunning re-check,
//      MarkRestartPending+RecordAttempt, PERSIST IMMEDIATELY (§30),
//      RestartContextWithSnapshot (§59), ClearRestartPending,
//      WaitDaemonRunning verify (observation-only — §14).
//  12. End-of-tick WriteWatchdogState (§36 err-first contract).
//
// Test seam strategy: the driver is split into the public RunE (cobra
// integration only) and an internal entrypoint runWatchdogOnce that
// accepts a context + an io.Writer for diagnostic output. Tests drive
// runWatchdogOnce directly with seam-injected scheduler / status /
// audit / intent / state-path fakes.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------------
// Tunables.
// ---------------------------------------------------------------------------

// watchdogTickDeadline is the §14 app-level ctx deadline for one
// `--once` invocation. The OS-level ExecutionTimeLimit (PT5M) is the
// hard ceiling — this 4-min app deadline lands just before that so the
// driver can perform a clean shutdown (final WriteWatchdogState) when
// it detects budget exhaustion.
const watchdogTickDeadline = 4 * time.Minute

// watchdogRestartBudgetMin is the §14 restart-budget guard: each
// individual restart attempt requires at least this much remaining ctx
// budget. Below the threshold the driver yields `ctx-budget-exhausted`
// and continues to the next decision rather than starting a restart it
// cannot finish.
const watchdogRestartBudgetMin = 60 * time.Second

// watchdogVerifyDeadline is the post-restart observation window. Per
// §14: verification is observation-only — the driver waits up to this
// long for the row to flip to Running but does NOT mutate the cooldown
// engine on success (the §6 5-min reset rule lives on the healthy-
// Running iteration in step 10 above).
const watchdogVerifyDeadline = 30 * time.Second

// wallClockJumpForwardThreshold is the upper §29 wall-clock-jump bound.
// A delta beyond this is treated as a system suspend/resume or NTP step;
// the watchdog persists the new value and skips the rest of the tick.
const wallClockJumpForwardThreshold = 24 * time.Hour

// wallClockSkewForwardThreshold is the §29 partial-skew log threshold.
// Deltas in (this, wallClockJumpForwardThreshold) are logged as
// `system-paused-or-skew-forward` but do not suppress the tick.
const wallClockSkewForwardThreshold = 7*time.Minute + 30*time.Second

// onceLockLeaf is the §33 singleton flock leaf name.
const onceLockLeaf = "--once.lock"

// onceOwnerInfoSuffix is appended to onceLockLeaf to form the sidecar
// file holding the {pid, started_at, hostname} JSON.
const onceOwnerInfoSuffix = ".owner.json"

// recentEventsTailN is the number of recent events surfaced by
// `mcphub watchdog status` per §34.
const recentEventsTailN = 20

// ---------------------------------------------------------------------------
// Exit codes (plan §10 + §16 + §49 + §64).
// ---------------------------------------------------------------------------

const (
	exitWatchdogSuccess              = 0
	exitWatchdogBackend              = 1
	exitWatchdogCtxDeadline          = 2
	exitWatchdogStatePathRejected    = 8
	exitWatchdogSelfQuarantined      = 9
	exitWatchdogEmergencyFallback    = 10
	exitWatchdogNonInteractiveNoYes  = 6
)

// ---------------------------------------------------------------------------
// Test seams.
// ---------------------------------------------------------------------------

// watchdogStdinIsTerminalFn, when non-nil, replaces term.IsTerminal on
// os.Stdin. Tests inject deterministic returns to drive the §64 non-TTY
// fail-fast path without forging a real PTY.
var watchdogStdinIsTerminalFn func() bool

// watchdogConfirmReaderFn, when non-nil, replaces os.Stdin reading for
// the interactive uninstall confirm. Tests inject "y\n" / "n\n" / EOF
// without redirecting actual stdin.
var watchdogConfirmReaderFn func() (string, error)

// watchdogNowFn, when non-nil, replaces time.Now() for deterministic
// tests. Production callers use the wall clock directly.
var watchdogNowFn func() time.Time

// watchdogSchtasksQueryWatchdogFn, when non-nil, replaces the
// `schtasks /Query /TN \mcp-local-hub-watchdog` invocation in the
// status command. Returns (taskExists, lastResult, err).
var watchdogSchtasksQueryWatchdogFn func() (bool, int32, error)

// ---------------------------------------------------------------------------
// Cobra command tree.
// ---------------------------------------------------------------------------

// newWatchdogCmdReal builds the `mcphub watchdog` parent command and its
// subcommand tree. Wired up by internal/cli/root.go.
func newWatchdogCmdReal() *cobra.Command {
	root := &cobra.Command{
		Use:   "watchdog",
		Short: "Auto-recovery layer for daemons (legacy v0.4.x; see `mcphub supervise`)",
		Long: `Manage the mcp-local-hub watchdog: a separate scheduled task that
runs every 5 minutes and revives daemons whose Task Scheduler
RestartOnFailure cannot recover (force-killed via Task Manager,
processes started via 'schtasks /Run' outside trigger context).

NOTE: v0.5.0 introduces a supervisor architecture (a single long-lived
'mcphub supervise' parent process per user) that replaces the per-daemon
watchdog model for new installs. See 'mcphub supervise --help' for the
canonical management surface. This 'mcphub watchdog' command remains
functional for legacy v0.4.x state files but does NOT extend or write
v0.5.0 supervisor state; new installs do not need it. 'mcphub watchdog
status' continues to surface v0.4.x diagnostics for incident
investigation on legacy installs.

Subcommands:
  watchdog --once                 # one-shot recovery tick (singleton-locked)
  watchdog enable [--server X]    # clear stop intent (per daemon or all)
  watchdog disable [--server X]   # write desired=stopped intent (permanent)
  watchdog install                # install scheduled task (idempotent)
  watchdog uninstall              # remove scheduled task (interactive confirm + --yes)
  watchdog status [--json]        # rich observability output

Self-quarantine: after 4 corrupt-state ticks within a 30-minute sliding
window the watchdog uninstalls its own scheduled task. Re-enable with
'mcphub watchdog install' after verifying state files are clean.

State files (per-user app-data):
  daemon-intent.json   — intent (desired state per daemon)
  watchdog-state.json  — cooldown + sliding-30min strike windows
  intent-audit.log     — audit log (10MB rotation)
  watchdog.log         — decision log (10MB rotation)
  --once.lock          — singleton lock with owner-info sidecar
`,
	}
	// Top-level --once flag. Cobra rejects PreRunE on the parent with
	// no subcommand, so we attach the flag here and route via RunE.
	var once bool
	root.Flags().BoolVar(&once, "once", false, "run one watchdog tick (used by the scheduled task)")
	root.RunE = func(cmd *cobra.Command, args []string) error {
		if !once {
			return cmd.Help()
		}
		// Drive the production once path with a fresh context. The OS-level
		// ExecutionTimeLimit (PT5M) bounds the process; the in-driver
		// 4-min ctx is the app-level guard.
		code := runWatchdogOnce(cmd.Context(), cmd.OutOrStderr())
		// Exit code is propagated via the typed forceExitError so cobra
		// preserves distinct codes (0/1/2/8/9/10) instead of collapsing
		// to its default exit-1-on-error.
		if code == exitWatchdogSuccess {
			return nil
		}
		return forceExit(code)
	}
	root.AddCommand(newWatchdogEnableCmd())
	root.AddCommand(newWatchdogDisableCmd())
	root.AddCommand(newWatchdogInstallCmd())
	root.AddCommand(newWatchdogUninstallCmd())
	root.AddCommand(newWatchdogStatusCmd())
	return root
}

// newWatchdogEnableCmd clears stop intent (per daemon or all) and emits
// an audit entry per Task 2's WriteDaemonIntent path.
func newWatchdogEnableCmd() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "enable",
		Short: "Clear stop intent (per daemon via --server NAME, or all)",
		Long: `Clear any user-stop / user-disabled intent so the watchdog auto-
revives the daemon on its next tick. Without --server, every recorded
intent is cleared.

Legacy v0.4.x surface: this writes to daemon-intent.json which the
v0.4.x watchdog scheduled task consumes. For v0.5.0+ installs, daemon
desired-state lives in supervisor-intent.json and is mutated via the
'mcphub supervise' control IPC; this enable command does not touch
v0.5.0 supervisor state.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			if server != "" {
				taskNames := serverTaskNames(a, server)
				if len(taskNames) == 0 {
					return fmt.Errorf("no managed daemons found for server %q", server)
				}
				for _, tn := range taskNames {
					if err := a.ClearDaemonIntent(tn, "mcphub watchdog enable"); err != nil {
						return fmt.Errorf("clear intent %s: %w", tn, err)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "✓ Cleared intent: %s\n", tn)
				}
				return nil
			}
			// Clear every recorded intent.
			intentR := a.ReadDaemonIntent()
			if intentR.State == api.IntentStateCorrupt {
				return fmt.Errorf("intent file is corrupt; quarantined to %s; aborting enable", intentR.QuarantinePath)
			}
			for tn := range intentR.File.Tasks {
				if err := a.ClearDaemonIntent(tn, "mcphub watchdog enable"); err != nil {
					return fmt.Errorf("clear intent %s: %w", tn, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Cleared intent: %s\n", tn)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "limit to one server's daemons; default: clear all recorded intents")
	return c
}

// newWatchdogDisableCmd writes desired=stopped + reason=user-disabled
// for one server's daemons. Audit fail-closed per Task 10 §10 contract.
func newWatchdogDisableCmd() *cobra.Command {
	var server string
	c := &cobra.Command{
		Use:   "disable",
		Short: "Write desired=stopped intent (--server X for one server, or all daemons)",
		Long: `Record a permanent stop directive. The watchdog will refuse to
auto-revive the daemon until 'mcphub watchdog enable' clears the intent.
Distinct from 'mcphub stop': stop is a 24-hour TTL'd directive, while
disable persists until explicitly cleared.

Legacy v0.4.x surface: this writes to daemon-intent.json which the
v0.4.x watchdog scheduled task consumes. For v0.5.0+ installs, daemon
desired-state lives in supervisor-intent.json and is mutated via the
'mcphub supervise' control IPC; this disable command does not touch
v0.5.0 supervisor state.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a := api.NewAPI()
			now := watchdogNow()
			intent := api.DaemonIntent{
				Desired:   api.IntentDesiredStopped,
				Reason:    api.IntentReasonUserDisabled,
				UpdatedAt: now,
			}
			var taskNames []string
			if server != "" {
				taskNames = serverTaskNames(a, server)
				if len(taskNames) == 0 {
					return fmt.Errorf("no managed daemons found for server %q", server)
				}
			} else {
				// Disable all known managed daemons (registry-driven so
				// orphans are skipped).
				snap := a.LoadOwnershipSnapshot()
				for srv, daemons := range snap.ManifestDaemons {
					for d := range daemons {
						taskNames = append(taskNames, "\\mcp-local-hub-"+srv+"-"+d)
					}
				}
				for _, tn := range snap.WorkspaceTasksByKey {
					taskNames = append(taskNames, tn)
				}
			}
			if len(taskNames) == 0 {
				return fmt.Errorf("no managed daemons found")
			}
			for _, tn := range taskNames {
				if err := a.WriteDaemonIntent(tn, intent, "mcphub watchdog disable"); err != nil {
					return fmt.Errorf("write intent %s: %w", tn, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ Disabled: %s\n", tn)
			}
			return nil
		},
	}
	c.Flags().StringVar(&server, "server", "", "limit to one server's daemons; default: disable all managed daemons")
	return c
}

// newWatchdogInstallCmd installs the scheduled task that runs
// `mcphub watchdog --once` every 5 minutes. Idempotent.
//
// Plan v13 §42 + §61: refuses to proceed when the current process
// is elevated (Administrator/root) UNLESS --allow-elevated is set.
// With --allow-elevated, a high-priority audit entry is written
// FIRST per §61; audit-write failure → exit 11 (audit-required-
// but-failed) and the scheduled task is NOT installed.
func newWatchdogInstallCmd() *cobra.Command {
	var allowElevated bool
	c := &cobra.Command{
		Use:   "install",
		Short: "Install the watchdog scheduled task (idempotent; plan §42 + §61)",
		Long: `Create the \mcp-local-hub-watchdog scheduled task. Re-running this
overwrites any existing task with the canonical XML body.

Legacy v0.4.x surface: 'mcphub setup' auto-installs the watchdog task
on v0.4.x. For v0.5.0+ installs, the canonical lifecycle is owned by
the supervisor task ('mcphub supervise' under \mcp-local-hub-supervisor
LogonTrigger). New installs should not need 'mcphub watchdog install';
the watchdog scheduled task can coexist with the v0.5.0 supervisor for
operators running mixed legacy state, but it is not required.

Plan §42 elevation refusal: refuses to install when invoked from an
elevated process (Administrator on Windows; root on POSIX) UNLESS
--allow-elevated is passed. The watchdog runs as a per-user task; an
install from an elevated context could land with the wrong principal.

Plan §61 fail-closed audit: with --allow-elevated, a high-priority
audit entry is written FIRST. If the audit write fails (any error),
exit 11 (audit-required-but-failed) and the scheduled task is NOT
installed.

Exit codes:
  0   success
  11  --allow-elevated audit write failed (plan §61)
  Other failures use cobra's default non-zero exit.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()

			// §42 elevation detection (shared with mcphub setup).
			elevated, elevErr := setupIsElevated()
			if elevErr != nil {
				fmt.Fprintf(out, "⚠ elevation detector failed: %v (treating as elevated)\n", elevErr)
				elevated = true
			}
			if elevated && !allowElevated {
				return fmt.Errorf(
					"mcphub watchdog install must run un-elevated (plan §42); use --allow-elevated to override (audit fail-closed per §61)")
			}
			a := api.NewAPI()
			if elevated && allowElevated {
				if err := a.AppendIntentAudit(api.NewIntentAuditEntry(
					api.WithAction(api.AuditActionWatchdogInstallElevatedOverride),
					api.WithTask(api.WatchdogTaskName),
					api.WithWho(api.AuditWhoMcphubWatchdogInstall),
					api.WithPriority("high"),
					api.WithReason("--allow-elevated flag explicit override"),
				)); err != nil {
					fmt.Fprintf(out,
						"✗ audit log unwritable; --allow-elevated requires audit trail (plan §61): %v\n", err)
					return forceExit(exitSetupAuditRequiredButFailed)
				}
			}
			if err := a.InstallWatchdogTask(); err != nil {
				return fmt.Errorf("install watchdog task: %w", err)
			}
			fmt.Fprintln(out, "✓ Installed scheduled task: \\mcp-local-hub-watchdog (cadence 5 min)")
			return nil
		},
	}
	c.Flags().BoolVar(&allowElevated, "allow-elevated", false,
		"override plan §42 elevation refusal (records a high-priority audit entry; fail-closed if audit fails per §61)")
	return c
}

// newWatchdogUninstallCmd removes the scheduled task. Per §64:
//   - --yes: proceed without prompt; audit Priority=high.
//   - no --yes + TTY: prompt y/N; cancel on n/N/EOF.
//   - no --yes + non-TTY: fail-fast at exit 6.
func newWatchdogUninstallCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the watchdog scheduled task (interactive confirm + --yes)",
		Long: `Remove the \mcp-local-hub-watchdog scheduled task. The watchdog
state files (daemon-intent.json, watchdog-state.json, *.log) are NOT
touched — re-running 'mcphub watchdog install' resumes from the same
cooldown / strike-window state.

Legacy v0.4.x surface: removing the watchdog task is safe on v0.5.0+
installs where 'mcphub supervise' owns the daemon lifecycle. Operators
migrating from v0.4.x can run this uninstall once the v0.5.0 supervisor
is confirmed reconcile-ready (see 'mcphub install --upgrade' migration
journal markers). State files are preserved so a future v0.4.x rollback
keeps the same per-daemon cooldown / strike-window history.

Decision tree:
  --yes set                       → proceed without prompt + audit (Priority=high)
  --yes not set + TTY stdin       → prompt 'y/N'; cancel on n/N/EOF
  --yes not set + non-TTY stdin   → fail-fast at exit 6 (CI / scripted)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatchdogUninstall(cmd.OutOrStdout(), cmd.OutOrStderr(), yes)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "proceed without interactive confirm (CI / scripted)")
	return c
}

// runWatchdogUninstall is the testable entry point for `mcphub watchdog
// uninstall`. Returns nil on success, a typed forceExitError on the
// non-TTY-without-yes path, or a regular error on any other failure.
func runWatchdogUninstall(stdout, stderr io.Writer, yes bool) error {
	a := api.NewAPI()

	// 1. --yes flag set: proceed unconditionally.
	if yes {
		if err := writeWatchdogUninstallAudit(a); err != nil {
			return fmt.Errorf("audit write failed: %w", err)
		}
		if err := a.UninstallWatchdogTask(); err != nil {
			return fmt.Errorf("uninstall watchdog task: %w", err)
		}
		fmt.Fprintln(stdout, "✓ Uninstalled scheduled task: \\mcp-local-hub-watchdog")
		return nil
	}

	// 2. No --yes: stdin TTY check.
	if !watchdogStdinIsTerminal() {
		fmt.Fprintln(stderr, "error: mcphub watchdog uninstall is interactive; use --yes flag in non-interactive contexts")
		return forceExit(exitWatchdogNonInteractiveNoYes)
	}

	// 3. TTY: prompt + read response.
	fmt.Fprint(stdout, "This will disable mcp-local-hub watchdog. Confirm? [y/N] ")
	resp, err := readWatchdogConfirm()
	if err != nil {
		// EOF / read error → treat as cancel (consistent with §64).
		fmt.Fprintln(stdout, "uninstall cancelled")
		return nil
	}
	resp = strings.TrimSpace(resp)
	if resp != "y" && resp != "Y" {
		fmt.Fprintln(stdout, "uninstall cancelled")
		return nil
	}

	// 4. Confirmed: write audit + remove task.
	if err := writeWatchdogUninstallAudit(a); err != nil {
		return fmt.Errorf("audit write failed: %w", err)
	}
	if err := a.UninstallWatchdogTask(); err != nil {
		return fmt.Errorf("uninstall watchdog task: %w", err)
	}
	fmt.Fprintln(stdout, "✓ Uninstalled scheduled task: \\mcp-local-hub-watchdog")
	return nil
}

// writeWatchdogUninstallAudit emits the public-uninstall audit entry
// per §64. Action="watchdog-uninstalled-by-user" + Priority="high"
// (non-system entry — IS subject to caller_user redaction in display).
func writeWatchdogUninstallAudit(a *api.API) error {
	return a.AppendIntentAudit(api.NewIntentAuditEntry(
		api.WithAction("watchdog-uninstalled-by-user"),
		api.WithTask(api.WatchdogTaskName),
		api.WithPriority("high"),
		api.WithReason("mcphub watchdog uninstall (public)"),
	))
}

// watchdogStdinIsTerminal returns true if stdin is a real TTY. Tests
// inject via watchdogStdinIsTerminalFn.
func watchdogStdinIsTerminal() bool {
	if watchdogStdinIsTerminalFn != nil {
		return watchdogStdinIsTerminalFn()
	}
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// readWatchdogConfirm reads one line from stdin. Tests inject via
// watchdogConfirmReaderFn.
func readWatchdogConfirm() (string, error) {
	if watchdogConfirmReaderFn != nil {
		return watchdogConfirmReaderFn()
	}
	var buf [256]byte
	n, err := os.Stdin.Read(buf[:])
	if err != nil && n == 0 {
		return "", err
	}
	return string(buf[:n]), nil
}

// ---------------------------------------------------------------------------
// `--once` driver — the heart of Task 9 (plan §10 v8).
// ---------------------------------------------------------------------------

// runWatchdogOnce drives one tick of the watchdog. Returns the desired
// process exit code (see exitWatchdog* constants). All persistent state
// mutations happen here (Cooldown engine writes, audit appends,
// watchdog.log appends, scheduled-task uninstall on self-quarantine).
//
// The function is split into a singleton-lock outer wrapper +
// once-locked inner body so unit tests can drive the inner body
// without flock interference. The outer body owns ctx-deadline and
// owner-info sidecar lifecycle.
func runWatchdogOnce(parentCtx context.Context, diagnostic io.Writer) int {
	a := api.NewAPI()
	now := watchdogNow()

	// 1. State path resolution. Failure → exit 8 per §16.
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		fmt.Fprintf(diagnostic, "watchdog: state path rejected: %v\n", err)
		return exitWatchdogStatePathRejected
	}

	// 2. Singleton flock (§33).
	lockPath := filepath.Join(stateDir, onceLockLeaf)
	ownerPath := lockPath + onceOwnerInfoSuffix

	lock := flock.New(lockPath)
	locked, lockErr := lock.TryLock()
	if lockErr != nil {
		fmt.Fprintf(diagnostic, "watchdog: TryLock error: %v\n", lockErr)
		// Permission/IO errors at lock-acquire time count as backend
		// errors. Subsequent ticks may succeed.
		return exitWatchdogBackend
	}
	if !locked {
		// Already-running: read owner-info best-effort, log, return 0.
		owner := readWatchdogOwnerInfoBestEffort(ownerPath)
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "already-running-skipped",
			Note:   formatOwnerInfoNote(owner),
		})
		return exitWatchdogSuccess
	}

	// 3. Owner-info sidecar.
	ownerJSON, _ := json.Marshal(map[string]any{
		"pid":        os.Getpid(),
		"started_at": now.UTC().Format(time.RFC3339Nano),
		"hostname":   safeHostname(),
	})
	_ = os.WriteFile(ownerPath, ownerJSON, 0o600)
	defer func() {
		_ = lock.Unlock()
		_ = os.Remove(ownerPath)
	}()

	// 4. App-level ctx deadline (§14).
	ctx, cancel := context.WithTimeout(parentCtx, watchdogTickDeadline)
	defer cancel()

	// 5. Drive the inner body.
	return runWatchdogOnceInner(ctx, a, now, diagnostic)
}

// runWatchdogOnceInner is the testable body of `--once`. The singleton
// flock + owner-info sidecar lifecycle is owned by runWatchdogOnce; the
// inner body assumes the lock is held and the state dir exists.
//
// Tests in watchdog_test.go drive this directly with seam-injected
// scheduler / status / audit / intent / state-path fakes.
func runWatchdogOnceInner(ctx context.Context, a *api.API, now time.Time, diagnostic io.Writer) int {
	intentR := a.ReadDaemonIntent()
	coolR := a.ReadWatchdogState()

	// 5b. A4-b PR #2 runtime applier — wire daemons.retry_policy
	// into the cooldown engine BEFORE any Due() consultation. The
	// policy's MaxAttempts() becomes the per-15-min-window attempt
	// cap; "none" → 1, "linear" → 3, "exponential" → 5. Backoff()
	// is irrelevant under the fixed 5-min scheduler cadence so
	// only MaxAttempts is honored. SettingsGet errors fall through
	// to the registry default ("exponential") via PolicyFromString.
	policyVal, _ := a.SettingsGet("daemons.retry_policy")
	resolved, maxAttempts := api.ApplyRetryPolicy(&coolR, policyVal)
	fmt.Fprintf(diagnostic, "watchdog: retry policy = %s (maxAttempts=%d)\n", resolved, maxAttempts)

	// 6. Wall-clock jump check (§29).
	if code, suppressed := wallClockJumpCheck(a, &coolR, intentR, now, diagnostic); suppressed {
		return code
	}

	// 7. Corrupt-strike accumulation + self-quarantine (§28).
	if intentR.State == api.IntentStateCorrupt || coolR.State == api.WatchdogStateCorrupt {
		return runCorruptStrikePath(a, &coolR, intentR, now, diagnostic)
	}

	// 8. Defensive-copy snapshots.
	//
	// Codex deep-sec PR #135 Finding 4: ownership snapshot loads MUST
	// fail-closed when the workspace registry is corrupt or unreachable
	// — running a tick on a partial snapshot risks classifying a real
	// lazy-proxy task as orphan (lost recovery) OR a phantom task as
	// owned (false-positive restart). Drop the tick on this exit path
	// with the same backend exit code (1) that other persistence-side
	// failures use.
	registry := a.LoadDaemonRegistry()
	ownership, ownershipErr := a.LoadOwnershipSnapshotChecked()
	if ownershipErr != nil {
		fmt.Fprintf(diagnostic, "watchdog: LoadOwnershipSnapshotChecked: %v\n", ownershipErr)
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "ownership-snapshot-failed",
			Err:    ownershipErr.Error(),
		})
		_ = persistEndOfTickState(a, &coolR, now, diagnostic)
		return exitWatchdogBackend
	}

	// 9. StatusContext.
	rows, statusErr := a.StatusContext(ctx)
	if statusErr != nil {
		fmt.Fprintf(diagnostic, "watchdog: StatusContext: %v\n", statusErr)
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "status-failed",
			Err:    statusErr.Error(),
		})
		// Final state write (records LastWallClockSeen even on partial
		// tick) so the next tick can detect wall-clock baseline.
		_ = persistEndOfTickState(a, &coolR, now, diagnostic)
		// Distinguish ctx-deadline (exit 2) from generic backend (exit 1).
		if errors.Is(statusErr, context.DeadlineExceeded) || errors.Is(statusErr, context.Canceled) {
			return exitWatchdogCtxDeadline
		}
		return exitWatchdogBackend
	}

	// 10. Snapshot-bound XML validator (§47).
	validator := api.NewOwnedXMLValidatorFromSnapshot(ownership)

	// 11. Pure-decision pass (§1, §12).
	decisions := api.RecoverStoppedDaemons(now, rows, intentR.File, coolR.Cool, validator, registry)

	// 12. Healthy-Running cooldown reset (§6).
	for _, row := range rows {
		if row.State == "Running" {
			coolR.Cool.RecordRunning(row.TaskName, now)
		}
	}

	// 13. Apply decisions.
	for _, d := range decisions {
		// Per-decision ctx check — a hung Restart on the previous decision
		// can blow the budget; check before each one.
		if ctx.Err() != nil {
			_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
				Task:   "<once-driver>",
				Action: "ctx-deadline-exceeded",
				Err:    ctx.Err().Error(),
			})
			// Persist + return 2 per §58 + §10 v8.
			_ = persistEndOfTickState(a, &coolR, now, diagnostic)
			return exitWatchdogCtxDeadline
		}
		switch d.Action {
		case "restart":
			applyRestartDecision(ctx, a, &coolR, ownership, d, now, diagnostic)
		case "chronic-failure":
			applyChronicFailureDecision(a, d, diagnostic)
		case "suspicious-xml":
			_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
				Task:     d.TaskName,
				Action:   "suspicious-xml",
				Priority: "high",
				Note:     "validator rejected — see watchdog-self-quarantined audit entries for context",
			})
		default:
			// Informational vocabulary — log-only.
			_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
				Task:   d.TaskName,
				Action: d.Action,
				Intent: d.Reason,
			})
		}
	}

	// 14. End-of-tick state write (§36 err-first contract).
	if !persistEndOfTickState(a, &coolR, now, diagnostic) {
		return exitWatchdogBackend
	}
	return exitWatchdogSuccess
}

// applyRestartDecision implements the restart branch from plan §10 v8 +
// §30. Idempotent against ctx-cancel and audit-failure paths.
//
// coolR is taken by pointer so stale-clear strike accumulation
// (Codex bot P3 — populate coolR.StaleClearWindow) is visible to the
// caller's end-of-tick WriteWatchdogState. Cooldown engine mutation
// already worked via interface reference, but the StaleClearWindow
// slice header is value-typed and would not propagate by value.
func applyRestartDecision(
	ctx context.Context,
	a *api.API,
	coolR *api.WatchdogStateRead,
	ownership api.OwnershipSnapshot,
	d api.RecoveryDecision,
	now time.Time,
	diagnostic io.Writer,
) {
	// 1. Restart-budget guard (§14).
	deadline, ok := ctx.Deadline()
	if ok && time.Until(deadline) < watchdogRestartBudgetMin {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:     d.TaskName,
			Action:   "ctx-budget-exhausted",
			Attempts: d.Attempt,
		})
		return
	}

	// 2. Concurrency-safe intent re-read (§11): a `mcphub stop` could
	//    have landed between the pure-decision pass and this branch.
	if !a.IntentStillRunning(d.TaskName, watchdogNow()) {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   d.TaskName,
			Action: "stop-race-aborted",
		})
		return
	}

	// 3. MarkRestartPending + RecordAttempt.
	mutNow := watchdogNow()
	coolR.Cool.MarkRestartPending(d.TaskName, mutNow)
	coolR.Cool.RecordAttempt(d.TaskName, mutNow)

	// 4. PERSIST IMMEDIATELY (§30) — durability against mid-restart kill.
	staleEvents, writeErr := a.WriteWatchdogState(*coolR, mutNow)
	if writeErr != nil {
		// Per §36 staleEvents is guaranteed nil on err.
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   d.TaskName,
			Action: "pre-restart-state-write-failed",
			Err:    writeErr.Error(),
		})
		// IMPORTANT: do NOT call RestartContextWithSnapshot — fail closed.
		// Next tick re-reads stale state and re-evaluates fresh.
		return
	}
	// Successful write: log any stale-clear events surfaced by the sweep
	// AND populate coolR.StaleClearWindow so `mcphub watchdog status`
	// stops reporting a perpetual zero (Codex bot P3). The strikes are
	// persisted by the end-of-tick WriteWatchdogState in
	// runWatchdogOnceInner; if the process is killed before that final
	// write, the next tick re-reads the older window — acceptable per
	// the §45 v9 best-effort observability contract.
	for _, name := range staleEvents {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:      name,
			Action:    "restart-pending-stale-cleared",
			Note:      "likely process kill mid-restart on prior tick",
			ClearedAt: mutNow,
		})
		coolR.StaleClearWindow = api.AppendStrike(coolR.StaleClearWindow, mutNow, api.StaleClearThreshold)
	}
	maybeEmitStaleClearStrikeAlert(a, coolR.StaleClearWindow, mutNow)

	// 5. Snapshot-bound restart (§59) — kill-by-port targets snap.PortMap,
	//    not the live manifest.
	_, restartErr := a.RestartContextWithSnapshot(ctx, d.Server, d.Daemon, ownership)
	coolR.Cool.ClearRestartPending(d.TaskName)

	if restartErr != nil {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   d.TaskName,
			Action: "restart-failed",
			Err:    restartErr.Error(),
		})
		return
	}

	// 6. Restart-verify (observation-only — §14). The §6 healthy-Running
	//    reset rule lives on the top-level loop; this branch must NOT
	//    call RecordRunning (Code Review #5 IMPORTANT).
	verifyCtx, vc := context.WithTimeout(ctx, watchdogVerifyDeadline)
	running := a.WaitDaemonRunning(verifyCtx, d.TaskName)
	vc()

	if running {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:     d.TaskName,
			Action:   "restart-verified-running",
			Attempts: d.Attempt,
		})
	} else {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:     d.TaskName,
			Action:   "restart-not-yet-running-after-30s",
			Attempts: d.Attempt,
		})
	}
	_ = now
	_ = diagnostic
}

// applyChronicFailureDecision writes a chronic-failure stop intent for
// the daemon and emits the watchdog.log entry.
func applyChronicFailureDecision(a *api.API, d api.RecoveryDecision, diagnostic io.Writer) {
	intent := api.DaemonIntent{
		Desired:   api.IntentDesiredStopped,
		Reason:    api.IntentReasonChronicFailure,
		UpdatedAt: watchdogNow(),
	}
	if err := a.WriteDaemonIntent(d.TaskName, intent, "mcphub watchdog --once"); err != nil {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   d.TaskName,
			Action: "chronic-failure-intent-write-failed",
			Err:    err.Error(),
		})
		return
	}
	_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
		Task:     d.TaskName,
		Action:   "chronic-failure",
		Priority: "high",
		Intent:   "user-disabled",
		Note:     "chronic-failure cycle limit reached; auto-revive halted",
	})
	_ = diagnostic
}

// persistEndOfTickState writes coolR's state to disk and logs any
// stale-clear events. Returns true on success, false on write failure
// (caller maps to exit 1 per §10 v8).
//
// Codex bot P3: each stale-clear event surfaced by the sweep also
// appends a timestamp to coolR.StaleClearWindow so the documented
// 4-events-in-30min threshold can actually trigger. The newly
// appended strikes won't be persisted by THIS write (the write has
// already committed); they land on the next tick's
// WriteWatchdogState. That one-tick lag is acceptable for an
// observability-only signal (no quarantine action).
func persistEndOfTickState(a *api.API, coolR *api.WatchdogStateRead, now time.Time, diagnostic io.Writer) bool {
	staleEvents, err := a.WriteWatchdogState(*coolR, now)
	if err != nil {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "end-of-tick-state-write-failed",
			Err:    err.Error(),
		})
		fmt.Fprintf(diagnostic, "watchdog: end-of-tick state write failed: %v\n", err)
		return false
	}
	for _, name := range staleEvents {
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:      name,
			Action:    "restart-pending-stale-cleared",
			Note:      "end-of-tick stale-clear",
			ClearedAt: now,
		})
		coolR.StaleClearWindow = api.AppendStrike(coolR.StaleClearWindow, now, api.StaleClearThreshold)
	}
	maybeEmitStaleClearStrikeAlert(a, coolR.StaleClearWindow, now)
	return true
}

// maybeEmitStaleClearStrikeAlert emits a single high-priority
// `stale-clear-strike-alert` watchdog.log entry when the supplied
// StaleClearWindow has reached the §45 v9 threshold of 4 events
// within a 30-minute sliding window. Mirrors the §28 corrupt-strike
// self-quarantine check shape, but observability-only — no
// quarantine, no scheduler.Delete.
//
// The alert fires when len(window) >= StaleClearThreshold AND the
// span between the oldest and newest entries is <= 30 minutes. The
// in-window check is redundant given AppendStrike's drop-old-then-
// append shape, but the explicit guard documents the contract and
// defends against future callers that bypass AppendStrike.
func maybeEmitStaleClearStrikeAlert(a *api.API, window []time.Time, now time.Time) {
	if len(window) < api.StaleClearThreshold {
		return
	}
	oldest := oldestStrike(window)
	if oldest.IsZero() {
		return
	}
	if now.Sub(oldest) > api.StrikeWindowDuration {
		return
	}
	_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
		Task:     "<once-driver>",
		Action:   "stale-clear-strike-alert",
		Priority: "high",
		Note: fmt.Sprintf(
			"%d stale-clear events in last 30min; observability-only (no quarantine)",
			len(window),
		),
	})
}

// ---------------------------------------------------------------------------
// Wall-clock jump check (plan §29).
// ---------------------------------------------------------------------------

// wallClockJumpCheck implements the §29 decision tree. Returns
// (exitCode, suppressed) — suppressed=true means the tick is over and
// the caller returns exitCode immediately.
//
// coolR is a pointer so the persistEndOfTickState call inside this
// helper can append to coolR.StaleClearWindow (Codex bot P3) when
// WriteWatchdogState surfaces stale-clear events.
func wallClockJumpCheck(
	a *api.API,
	coolR *api.WatchdogStateRead,
	intentR api.IntentReadResult,
	now time.Time,
	diagnostic io.Writer,
) (int, bool) {
	last := coolR.LastWallClock

	// Missing-baseline-after-corrupt (§29): if there's no recorded
	// LastWallClockSeen but we have prior corrupt strikes, the previous
	// tick was likely a corrupt-state suppression that didn't get to
	// persist the baseline. Treat conservatively — suppress + return 0
	// rather than restarting from a synthetic now.
	if last.IsZero() {
		// Look at the most recent corrupt strike. If one exists, this is
		// the missing-baseline-after-corrupt case.
		if len(coolR.CorruptStrikeWindow) > 0 {
			_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
				Task:   "<once-driver>",
				Action: "wall-clock-baseline-missing-after-corrupt",
				Note:   "prior tick corrupt; deferring restart eligibility until next tick",
			})
			// Persist the new baseline so the next tick can compute deltas.
			_ = persistEndOfTickState(a, coolR, now, diagnostic)
			return exitWatchdogSuccess, true
		}
		// First-ever tick: no baseline, no strikes — record the baseline
		// and continue normally.
		return exitWatchdogSuccess, false
	}

	delta := now.Sub(last)
	switch {
	case delta < 0:
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "clock-skew-backward",
			Note:   fmt.Sprintf("delta=%s now=%s last=%s", delta, now.Format(time.RFC3339Nano), last.Format(time.RFC3339Nano)),
		})
		// Continue — backward skew is logged but not suppression.
	case delta > wallClockJumpForwardThreshold:
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "wall-clock-jump-suspect",
			Note:   fmt.Sprintf("delta=%s exceeds 24h threshold; suppressing tick", delta),
		})
		// Persist the new last-seen so the next tick has a fresh baseline.
		_ = persistEndOfTickState(a, coolR, now, diagnostic)
		return exitWatchdogSuccess, true
	case delta > wallClockSkewForwardThreshold:
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:   "<once-driver>",
			Action: "system-paused-or-skew-forward",
			Note:   fmt.Sprintf("delta=%s above 7m30s threshold", delta),
		})
		// Continue — partial skew is logged but does not suppress.
	}
	_ = intentR
	return exitWatchdogSuccess, false
}

// runCorruptStrikePath implements the §28 strike accumulation +
// self-quarantine path. Returns the desired exit code.
//
// coolR is a pointer so AppendStrike mutations to
// CorruptStrikeWindow propagate back to the caller (and
// persistEndOfTickState picks up StaleClearWindow updates per
// Codex bot P3).
func runCorruptStrikePath(
	a *api.API,
	coolR *api.WatchdogStateRead,
	intentR api.IntentReadResult,
	now time.Time,
	diagnostic io.Writer,
) int {
	// Append a strike to the corrupt window, then check threshold.
	coolR.CorruptStrikeWindow = api.AppendStrike(coolR.CorruptStrikeWindow, now, api.CorruptStrikeThreshold)
	_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
		Task:     "<once-driver>",
		Action:   "corrupt-strike-recorded",
		Priority: "high",
		Note: fmt.Sprintf(
			"intent_state=%s intent_quarantine=%s cooldown_state=%s cooldown_quarantine=%s strikes_in_window=%d",
			intentR.State, intentR.QuarantinePath, coolR.State, coolR.QuarantinePath, len(coolR.CorruptStrikeWindow),
		),
	})
	if len(coolR.CorruptStrikeWindow) >= api.CorruptStrikeThreshold {
		// Persist the strike window BEFORE uninstalling so forensic
		// state survives — even though state files were already corrupt,
		// this write may succeed against the freshly quarantined slate.
		_ = persistEndOfTickState(a, coolR, now, diagnostic)
		if err := a.UninstallWatchdogTaskInternal(api.QuarantineFourStrikes30Min); err != nil {
			// Audit-degraded cascade (§49). For Task 9 v0.3.0 we surface
			// stderr + return exit 10. The eventlog/syslog richer paths
			// land in Task 11+.
			fmt.Fprintf(diagnostic, "[mcphub-watchdog] AUDIT-DEGRADED: self-quarantine uninstall failed: %v\n", err)
			_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
				Task:     api.WatchdogTaskName,
				Action:   "self-quarantine-uninstall-failed",
				Priority: "high",
				Err:      err.Error(),
			})
			return exitWatchdogEmergencyFallback
		}
		_ = a.AppendWatchdogLog(api.WatchdogLogEntry{
			Task:     api.WatchdogTaskName,
			Action:   "watchdog-self-quarantined",
			Priority: "high",
			Note:     api.QuarantineFourStrikes30Min.SuggestedAction(),
		})
		return exitWatchdogSelfQuarantined
	}
	// Below threshold: persist + return 0.
	if !persistEndOfTickState(a, coolR, now, diagnostic) {
		return exitWatchdogBackend
	}
	return exitWatchdogSuccess
}

// ---------------------------------------------------------------------------
// `watchdog status` (plan §34, §44, §53, §57, §63).
// ---------------------------------------------------------------------------

// watchdogStatusJSON is the serialized form of `mcphub watchdog status
// --json`. Stable across versions (no implicit changes); §52 schema
// extension lives in plan v0.4.x.
type watchdogStatusJSON struct {
	StateDir            string                  `json:"state_dir"`
	Files               watchdogStatusFiles     `json:"files"`
	ScheduledTask       string                  `json:"scheduled_task"`
	Cadence             string                  `json:"cadence"`
	LastWallClock       time.Time               `json:"last_wall_clock"`
	LastWallClockAge    string                  `json:"last_wall_clock_age,omitempty"`
	CorruptStrikeCount  int                     `json:"corrupt_strike_count"`
	CorruptStrikeOldest time.Time               `json:"corrupt_strike_oldest,omitempty"`
	AuditFailureCount   int                     `json:"audit_failure_count"`
	StaleClearCount     int                     `json:"stale_clear_count"`
	SelfQuarantined     bool                    `json:"self_quarantined"`
	SelfQuarantineNote  string                  `json:"self_quarantine_note,omitempty"`
	StatusUnknown       bool                    `json:"status_unknown,omitempty"`
	StatusUnknownNote   string                  `json:"status_unknown_note,omitempty"`
	WatchdogLastResult  *int32                  `json:"watchdog_last_result,omitempty"`
	RecentEvents        []api.WatchdogLogEntry  `json:"recent_events"`
	AuditTail           []api.IntentAuditEntry  `json:"audit_tail"`
	LastFlockSkip       *watchdogStatusFlockSkip `json:"last_flock_skip,omitempty"`
}

// watchdogStatusFiles lists the absolute paths of all per-user state
// files (plan §57).
type watchdogStatusFiles struct {
	DaemonIntent    string `json:"daemon_intent"`
	WatchdogState   string `json:"watchdog_state"`
	IntentAuditLog  string `json:"intent_audit_log"`
	WatchdogLog     string `json:"watchdog_log"`
	OnceLock        string `json:"once_lock"`
}

// watchdogStatusFlockSkip describes the most recent --once-lock owner
// recorded in --once.lock.owner.json. Plan §40: status surfaces both
// LOCK BUSY and STALE labels.
type watchdogStatusFlockSkip struct {
	PID       int       `json:"pid"`
	Hostname  string    `json:"hostname"`
	StartedAt time.Time `json:"started_at"`
	Age       string    `json:"age"`
	Stale     bool      `json:"stale"`
}

// newWatchdogStatusCmd builds `mcphub watchdog status [--json]`.
func newWatchdogStatusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Rich observability output for the watchdog (--json for machine-readable form)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWatchdogStatus(cmd.OutOrStdout(), asJSON)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON instead of human-readable text")
	return c
}

// runWatchdogStatus is the testable entry point for `mcphub watchdog
// status`. Pulls every datapoint from the same APIs the driver uses;
// no live schtasks invocation if the watchdogSchtasksQueryWatchdogFn
// seam is set.
func runWatchdogStatus(out io.Writer, asJSON bool) error {
	a := api.NewAPI()

	stateDir, _ := api.DaemonStateDir() // empty on failure; render anyway

	files := watchdogStatusFiles{
		DaemonIntent:   filepath.Join(stateDir, "daemon-intent.json"),
		WatchdogState:  filepath.Join(stateDir, "watchdog-state.json"),
		IntentAuditLog: filepath.Join(stateDir, "intent-audit.log"),
		WatchdogLog:    filepath.Join(stateDir, "watchdog.log"),
		OnceLock:       filepath.Join(stateDir, onceLockLeaf),
	}

	cool := a.ReadWatchdogState()
	now := watchdogNow()

	// Scheduled-task probe.
	taskInstalled, lastResult, queryErr := watchdogSchtasksQueryWatchdog()

	// Self-quarantine status (§63 v11): both signals required.
	auditTail := a.ReadIntentAuditTail(recentEventsTailN)
	hasSelfQuarantineEntry := false
	for _, e := range auditTail {
		if e.Action == "watchdog-self-quarantined" {
			hasSelfQuarantineEntry = true
			break
		}
	}
	// If audit tail didn't have it, scan the older entries via a deeper
	// read. The recent-events tail bound is N=20; in a high-throughput
	// system the self-quarantine entry could have rolled out. Read
	// 1000 entries for the determination.
	if !hasSelfQuarantineEntry {
		extended := a.ReadIntentAuditTail(1000)
		for _, e := range extended {
			if e.Action == "watchdog-self-quarantined" {
				hasSelfQuarantineEntry = true
				break
			}
		}
	}

	selfQuarantined := false
	statusUnknown := false
	statusUnknownNote := ""
	selfQuarantineNote := ""

	switch {
	case !taskInstalled && hasSelfQuarantineEntry:
		selfQuarantined = true
		selfQuarantineNote = api.QuarantineFourStrikes30Min.SuggestedAction()
	case !taskInstalled && !hasSelfQuarantineEntry:
		// Either someone hand-deleted the task OR audit log corrupted.
		// Per §63: STATUS UNKNOWN when only one signal is present.
		if queryErr != nil {
			// The query reported "task not found" cleanly (taskInstalled=false
			// + no error) is one path; queryErr non-nil means a different
			// failure mode (access denied / generic).
			statusUnknown = true
			statusUnknownNote = fmt.Sprintf("schtasks query returned error (%v); audit log shows no self-quarantine entry. Possible causes: schtasks permissions issue OR partial uninstall. Investigate manually.", queryErr)
		} else {
			statusUnknown = true
			statusUnknownNote = "scheduled task missing but no self-quarantine audit entry found. Possible causes: hand-deleted by operator without using mcphub OR audit log corrupted. Investigate manually."
		}
	case taskInstalled && hasSelfQuarantineEntry:
		// Task is back (operator re-installed) but historical audit
		// shows quarantine. Not currently quarantined.
		selfQuarantined = false
	}

	// Flock-skip section (§33 + §40).
	flockSkip := readWatchdogFlockSkip(files.OnceLock, now)

	recentEvents := a.ReadWatchdogLogTail(recentEventsTailN)
	// Apply caller_user redaction to non-system audit entries (§34).
	redactedAudit := make([]api.IntentAuditEntry, len(auditTail))
	for i, e := range auditTail {
		redactedAudit[i] = api.RedactIntentAuditEntryForNonOwner(e)
	}

	// Build the JSON struct (used for both --json and the text path's
	// internal data marshalling).
	statusData := watchdogStatusJSON{
		StateDir:            stateDir,
		Files:               files,
		ScheduledTask:       formatScheduledTaskState(taskInstalled, queryErr, statusUnknown, selfQuarantined),
		Cadence:             "5 min",
		LastWallClock:       cool.LastWallClock.UTC(),
		LastWallClockAge:    formatAge(cool.LastWallClock, now),
		CorruptStrikeCount:  len(cool.CorruptStrikeWindow),
		CorruptStrikeOldest: oldestStrike(cool.CorruptStrikeWindow),
		AuditFailureCount:   len(cool.AuditFailureWindow),
		StaleClearCount:     len(cool.StaleClearWindow),
		SelfQuarantined:     selfQuarantined,
		SelfQuarantineNote:  selfQuarantineNote,
		StatusUnknown:       statusUnknown,
		StatusUnknownNote:   statusUnknownNote,
		RecentEvents:        recentEvents,
		AuditTail:           redactedAudit,
		LastFlockSkip:       flockSkip,
	}
	if taskInstalled && lastResult != 0 {
		lr := lastResult
		statusData.WatchdogLastResult = &lr
	}

	if asJSON {
		raw, err := json.MarshalIndent(statusData, "", "  ")
		if err != nil {
			return fmt.Errorf("status json: %w", err)
		}
		fmt.Fprintln(out, string(raw))
		return nil
	}

	return renderWatchdogStatusText(out, statusData, now)
}

// renderWatchdogStatusText emits the human-readable form (plan §57).
func renderWatchdogStatusText(out io.Writer, s watchdogStatusJSON, now time.Time) error {
	fmt.Fprintln(out, "NOTE: v0.5.0 introduces a supervisor architecture that replaces the")
	fmt.Fprintln(out, "      per-daemon watchdog model for new installs. See `mcphub supervise")
	fmt.Fprintln(out, "      --help` for the canonical management surface; this watchdog command")
	fmt.Fprintln(out, "      remains read-only for legacy v0.4.x diagnostics.")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "State dir: %s\n", s.StateDir)
	fmt.Fprintln(out, "Files:")
	fmt.Fprintf(out, "  %s    (intent)\n", s.Files.DaemonIntent)
	fmt.Fprintf(out, "  %s   (cooldown + windows)\n", s.Files.WatchdogState)
	fmt.Fprintf(out, "  %s      (audit log; rotates at 10MB → .log.1)\n", s.Files.IntentAuditLog)
	fmt.Fprintf(out, "  %s          (decision log; rotates at 10MB → .log.1)\n", s.Files.WatchdogLog)
	fmt.Fprintf(out, "  %s           (singleton lock)\n", s.Files.OnceLock)

	fmt.Fprintln(out)
	fmt.Fprintln(out, "Watchdog status:")
	fmt.Fprintf(out, "  Scheduled task: %s\n", s.ScheduledTask)
	fmt.Fprintf(out, "  Cadence: %s\n", s.Cadence)
	if s.LastWallClock.IsZero() {
		fmt.Fprintln(out, "  Last wall-clock seen: <never>")
	} else {
		fmt.Fprintf(out, "  Last wall-clock seen: %s (%s ago)\n", s.LastWallClock.Format(time.RFC3339Nano), s.LastWallClockAge)
	}
	if s.CorruptStrikeCount == 0 {
		fmt.Fprintln(out, "  Corrupt strike window: 0 entries")
	} else {
		fmt.Fprintf(out, "  Corrupt strike window: %d entries (oldest %s ago)\n",
			s.CorruptStrikeCount, formatAge(s.CorruptStrikeOldest, now))
	}
	fmt.Fprintf(out, "  Audit failure window: %d entries\n", s.AuditFailureCount)
	fmt.Fprintf(out, "  Stale clear window: %d entries\n", s.StaleClearCount)

	switch {
	case s.SelfQuarantined:
		fmt.Fprintln(out, "  Self-quarantined: yes")
		fmt.Fprintf(out, "    Suggested action: %s\n", s.SelfQuarantineNote)
	case s.StatusUnknown:
		fmt.Fprintf(out, "  WATCHDOG STATUS UNKNOWN: %s\n", s.StatusUnknownNote)
	default:
		fmt.Fprintln(out, "  Self-quarantined: no")
	}

	if s.WatchdogLastResult != nil && *s.WatchdogLastResult != 0 {
		fmt.Fprintf(out, "  WATCHDOG SELF-FAILURE INDICATOR: last run exited 0x%x\n", *s.WatchdogLastResult)
	}

	if s.LastFlockSkip != nil {
		busy := "[LOCK BUSY]"
		if s.LastFlockSkip.Stale {
			busy = "[STALE — lock not currently held]"
		}
		fmt.Fprintf(out, "  Last flock skip: PID %d from %s started %s (%s ago) %s\n",
			s.LastFlockSkip.PID, s.LastFlockSkip.Hostname,
			s.LastFlockSkip.StartedAt.Format(time.RFC3339Nano),
			s.LastFlockSkip.Age, busy)
	}

	fmt.Fprintln(out)
	if len(s.RecentEvents) == 0 {
		fmt.Fprintln(out, "  Recent events: <none>")
	} else {
		fmt.Fprintln(out, "  Recent events (last 20 from watchdog.log):")
		for _, e := range s.RecentEvents {
			fmt.Fprintf(out, "    %s  %-32s  task=%s",
				e.TS.Format(time.RFC3339Nano), e.Action, e.Task)
			if e.Err != "" {
				fmt.Fprintf(out, "  err=%q", e.Err)
			}
			if e.Note != "" {
				fmt.Fprintf(out, "  note=%q", e.Note)
			}
			fmt.Fprintln(out)
		}
	}

	fmt.Fprintln(out)
	if len(s.AuditTail) == 0 {
		fmt.Fprintln(out, "  Audit log tail: <none>")
	} else {
		fmt.Fprintln(out, "  Audit log tail (last 20 from intent-audit.log, redacted for non-owner):")
		for _, e := range s.AuditTail {
			fmt.Fprintf(out, "    %s  %-32s  task=%s  user=%s",
				e.TS.Format(time.RFC3339Nano), e.Action, e.Task, e.CallerUser)
			if e.Reason != "" {
				fmt.Fprintf(out, "  reason=%q", e.Reason)
			}
			fmt.Fprintln(out)
		}
	}
	return nil
}

// formatScheduledTaskState renders the §53 status block.
func formatScheduledTaskState(installed bool, queryErr error, statusUnknown, selfQuarantined bool) string {
	switch {
	case selfQuarantined:
		return "not-installed-self-quarantined"
	case statusUnknown && queryErr != nil:
		return fmt.Sprintf("STATUS UNKNOWN [%v]", queryErr)
	case statusUnknown:
		return "STATUS UNKNOWN [task missing; no self-quarantine audit entry]"
	case installed:
		return "installed"
	default:
		return "not-installed"
	}
}

// formatAge renders a duration relative to `now` as a coarse human-
// readable string ("4m21s ago").
func formatAge(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	return now.Sub(t).Truncate(time.Second).String()
}

// oldestStrike returns the oldest entry in the supplied window, or the
// zero Time when the window is empty.
func oldestStrike(window []time.Time) time.Time {
	if len(window) == 0 {
		return time.Time{}
	}
	oldest := window[0]
	for _, t := range window[1:] {
		if t.Before(oldest) {
			oldest = t
		}
	}
	return oldest
}

// readWatchdogFlockSkip reads the --once.lock.owner.json sidecar (best-
// effort) and probes whether the lock is currently held. Returns nil
// when no sidecar exists. Otherwise sets Stale=true when the lock is
// NOT busy — operator can manually break a stale lock.
func readWatchdogFlockSkip(lockPath string, now time.Time) *watchdogStatusFlockSkip {
	ownerPath := lockPath + onceOwnerInfoSuffix
	raw, err := os.ReadFile(ownerPath)
	if err != nil {
		return nil
	}
	var owner ownerInfo
	if err := json.Unmarshal(raw, &owner); err != nil {
		return nil
	}
	// Probe lock-busy: TryLock + Unlock if successful means the lock
	// is currently NOT held. Per §40 we treat that combination as STALE.
	stale := false
	probe := flock.New(lockPath)
	locked, lockErr := probe.TryLock()
	if lockErr == nil && locked {
		_ = probe.Unlock()
		stale = true
	}
	startedAt := owner.StartedAt
	return &watchdogStatusFlockSkip{
		PID:       owner.PID,
		Hostname:  owner.Hostname,
		StartedAt: startedAt,
		Age:       formatAge(startedAt, now),
		Stale:     stale,
	}
}

// ownerInfo mirrors the owner-JSON sidecar shape.
type ownerInfo struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Hostname  string    `json:"hostname"`
}

// readWatchdogOwnerInfoBestEffort reads + parses the owner-info sidecar
// during the `already-running-skipped` log path. Returns the zero
// ownerInfo on any failure (display path tolerates blanks).
func readWatchdogOwnerInfoBestEffort(path string) ownerInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ownerInfo{}
	}
	var owner ownerInfo
	if err := json.Unmarshal(raw, &owner); err != nil {
		return ownerInfo{}
	}
	return owner
}

// formatOwnerInfoNote renders the owner-info into a single-line note
// suitable for the `already-running-skipped` watchdog.log entry.
func formatOwnerInfoNote(owner ownerInfo) string {
	if owner.PID == 0 && owner.Hostname == "" && owner.StartedAt.IsZero() {
		return ""
	}
	return fmt.Sprintf("PID %d from %s started %s",
		owner.PID, owner.Hostname, owner.StartedAt.Format(time.RFC3339Nano))
}

// safeHostname returns the OS hostname or "<unknown>" on error.
func safeHostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "<unknown>"
	}
	return h
}

// watchdogNow returns the current UTC instant, routed through the
// watchdogNowFn seam for tests.
func watchdogNow() time.Time {
	if watchdogNowFn != nil {
		return watchdogNowFn().UTC()
	}
	return time.Now().UTC()
}

// watchdogSchtasksQueryWatchdog probes the scheduled task. Returns
// (installed, lastResult, err). On non-Windows hosts and tests with
// the seam unset, returns (true, 0, nil) so status keeps rendering
// ("installed" without a self-failure indicator).
func watchdogSchtasksQueryWatchdog() (bool, int32, error) {
	if watchdogSchtasksQueryWatchdogFn != nil {
		return watchdogSchtasksQueryWatchdogFn()
	}
	// Production probe: exec schtasks /Query /TN \mcp-local-hub-watchdog
	// /XML and inspect output. For Task 9 v0.3.0 we delegate to the
	// scheduler interface — Status() returns the LastResult code; an
	// ErrTaskNotFound means the task is uninstalled.
	a := api.NewAPI()
	rows, err := a.Status()
	if err != nil {
		return false, 0, err
	}
	for _, row := range rows {
		if row.TaskName == api.WatchdogTaskName {
			return true, row.LastResult, nil
		}
	}
	return false, 0, nil
}

// serverTaskNames returns the canonical task names for one server. Used
// by `enable` / `disable` to expand --server NAME into a list of
// per-daemon tasks. Skips workspace-scoped lazy proxies.
func serverTaskNames(a *api.API, server string) []string {
	snap := a.LoadOwnershipSnapshot()
	daemons, ok := snap.ManifestDaemons[server]
	if !ok {
		return nil
	}
	var out []string
	for d := range daemons {
		out = append(out, "\\mcp-local-hub-"+server+"-"+d)
	}
	return out
}

// ---------------------------------------------------------------------------
// Helpers / placeholders for compile-time symbol resolution.
// ---------------------------------------------------------------------------

// stateDirFromAPI keeps the package depending on api.DaemonStateDir
// without exposing it in the cmd plumbing. Currently unused outside
// tests; retained for future surface additions.
//
//nolint:unused // referenced by future watchdog tests per plan v13.
func stateDirFromAPI() string {
	dir, _ := api.DaemonStateDir()
	return dir
}

// formatLastResult returns the LastResult code as a hex-like string.
//
//nolint:unused // referenced by future watchdog tests per plan v13.
func formatLastResult(code int32) string {
	if code == 0 {
		return "0"
	}
	return strconv.FormatInt(int64(code), 10)
}

