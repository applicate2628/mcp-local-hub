//go:build windows

// Package cli — Windows-only production wiring for the v0.5.0
// migration routing surface (Fix Group 1 / codex-c-p0-4).
//
// The cross-platform install.go declares routing seams
// (forwardMigrationFn, rollbackMigrationFn, v5UpgradeFn, and
// enumerateAllMcphubTasksFn) as nil-by-default function pointers.
// This file fills them in with the real Windows implementations via
// an init() hook so POSIX builds compile a no-op and Windows builds
// gain the full migration drive surface.
//
// The wiring is deliberately thin: each helper here adapts a
// production primitive (scheduler.EnumerateAllMcphubTasks, the
// existing scheduler.Scheduler interface, process.LookupProcessIdentity,
// the netstat-based port-PID resolver in internal/api/processes.go,
// autostart.Backend.Enable, the supervisor IPC named-pipe client) into
// the function signature migration.ForwardOptions /
// migration.RollbackOptions expects. New production paths should
// extend the adapters here rather than reaching into the migration
// package or duplicating netstat/scheduler code.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/migration"
	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/scheduler"
)

func init() {
	enumerateAllMcphubTasksFn = enumerateMcphubTaskCount
	forwardMigrationFn = runForwardMigrationWindows
	rollbackMigrationFn = runRollbackMigrationWindows
	v5UpgradeFn = runV5UpgradeWindows
}

// enumerateMcphubTaskCount returns the number of registered
// `\mcp-local-hub-*` Scheduled Tasks regardless of Run As account.
// Used by the upgrade routing decision tree to discriminate
// "fresh install" (no tasks, no v0.5.0 state) from "v0.4.x present"
// (legacy tasks present, no v0.5.0 state). Failures bubble up so the
// routing layer surfaces the cause instead of silently choosing the
// legacy branch.
func enumerateMcphubTaskCount() (int, error) {
	tasks, err := scheduler.EnumerateAllMcphubTasks()
	if err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// runForwardMigrationWindows assembles the production
// migration.ForwardOptions and invokes migration.RunForward. Every
// callback is sourced from an existing primitive in this codebase
// (see file-level docstring).
func runForwardMigrationWindows(cmd *cobra.Command, opts dispatchUpgradeOpts) error {
	state, mopts, err := buildForwardMigrationOptions(opts)
	if err != nil {
		return err
	}

	if err := migration.RunForward(state, mopts); err != nil {
		// Propagate migration.ExitCodeError unchanged so cmd/mcphub/main.go's
		// errors.As mapping picks up the declared exit code (8/9/13/14).
		// Wrap other errors with operator-facing context.
		var ec *migration.ExitCodeError
		if errors.As(err, &ec) {
			return err
		}
		return fmt.Errorf("forward migration: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Forward migration to v0.5.0 supervisor architecture complete.")
	return nil
}

// buildForwardMigrationOptions resolves every dependency the forward
// migration needs (state-dir, executable path, scheduler, current
// user, pre-migration strict-mode) and assembles a fully-wired
// migration.ForwardOptions. Extracted from runForwardMigrationWindows
// so unit tests can inspect the wired callbacks (notably
// RollbackOnFailure for Lane C P0 #2 + LookupProcessIdentity for
// Lane F P0 #2) without driving a real migration run.
//
// The RollbackOnFailure closure captures the same resolved dependencies
// (stateDir, sch, currentUser) so the auto-rollback at step 14 reuses
// them instead of re-running every resolver — keeping behavior identical
// to invoking `mcphub install --rollback-to-legacy` by hand. On builder
// failure inside the closure the helper logs to stderr and returns nil
// so the journal falls back to its manual-rollback error path (the
// operator still sees "consider --rollback-to-legacy" guidance).
func buildForwardMigrationOptions(opts dispatchUpgradeOpts) (migration.State, migration.ForwardOptions, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return migration.State{}, migration.ForwardOptions{}, fmt.Errorf("forward migration: resolve state-dir: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return migration.State{}, migration.ForwardOptions{}, fmt.Errorf("forward migration: resolve executable: %w", err)
	}
	installDir := filepath.Dir(exe)

	sch, err := scheduler.New()
	if err != nil {
		return migration.State{}, migration.ForwardOptions{}, fmt.Errorf("forward migration: scheduler.New: %w", err)
	}
	currentUser, err := currentWindowsUsername()
	if err != nil {
		return migration.State{}, migration.ForwardOptions{}, fmt.Errorf("forward migration: resolve current user: %w", err)
	}

	preStrict, err := readPreMigrationStrictMode(stateDir)
	if err != nil {
		// A corrupt strict-mode file would silently default to false
		// and the migration would propagate the wrong intent. Fail
		// fast — operator repair is the right next step.
		return migration.State{}, migration.ForwardOptions{}, fmt.Errorf("forward migration: read pre-migration strict-mode: %w", err)
	}

	state := migration.State{
		StateDir:   stateDir,
		InstallDir: installDir,
		Now:        time.Now,
	}

	pipePath := superviseIPCPipePath(currentUser)

	mopts := migration.ForwardOptions{
		DiscardSchedulerCustomizations: opts.DiscardSchedulerCustomizations,
		StrictTemplate:                 opts.StrictTemplate,
		PreMigrationStrictMode:         preStrict,
		Scheduler:                      newMigrationSchedulerAdapter(sch),
		PowerShellProbe:                process.ProbePowerShellCLM,
		WmicPresent:                    wmicPresentFn,
		LookupProcessIdentity:          lookupMigrationProcessIdentity,
		PortForPID:                     portForPIDViaNetstat,
		PIDForServerDaemon:             pidForServerDaemonViaTasklist,
		KillPID:                        killPIDViaTaskkill,
		PortBindWait:                   portBindWaitForRelease,
		ShimInstaller:                  installAutostartShim,
		SupervisorSpawner:              spawnSupervisorDetached(exe, preStrict),
		ReconcileReady:                 waitReconcileReadyViaIPC(pipePath),
		CurrentUser:                    currentUser,
		// RollbackOnFailure (Lane C P0 #2 + codex-r2-a/b/c-p0): wire
		// the auto-rollback callback so a step-14 reconcile-ready
		// timeout drives RunRollback in-process instead of leaving
		// the host half-migrated (legacy tasks already deleted by
		// step 11, supervisor never reached reconcile-ready). The
		// closure captures the already-resolved (stateDir, sch,
		// currentUser) so the rollback reuses them rather than
		// re-running every resolver; the journal releases the
		// migration locks BEFORE invoking us (journal.go:789), so
		// RunRollback can acquire them itself with no deadlock.
		RollbackOnFailure: func() *migration.RollbackOptions {
			_, rbOpts, rbErr := buildRollbackMigrationOptions(stateDir, sch, currentUser)
			if rbErr != nil {
				// Diagnostics-only: let the journal fall back to its
				// manual-rollback error message so the operator sees
				// the "consider --rollback-to-legacy" guidance plus
				// the underlying cause here. We do NOT bubble up via
				// the migration error chain because the closure
				// signature returns *RollbackOptions, not error.
				fmt.Fprintf(os.Stderr, "auto-rollback: build options failed: %v (falling back to manual rollback)\n", rbErr)
				return nil
			}
			return &rbOpts
		},
	}

	return state, mopts, nil
}

// runRollbackMigrationWindows assembles the production
// migration.RollbackOptions and invokes migration.RunRollback. Mirrors
// runForwardMigrationWindows — every callback adapts an existing
// primitive.
func runRollbackMigrationWindows(cmd *cobra.Command) error {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("rollback: resolve state-dir: %w", err)
	}

	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("rollback: scheduler.New: %w", err)
	}
	currentUser, err := currentWindowsUsername()
	if err != nil {
		return fmt.Errorf("rollback: resolve current user: %w", err)
	}

	state, mopts, err := buildRollbackMigrationOptions(stateDir, sch, currentUser)
	if err != nil {
		return err
	}

	if err := migration.RunRollback(state, mopts); err != nil {
		var ec *migration.ExitCodeError
		if errors.As(err, &ec) {
			return err
		}
		return fmt.Errorf("rollback: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Rollback to v0.4.x Scheduler-backed layout complete.")
	return nil
}

// buildRollbackMigrationOptions assembles a fully-wired
// migration.RollbackOptions from already-resolved dependencies. Used by
// both runRollbackMigrationWindows (operator-invoked
// `mcphub install --rollback-to-legacy`) and the ForwardOptions
// RollbackOnFailure closure (auto-rollback after step-14 reconcile-
// ready timeout).
//
// Reads supervisor-intent.json from disk so ExpectedDaemons reflects
// the exact daemon set the forward migration committed at step 7
// (journal.go:1027 writes it to <stateDir>/supervisor-intent.json
// BEFORE step 14's reconcile-ready check, so the file is on disk
// regardless of which caller invokes us). A missing intent means
// there's nothing to roll back from — surface a clear error.
func buildRollbackMigrationOptions(stateDir string, sch scheduler.Scheduler, currentUser string) (migration.State, migration.RollbackOptions, error) {
	pipePath := superviseIPCPipePath(currentUser)

	// Load the supervisor-intent.json so the rollback driver knows
	// which ports to wait for after force-kill. A missing intent
	// means there's nothing to roll back from — surface a clear
	// error instead of silently passing an empty daemon list.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, intentErr := api.ReadSupervisorIntent(intentPath)
	if intentErr != nil {
		return migration.State{}, migration.RollbackOptions{}, fmt.Errorf("rollback: no supervisor-intent.json at %s (forward migration never ran, or already rolled back): %w", intentPath, intentErr)
	}

	state := migration.State{
		StateDir: stateDir,
		Now:      time.Now,
	}

	supervisorLockPath := filepath.Join(stateDir, "supervisor.lock")

	mopts := migration.RollbackOptions{
		Scheduler:                    newMigrationSchedulerAdapter(sch),
		SupervisorIPC:                supervisorIPCSender(pipePath, supervisorLockPath),
		ProbeSupervisorTokenMismatch: probeSupervisorTokenMismatch(supervisorLockPath),
		ForceKillSupervisor:          forceKillSupervisor(supervisorLockPath),
		PortBindWait:                 portBindWaitForRelease,
		LookupProcessIdentity:        lookupMigrationProcessIdentity,
		ShimUninstaller:              uninstallAutostartShim,
		ExpectedDaemons:              intent.Daemons,
	}

	return state, mopts, nil
}

// runV5UpgradeWindows wires cli.RunInstallUpgrade with the
// production rename-aside + IPC + supervisor-spawn callbacks. The
// caller already verified supervisor-intent.json is present (the
// routing branch's discriminator), so this path is the "v0.5.0 → v0.5.x
// same-version upgrade" flow per spec §"Upgrade sequence".
func runV5UpgradeWindows(cmd *cobra.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve executable: %w", err)
	}
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve canonical target: %w", err)
	}
	currentUser, err := currentWindowsUsername()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve current user: %w", err)
	}
	deps := &v5UpgradeDeps{
		exePath:           exe,
		newBinaryPath:     exe, // current exe IS the new image
		supervisorLockDir: "",  // filled in below from state-dir
		pipePath:          superviseIPCPipePath(currentUser),
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve state-dir: %w", err)
	}
	deps.supervisorLockDir = filepath.Join(stateDir, "supervisor.lock")

	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath: target,
		NewBinary:  exe,
		PipePath:   deps.pipePath,
		Deps:       deps,
	}); err != nil {
		return fmt.Errorf("v0.5 upgrade: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "v0.5 upgrade complete.")
	return nil
}

// ---------------------------------------------------------------------------
// Adapters.
// ---------------------------------------------------------------------------

// migrationSchedulerAdapter wraps scheduler.Scheduler + the package-level
// EnumerateAllMcphubTasks helper as a migration.SchedulerBackend. The
// interface adapter exists so migration's CreateXML (name, string)
// shape can call through to scheduler.Scheduler.ImportXML(name, []byte)
// without forcing migration to depend on the scheduler package's byte-
// slice convention.
type migrationSchedulerAdapter struct {
	sch scheduler.Scheduler
}

func newMigrationSchedulerAdapter(sch scheduler.Scheduler) *migrationSchedulerAdapter {
	return &migrationSchedulerAdapter{sch: sch}
}

func (a *migrationSchedulerAdapter) EnumerateAllMcphubTasks() ([]scheduler.TaskStatus, error) {
	return scheduler.EnumerateAllMcphubTasks()
}

func (a *migrationSchedulerAdapter) ExportXML(name string) (string, error) {
	raw, err := a.sch.ExportXML(name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (a *migrationSchedulerAdapter) Delete(name string) error {
	return a.sch.Delete(name)
}

func (a *migrationSchedulerAdapter) CreateXML(name, xml string) error {
	return a.sch.ImportXML(name, []byte(xml))
}

func (a *migrationSchedulerAdapter) Run(name string) error {
	return a.sch.Run(name)
}

// ---------------------------------------------------------------------------
// Process / port helpers.
// ---------------------------------------------------------------------------

// wmicPresentFn reports whether wmic.exe is on PATH. Used by
// migration.RunForward step 0 to decide whether wmic can serve as a
// PowerShell-CLM fallback for the PID identity probe.
var wmicPresentFn = func() bool {
	_, err := exec.LookPath("wmic.exe")
	return err == nil
}

// processLookupIdentityFn is a test seam over process.LookupProcessIdentity.
// Production wires it to the real Windows implementation; tests inject
// a fake that returns process.ErrProcessNotFound (or an arbitrary
// other error) to drive lookupMigrationProcessIdentity's sentinel
// mapping path.
var processLookupIdentityFn = process.LookupProcessIdentity

// lookupMigrationProcessIdentity adapts process.LookupProcessIdentity
// into the migration.ProcessIdentity shape AND maps the package-local
// process.ErrProcessNotFound sentinel onto migration.ErrProcessNotFound
// so the journal's `errors.Is(err, migration.ErrProcessNotFound)`
// genuine-unbound cross-check at journal.go:1142 fires correctly.
// Without this mapping the two sentinels live in different packages
// and the cross-check would treat every "PID gone" as a transient-
// retry-exhaustion abort, breaking the Lane F P0 #2 contract.
//
// The two ProcessIdentity types are field-for-field parallel by design
// (see migration/journal.go's ProcessIdentity docstring); the adapter
// is a struct copy on the success path.
func lookupMigrationProcessIdentity(pid int) (migration.ProcessIdentity, error) {
	id, err := processLookupIdentityFn(pid)
	if err != nil {
		if errors.Is(err, process.ErrProcessNotFound) {
			return migration.ProcessIdentity{}, migration.ErrProcessNotFound
		}
		return migration.ProcessIdentity{}, err
	}
	return migration.ProcessIdentity{
		PID:              id.PID,
		Basename:         id.Basename,
		CommandLine:      id.CommandLine,
		ExecutablePath:   id.ExecutablePath,
		CreationDateUnix: id.CreationDateUnix,
	}, nil
}

// portForPIDViaNetstat scans `netstat -ano` for a 127.0.0.1 LISTENING
// row owned by PID and returns the listening port. Used by
// migration.RunForward step 9 to record the port a daemon was bound
// to before the kill, so rollback can re-verify the port re-binds
// post-restore.
//
// Production behavior: returns (0, false) when the PID has no
// listener (or netstat fails). The migration driver treats false as
// "no listener" and falls through to the no-running-daemon audit
// branch.
func portForPIDViaNetstat(pid int) (int, bool) {
	if pid <= 0 {
		return 0, false
	}
	cmd := exec.Command("netstat", "-ano")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	target := strconv.Itoa(pid)
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "LISTENING") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		// Match the trailing PID column.
		if fields[len(fields)-1] != target {
			continue
		}
		// Expect format `127.0.0.1:PORT` in field[1] (IPv4 loopback;
		// IPv6 `[::1]:PORT` is ignored — mcphub daemons bind v4 only).
		addr := fields[1]
		if !strings.HasPrefix(addr, "127.0.0.1:") {
			continue
		}
		portStr := strings.TrimPrefix(addr, "127.0.0.1:")
		port, atoiErr := strconv.Atoi(portStr)
		if atoiErr != nil || port <= 0 {
			continue
		}
		return port, true
	}
	return 0, false
}

// pidForServerDaemonViaTasklist scans the running process list for a
// mcphub.exe instance whose command-line contains `daemon --server X
// --daemon Y`. Returns (pid, true) on first match; (0, false) when no
// matching process is found.
//
// Used by migration.RunForward step 9 to resolve the PID of the
// running daemon for a given (server, daemon) pair before killing it.
//
// Implementation: uses the same wmic + PowerShell fallback as the rest
// of the codebase via api.API.ListMatchingProcesses, but inline here
// to avoid pulling the api.API dependency into the migration wiring.
func pidForServerDaemonViaTasklist(server, daemon string) (int, bool) {
	if server == "" || daemon == "" {
		return 0, false
	}
	wantArgv := fmt.Sprintf("daemon --server %s --daemon %s", server, daemon)
	cmd := exec.Command("wmic", "process", "where",
		"name='mcphub.exe'",
		"get", "ProcessId,CommandLine", "/format:csv")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		// Try PowerShell fallback for hosts where wmic is removed.
		psScript := `Get-CimInstance Win32_Process -Filter 'Name="mcphub.exe"' | ` +
			`ForEach-Object { "$($_.ProcessId)|$($_.CommandLine)" }`
		psCmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", psScript)
		process.NoConsole(psCmd)
		out, err = psCmd.Output()
		if err != nil {
			return 0, false
		}
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(strings.TrimSpace(line), "|", 2)
			if len(parts) != 2 {
				continue
			}
			pid, perr := strconv.Atoi(strings.TrimSpace(parts[0]))
			if perr != nil || pid <= 0 {
				continue
			}
			if strings.Contains(parts[1], wantArgv) {
				return pid, true
			}
		}
		return 0, false
	}
	// wmic CSV format: header line "Node,CommandLine,ProcessId" then
	// data rows. The CommandLine column may contain commas, so we
	// reconstruct it from the indices we know.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Node,") {
			continue
		}
		if !strings.Contains(line, wantArgv) {
			continue
		}
		// PID is the LAST CSV field.
		lastComma := strings.LastIndex(line, ",")
		if lastComma < 0 {
			continue
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(line[lastComma+1:]))
		if perr != nil || pid <= 0 {
			continue
		}
		return pid, true
	}
	return 0, false
}

// killPIDViaTaskkill kills a process by PID via `taskkill /F /T /PID`.
// /T also kills the process tree so child npx-cache node.exe instances
// (legitimate child of mcphub.exe daemon, per CLAUDE.md
// feedback_kosyak_npx_cache_processes_can_be_active_daemons.md) are
// reaped together.
func killPIDViaTaskkill(pid int) error {
	cmd := exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid))
	process.NoConsole(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("taskkill PID %d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// portBindWaitForRelease blocks until 127.0.0.1:port is unbound OR
// timeout elapses. Polling cadence: 100 ms; total budget bounded by
// timeout. Returns nil on unbound, non-nil error on timeout (or hard
// listen failure other than EADDRINUSE).
//
// Used by both forward (post-kill verification) and rollback (post-
// force-kill verification + post-/Run rebound) paths. The migration
// driver passes different timeouts (10s vs 60s) to discriminate; the
// helper is timeout-agnostic.
func portBindWaitForRelease(port int, timeout time.Duration) error {
	if port <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			// Port was unbound — close immediately so the daemon (or
			// the rollback /Run) can re-bind. Best-effort: a Close
			// failure is unlikely and would surface on next operation.
			_ = l.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("port %d not released within %s: %w", port, timeout, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Shim installer / uninstaller.
// ---------------------------------------------------------------------------

// installAutostartShim wraps autostart.Backend.Enable so migration's
// ShimInstaller signature `func(strict bool) error` works against the
// production Backend without leaking the Options struct shape.
func installAutostartShim(strictMode bool) error {
	be, err := autostart.New()
	if err != nil {
		return fmt.Errorf("autostart.New: %w", err)
	}
	return be.Enable(autostart.Options{StrictMode: strictMode})
}

// uninstallAutostartShim wraps autostart.Backend.Disable.
func uninstallAutostartShim() error {
	be, err := autostart.New()
	if err != nil {
		return fmt.Errorf("autostart.New: %w", err)
	}
	return be.Disable()
}

// ---------------------------------------------------------------------------
// Supervisor spawn / IPC wiring.
// ---------------------------------------------------------------------------

// spawnSupervisorDetached returns a migration.SupervisorSpawner closure
// that launches `<exe> supervise [--strict-mode]` as a detached
// background process via the per-OS process-detachment primitive.
//
// Windows: DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP so the new
// supervisor's stdin/stdout/stderr inherit nothing from this CLI
// process. It survives the CLI's own exit.
func spawnSupervisorDetached(exePath string, strictMode bool) func() error {
	return func() error {
		args := []string{"supervise"}
		if strictMode {
			args = append(args, "--strict-mode")
		}
		cmd := exec.Command(exePath, args...)
		cmd.SysProcAttr = &windows.SysProcAttr{
			CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("spawn supervisor: %w", err)
		}
		// Release the handle — the supervisor manages its own lifetime
		// from here. Caller does NOT Wait().
		if cmd.Process != nil {
			_ = cmd.Process.Release()
		}
		return nil
	}
}

// waitReconcileReadyViaIPC returns a migration.ReconcileReady closure
// that polls supervisor IPC `status` until `reconcile_ready: true` is
// observed in the response result map OR the timeout elapses.
//
// Production note: the supervisor's IPC `status` handler is plan
// §"status" — the response includes a `reconcile_ready` bool that
// transitions from false → true after the first successful reconcile
// pass. The poll interval here is 200 ms.
func waitReconcileReadyViaIPC(pipePath string) func(timeout time.Duration) error {
	return func(timeout time.Duration) error {
		deadline := time.Now().Add(timeout)
		var lastErr error
		for {
			ready, err := probeReconcileReadyOnce(pipePath)
			if err == nil && ready {
				return nil
			}
			lastErr = err
			if time.Now().After(deadline) {
				if lastErr != nil {
					return fmt.Errorf("reconcile-ready poll timed out after %s; last error: %w", timeout, lastErr)
				}
				return fmt.Errorf("reconcile-ready poll timed out after %s (supervisor never reported reconcile_ready=true)", timeout)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
}

// probeReconcileReadyOnce dials the supervisor IPC pipe and issues
// one `status` request, returning (ready, err). `err` reports
// transport-level failure (dial, JSON, timeout); ready reports the
// supervisor's reported state. The caller's poll loop tolerates
// non-nil errors during the supervisor's startup window.
func probeReconcileReadyOnce(pipePath string) (bool, error) {
	conn, err := winio.DialPipe(pipePath, durPtr(2*time.Second))
	if err != nil {
		return false, fmt.Errorf("DialPipe: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	// Read hello frame first (1 line of JSON terminated by \n).
	if err := skipHelloFrame(conn); err != nil {
		return false, fmt.Errorf("hello: %w", err)
	}
	// Send status request.
	req := api.IPCRequest{ID: 1, Cmd: "status"}
	if err := writeFrame(conn, req); err != nil {
		return false, fmt.Errorf("send status: %w", err)
	}
	resp, err := readFrame(conn)
	if err != nil {
		return false, fmt.Errorf("read response: %w", err)
	}
	if !resp.OK {
		return false, nil
	}
	result, _ := resp.Result.(map[string]any)
	if result == nil {
		return false, nil
	}
	if v, ok := result["reconcile_ready"].(bool); ok {
		return v, nil
	}
	return false, nil
}

// supervisorIPCSender returns a migration.SupervisorIPC closure that
// dials the named pipe, validates the hello frame against
// supervisor.lock owner, sends one IPC request, and returns the
// response error (or nil on OK). Used by migration.RunRollback steps
// 2 (quiesce-timers) and 3 (exit{graceful}).
func supervisorIPCSender(pipePath, lockPath string) func(cmd string, args map[string]any, timeout time.Duration) error {
	return func(cmdName string, args map[string]any, timeout time.Duration) error {
		// Pre-read supervisor.lock owner for handshake verification.
		// On read failure proceed without validation (best-effort) —
		// the migration driver is the only legitimate caller and a
		// stale-lock scenario shows up as IPC handshake mismatch.
		owner, _ := api.ReadSupervisorLockOwner(lockPath)

		conn, err := winio.DialPipe(pipePath, durPtr(timeout))
		if err != nil {
			return fmt.Errorf("DialPipe: %w", err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(timeout))

		if err := verifyHelloFrame(conn, owner); err != nil {
			return fmt.Errorf("hello validation: %w", err)
		}
		req := api.IPCRequest{ID: time.Now().UnixNano(), Cmd: cmdName, Args: args}
		if err := writeFrame(conn, req); err != nil {
			return fmt.Errorf("send %s: %w", cmdName, err)
		}
		// Read frames until Final=true OR ok=true non-final (the IPC
		// dispatcher's two-frame convention from spec §"Wire format").
		for {
			resp, err := readFrame(conn)
			if err != nil {
				return fmt.Errorf("read %s response: %w", cmdName, err)
			}
			if resp.Error != nil {
				return fmt.Errorf("%s: %s: %s", cmdName, resp.Error.Code, resp.Error.Message)
			}
			if resp.Final {
				return nil
			}
			if !resp.OK {
				return fmt.Errorf("%s: non-OK non-final response", cmdName)
			}
			// Non-final OK = acceptance ack; continue reading for the
			// final frame. The migration driver expects this two-frame
			// envelope for quiesce-timers; for exit{graceful} the
			// supervisor sends a single Final=true frame.
		}
	}
}

// probeSupervisorTokenMismatch returns a closure that opens the
// supervisor PID with PROCESS_TERMINATE rights. A successful open
// returns nil; access-denied returns an error mapped by the migration
// driver to ExitRollbackTokenMismatch (exit 13).
//
// The check exists because rollback's force-kill fallback uses
// taskkill /F /T, which on Windows requires the caller to hold
// PROCESS_TERMINATE rights. A non-elevated shell trying to roll back
// a supervisor started under runas /user:Administrator would otherwise
// fail at the force-kill step with a more confusing error.
func probeSupervisorTokenMismatch(lockPath string) func() error {
	return func() error {
		owner, err := api.ReadSupervisorLockOwner(lockPath)
		if err != nil {
			// No lock owner = no running supervisor = no token issue.
			return nil
		}
		if owner.PID <= 0 {
			return nil
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(owner.PID))
		if err != nil {
			if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
				return fmt.Errorf("PROCESS_TERMINATE denied on supervisor PID %d: %w", owner.PID, err)
			}
			// Other errors (PID gone, etc.) are benign here.
			return nil
		}
		_ = windows.CloseHandle(h)
		return nil
	}
}

// forceKillSupervisor returns a closure that taskkill's the
// supervisor's PID + its tree. Used after IPC exit{graceful} times
// out in rollback.
func forceKillSupervisor(lockPath string) func() error {
	return func() error {
		owner, err := api.ReadSupervisorLockOwner(lockPath)
		if err != nil {
			// No owner = nothing to kill.
			return nil
		}
		if owner.PID <= 0 {
			return nil
		}
		return killPIDViaTaskkill(owner.PID)
	}
}

// ---------------------------------------------------------------------------
// v5UpgradeDeps: production wiring for cli.UpgradeDeps.
// ---------------------------------------------------------------------------

// v5UpgradeDeps is the production implementation of UpgradeDeps that
// drives the rename-aside + IPC + supervisor-spawn flow on Windows.
// Field semantics match the cli.UpgradeOpts shape exactly.
type v5UpgradeDeps struct {
	exePath           string
	newBinaryPath     string
	supervisorLockDir string
	pipePath          string
}

func (d *v5UpgradeDeps) RenameAsideBinary(target, newSrc string) error {
	return api.RenameAsideReplace(target, newSrc)
}

func (d *v5UpgradeDeps) QuiesceTimers(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	owner, _ := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	return sendIPCWithResponse(ctx, pipePath, owner, "quiesce-timers", map[string]any{"timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d *v5UpgradeDeps) ExitGraceful(ctx context.Context, pipePath string, timeoutMs int) (api.IPCResponse, error) {
	owner, _ := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	return sendIPCWithResponse(ctx, pipePath, owner, "exit", map[string]any{"graceful": true, "timeout_ms": timeoutMs}, time.Duration(timeoutMs+5000)*time.Millisecond)
}

func (d *v5UpgradeDeps) ForceKillSupervisor(pipePath string) error {
	owner, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil || owner.PID <= 0 {
		return nil
	}
	return killPIDViaTaskkill(owner.PID)
}

func (d *v5UpgradeDeps) StartSupervisor(binaryPath string) error {
	// Read strict-mode intent from disk so the new supervisor honors
	// the operator's last setting after the rename-aside swap.
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("resolve state-dir: %w", err)
	}
	strictMode, _ := readPreMigrationStrictMode(stateDir)
	return spawnSupervisorDetached(binaryPath, strictMode)()
}

// sendIPCWithResponse is the response-returning variant of
// supervisorIPCSender used by v5UpgradeDeps. Returns the final-frame
// response (or the only frame for single-frame commands).
func sendIPCWithResponse(ctx context.Context, pipePath string, owner api.SupervisorLockOwner, cmdName string, args map[string]any, timeout time.Duration) (api.IPCResponse, error) {
	conn, err := winio.DialPipe(pipePath, durPtr(timeout))
	if err != nil {
		return api.IPCResponse{}, fmt.Errorf("DialPipe: %w", err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}
	_ = conn.SetDeadline(deadline)

	if err := verifyHelloFrame(conn, owner); err != nil {
		return api.IPCResponse{}, fmt.Errorf("hello: %w", err)
	}
	req := api.IPCRequest{ID: time.Now().UnixNano(), Cmd: cmdName, Args: args}
	if err := writeFrame(conn, req); err != nil {
		return api.IPCResponse{}, fmt.Errorf("send %s: %w", cmdName, err)
	}
	// Loop for the final frame.
	for {
		resp, err := readFrame(conn)
		if err != nil {
			return api.IPCResponse{}, fmt.Errorf("read %s: %w", cmdName, err)
		}
		if resp.Final {
			return resp, nil
		}
		if !resp.OK && resp.Error != nil {
			return resp, fmt.Errorf("%s: %s: %s", cmdName, resp.Error.Code, resp.Error.Message)
		}
		// Continue: this was the immediate accepted-frame for the
		// two-frame command shape.
	}
}

// ---------------------------------------------------------------------------
// Low-level IPC frame I/O.
// ---------------------------------------------------------------------------

// skipHelloFrame reads + discards the supervisor's hello frame at
// connection start.
func skipHelloFrame(conn net.Conn) error {
	var buf [4096]byte
	for i := 0; i < 4096; i++ {
		n, err := conn.Read(buf[i : i+1])
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if buf[i] == '\n' {
			return nil
		}
	}
	return fmt.Errorf("hello frame exceeded 4 KiB")
}

// verifyHelloFrame reads the hello frame and validates it against the
// expected supervisor.lock owner. Mismatch returns an error; the
// caller closes the connection.
func verifyHelloFrame(conn net.Conn, expected api.SupervisorLockOwner) error {
	line, err := readLine(conn, 4096)
	if err != nil {
		return err
	}
	var env struct {
		Hello api.IPCHello `json:"hello"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("decode hello: %w (raw=%q)", err, string(line))
	}
	if expected.PID == 0 && expected.StartedAt == "" {
		// No owner sidecar to compare against — best-effort, accept.
		return nil
	}
	if !api.ValidateHandshake(env.Hello, expected) {
		return fmt.Errorf("hello mismatch: got pid=%d started_at=%s expected pid=%d started_at=%s",
			env.Hello.PID, env.Hello.StartedAt, expected.PID, expected.StartedAt)
	}
	return nil
}

// writeFrame JSON-encodes req + appends a trailing newline (the
// supervisor's frame delimiter per spec §"Wire format").
func writeFrame(conn net.Conn, req api.IPCRequest) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	raw = append(raw, '\n')
	if _, err := conn.Write(raw); err != nil {
		return err
	}
	return nil
}

// readFrame reads one JSON line from conn and decodes it into an
// IPCResponse.
func readFrame(conn net.Conn) (api.IPCResponse, error) {
	line, err := readLine(conn, 16384)
	if err != nil {
		return api.IPCResponse{}, err
	}
	var resp api.IPCResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return api.IPCResponse{}, fmt.Errorf("decode response: %w (raw=%q)", err, string(line))
	}
	return resp, nil
}

// readLine reads bytes until '\n' or max is hit. Returns the line
// WITHOUT the trailing newline. Returns error on max exceeded.
func readLine(conn net.Conn, max int) ([]byte, error) {
	buf := make([]byte, 0, 256)
	for i := 0; i < max; i++ {
		var b [1]byte
		n, err := conn.Read(b[:])
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if b[0] == '\n' {
			return buf, nil
		}
		buf = append(buf, b[0])
	}
	return buf, fmt.Errorf("line exceeded %d bytes", max)
}

// ---------------------------------------------------------------------------
// Misc helpers.
// ---------------------------------------------------------------------------

// currentWindowsUsername returns the bare username (no DOMAIN\ prefix).
// Mirrors scheduler_windows.go's resolution.
func currentWindowsUsername() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	name := u.Username
	if i := strings.LastIndex(name, "\\"); i >= 0 {
		name = name[i+1:]
	}
	return name, nil
}

// superviseIPCPipePath returns the per-user named-pipe path the
// supervisor listens on. Mirrors the convention recorded in
// supervise_ipc_windows.go: `\\.\pipe\mcphub-supervisor-<USERNAME>`.
func superviseIPCPipePath(username string) string {
	return `\\.\pipe\mcphub-supervisor-` + username
}

// readPreMigrationStrictMode reads strict_mode from supervisor-intent.json
// if present. Returns false when the file is missing (first migration).
func readPreMigrationStrictMode(stateDir string) (bool, error) {
	path := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		// Surface other errors so the caller can decide whether to
		// abort the migration or proceed with strict_mode=false.
		return false, err
	}
	return intent.StrictMode, nil
}

// durPtr returns a pointer to d. winio.DialPipe takes a *time.Duration
// (nil = no timeout); the helper saves the callsite from a local var.
func durPtr(d time.Duration) *time.Duration {
	return &d
}
