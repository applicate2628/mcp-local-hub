//go:build windows

// Package cli — Windows-only production wiring for the `mcphub install
// --upgrade` cold-restart flow (v0.5.x → v0.5.x rename-aside + IPC handoff).
//
// The cross-platform install.go declares the v5UpgradeFn routing seam as a
// nil-by-default function pointer. This file fills it in with the real Windows
// implementation via an init() hook so POSIX builds compile a no-op (falling
// back to the legacy runInstallUpgrade body) and Windows builds gain the full
// cold-restart upgrade surface.
//
// v0.6 Phase F NOTE: the v0.4.x→v0.5.0 forward-migration engine and the
// `mcphub install --rollback-to-legacy` demotion path were deleted in Phase F
// (the internal/migration package is gone). This file used to also wire
// forwardMigrationFn / rollbackMigrationFn / enumerateAllMcphubTasksFn into the
// migration package; those are removed. Only the cold-restart upgrade wiring
// (RunInstallUpgrade) remains.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

func init() {
	v5UpgradeFn = runV5UpgradeWindows
}

// runV5UpgradeWindows wires cli.RunInstallUpgrade with the production
// rename-aside + IPC + supervisor-spawn callbacks. The caller already verified
// supervisor-intent.json is present (the routing branch's discriminator), so
// this path is the "v0.5.x → v0.5.x same-version upgrade" flow per spec
// §"Upgrade sequence".
func runV5UpgradeWindows(cmd *cobra.Command) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve executable: %w", err)
	}
	target, err := setupTargetPath()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve canonical target: %w", err)
	}
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve state-dir: %w", err)
	}
	deps := buildV5UpgradeDeps(exe, stateDir)

	// Resolve expected daemon ports from supervisor-intent.json so the
	// post-force-kill verification (codex-r2-c-p1-8 fix) can prove no
	// zombie children survived.
	//
	// Codex round-3 Lane C P1 #3: the routing layer chose this path
	// because supervisor-intent.json existed (`os.Stat` returned
	// success). A subsequent ReadSupervisorIntent failure here is NOT
	// best-effort — it means the file the routing discriminator
	// relied on is now unreadable (corrupt JSON, EBUSY race, permission
	// drift). Returning a no-verify upgrade in that case would let the
	// new supervisor start without proving the old daemon ports
	// unbound, which is exactly the zombie-children regression the
	// codex-r2-c-p1-8 fix exists to prevent. Fail closed.
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: supervisor-intent.json present but unreadable at %s: %w (routing chose v5 upgrade because the file existed; refusing to skip post-force-kill port verification)", intentPath, err)
	}
	if intent == nil {
		return fmt.Errorf("v0.5 upgrade: supervisor-intent.json at %s decoded to nil (corrupt envelope); refusing to skip post-force-kill port verification", intentPath)
	}
	var expectedPorts []int
	for _, d := range intent.Daemons {
		if d.Port > 0 {
			expectedPorts = append(expectedPorts, d.Port)
		}
	}

	if err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:         target,
		NewBinary:          exe,
		PipePath:           deps.pipePath,
		Deps:               deps,
		ExpectedPorts:      expectedPorts,
		VerifyPortsUnbound: verifyPortsUnboundForUpgrade,
	}); err != nil {
		return fmt.Errorf("v0.5 upgrade: %w", err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "v0.5 upgrade complete.")
	return nil
}

// buildV5UpgradeDeps constructs the production v5UpgradeDeps for the
// `mcphub install --upgrade` cold-restart flow.
//
// SEC-F1: the IPC dial path is the SID-based canonical resolver
// api.SupervisorIPCAddress(stateDir) — the SAME pipe the supervisor LISTENS
// on (supervise_pipe_windows.go → api.SupervisorIPCAddress) and the same pipe
// the status/exit clients dial. The previous wiring built
// `\\.\pipe\mcphub-supervisor-<USERNAME>` via superviseIPCPipePath, but the
// listener keys the pipe NAME off the kernel-authoritative user SID
// (S-1-5-21-…), not the USERNAME env var (PR #212 r3 SID-consistency
// migration). USERNAME ≠ SID, so every quiesce-timers/exit{graceful} handshake
// dialed a pipe no supervisor listened on, timed out, and fell through to the
// force-kill fallback — bypassing the graceful drain and opening the
// orphan-daemon window the quiesce path exists to avoid (it surfaced in the
// field as "supervisor won't restart after install --upgrade; recovery needs
// manual schtasks /Run").
//
// stateDir is passed through so the test-isolation discriminator
// (api.EnableSupervisorIPCTestPipeIsolation) can redirect the dial onto a per-
// test pipe; production ignores the arg and always derives the SID.
func buildV5UpgradeDeps(exe, stateDir string) *v5UpgradeDeps {
	return &v5UpgradeDeps{
		exePath:           exe,
		newBinaryPath:     exe, // current exe IS the new image
		supervisorLockDir: filepath.Join(stateDir, "supervisor.lock"),
		pipePath:          api.SupervisorIPCAddress(stateDir),
	}
}

// ---------------------------------------------------------------------------
// Kill-target identity gate (shared by v5UpgradeDeps.ForceKillSupervisor).
// ---------------------------------------------------------------------------

// processLookupIdentityFn is a test seam over process.LookupProcessIdentity.
// Production wires it to the real Windows implementation; tests inject a fake.
// It backs supervisorPIDIsLiveMcphubSupervisor's kill-target identity gate (bot
// PR #276 r3 P2).
var processLookupIdentityFn = process.LookupProcessIdentity

// killPIDViaTaskkillFn is a test seam over killPIDViaTaskkill so the
// supervisor-reaper identity-gate tests (bot PR #276 r3 P2) can observe WHICH
// PID — if any — the reaper would force-kill WITHOUT ever shelling `taskkill`
// against a real process. Production wires the real helper; a test swaps it to
// record the call. The interlock/reuse hazard tests must never actually kill
// anything (CLAUDE.md: the developer runs ~21 live production daemons under
// their supervisor), so the kill is mediated through this seam.
var killPIDViaTaskkillFn = killPIDViaTaskkill

// supervisorReapInstallDirFn is the test seam for the install-dir anchor of the
// kill-target identity gate's path check (Gate 4). Production resolves it to the
// directory of the running mcphub binary. Tests inject a deterministic dir so
// the path gate can be exercised without touching the real executable layout.
// An empty result disables Gate 4 — fail-open on the path axis only, never on
// the identity axes.
var supervisorReapInstallDirFn = defaultSupervisorReapInstallDir

func defaultSupervisorReapInstallDir() string {
	exe := canonicalMcphubPath()
	if exe == "" {
		return ""
	}
	return filepath.Dir(exe)
}

// supervisorPIDIsLiveMcphubSupervisor validates that the PID recorded in
// supervisor.lock.owner.json is actually a LIVE mcphub supervisor process —
// the SAME process the sidecar was written for — before any caller
// force-kills it (bot PR #276 r3 P2; hardened to four-gate parity per the
// fable-5 #276 security review). It is the kill-target identity gate the
// supervisor reaper (v5UpgradeDeps.ForceKillSupervisor) consults.
//
// WHY this gate exists: the owner sidecar is best-effort and SURVIVES a
// supervisor crash (an OS-killed supervisor never tidies it). If that crashed
// supervisor's PID is later REUSED by an unrelated OS process, a reaper that
// blindly trusts the sidecar PID would `taskkill /F /T` that unrelated process.
// So the reaper must NAME and VALIDATE its target and refuse to kill anything
// that fails, treating a stale/reused/unrelated PID as "no supervisor to reap"
// (no-op), exactly as a genuinely-absent supervisor is treated.
//
// The four gates:
//
//	Gate 1 (image basename)  — mcphub(.exe), case-insensitive.
//	Gate 2 (argv token)      — argv[1] == "supervise" EXACTLY (token, not
//	                           substring).
//	Gate 3 (creation-time)   — the process's CreationDateUnix must PRECEDE the
//	                           StartedAt the sidecar recorded. A PID created AFTER
//	                           the sidecar write cannot be the process the sidecar
//	                           was written for, so it is a reuse — refuse.
//	Gate 4 (executable path)  — ExecutablePath under the mcphub install dir,
//	                           anchoring against a same-user attacker who spoofs
//	                           name+argv from another directory.
//
// Tri-state return:
//
//	(true,  nil) — proven live supervisor → kill.
//	(false, nil) — PROVEN not the supervisor → benign no-op.
//	(false, err) — UNPROVABLE: a transient lookup error means the reaper cannot
//	               prove the supervisor is gone, so it must propagate as a reap
//	               FAILURE rather than silently report success.
func supervisorPIDIsLiveMcphubSupervisor(pid int, sidecarStartedAt string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	ident, err := processLookupIdentityFn(pid)
	if err != nil {
		if errors.Is(err, process.ErrProcessNotFound) {
			// PID is PROVEN gone — there is no supervisor to reap. Benign
			// no-op, identical to a genuinely-absent supervisor.
			return false, nil
		}
		// Transient / probe error. We CANNOT prove the recorded PID is dead, so
		// we must NOT report "nothing to kill". Propagate as a reap failure.
		return false, fmt.Errorf("supervisor kill-target identity probe failed for PID %d: %w", pid, err)
	}
	// Gate 1: image basename is mcphub(.exe), case-insensitive.
	base := strings.TrimSpace(ident.Basename)
	if !strings.EqualFold(base, "mcphub.exe") && !strings.EqualFold(base, "mcphub") {
		return false, nil
	}
	// Gate 2: argv[1] is EXACTLY "supervise" (token, not substring).
	if supervisorCommandLineSubcommand(ident.CommandLine) != "supervise" {
		return false, nil
	}
	// Gate 3: the process creation time PRECEDES the StartedAt the sidecar
	// recorded. An empty/unparseable StartedAt cannot anchor this defense →
	// fail closed (no-op).
	startedAtUnix, ok := parseSidecarStartedAtUnix(sidecarStartedAt)
	if !ok {
		return false, nil
	}
	if ident.CreationDateUnix == 0 || ident.CreationDateUnix > startedAtUnix {
		return false, nil
	}
	// Gate 4: ExecutablePath under the mcphub install dir. When the install
	// dir cannot be resolved (empty) the gate is skipped — fail-open on the
	// path axis only, never on the identity axes above.
	installDir := supervisorReapInstallDirFn()
	if installDir != "" {
		absInstall, _ := filepath.Abs(installDir)
		absExe, _ := filepath.Abs(ident.ExecutablePath)
		if !supervisorPathHasPrefix(absExe, absInstall) {
			return false, nil
		}
	}
	return true, nil
}

// supervisorCommandLineSubcommand extracts argv[1] (the cobra subcommand token)
// from a process command-line, honoring quoted-image paths so a
// `"C:\Program Files\mcphub.exe" supervise` form yields "supervise". Returns ""
// when there is no argv[1]. Keys on the precise daemon/command shape rather than
// a substring (fable-5 #276 Finding 3).
func supervisorCommandLineSubcommand(cmdline string) string {
	rest := strings.TrimSpace(cmdline)
	if rest == "" {
		return ""
	}
	// Strip the image (argv[0]). A leading double-quote means the image path
	// is quoted and may contain spaces — consume up to the closing quote.
	if strings.HasPrefix(rest, `"`) {
		if end := strings.IndexByte(rest[1:], '"'); end >= 0 {
			rest = rest[1+end+1:]
		} else {
			// Unterminated quote — no parseable argv[1].
			return ""
		}
	} else if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		rest = rest[idx:]
	} else {
		// Image only, no argv[1].
		return ""
	}
	rest = strings.TrimLeft(rest, " \t")
	if rest == "" {
		return ""
	}
	// argv[1] is the next whitespace-delimited token.
	if idx := strings.IndexAny(rest, " \t"); idx >= 0 {
		return rest[:idx]
	}
	return rest
}

// parseSidecarStartedAtUnix parses SupervisorLockOwner.StartedAt (RFC3339Nano
// UTC) into Unix SECONDS so it compares against
// process.ProcessIdentity.CreationDateUnix (also Unix seconds). Returns
// (0, false) on empty or unparseable input so the caller fails closed.
func parseSidecarStartedAtUnix(startedAt string) (int64, bool) {
	s := strings.TrimSpace(startedAt)
	if s == "" {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0, false
	}
	return t.Unix(), true
}

// supervisorPathHasPrefix reports whether path is prefix itself or any nested
// child, case-insensitively (Windows).
func supervisorPathHasPrefix(path, prefix string) bool {
	cleanPath := filepath.Clean(path)
	cleanPrefix := filepath.Clean(prefix)
	if strings.EqualFold(cleanPath, cleanPrefix) {
		return true
	}
	if !strings.HasSuffix(cleanPrefix, string(filepath.Separator)) {
		cleanPrefix += string(filepath.Separator)
	}
	if len(cleanPath) < len(cleanPrefix) {
		return false
	}
	return strings.EqualFold(cleanPath[:len(cleanPrefix)], cleanPrefix)
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

// verifyPortsUnboundForUpgrade is the production wiring for
// cli.UpgradeOpts.VerifyPortsUnbound. After a force-kill fallback in
// the upgrade flow, every prior-daemon listening port must release
// before the new supervisor spawns (otherwise zombie children would
// collide with the supervisor's reconcile loop). Polls each port with
// portBindWaitForRelease; returns the first per-port timeout error.
func verifyPortsUnboundForUpgrade(ports []int, perPortTimeout time.Duration) error {
	for _, p := range ports {
		if err := portBindWaitForRelease(p, perPortTimeout); err != nil {
			return err
		}
	}
	return nil
}

// portBindWaitForRelease blocks until 127.0.0.1:port is unbound OR
// timeout elapses. Polling cadence: 100 ms; total budget bounded by
// timeout. Returns nil on unbound, non-nil error on timeout (or hard
// listen failure other than EADDRINUSE).
func portBindWaitForRelease(port int, timeout time.Duration) error {
	if port <= 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			// Port was unbound — close immediately so the daemon can re-bind.
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
// Supervisor spawn wiring.
// ---------------------------------------------------------------------------

// spawnSupervisorDetached returns a closure that launches
// `<exe> supervise [--strict-mode]` as a detached background process via the
// per-OS process-detachment primitive.
//
// Windows: DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP so the new supervisor's
// stdin/stdout/stderr inherit nothing from this CLI process. It survives the
// CLI's own exit.
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
	// Codex round-4 Lane C P1 (codex-r4-c-p1): the historical
	// implementation collapsed every ReadSupervisorLockOwner error
	// onto benign nil — including permission denied, corrupt JSON,
	// and non-positive PID values. Under the now-strict
	// RunInstallUpgrade path that swallow hides real failures from
	// the orchestrator (which relies on a non-nil return to escalate
	// to verifyPortsUnbound / abort). Only the os.IsNotExist branch
	// represents a proven "no supervisor running" condition; every
	// other read failure or corrupt-sidecar shape must propagate.
	owner, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Genuine "no supervisor running" — benign.
			return nil
		}
		return fmt.Errorf("force-kill: read supervisor lock owner: %w", err)
	}
	if owner.PID <= 0 {
		return fmt.Errorf("force-kill: supervisor.lock.owner.json has invalid PID %d (corrupt sidecar)", owner.PID)
	}
	// Kill-target identity gate (bot PR #276 r3 P2; hardened to four-gate
	// parity per fable-5 #276). The owner sidecar survives a supervisor crash
	// and its PID can be REUSED by an unrelated process. Validate the PID is the
	// live mcphub supervisor the sidecar names before force-killing it:
	//   - identity-mismatch / process-gone → benign no-op (return nil).
	//   - a transient / non-ErrProcessNotFound probe error → PROPAGATE
	//     (fable-5 #276 Finding 2).
	live, err := supervisorPIDIsLiveMcphubSupervisor(owner.PID, owner.StartedAt)
	if err != nil {
		return fmt.Errorf("force-kill: %w", err)
	}
	if !live {
		return nil
	}
	return killPIDViaTaskkillFn(owner.PID)
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

// sendIPCWithResponse is the response-returning IPC sender used by
// v5UpgradeDeps. Returns the final-frame response (or the only frame for
// single-frame commands).
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
// Reconcile-ready IPC poll (shared with migrate_serena_restart_windows.go).
// ---------------------------------------------------------------------------

// waitReconcileReadyViaIPC returns a closure that polls supervisor IPC
// `status` until `reconcile_ready: true` is observed in the response result
// map OR the timeout elapses. Used by the serena migrate START driver
// (migrate_serena_restart_windows.go) after it (re)starts the supervisor.
//
// Production note: the supervisor's IPC `status` handler includes a
// `reconcile_ready` bool that transitions from false → true after the first
// successful reconcile pass. The poll interval here is 200 ms.
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

// probeReconcileReadyOnce dials the supervisor IPC pipe and issues one
// `status` request, returning (ready, err). `err` reports transport-level
// failure (dial, JSON, timeout); ready reports the supervisor's reported
// state. The caller's poll loop tolerates non-nil errors during the
// supervisor's startup window.
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

// SEC-F1 removed the USERNAME-based superviseIPCPipePath + currentWindowsUsername
// helpers. Every IPC dial in the upgrade/migrate flow now uses the SID-based
// canonical resolver api.SupervisorIPCAddress (the path the supervisor LISTENS
// on), closing the PR #212 r3 SID-consistency propagation gap.

// readPreMigrationStrictMode reads strict_mode from supervisor-intent.json
// if present. Returns false when the file is missing.
func readPreMigrationStrictMode(stateDir string) (bool, error) {
	path := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		// Surface other errors so the caller can decide whether to
		// abort or proceed with strict_mode=false.
		return false, err
	}
	return intent.StrictMode, nil
}

// durPtr returns a pointer to d. winio.DialPipe takes a *time.Duration
// (nil = no timeout); the helper saves the callsite from a local var.
func durPtr(d time.Duration) *time.Duration {
	return &d
}
