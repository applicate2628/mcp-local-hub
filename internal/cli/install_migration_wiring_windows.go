//go:build windows

// Package cli — Windows-only production wiring for the `mcphub install
// --upgrade` cold-restart flow (v0.5.x → v0.5.x rename-aside + IPC handoff).
//
// The cross-platform dispatcher has one optional managed-transaction adapter.
// This file installs the Windows implementation; unsupported platforms keep it
// nil and fail closed before mutation.
//
// v0.6 Phase F NOTE: the v0.4.x→v0.5.0 forward-migration engine and the
// `mcphub install --rollback-to-legacy` demotion path were deleted in Phase F
// (the internal/migration package is gone). This file used to also wire
// forwardMigrationFn / rollbackMigrationFn / enumerateAllMcphubTasksFn into the
// migration package; those are removed. Only the cold-restart upgrade wiring
// (RunInstallUpgrade) remains.
package cli

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

func init() {
	v5UpgradeFn = runV5UpgradeWindows
}

var runInstallUpgradeWindowsFn = RunInstallUpgrade

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
	return runV5UpgradeWindowsWithPaths(cmd, exe, target)
}

// runV5UpgradeWindowsWithPaths is the internal, hermetic entry point for the
// upgrade sequence. Production resolves exe and target through the canonical
// owners above; focused tests provide an admitted GUI PE and a temporary target
// so they can reach post-staging failure paths without touching the running
// test binary or the installed product.
func runV5UpgradeWindowsWithPaths(cmd *cobra.Command, exe, target string) (retErr error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: resolve state-dir: %w", err)
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	fence, err := api.AcquireUpgradeFence(ctx, stateDir)
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: acquire upgrade transaction fence: %w", err)
	}
	fenceReleased := false
	defer func() {
		if !fenceReleased {
			api.ReleaseAndJoin(&retErr, fence.Release, "release upgrade transaction fence")
		}
	}()

	staged, err := stageV5UpgradeBinary(exe, target)
	if err != nil {
		return fmt.Errorf("v0.5 upgrade: stage binary beside canonical target: %w", err)
	}
	// RenameAsideReplace consumes staged on success. Remove it on every earlier
	// or failed return so a stale candidate cannot survive into a later upgrade.
	defer os.Remove(staged)
	deps := buildV5UpgradeDeps(target, stateDir)

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
		// Resolve the EFFECTIVE port through the owner, not the raw field: a legacy
		// Port=0 row (no longer backfilled since F5's deletion) still declares a
		// manifest port that its daemon binds, and this list gates the post-
		// force-terminate unbound check (ROLLBACK_DETACHED_DAEMONS_REMAIN). Reading
		// the raw field would silently drop every legacy row from verification, so a
		// transient that survived /T and still holds the port goes undetected and the
		// new supervisor spawns a duplicate → EADDRINUSE cycle (commission fable-F1).
		if port, ok := api.EffectiveDaemonPort(d); ok && port > 0 {
			expectedPorts = append(expectedPorts, port)
		}
	}

	if err := runInstallUpgradeWindowsFn(ctx, UpgradeOpts{
		BinaryPath:                 target,
		NewBinary:                  staged,
		PipePath:                   deps.pipePath,
		Deps:                       deps,
		ExpectedPorts:              expectedPorts,
		VerifyPortsUnbound:         verifyPortsUnboundForUpgrade,
		WaitSupervisorLockReleased: deps.WaitSupervisorLockReleased,
		WaitSupervisorReady:        deps.WaitSupervisorReady,
		AdmitStaged:                admitV5UpgradeCandidate,
		AdmitPrior:                 admitV5UpgradePrior,
		VerifyPrior:                verifyV5UpgradePrior,
		VerifyCanonical:            verifyV5UpgradeCanonical,
		WriteReceipt: func(receipt UpgradeReceiptV1) error {
			return api.WriteStateFileAtomic(filepath.Join(stateDir, UpgradeReceiptSchemaV1+".json"), receipt)
		},
		WithRollbackStopSettlementFence: func(ctx context.Context, critical func() error) error {
			return api.WithEmptyStopSettlementFence(ctx, filepath.Join(stateDir, "supervisor-state.json"), critical)
		},
	}); err != nil {
		return fmt.Errorf("v0.5 upgrade: %w", err)
	}
	if err := fence.Release(); err != nil {
		fenceReleased = true
		return fmt.Errorf("v0.5 upgrade: release upgrade transaction fence: %w", err)
	}
	fenceReleased = true
	fmt.Fprintln(cmd.OutOrStdout(), "v0.5 upgrade complete.")
	return nil
}

func admitV5UpgradeCandidate(path string) (UpgradeCandidateV1, error) {
	if err := binaryadmission.AdmitWindowsGUI(path); err != nil {
		return UpgradeCandidateV1{}, fmt.Errorf("admit Windows product PE: %w", err)
	}
	version, commit, buildDate := buildinfo.Get()
	for _, field := range []struct{ name, value string }{
		{name: "version", value: version},
		{name: "commit", value: commit},
		{name: "build_date", value: buildDate},
	} {
		trimmed := strings.TrimSpace(field.value)
		if trimmed == "" || strings.EqualFold(trimmed, "dev") || strings.EqualFold(trimmed, "unknown") {
			return UpgradeCandidateV1{}, fmt.Errorf("admit local product build: %s is placeholder %q", field.name, field.value)
		}
	}
	hash, err := hashFile(path)
	if err != nil {
		return UpgradeCandidateV1{}, fmt.Errorf("hash staged product binary: %w", err)
	}
	return UpgradeCandidateV1{
		Admission: UpgradeAdmissionLocalProduct,
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		SHA256:    hex.EncodeToString(hash),
	}, nil
}

func verifyV5UpgradeCanonical(path string, candidate UpgradeCandidateV1) error {
	if err := binaryadmission.AdmitWindowsGUI(path); err != nil {
		return fmt.Errorf("re-admit promoted Windows product PE: %w", err)
	}
	hash, err := hashFile(path)
	if err != nil {
		return fmt.Errorf("hash promoted canonical binary: %w", err)
	}
	if got := hex.EncodeToString(hash); got != candidate.SHA256 {
		return fmt.Errorf("canonical SHA-256 mismatch: got %s want %s", got, candidate.SHA256)
	}
	return nil
}

func admitV5UpgradePrior(path string) (string, error) {
	if err := binaryadmission.AdmitWindowsUpgradePrior(path); err != nil {
		return "", err
	}
	hash, err := hashFile(path)
	if err != nil {
		return "", fmt.Errorf("hash prior canonical binary: %w", err)
	}
	return hex.EncodeToString(hash), nil
}

func verifyV5UpgradePrior(path, expectedSHA256 string) error {
	if err := binaryadmission.AdmitWindowsUpgradePrior(path); err != nil {
		return err
	}
	hash, err := hashFile(path)
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(hash); got != expectedSHA256 {
		return fmt.Errorf("prior SHA-256 mismatch: got %s want %s", got, expectedSHA256)
	}
	return nil
}

// stageV5UpgradeBinary copies the running candidate into the canonical target
// directory before rename-aside. MoveFileEx cannot move a file across Windows
// volumes, while RenameAsideReplace's contract requires newSrc to be a sibling
// staged by the caller. copyExe provides the existing tempfile+atomic-rename
// pipeline, so a failed copy never exposes a partial .new file.
func stageV5UpgradeBinary(exe, target string) (string, error) {
	staged := target + ".new"
	if err := copyExe(exe, staged); err != nil {
		return "", err
	}
	return staged, nil
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
func buildV5UpgradeDeps(canonicalTarget, stateDir string) *v5UpgradeDeps {
	return &v5UpgradeDeps{
		// Force-kill identity is transaction-bound to the canonical target,
		// never to the updater image returned by os.Executable.
		exePath:           canonicalTarget,
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

// reapOwnerSIDMatchesCurrentFn is the test seam for the SEC-F3 owner-SID arm
// (Gate 5) of the kill-target identity gate. Production delegates to the single
// shared owner-SID helper process.ProcessOwnerSIDMatchesCurrent so the SID
// logic has one owner across api + cli + gui. Tests inject same-SID /
// different-SID / unverifiable verdicts without touching real process tokens.
var reapOwnerSIDMatchesCurrentFn = process.ProcessOwnerSIDMatchesCurrent

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
//	Gate 5 (owner SID)        — SEC-F3: the target's owner SID must equal the
//	                           current user's SID. A different-owner supervisor
//	                           (a multi-user host where an admin-token mcphub
//	                           could otherwise taskkill another user's
//	                           supervisor) is refused; an unverifiable owner
//	                           propagates as a reap failure. Mirrors the POSIX
//	                           reaper's UID gate (supervise_reaper_posix.go).
//
// Tri-state return:
//
//	(true,  nil) — proven live supervisor → kill.
//	(false, nil) — PROVEN not the supervisor (or a different-owner SID) → benign
//	               no-op.
//	(false, err) — UNPROVABLE: a transient lookup error OR an unverifiable owner
//	               SID means the reaper cannot prove the supervisor is gone, so
//	               it must propagate as a reap FAILURE rather than silently
//	               report success.
func supervisorPIDIsLiveMcphubSupervisor(pid int, sidecarStartedAt string, transactionInstallDir ...string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	ident, err := processLookupIdentityFn(pid)
	if err != nil {
		if errors.Is(err, process.ErrProcessNotFound) {
			return false, errUpgradeSupervisorAlreadyExited
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
	installDir := ""
	if len(transactionInstallDir) > 0 {
		installDir = strings.TrimSpace(transactionInstallDir[0])
	} else {
		installDir = supervisorReapInstallDirFn()
	}
	if installDir != "" {
		absInstall, _ := filepath.Abs(installDir)
		absExe, _ := filepath.Abs(ident.ExecutablePath)
		if !supervisorPathHasPrefix(absExe, absInstall) {
			return false, nil
		}
	}
	// Gate 5 (SEC-F3 owner SID): refuse to taskkill a supervisor owned by a
	// DIFFERENT user even when its image/argv/creation-time/path all match.
	//   - SID matches              → proceed (fall through to the live verdict).
	//   - proven different-owner    → benign no-op (false, nil): not OUR
	//     supervisor to reap, so treat it like an identity mismatch.
	//   - target GONE (pr301 r4 Finding 2): the supervisor exited between Gate
	//     1's identity probe and this gate's OpenProcess (a TOCTOU window). The
	//     SID gate returns ErrProcessAlreadyExited. Treat it as a benign no-op
	//     (false, nil) — IDENTICAL to Gate 1's ErrProcessNotFound branch above:
	//     a gone supervisor is "nothing to reap", so the reaper reports success
	//     by skipping it rather than ABORTING install --upgrade.
	//   - owner unverifiable (err)  → propagate as a reap failure (false, err),
	//     consistent with the transient-probe-error contract above.
	// Error-first: resolve the already-dead sentinel BEFORE the generic error
	// path so a gone target is never misread as an unverifiable live one.
	match, sidErr := reapOwnerSIDMatchesCurrentFn(pid)
	if sidErr != nil {
		if errors.Is(sidErr, process.ErrProcessAlreadyExited) {
			return false, errUpgradeSupervisorAlreadyExited
		}
		return false, fmt.Errorf("supervisor kill-target owner-SID gate failed for PID %d: %w", pid, sidErr)
	}
	if !match {
		return false, nil
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

// newInstallSupervisorCmd builds the detached `<exe> supervise [--strict-mode]`
// command for the install/upgrade path. Package-level (rather than an inline
// closure) so the spawn CONFIGURATION can be asserted without starting a
// supervisor.
//
// Process detachment preserves supervisor lifetime independently of the caller.
// process.NoConsole adds the shared CREATE_NO_WINDOW child contract, and the
// product's ordinary startup policy never attaches or allocates a console.
// Every degraded retry rebuilds through this function and therefore reapplies
// the same no-window contract without environment markers or argv propagation.
func newInstallSupervisorCmd(exePath string, args []string) *exec.Cmd {
	c := exec.Command(exePath, args...)
	c.SysProcAttr = &windows.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
	process.NoConsole(c)
	return c
}

// installSupervisorCmdBuilder returns the builder the spawn path hands to
// startSupervisorDetachedBreakaway. Package-level so a test can invoke the
// builder REPEATEDLY and assert that each rebuild — not merely the first
// attempt — carries the no-window child contract.
func installSupervisorCmdBuilder(exePath string, strictMode bool) func() *exec.Cmd {
	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	return func() *exec.Cmd { return newInstallSupervisorCmd(exePath, args) }
}

// spawnSupervisorDetached returns a closure that launches
// `<exe> supervise [--strict-mode]` as a detached background process via the
// per-OS process-detachment primitive.
//
// Windows: DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP provides lifetime and
// signal isolation. The command builder independently applies the shared
// no-window policy on every attempt. The spawned supervisor
// survives the CLI's own exit AND the closing of the terminal the operator ran
// `mcphub install --upgrade` in.
func spawnSupervisorDetached(exePath string, strictMode bool) func() error {
	return func() error {
		// build constructs a fresh detached supervisor cmd so the
		// breakaway-tolerant flagless retry (PART 1) can rebuild an
		// equivalent one if the parent job forbids breakaway.
		build := installSupervisorCmdBuilder(exePath, strictMode)
		// PART 1 (§5 permanent fix): add CREATE_BREAKAWAY_FROM_JOB so the
		// new supervisor escapes any KILL_ON_JOB_CLOSE job inherited from
		// the install/migrate CLI's launcher. On a locked-down host that
		// forbids breakaway it retries flagless rather than hard-failing
		// the upgrade (a hard abort here leaves the binary swapped + the
		// prior supervisor dead + nothing running — a worse regression).
		started, err := startSupervisorDetachedBreakaway(build(), build, func(degradeErr error) {
			fmt.Fprintf(os.Stderr, "install: supervisor spawn CREATE_BREAKAWAY_FROM_JOB rejected by parent job (no BREAKAWAY_OK); spawned flagless: %v\n", degradeErr)
		})
		if err != nil {
			return fmt.Errorf("spawn supervisor: %w", err)
		}
		// Release the handle — the supervisor manages its own lifetime
		// from here. Caller does NOT Wait().
		if started.Process != nil {
			_ = started.Process.Release()
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
	// exePath is the canonical transaction target used to start and identify
	// the managed supervisor. It is deliberately not the updater executable.
	exePath           string
	supervisorLockDir string
	pipePath          string
}

func (d *v5UpgradeDeps) RenameAsideBinary(target, newSrc string) (api.RenameAsideResult, error) {
	if err := binaryadmission.AdmitWindowsUpgradePrior(target); err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("admit prior canonical binary: %w", err)
	}
	if err := binaryadmission.AdmitWindowsGUI(newSrc); err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("admit staged successor binary: %w", err)
	}
	priorHash, err := hashFile(target)
	if err != nil {
		return api.RenameAsideResult{}, fmt.Errorf("hash prior canonical binary: %w", err)
	}
	result, err := api.RenameAsideReplaceWithResult(target, newSrc)
	if err != nil {
		return result, err
	}
	retainedHash, err := hashFile(result.RetainedPrior)
	if err != nil {
		return result, fmt.Errorf("hash exact retained prior %s after promotion: %w", result.RetainedPrior, err)
	}
	if !bytes.Equal(retainedHash, priorHash) {
		return result, fmt.Errorf("exact retained prior SHA-256 mismatch after promotion")
	}
	return result, nil
}

func (d *v5UpgradeDeps) RestoreRetainedBinary(target, retainedPrior string) error {
	return copyExeWithWindowsAdmission(retainedPrior, target, binaryadmission.AdmitWindowsUpgradePrior)
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
			return fmt.Errorf("%w: supervisor lock owner is absent", errUpgradeSupervisorAlreadyExited)
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
	//   - process-gone → typed already-exited outcome consumed by orchestration.
	//   - identity mismatch → explicit refusal; never report a kill.
	//   - transient probe error → propagate (fable-5 #276 Finding 2).
	installDir := ""
	if strings.TrimSpace(d.exePath) != "" {
		installDir = filepath.Dir(d.exePath)
	}
	live, err := supervisorPIDIsLiveMcphubSupervisor(owner.PID, owner.StartedAt, installDir)
	if err != nil {
		return fmt.Errorf("force-kill: %w", err)
	}
	if !live {
		return fmt.Errorf("force-kill: refusing PID %d because its live process identity does not match the transaction canonical target %s", owner.PID, d.exePath)
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

func (d *v5UpgradeDeps) WaitSupervisorLockReleased(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		lk, err := api.AcquireSupervisorLockQuiet(d.supervisorLockDir)
		if err == nil {
			lk.Release()
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last lock probe: %v)", waitCtx.Err(), lastErr)
			}
			return waitCtx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *v5UpgradeDeps) WaitSupervisorReady(ctx context.Context, timeout time.Duration, binaryPath string, candidate UpgradeCandidateV1) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lastErr error
	for {
		ready, err := d.probeUpgradeReadyOnce(binaryPath, candidate)
		if err == nil && ready {
			return nil
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("%w (last IPC status probe: %v)", waitCtx.Err(), lastErr)
			}
			return fmt.Errorf("%w (supervisor status never reported reconcile_ready=true)", waitCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (d *v5UpgradeDeps) probeUpgradeReadyOnce(binaryPath string, candidate UpgradeCandidateV1) (bool, error) {
	owner, err := api.ReadSupervisorLockOwner(d.supervisorLockDir)
	if err != nil {
		return false, fmt.Errorf("read successor supervisor lock owner: %w", err)
	}
	if owner.PID <= 0 {
		return false, fmt.Errorf("successor supervisor lock owner has invalid PID %d", owner.PID)
	}
	identity, err := processLookupIdentityFn(owner.PID)
	if err != nil {
		return false, fmt.Errorf("probe successor process identity: %w", err)
	}
	if supervisorCommandLineSubcommand(identity.CommandLine) != "supervise" {
		return false, fmt.Errorf("successor PID %d is not a supervise process", owner.PID)
	}
	ownerStartedUnix, ok := parseSidecarStartedAtUnix(owner.StartedAt)
	if !ok || identity.CreationDateUnix == 0 || identity.CreationDateUnix > ownerStartedUnix {
		return false, fmt.Errorf("successor PID %d creation identity does not match lock generation", owner.PID)
	}
	if !samePath(identity.ExecutablePath, binaryPath) {
		return false, fmt.Errorf("successor PID %d executable %s does not match canonical %s", owner.PID, identity.ExecutablePath, binaryPath)
	}
	if candidate.SHA256 != "" {
		hash, err := hashFile(identity.ExecutablePath)
		if err != nil {
			return false, fmt.Errorf("hash successor executable identity: %w", err)
		}
		if got := hex.EncodeToString(hash); got != candidate.SHA256 {
			return false, fmt.Errorf("successor executable SHA-256 mismatch: got %s want %s", got, candidate.SHA256)
		}
	}
	return probeReconcileReadyOnceWithOwner(d.pipePath, &owner)
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
	return probeReconcileReadyOnceWithOwner(pipePath, nil)
}

func probeReconcileReadyOnceWithOwner(pipePath string, expectedOwner *api.SupervisorLockOwner) (bool, error) {
	conn, err := winio.DialPipe(pipePath, durPtr(2*time.Second))
	if err != nil {
		return false, fmt.Errorf("DialPipe: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))

	var helloErr error
	if expectedOwner != nil {
		helloErr = verifyHelloFrame(conn, *expectedOwner)
	} else {
		helloErr = skipHelloFrame(conn)
	}
	if helloErr != nil {
		return false, fmt.Errorf("hello: %w", helloErr)
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
	line, err := readLine(conn, api.SupervisorIPCResponseMaxBytes)
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
