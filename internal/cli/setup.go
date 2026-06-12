package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
)

// ---------------------------------------------------------------------------
// Task 11 watchdog wiring — exit codes (plan §16, §61).
// ---------------------------------------------------------------------------

// exitSetupStatePathRejected is returned when DaemonStateDir() fails
// during `mcphub setup` per plan §16. The watchdog can never run
// without a valid per-user state dir; aborting before any scheduler
// call leaves a clean rollback.
const exitSetupStatePathRejected = 8

// exitSetupAuditRequiredButFailed is returned when the
// --allow-elevated audit entry write fails per plan §61. The audit
// trail is a HARD requirement for the override; if we cannot record
// the override, the install is rejected.
const exitSetupAuditRequiredButFailed = 11

// exitSetupLivenessInstallFailed is returned when setup cannot install the
// supervisor-liveness recovery task. Setup must fail closed here because the
// legacy watchdog is removed only after this replacement exists.
const exitSetupLivenessInstallFailed = 12

// ---------------------------------------------------------------------------
// Test seams (package-level fn vars).
//
// Production: nil → fall back to the real OS-bound implementation.
// Tests in setup_watchdog_test.go set these to deterministic fakes
// inside setupWatchdogTestHelper.
// ---------------------------------------------------------------------------

// setupIsElevatedFn, when non-nil, replaces the real
// isElevatedReal() helper. Tests inject deterministic returns to
// drive the §42 elevation refusal + §61 audit fail-closed paths
// without needing to spawn a real elevated process.
var setupIsElevatedFn func() (bool, error)

// setupRegisterEventLogFn, when non-nil, replaces
// registerEventLogSourceReal() during runSetupWatchdog. Tests
// inject failures to verify the §60 non-fatal cascade.
var setupRegisterEventLogFn func() error

// setupRemoveEventLogFn, when non-nil, replaces
// removeEventLogSourceReal() during runUninstallWatchdog. Tests
// assert the call count to verify the §60 uninstall path runs.
var setupRemoveEventLogFn func() error

// setupStateDirSanityFn, when non-nil, replaces the api.DaemonStateDir
// resolver call inside runSetupWatchdog. Tests inject the synthetic
// rejection that the production resolver would emit on a hostile
// environment (KnownFolderUnavailable / posixDirSanityCheck).
var setupStateDirSanityFn func() (string, error)

// setupLSPClientRouterFn is the test seam for the Phase 3 client-config
// reconcile. Production routes through api.NewAPI(); tests replace it so
// setup coverage does not copy binaries, install watchdogs, or touch real
// client configs.
var setupLSPClientRouterFn = func(rollback bool) (*api.LSPClientRouterReport, error) {
	if rollback {
		return api.NewAPI().RollbackLSPRouterClientEntries(api.LSPClientRouterOpts{})
	}
	return api.NewAPI().EnsureLSPRouterClientEntries(api.LSPClientRouterOpts{})
}

// setupBootstrapFn and setupWatchdogFn let command-level setup tests assert
// orchestration without copying the current binary or mutating Task Scheduler.
// Production leaves them nil and routes through Bootstrap/runSetupWatchdog.
var (
	setupBootstrapFn func(io.Writer) error
	setupWatchdogFn  func(io.Writer, bool) error
)

// mcphubShortName is the bare executable name that scheduler tasks and relay
// entries reference. PATH resolution picks the correct binary from whatever
// directory the user has on PATH (usually ~/.local/bin after `mcphub setup`).
var mcphubShortName = func() string {
	if runtime.GOOS == "windows" {
		return "mcphub.exe"
	}
	return "mcphub"
}()

// setupTargetDir returns the canonical install directory for the current
// user: <home>/.local/bin on all platforms.
func setupTargetDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "bin"), nil
}

// setupTargetPath returns the canonical install path for the mcphub binary.
func setupTargetPath() (string, error) {
	dir, err := setupTargetDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, mcphubShortName), nil
}

// samePath returns true when a and b refer to the same absolute filesystem
// location. Case-insensitive on Windows (NTFS/ReFS are case-preserving but
// case-insensitive by default); case-sensitive elsewhere.
func samePath(a, b string) bool {
	ac, err := filepath.Abs(filepath.Clean(a))
	if err != nil {
		return false
	}
	bc, err := filepath.Abs(filepath.Clean(b))
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(ac, bc)
	}
	return ac == bc
}

// copyExe copies src to dst via a tempfile + rename so a failed copy never
// leaves a partial exe at dst. On Windows an existing dst must be removed
// first because os.Rename refuses to overwrite; if dst is locked by a
// running process we surface a friendly hint.
func copyExe(src, dst string) error {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return fmt.Errorf("tempfile in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	// Copy + close; on any failure make sure the tempfile does not survive.
	_, copyErr := io.Copy(tmp, in)
	closeErr := tmp.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("copy to tempfile: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close tempfile: %w", closeErr)
	}
	// Preserve executable bit on non-Windows; Windows ignores mode bits here.
	if err := os.Chmod(tmpPath, 0755); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	// Windows: os.Rename over an existing file fails. Remove first; if that
	// fails because the target is held open by a running daemon, give a clear
	// hint instead of the raw sharing-violation error.
	if _, err := os.Stat(dst); err == nil {
		if err := os.Remove(dst); err != nil {
			os.Remove(tmpPath)
			return fmt.Errorf(
				"target %s is in use — stop running daemons first with `mcphub stop --all`, then re-run setup: %w",
				dst, err)
		}
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename tempfile to %s: %w", dst, err)
	}
	return nil
}

// targetDirOnPath reports whether the canonical install directory
// (~/.local/bin) is already in the current process's PATH env var. When
// it is, `mcphub install` can silently copy the binary there without
// prompting the user or touching the PATH registry — a pre-existing PATH
// entry means future invocations and Task Scheduler launches will find
// the newly-copied binary automatically.
func targetDirOnPath() bool {
	targetDir, err := setupTargetDir()
	if err != nil {
		return false
	}
	return dirOnPath(targetDir, os.Getenv("PATH"))
}

// dirOnPath splits a PATH-style string on the OS list separator and reports
// whether any entry references the same directory as dir. Uses samePath so
// comparisons are case-insensitive on Windows and tolerate mixed separators
// (e.g. `C:\Users\x/.local/bin`).
func dirOnPath(dir, pathEnv string) bool {
	sep := string(os.PathListSeparator)
	for entry := range strings.SplitSeq(pathEnv, sep) {
		if entry == "" {
			continue
		}
		if samePath(entry, dir) {
			return true
		}
	}
	return false
}

// Bootstrap installs the currently-running mcphub to ~/.local/bin and ensures
// that directory is on the user's PATH. Idempotent: a second call makes no
// changes if the target already matches the current exe and PATH is set up.
//
// Exported so `mcphub install` can invoke the same flow when it detects that
// mcphub is not yet on PATH and stdin is a terminal.
func Bootstrap(w io.Writer) error {
	if err := bootstrapCopyOnly(w); err != nil {
		return err
	}
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("resolve target path: %w", err)
	}
	// Platform-specific PATH registration; prints its own success line.
	return ensureOnPath(w, filepath.Dir(target))
}

// bootstrapCopyOnly does the binary-copy half of Bootstrap WITHOUT
// touching PATH. Used by `mcphub install --upgrade` (bot r2 P1 closure
// on PR #181): an upgrade-time PATH registration failure (HKCU write
// contention, registry ACL issue, transient WM_SETTINGCHANGE broadcast
// hiccup) would otherwise leave daemons stopped and the fleet down,
// since `runInstallUpgrade` propagates the bootstrap error and skips
// RestartAll. PATH registration is a one-time setup concern, not an
// upgrade concern \u2014 the canonical path has already been on PATH since
// the operator's first `mcphub setup`, and re-registering on every
// upgrade adds zero value while introducing a new fail-stop. If PATH
// ever falls out, the operator runs `mcphub setup` to reconcile it.
func bootstrapCopyOnly(w io.Writer) error {
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("resolve target path: %w", err)
	}
	curExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	if samePath(curExe, target) {
		fmt.Fprintf(w, "\u2713 mcphub already at %s (no copy needed)\n", target)
		return nil
	}
	if err := copyExe(curExe, target); err != nil {
		return err
	}
	fmt.Fprintf(w, "\u2713 mcphub installed at %s\n", target)
	return nil
}

// newSetupCmdReal returns the `mcphub setup` command.
func newSetupCmdReal() *cobra.Command {
	var allowElevated bool
	var rollbackLSPRouter bool
	c := &cobra.Command{
		Use:   "setup",
		Short: "Install mcphub to ~/.local/bin, register PATH, install watchdog task",
		Long: `Canonicalize the mcphub binary at ~/.local/bin/mcphub.exe (Windows) or
~/.local/bin/mcphub (Linux/macOS), ensure that directory is on user PATH,
and install the watchdog scheduled task that auto-recovers daemons every
5 minutes.

What setup does:
  1. Copies the currently-running mcphub binary to ~/.local/bin/
     (idempotent — skips copy if already at target)
  2. Windows: ensures %USERPROFILE%\.local\bin is in HKCU\Environment\Path,
     broadcasts WM_SETTINGCHANGE so new shells pick it up
  3. Linux/macOS: prints the 'export PATH=...' line to paste into shell rc
     (does NOT modify rc files automatically)
  4. Verifies the watchdog state directory is reachable (plan §16);
     fails with exit 8 if not.
  5. Attempts to ensure every present MCP client has mcp-language-server-<lang>
     entries pointing at the GUI LSP router URL
     http://localhost:<gui_server.port>/lsp/<lang>/mcp, migrating old
     per-project LSP proxy URLs after timestamped backups. Failures are
     warned and do not block watchdog setup.
  6. Installs \mcp-local-hub-watchdog scheduled task (cadence 5 min).
     Refuses if the current process is elevated (plan §42) unless
     --allow-elevated is passed; with --allow-elevated, a high-priority
     audit entry is written first and audit-write failure is fail-
     closed at exit 11 (plan §61).
  7. Registers the Windows EventLog source 'mcp-local-hub' so the
     audit-degraded cascade can use eventlog.Notify (plan §60).
     Failure here is non-fatal — the cascade still has stderr/syslog
     fallbacks.

Rollback:
  mcphub setup --rollback-lsp-router restores the latest pre-router
  client-config backup for each LSP entry when available. If a client has
  no backup, it removes the router entries. It does not touch workspace
  registry rows; existing per-(workspace, language) rows are warm
  preregistrations for the router's auto-register path.

Why this exists:
  Scheduler tasks reference ~/.local/bin/mcphub.exe by absolute path
  (Task Scheduler's CreateProcess doesn't honor PATH reliably, so a
  bare 'mcphub.exe' Command fails with ERROR_FILE_NOT_FOUND). The
  canonical path depends only on $HOME, not on dev checkout location,
  so moving or rebuilding the binary only requires 're-running setup'
  — scheduler tasks keep working without any rewrite.

Examples:
  mcphub setup                    # after 'go build', before first 'install'
  mcphub setup                    # after pulling + rebuilding — replaces the canonical copy
  mcphub setup --rollback-lsp-router
  mcphub setup --allow-elevated   # bypass §42 elevation refusal (audit fail-closed)

Caveats:
  - The shell that ran 'setup' won't see the updated PATH — close and
    reopen it. WM_SETTINGCHANGE only reaches NEW shells.
  - If ~/.local/bin/mcphub.exe is currently running (as a hub daemon),
    the copy step fails with 'target is in use' — run 'mcphub stop --all'
    first, or kill the daemon processes manually.
  - --allow-elevated overrides plan §42 administrator-install refusal.
    Use it ONLY when you know the watchdog must run as the elevated
    user. The override is recorded in intent-audit.log with Priority=high.

Exit codes:
  0   success
  8   state-dir sanity rejected (plan §16: KnownFolder unavailable
      or POSIX parent insecure)
  11  --allow-elevated audit write failed (plan §61)
  Other failures use cobra's default non-zero exit.

See also: install, scheduler upgrade, watchdog install, watchdog uninstall.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if rollbackLSPRouter {
				return runSetupLSPClientRouter(cmd.OutOrStdout(), true)
			}
			if err := runSetupBootstrap(cmd.OutOrStdout()); err != nil {
				return err
			}
			if err := runSetupLSPClientRouter(cmd.OutOrStdout(), false); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: LSP router wiring failed (continuing to watchdog): %v\n", err)
			}
			return runSetupWatchdogForSetup(cmd.OutOrStdout(), allowElevated)
		},
	}
	c.Flags().BoolVar(&allowElevated, "allow-elevated", false,
		"override plan §42 elevation refusal (records a high-priority audit entry; fail-closed if audit fails per §61)")
	c.Flags().BoolVar(&rollbackLSPRouter, "rollback-lsp-router", false,
		"restore/remove Phase 3 LSP router client entries from latest backups; skips bootstrap/watchdog setup")
	return c
}

func runSetupBootstrap(out io.Writer) error {
	if setupBootstrapFn != nil {
		return setupBootstrapFn(out)
	}
	return Bootstrap(out)
}

func runSetupWatchdogForSetup(out io.Writer, allowElevated bool) error {
	if setupWatchdogFn != nil {
		return setupWatchdogFn(out, allowElevated)
	}
	return runSetupWatchdog(out, allowElevated)
}

func runSetupLSPClientRouter(out io.Writer, rollback bool) error {
	report, err := setupLSPClientRouterFn(rollback)
	if report == nil {
		report = &api.LSPClientRouterReport{}
	}
	action := "wiring"
	if rollback {
		action = "rollback"
	}
	for _, backup := range report.Backups {
		fmt.Fprintf(out, "✓ %s backup before LSP router %s: %s\n", backup.Client, action, backup.Path)
	}
	for _, applied := range report.Applied {
		fmt.Fprintf(out, "✓ %s → %s (entry %s)\n", applied.Client, applied.URL, applied.EntryName)
	}
	for _, removed := range report.Removed {
		fmt.Fprintf(out, "✓ removed %s entry %s\n", removed.Client, removed.EntryName)
	}
	for _, restored := range report.Restored {
		fmt.Fprintf(out, "✓ restored %s entry %s\n", restored.Client, restored.EntryName)
	}
	for _, failed := range report.Failed {
		fmt.Fprintf(out, "✗ %s %s entry %s failed during %s: %s\n",
			failed.Client, failed.Language, failed.EntryName, failed.Op, failed.Err)
	}
	return err
}

// ---------------------------------------------------------------------------
// runSetupWatchdog — Task 11 wiring (plan §16, §42, §60, §61).
// ---------------------------------------------------------------------------

// runSetupWatchdog runs the post-Bootstrap watchdog wiring per Task
// 11. Caller (newSetupCmdReal) is responsible for Bootstrap; this
// function is a pure follow-up that installs the maintenance scheduled
// task (the supervisor-liveness recovery) and tidies up legacy state.
//
// v0.6 Phase C (spec §5): the legacy `\mcp-local-hub-watchdog` task is no
// longer installed here — the v0.6 supervisor owns daemon revival and the
// liveness task owns owner-death recovery, so the watchdog only fights the
// supervisor. This entrypoint now:
//
//  1. Verifies DaemonStateDir() resolves cleanly (plan §16). Failure
//     → return forceExit(8) so cmd/mcphub/main.go maps to exit 8.
//  2. Detects elevation (plan §42). If elevated AND --allow-elevated
//     not passed, refuse with a clear error. If elevated AND
//     --allow-elevated set, write the high-priority audit entry per
//     §61; audit-write failure → forceExit(11). (The elevation gate now
//     protects the liveness-task install.)
//  3. Installs the supervisor-liveness scheduled task (Phase 3a). Failure
//     fails closed before any legacy-watchdog removal.
//  4. Removes the leftover legacy `\mcp-local-hub-watchdog` task on
//     existing hosts (idempotent, non-fatal — Phase C). On clean hosts
//     this is a no-op (scheduler.Delete returns nil for an absent task).
//  5. Registers the Windows EventLog source per §60. Failure is
//     non-fatal: logged + continue.
//  6. Prints the confirmation lines to stdout.
//
// The function is the testable entrypoint for setup_watchdog_test.go.
// All test seams (setupIsElevatedFn, setupRegisterEventLogFn,
// setupStateDirSanityFn) are consulted here.
func runSetupWatchdog(out io.Writer, allowElevated bool) error {
	// 1. State path sanity (§16). Use the seam if set, otherwise the
	//    production api.DaemonStateDir().
	var (
		stateDir string
		err      error
	)
	if setupStateDirSanityFn != nil {
		stateDir, err = setupStateDirSanityFn()
	} else {
		stateDir, err = api.DaemonStateDir()
	}
	if err != nil {
		fmt.Fprintf(out, "✗ watchdog setup aborted: state-dir sanity rejection: %v\n", err)
		fmt.Fprintln(out, "  See plan §16 (KnownFolder unavailable / POSIX parent insecure).")
		return forceExit(exitSetupStatePathRejected)
	}

	// 2. Elevation detection (§42).
	elevated, elevErr := setupIsElevated()
	if elevErr != nil {
		// Per plan §42 production fail-closed: treat resolution
		// failure as elevated → require --allow-elevated.
		fmt.Fprintf(out, "⚠ elevation detector failed: %v (treating as elevated)\n", elevErr)
		elevated = true
	}
	if elevated && !allowElevated {
		return fmt.Errorf(
			"mcphub setup must run un-elevated (plan §42); use --allow-elevated to override (audit fail-closed per §61)")
	}
	if elevated && allowElevated {
		// §61: high-priority audit entry BEFORE any mutation. The elevation
		// gate now protects the liveness-task install (the legacy watchdog
		// install was removed in Phase C); the audit action string is kept
		// stable for log consumers.
		a := api.NewAPI()
		if err := a.AppendIntentAudit(api.NewIntentAuditEntry(
			api.WithAction(api.AuditActionWatchdogInstallElevatedOverride),
			api.WithTask(api.LivenessTaskName),
			api.WithWho(api.AuditWhoMcphubSetup),
			api.WithPriority("high"),
			api.WithReason("--allow-elevated flag explicit override"),
		)); err != nil {
			fmt.Fprintf(out,
				"✗ audit log unwritable; --allow-elevated requires audit trail (plan §61): %v\n", err)
			return forceExit(exitSetupAuditRequiredButFailed)
		}
	}

	a := api.NewAPI()

	// 3. Install the supervisor-liveness scheduled task (v0.6 spec §15 P1-b /
	// §5.x Phase 3a). This is the sole maintenance-task install after Phase C
	// dropped the watchdog: the liveness task relaunches the supervisor/GUI
	// OWNER (`supervise --ensure-alive`, ~1-min) if it dies mid-session.
	//
	// Fail-closed: a failed liveness install must abort setup before the
	// legacy watchdog is removed, otherwise an existing host can be left with
	// neither recovery mechanism while automation observes exit 0. The next
	// `mcphub setup` re-attempts the idempotent ImportXML.
	//
	// SCOPING NOTE (PR #283 review P3-b): the liveness action's relaunch
	// target is the AUTOSTART task `\mcp-local-hub-supervisor`, which `mcphub
	// setup` does NOT install — it is created only by `mcphub install`
	// (migration shim) or an explicit `mcphub autostart enable`. So in a
	// setup-only state the liveness task exists but its relaunch is inert
	// until autostart is enabled. This is fail-safe (the normal sequence is
	// setup → install, and install enables autostart before any persistent
	// supervisor exists, so there is nothing to recover during the gap), and
	// a relaunch against an absent target lands a durable
	// `liveness-relaunch-failed` warn in supervisor-events.log (carrying the
	// target name + the schtasks error) so the inert state is operator-visible
	// rather than silent.
	if livenessErr := a.InstallLivenessTask(); livenessErr != nil {
		fmt.Fprintf(out, "✗ supervisor-liveness task install failed: %v\n", livenessErr)
		fmt.Fprintf(out, "  Recovery: rerun `mcphub setup`; if Task Scheduler still rejects the task, inspect it with `schtasks /Query /TN %s` and rerun setup after Scheduler is healthy.\n", api.LivenessTaskName)
		return forceExit(exitSetupLivenessInstallFailed)
	} else {
		fmt.Fprintf(out, "✓ Installed scheduled task: %s (supervisor-liveness, cadence 1 min)\n", api.LivenessTaskName)
		fmt.Fprintf(out, "  State directory: %s\n", stateDir)
		fmt.Fprintf(out, "  Note: liveness recovery relaunches via the autostart task %s, which is installed by `mcphub install` / `mcphub autostart enable` — until then the relaunch is inert (no-op; recorded in supervisor-events.log).\n", autostart.WindowsTaskName)
	}

	// 4. Remove the leftover legacy `\mcp-local-hub-watchdog` task on existing
	// hosts (v0.6 spec §5 Phase C). The v0.6 supervisor owns daemon revival
	// (Job-Object reaper + reconcile loop) and the liveness task above owns
	// owner-death recovery, so the watchdog is a no-op vestige that fights the
	// supervisor every 5 min. Idempotent + non-fatal: scheduler.Delete returns
	// nil for an absent task, so on a clean host this is a silent no-op and a
	// transient delete failure must not abort setup after the replacement task
	// was installed.
	if rmErr := a.RemoveLegacyWatchdogTask(); rmErr != nil {
		fmt.Fprintf(out, "⚠ legacy watchdog task removal failed (non-fatal; manual: schtasks /Delete /TN %s /F): %v\n", api.LegacyWatchdogTaskName, rmErr)
	}

	// 5. EventLog source registration (§60). Non-fatal: print + continue.
	if regErr := setupRegisterEventLog(); regErr != nil {
		fmt.Fprintf(out, "⚠ EventLog source registration failed (non-fatal per §60): %v\n", regErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// runUninstallWatchdog — Task 11 ordering (plan §60).
// ---------------------------------------------------------------------------

// runUninstallWatchdog runs the watchdog-side teardown invoked by
// `mcphub uninstall`. Per Task 11.1 the canonical ordering is:
//
//  1. Disable/delete the watchdog scheduled task FIRST so it does
//     not race against per-server uninstall mid-teardown.
//  2. Per-server uninstall (existing api.Uninstall flow) — already
//     handles intent-marking + per-server task delete. Invoked by
//     the cobra wrapper in uninstall.go AFTER this helper returns.
//  3. EventLog source removal per §60 (Windows-only; POSIX no-op).
//     Invoked by the cobra wrapper AFTER api.Uninstall returns.
//
// This helper owns step 1 + step 3. Step 2 stays inside the cobra
// wrapper because it depends on the per-server `--server` flag the
// wrapper already parses. Audit + intent failures inside
// `api.Uninstall` are non-fatal per the §65 fail-handling table
// (`mcphub uninstall: log + proceed`).
//
// Codex bot P2 (medium): the watchdog + EventLog cleanup is GATED
// on "no remaining managed servers AFTER this uninstall". Removing
// one server when multiple are installed must NOT silently strip
// the global watchdog from peer servers — that would defeat
// auto-recovery for every non-target daemon.
//
// Gate algorithm:
//
//  1. List all `mcp-local-hub-*` scheduled tasks.
//  2. Filter out maintenance tasks (api.IsMaintenanceTaskName).
//  3. Map each remaining task to its server (api.ServerFromTaskName).
//  4. Subtract the server about to be uninstalled.
//  5. If the resulting set is empty → last managed server →
//     authorize watchdog + EventLog teardown.
//     Otherwise → log "watchdog kept installed" and skip both.
//
// Fail-closed on List error: a transient list failure cannot be
// allowed to silently uninstall the watchdog. Log the error and
// keep the watchdog installed.
//
// Returns nil on the watchdog teardown success path; per-step
// failures are logged via `out` and never propagate so the caller
// can continue with the per-server uninstall regardless.
func runUninstallWatchdog(out io.Writer, serverBeingUninstalled string) error {
	a := api.NewAPI()

	// 1. Partial-uninstall gate (Codex bot P2).
	if !shouldRemoveGlobalWatchdog(out, serverBeingUninstalled) {
		// Watchdog stays installed for the remaining servers; EventLog
		// source registration also stays in place (it is a global
		// resource paired with the watchdog).
		return nil
	}

	// 2. Legacy watchdog scheduled task deletion FIRST. The v0.6 watchdog
	// engine is deleted (spec §5 Phase D); this removes the leftover task on
	// existing hosts. Idempotent + non-fatal: scheduler.Delete returns nil for
	// an absent task, so a clean host is a silent no-op and a transient delete
	// failure must not abort the per-server uninstall / eventlog cleanup below.
	if err := a.RemoveLegacyWatchdogTask(); err != nil {
		fmt.Fprintf(out, "⚠ legacy watchdog removal failed (continuing): %v\n", err)
	} else {
		fmt.Fprintf(out, "✓ Removed scheduled task: %s\n", api.LegacyWatchdogTaskName)
	}

	// 2b. Supervisor-liveness scheduled task deletion (Phase 3a, v0.6 spec
	// §15 P1-b). The `\mcp-local-hub-liveness` task is a hub-wide shared
	// maintenance job exactly like the watchdog, so it is torn down INSIDE the
	// same last-server gate (shouldRemoveGlobalWatchdog above) — it must NOT
	// be removed while peer servers still rely on owner-relaunch recovery.
	// Non-fatal + idempotent: scheduler.Delete returns nil for an absent task,
	// matching the watchdog teardown polarity, so a missing liveness task or a
	// transient delete failure never aborts the uninstall.
	if err := a.UninstallLivenessTask(); err != nil {
		fmt.Fprintf(out, "⚠ supervisor-liveness uninstall failed (continuing): %v\n", err)
	} else {
		fmt.Fprintf(out, "✓ Removed scheduled task: %s\n", api.LivenessTaskName)
	}

	// 3. EventLog source removal (§60). Idempotent / non-fatal.
	// Step 2 (per-server api.Uninstall) is owned by the cobra wrapper
	// since this helper is also called by tests that don't drive
	// per-server uninstall; the EventLog cleanup belongs with the
	// watchdog teardown both topologically and in test coverage.
	if err := setupRemoveEventLog(); err != nil {
		fmt.Fprintf(out, "⚠ EventLog source removal failed (continuing): %v\n", err)
	}
	return nil
}

// shouldRemoveGlobalWatchdog implements the partial-uninstall gate
// (Codex bot P2). Returns true when the post-uninstall remaining
// managed-server set is empty, meaning this is the last server and
// the global watchdog can be removed safely.
//
// Fail-closed: any error from the live scheduler list keeps the
// watchdog installed. Operators get a single informational line on
// `out` either way so the decision is visible.
func shouldRemoveGlobalWatchdog(out io.Writer, serverBeingUninstalled string) bool {
	// Use the live scheduler view (raw List, no status enrichment).
	// api.ListManagedTasks routes through the same scheduler factory
	// seam that RemoveLegacyWatchdogTask / UninstallLivenessTask use, so
	// tests with a fake scheduler (setupWatchdogTestHelper) can drive
	// deterministic list responses for the gate decision.
	a := api.NewAPI()
	rows, err := a.ListManagedTasks()
	if err != nil {
		fmt.Fprintf(out, "⚠ watchdog gate: list failed (keeping watchdog installed): %v\n", err)
		return false
	}
	remaining := map[string]struct{}{}
	for _, row := range rows {
		if api.IsMaintenanceTaskName(row.Name) {
			continue
		}
		srv := api.ServerFromTaskName(row.Name)
		if srv == "" {
			// Hub-wide / unparseable mcp-local-hub-* task that is not
			// a maintenance task. Defensively count it as "still
			// present" so we do not strip the watchdog while an
			// unrecognized task is around.
			remaining[row.Name] = struct{}{}
			continue
		}
		if srv == serverBeingUninstalled {
			continue
		}
		remaining[srv] = struct{}{}
	}
	supervisorRemaining, siErr := supervisorIntentManagedServerSignals()
	if siErr != nil {
		fmt.Fprintf(out, "⚠ watchdog gate: supervisor-intent read failed (keeping watchdog installed): %v\n", siErr)
		return false
	}
	for srv := range supervisorRemaining {
		if srv == serverBeingUninstalled {
			continue
		}
		remaining[srv] = struct{}{}
	}
	if len(remaining) == 0 {
		return true
	}
	// Surface the names so an operator can see which servers retained
	// the watchdog. Order is non-deterministic from a map; sort for
	// stable output across calls.
	names := make([]string, 0, len(remaining))
	for k := range remaining {
		names = append(names, k)
	}
	// Cheap stable sort: compare-and-swap for typical small sets.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	fmt.Fprintf(out,
		"ℹ watchdog kept installed; %d other server(s) still managed: %s\n",
		len(names), strings.Join(names, ", "))
	return false
}

// ---------------------------------------------------------------------------
// Seam routers.
// ---------------------------------------------------------------------------

// setupIsElevated routes through the setupIsElevatedFn seam if set,
// otherwise the production OS-bound isElevatedReal helper.
func setupIsElevated() (bool, error) {
	if setupIsElevatedFn != nil {
		return setupIsElevatedFn()
	}
	return isElevatedReal()
}

// setupRegisterEventLog routes through the setupRegisterEventLogFn
// seam if set, otherwise the production helper.
func setupRegisterEventLog() error {
	if setupRegisterEventLogFn != nil {
		return setupRegisterEventLogFn()
	}
	return registerEventLogSourceReal()
}

// setupRemoveEventLog routes through the setupRemoveEventLogFn seam
// if set, otherwise the production helper.
func setupRemoveEventLog() error {
	if setupRemoveEventLogFn != nil {
		return setupRemoveEventLogFn()
	}
	return removeEventLogSourceReal()
}

// errSetupStubElevated is reserved for future tests that want a
// typed sentinel for "stub returned elevated". Currently unused.
//
//nolint:unused // referenced by future setup tests per plan v13.
var errSetupStubElevated = errors.New("cli: setup elevation stub returned elevated")
