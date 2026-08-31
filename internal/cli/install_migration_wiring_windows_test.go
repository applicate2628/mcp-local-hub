//go:build windows

// install_migration_wiring_windows_test.go — Windows-tagged unit tests for the
// production `mcphub install --upgrade` cold-restart wiring at
// install_migration_wiring_windows.go.
//
// v0.6 Phase F NOTE: the v0.4.x→v0.5.0 forward-migration + rollback wiring (and
// its tests) were deleted with the internal/migration package. What remains
// here covers the surviving cold-restart upgrade surface:
//
//   - runV5UpgradeWindows fails closed on an unreadable supervisor-intent.json
//     (codex round-3 Lane C P1 #3).
//   - v5UpgradeDeps.ForceKillSupervisor error-propagation contract (codex
//     round-4 Lane C P1).
//   - the kill-target identity gate supervisorPIDIsLiveMcphubSupervisor
//     (bot PR #276 r3 P2; fable-5 #276 four-gate hardening).

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/process"
)

func TestUpgradeIPCResponseFrameUsesSupervisorBound(t *testing.T) {
	t.Run("realistic status response above 30 KiB is accepted", func(t *testing.T) {
		raw, err := json.Marshal(api.IPCResponse{
			ID: 7, OK: true, Final: true,
			Result: map[string]any{
				"reconcile_ready": true,
				"daemons":         []any{map[string]any{"task_name": `\\mcp-local-hub-large-default`, "args": []any{strings.Repeat("x", 40*1024)}}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			_, _ = server.Write(append(raw, '\n'))
		}()
		resp, err := readFrame(client)
		if err != nil {
			t.Fatalf("readFrame rejected %d-byte supervisor response: %v", len(raw), err)
		}
		if !resp.OK || !resp.Final {
			t.Fatalf("response = %+v, want OK final response", resp)
		}
	})

	t.Run("response above one MiB is rejected", func(t *testing.T) {
		raw, err := json.Marshal(api.IPCResponse{
			ID: 8, OK: true, Final: true,
			Result: map[string]any{"padding": strings.Repeat("x", (1<<20)+1024)},
		})
		if err != nil {
			t.Fatal(err)
		}
		client, server := net.Pipe()
		defer client.Close()
		go func() {
			defer server.Close()
			_, _ = server.Write(append(raw, '\n'))
		}()
		if _, err := readFrame(client); err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("readFrame oversize error = %v, want bounded rejection", err)
		}
	})
}

type targetBoundRollbackDeps struct {
	*v5UpgradeDeps
	retained string
	restored []string
	started  []string
}

func (d *targetBoundRollbackDeps) RenameAsideBinary(string, string) (api.RenameAsideResult, error) {
	return api.RenameAsideResult{Promoted: true, RetainedPrior: d.retained}, nil
}

func (d *targetBoundRollbackDeps) RestoreRetainedBinary(target, retained string) error {
	d.restored = append(d.restored, target+"<-"+retained)
	return nil
}

func (d *targetBoundRollbackDeps) QuiesceTimers(context.Context, string, int) (api.IPCResponse, error) {
	return api.IPCResponse{OK: true, Final: true, Result: map[string]any{"still_running": []any{}}}, nil
}

func (d *targetBoundRollbackDeps) ExitGraceful(context.Context, string, int) (api.IPCResponse, error) {
	return api.IPCResponse{OK: true, Final: true}, nil
}

func (d *targetBoundRollbackDeps) StartSupervisor(binary string) error {
	d.started = append(d.started, binary)
	return nil
}

func TestUpgradeRollbackKillsCanonicalTargetSuccessorWhenUpdaterLivesElsewhere(t *testing.T) {
	root := t.TempDir()
	updaterDir := filepath.Join(root, "build-output")
	targetDir := filepath.Join(root, "installed")
	target := filepath.Join(targetDir, "mcphub.exe")
	retained := filepath.Join(targetDir, "mcphub.exe.old-exact")
	lockPath := filepath.Join(root, "state", "supervisor.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	const successorPID = 4242
	writeStateSidecarBytes(t, lockPath+".owner.json", []byte(`{"pid":4242,"started_at":"`+liveSupervisorStartedAt+`"}`))

	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:         "mcphub.exe",
		CommandLine:      `"` + target + `" supervise`,
		ExecutablePath:   target,
		CreationDateUnix: liveSupervisorCreatedUnix,
	}, nil)
	swapSupervisorReapInstallDirForTest(t, updaterDir)
	swapReapOwnerSIDForTest(t, true, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	deps := &targetBoundRollbackDeps{
		v5UpgradeDeps: &v5UpgradeDeps{exePath: target, supervisorLockDir: lockPath},
		retained:      retained,
	}
	readyCalls := 0
	receipts := 0
	lockReleaseChecks := 0
	err := RunInstallUpgrade(context.Background(), UpgradeOpts{
		BinaryPath:                      target,
		NewBinary:                       filepath.Join(updaterDir, "mcphub.exe.staged"),
		Deps:                            deps,
		WithRollbackStopSettlementFence: func(_ context.Context, critical func() error) error { return critical() },
		WaitSupervisorLockReleased: func(context.Context, time.Duration) error {
			lockReleaseChecks++
			return nil
		},
		WaitSupervisorReady: func(context.Context, time.Duration, string, UpgradeCandidateV1) error {
			readyCalls++
			if readyCalls == 1 {
				return errors.New("forced successor readiness failure")
			}
			return nil
		},
		WriteReceipt: func(UpgradeReceiptV1) error {
			receipts++
			return nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "automatic rollback restored") {
		t.Fatalf("upgrade error = %v, want successful retained-prior rollback report", err)
	}
	if len(*killed) != 1 || (*killed)[0] != successorPID {
		t.Fatalf("taskkill targets = %v, want exact canonical successor PID %d", *killed, successorPID)
	}
	if lockReleaseChecks != 2 {
		t.Fatalf("lock release checks = %d, want prior release + rollback successor release", lockReleaseChecks)
	}
	if len(deps.restored) != 1 || deps.restored[0] != target+"<-"+retained {
		t.Fatalf("restores = %v, want exact retained prior restored to canonical target", deps.restored)
	}
	if receipts != 0 {
		t.Fatalf("receipts = %d, want none after failed successor readiness", receipts)
	}
}

func TestUpgradeReadinessRejectsWrongLiveExecutableBeforePipeAcceptance(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "supervisor.lock")
	owner := api.SupervisorLockOwner{PID: 4242, StartedAt: "2026-08-31T19:00:00Z"}
	if err := api.WriteStateFileAtomic(lockPath+".owner.json", owner); err != nil {
		t.Fatal(err)
	}
	swapProcessLookupForTest(t, process.ProcessIdentity{
		ExecutablePath:   `C:\other\mcphub.exe`,
		Basename:         "mcphub.exe",
		CommandLine:      `"C:\other\mcphub.exe" supervise`,
		CreationDateUnix: time.Date(2026, 8, 31, 18, 59, 59, 0, time.UTC).Unix(),
	}, nil)
	d := &v5UpgradeDeps{supervisorLockDir: lockPath, pipePath: `\\.\pipe\must-not-dial`}
	ready, err := d.probeUpgradeReadyOnce(`C:\canonical\mcphub.exe`, UpgradeCandidateV1{SHA256: strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "does not match canonical") {
		t.Fatalf("ready=%v error=%v", ready, err)
	}
	if ready {
		t.Fatal("wrong live executable was accepted as upgrade successor")
	}
}

func TestUpgradeReadinessRejectsWrongHelloGeneration(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		_, _ = io.WriteString(server, `{"hello":{"pid":9999,"started_at":"2026-08-31T19:00:00Z"}}`+"\n")
	}()
	err := verifyHelloFrame(client, api.SupervisorLockOwner{PID: 4242, StartedAt: "2026-08-31T19:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "hello mismatch") {
		t.Fatalf("error = %v", err)
	}
}

// withNoopSchedulerEnv pins MCPHUB_E2E_SCHEDULER=none for the test
// scope so scheduler.New() returns a noopScheduler instead of dialing
// the real Windows Task Scheduler. The host CI scheduler is shared
// with developer-installed mcp-local-hub-* tasks; a real call here
// would surface those rows and risk side-effecting them.
func withNoopSchedulerEnv(t *testing.T) {
	t.Helper()
	prev, hadPrev := os.LookupEnv("MCPHUB_E2E_SCHEDULER")
	if err := os.Setenv("MCPHUB_E2E_SCHEDULER", "none"); err != nil {
		t.Fatalf("set MCPHUB_E2E_SCHEDULER: %v", err)
	}
	t.Cleanup(func() {
		if hadPrev {
			_ = os.Setenv("MCPHUB_E2E_SCHEDULER", prev)
		} else {
			_ = os.Unsetenv("MCPHUB_E2E_SCHEDULER")
		}
	})
}

// withTempStateDir routes api.DaemonStateDir() through a per-test
// tmp dir so the upgrade wiring does not write into the developer's
// real %LOCALAPPDATA%\mcp-local-hub. Returns the absolute tmp path so
// callers can seed supervisor-intent.json.
//
// Uses apitest.HardenedTempDir so the parent-directory DACL gate in
// api.WriteStateFileAtomic accepts the path.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	root := apitest.HardenedTempDir(t)
	restore := api.SetDaemonStateRootForTest(root)
	t.Cleanup(restore)
	return root
}

func writeStateSidecarBytes(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := api.WriteStateFileBytesAtomic(path, raw); err != nil {
		t.Fatalf("seed state sidecar: %v", err)
	}
}

// ---------------------------------------------------------------------------
// codex round-3 Lane C P1 #3: runV5UpgradeWindows fails closed on
// unreadable intent.
// ---------------------------------------------------------------------------

// TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade pins the
// fail-closed contract: routing chose this path because
// supervisor-intent.json existed; if ReadSupervisorIntent fails the
// upgrade MUST abort rather than fall through to a no-verify upgrade
// (which would let the new supervisor start without proving the old
// daemon ports unbound — exactly the regression codex-r2-c-p1-8
// already paid for).
func TestRunV5UpgradeWindows_UnreadableIntentAbortsUpgrade(t *testing.T) {
	withNoopSchedulerEnv(t)
	stateDir := withTempStateDir(t)

	// Seed a corrupt supervisor-intent.json (not valid JSON).
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	writeStateSidecarBytes(t, intentPath, []byte("not-valid-json{{{"))

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	exe := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "UNREADABLE-INTENT")
	target := filepath.Join(t.TempDir(), "mcphub.exe")

	err := runV5UpgradeWindowsWithPaths(cmd, exe, target)
	if err == nil {
		t.Fatal("runV5UpgradeWindows must return non-nil error when supervisor-intent.json is unreadable; got nil")
	}
	if !strings.Contains(err.Error(), "supervisor-intent.json present but unreadable") {
		t.Fatalf("error must name the unreadable-intent cause so operators can diagnose; got %v", err)
	}
}

func TestRunV5UpgradeWindows_ExistingLivenessTickBlocksBeforeStaging(t *testing.T) {
	stateDir := withTempStateDir(t)
	holder, err := api.AcquireUpgradeFence(context.Background(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Release() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	target := filepath.Join(t.TempDir(), "mcphub.exe")
	err = runV5UpgradeWindowsWithPaths(cmd, filepath.Join(t.TempDir(), "does-not-exist.exe"), target)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("upgrade behind held liveness fence error = %v, want context.Canceled", err)
	}
	if _, statErr := os.Stat(target + ".new"); !os.IsNotExist(statErr) {
		t.Fatalf("staged candidate exists before fence acquisition: stat=%v", statErr)
	}
}

func TestRunV5UpgradeWindows_HoldsFenceAcrossPrePromotionAndRollbackBarrier(t *testing.T) {
	stateDir := withTempStateDir(t)
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	originalRun := runInstallUpgradeWindowsFn
	t.Cleanup(func() { runInstallUpgradeWindowsFn = originalRun })
	var relaunches int
	restoreRelaunch := setLivenessRelaunchFnForTest(func() error { relaunches++; return nil })
	t.Cleanup(restoreRelaunch)
	restoreStandalone := setStandaloneRelaunchFnForTest(func() error { relaunches++; return nil })
	t.Cleanup(restoreStandalone)

	phaseOutputs := make([]string, 0, 2)
	runInstallUpgradeWindowsFn = func(context.Context, UpgradeOpts) error {
		for _, phase := range []string{"pre-promotion", "rollback-kill-restore"} {
			out := &strings.Builder{}
			if err := runEnsureAlive(stateDir, out); err != nil {
				t.Fatalf("ensure-alive during %s: %v", phase, err)
			}
			phaseOutputs = append(phaseOutputs, out.String())
		}
		return errors.New("synthetic post-rollback transaction failure")
	}

	cmd := &cobra.Command{}
	cmd.SetOut(io.Discard)
	exe := writeAdmissionPEFixtureWithTag(t, binaryadmission.WindowsGUISubsystem, "FENCE-HOLD")
	target := filepath.Join(t.TempDir(), "mcphub.exe")
	err := runV5UpgradeWindowsWithPaths(cmd, exe, target)
	if err == nil || !strings.Contains(err.Error(), "synthetic post-rollback") {
		t.Fatalf("upgrade error = %v", err)
	}
	for i, output := range phaseOutputs {
		if !strings.Contains(output, "liveness-suppressed-upgrade-in-progress") {
			t.Fatalf("phase %d output = %q, want upgrade suppression", i, output)
		}
	}
	if relaunches != 0 {
		t.Fatalf("liveness relaunches during transaction = %d, want 0", relaunches)
	}
	lease, acquired, acquireErr := api.TryAcquireUpgradeFence(context.Background(), stateDir)
	if acquireErr != nil || !acquired {
		t.Fatalf("fence after failed transaction = acquired=%v err=%v", acquired, acquireErr)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release reacquired fence: %v", err)
	}
}

// ---------------------------------------------------------------------------
// codex round-4 Lane C P1: v5UpgradeDeps.ForceKillSupervisor error
// propagation. The historical implementation swallowed every
// ReadSupervisorLockOwner error including permission denied / corrupt
// sidecar / invalid PID and returned nil — hiding the failure from
// the now-strict RunInstallUpgrade path which relies on a non-nil
// return to escalate to verifyPortsUnbound / abort. Only a proven
// "already exited / no owner" condition (file not present) should
// map to benign.
// ---------------------------------------------------------------------------

// TestV5UpgradeDeps_ForceKillSupervisor_NotExistIsBenign pins the
// genuine-no-supervisor-running path.
func TestV5UpgradeDeps_ForceKillSupervisor_NotExistIsBenign(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	// Do NOT create supervisor.lock.owner.json — absence is the test.

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); !isAlreadyExitedError(err) {
		t.Fatalf("absent .owner.json outcome = %v, want typed already-exited outcome", err)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_PermissionDeniedPropagates pins
// the round-4 fix: a ReadSupervisorLockOwner error that is NOT
// os.IsNotExist must propagate.
func TestV5UpgradeDeps_ForceKillSupervisor_PermissionDeniedPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	// Write a corrupt sidecar — json.Unmarshal will fail, which is a
	// non-IsNotExist error path through ReadSupervisorLockOwner.
	writeStateSidecarBytes(t, ownerPath, []byte("not-valid-json{{{"))

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	err := d.ForceKillSupervisor("")
	if err == nil {
		t.Fatal("non-IsNotExist ReadSupervisorLockOwner failure must propagate from ForceKillSupervisor; got nil")
	}
	if os.IsNotExist(err) {
		t.Fatalf("error should NOT be IsNotExist (file was written); got %v", err)
	}
	if !strings.Contains(err.Error(), "force-kill") && !strings.Contains(err.Error(), "owner") {
		t.Errorf("error should describe the force-kill / lock-owner read failure; got %v", err)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_InvalidPIDPropagates pins the
// round-4 fix: a valid sidecar JSON whose PID is non-positive is a
// corrupt-sidecar condition that must propagate.
func TestV5UpgradeDeps_ForceKillSupervisor_InvalidPIDPropagates(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	body := `{"pid":0,"started_at":"2026-05-17T00:00:00Z"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	err := d.ForceKillSupervisor("")
	if err == nil {
		t.Fatal("invalid PID (<=0) in .owner.json must propagate from ForceKillSupervisor; got nil")
	}
	if !strings.Contains(err.Error(), "PID") {
		t.Errorf("error should name the invalid-PID cause; got %v", err)
	}
}

// ---------------------------------------------------------------------------
// bot PR #276 r3 P2: kill-target identity gate. The supervisor reaper
// (v5UpgradeDeps.ForceKillSupervisor) reads the PID from
// supervisor.lock.owner.json and force-kills it. The owner sidecar SURVIVES a
// supervisor crash, so its recorded PID can be REUSED by an unrelated OS
// process. The fix validates the PID is a live mcphub supervisor (basename
// mcphub(.exe) + `supervise` command-line + creation-time precedence + exe
// under install dir) BEFORE the kill; a stale/reused/unrelated PID is treated
// as "no supervisor to reap" (no-op). These tests drive the gate through the
// processLookupIdentityFn seam and observe the kill through the
// killPIDViaTaskkillFn seam so NOTHING is ever actually killed.
// ---------------------------------------------------------------------------

// swapKillPIDViaTaskkillForTest replaces killPIDViaTaskkillFn for the test
// scope and returns a pointer to a slice that records every PID the reaper
// would have force-killed.
func swapKillPIDViaTaskkillForTest(t *testing.T) *[]int {
	t.Helper()
	var killed []int
	orig := killPIDViaTaskkillFn
	t.Cleanup(func() { killPIDViaTaskkillFn = orig })
	killPIDViaTaskkillFn = func(pid int) error {
		killed = append(killed, pid)
		return nil
	}
	return &killed
}

// swapProcessLookupForTest installs a fixed lookup result for the
// processLookupIdentityFn seam for the test scope.
func swapProcessLookupForTest(t *testing.T, ident process.ProcessIdentity, err error) {
	t.Helper()
	orig := processLookupIdentityFn
	t.Cleanup(func() { processLookupIdentityFn = orig })
	processLookupIdentityFn = func(pid int) (process.ProcessIdentity, error) {
		out := ident
		out.PID = pid
		return out, err
	}
}

// swapSupervisorReapInstallDirForTest overrides the install-dir anchor that
// Gate 4 of the kill-target identity gate checks ExecutablePath against.
func swapSupervisorReapInstallDirForTest(t *testing.T, dir string) {
	t.Helper()
	orig := supervisorReapInstallDirFn
	t.Cleanup(func() { supervisorReapInstallDirFn = orig })
	supervisorReapInstallDirFn = func() string { return dir }
}

// swapReapOwnerSIDForTest overrides the SEC-F3 owner-SID arm (Gate 5) of the
// kill-target identity gate so tests can simulate same-SID / different-SID /
// unverifiable verdicts WITHOUT opening any real process token. Default
// production wiring would call process.ProcessOwnerSIDMatchesCurrent against the
// (fake) test PID, which returns an error for a non-existent PID — so every
// gate test that wants to reach a `true` verdict must install a same-SID pass.
func swapReapOwnerSIDForTest(t *testing.T, match bool, err error) {
	t.Helper()
	orig := reapOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { reapOwnerSIDMatchesCurrentFn = orig })
	reapOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return match, err }
}

const liveSupervisorStartedAt = "2026-05-17T00:00:00Z"

var (
	liveSupervisorStartedAtUnix = mustParseUnix(liveSupervisorStartedAt)
	liveSupervisorCreatedUnix   = liveSupervisorStartedAtUnix - 3600 // 1h before
	reusedCreatedUnix           = liveSupervisorStartedAtUnix + 3600 // 1h after
)

func mustParseUnix(rfc3339 string) int64 {
	t, err := time.Parse(time.RFC3339Nano, rfc3339)
	if err != nil {
		panic(err)
	}
	return t.Unix()
}

// TestSupervisorPIDIsLiveMcphubSupervisor_Gate is the direct unit test of the
// four-gate kill-target identity gate (basename + argv[1]=="supervise" +
// creation-time precedence + install-dir path).
func TestSupervisorPIDIsLiveMcphubSupervisor_Gate(t *testing.T) {
	const pid = 4242
	const installDir = `C:\fixture-root\dev\.local\bin`
	const exeUnderInstall = `C:\fixture-root\dev\.local\bin\mcphub.exe`
	cases := []struct {
		name            string
		ident           process.ProcessIdentity
		startedAt       string
		err             error
		want            bool
		wantErr         bool
		wantAlreadyGone bool
	}{
		{
			name: "live mcphub supervisor (exe)",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `C:\fixture-root\dev\.local\bin\mcphub.exe supervise --strict-mode`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      true,
		},
		{
			name: "live mcphub supervisor (no .exe basename)",
			ident: process.ProcessIdentity{
				Basename: "mcphub", CommandLine: `C:\fixture-root\dev\.local\bin\mcphub supervise`,
				ExecutablePath: `C:\fixture-root\dev\.local\bin\mcphub`, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      true,
		},
		{
			name: "live mcphub supervisor (quoted image with spaces)",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `"C:\Program Files\mcphub\mcphub.exe" supervise`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      true,
		},
		{
			name: "reused PID belongs to unrelated process",
			ident: process.ProcessIdentity{
				Basename: "notepad.exe", CommandLine: `C:\Windows\System32\notepad.exe`,
				ExecutablePath: `C:\Windows\System32\notepad.exe`, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			name: "mcphub process but NOT a supervisor (gui)",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `mcphub.exe gui --no-browser`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			name: "mcphub process but NOT a supervisor (daemon child)",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `mcphub.exe daemon --server time --daemon default`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			// Finding 3: "supervise" as a substring of a path/flag value, NOT
			// argv[1], must fail the argv-token gate.
			name: "supervise in a path value but argv[1] is not supervise",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `mcphub.exe gui --log-dir C:\supervise\logs`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			// Finding 1 (creation-time): a PID created AFTER the sidecar write
			// is a reuse and must fail the creation-time precedence gate.
			name: "reused PID created after the sidecar StartedAt",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `C:\fixture-root\dev\.local\bin\mcphub.exe supervise`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: reusedCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			// Finding 1 (install dir): a supervisor-shaped process whose exe is
			// OUTSIDE the install dir fails Gate 4.
			name: "argv supervise but exe outside install dir",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `C:\Temp\evil\mcphub.exe supervise`,
				ExecutablePath: `C:\Temp\evil\mcphub.exe`, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: liveSupervisorStartedAt,
			want:      false,
		},
		{
			// Empty/unparseable StartedAt cannot anchor the creation-time
			// defense → fail closed (no-op).
			name: "empty sidecar StartedAt fails closed",
			ident: process.ProcessIdentity{
				Basename: "mcphub.exe", CommandLine: `C:\fixture-root\dev\.local\bin\mcphub.exe supervise`,
				ExecutablePath: exeUnderInstall, CreationDateUnix: liveSupervisorCreatedUnix,
			},
			startedAt: "",
			want:      false,
		},
		{
			name:            "process gone (ErrProcessNotFound) is explicit already-gone",
			ident:           process.ProcessIdentity{},
			err:             process.ErrProcessNotFound,
			want:            false,
			wantErr:         true,
			wantAlreadyGone: true,
		},
		{
			// Finding 2: a transient (non-ErrProcessNotFound) probe error must
			// surface as an error so the reaper does NOT report success on a
			// possibly-live old supervisor.
			name:    "transient lookup error propagates",
			ident:   process.ProcessIdentity{},
			err:     errors.New("simulated transient WMI stall"),
			want:    false,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapProcessLookupForTest(t, tc.ident, tc.err)
			swapSupervisorReapInstallDirForTest(t, installDir)
			// Gates 1-4 are under test here; pin Gate 5 (owner SID) to a
			// same-SID pass so these cases assert the prior gate behavior
			// without opening a real process token for the fake PID.
			swapReapOwnerSIDForTest(t, true, nil)
			got, err := supervisorPIDIsLiveMcphubSupervisor(pid, tc.startedAt)
			if got != tc.want {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor(%d) = %v; want %v (err=%v)", pid, got, tc.want, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor(%d) err = %v; wantErr %v", pid, err, tc.wantErr)
			}
			if isAlreadyExitedError(err) != tc.wantAlreadyGone {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor(%d) already-gone=%v; want %v (err=%v)", pid, isAlreadyExitedError(err), tc.wantAlreadyGone, err)
			}
		})
	}
	// PID <= 0 is rejected up front (no lookup needed) — benign no-op, no error.
	if live, err := supervisorPIDIsLiveMcphubSupervisor(0, liveSupervisorStartedAt); live || err != nil {
		t.Fatalf("PID 0 must be a benign no-op; got live=%v err=%v", live, err)
	}
	if live, err := supervisorPIDIsLiveMcphubSupervisor(-1, liveSupervisorStartedAt); live || err != nil {
		t.Fatalf("negative PID must be a benign no-op; got live=%v err=%v", live, err)
	}
}

// TestSupervisorPIDIsLiveMcphubSupervisor_OwnerSIDGate is the SEC-F3
// falsifying test for Gate 5. With Gates 1-4 all passing (an mcphub.exe
// supervise process created before the sidecar, under the install dir), the
// owner-SID arm is the ONLY thing left to decide:
//
//   - same-SID    → the kill proceeds (true, nil)  — the single-user happy path.
//   - different   → REFUSED as a benign no-op (false, nil) — pre-fix this would
//     return (true, nil) and force-kill another user's supervisor.
//   - unverifiable → propagated as a reap failure (false, err) — fail closed.
func TestSupervisorPIDIsLiveMcphubSupervisor_OwnerSIDGate(t *testing.T) {
	const (
		pid        = 4242
		installDir = `C:\fixture-root\dev\.local\bin`
		exe        = `C:\fixture-root\dev\.local\bin\mcphub.exe`
	)
	// An identity that passes Gates 1-4 cleanly, so only Gate 5 decides.
	liveIdent := process.ProcessIdentity{
		Basename:         "mcphub.exe",
		CommandLine:      `C:\fixture-root\dev\.local\bin\mcphub.exe supervise --strict-mode`,
		ExecutablePath:   exe,
		CreationDateUnix: liveSupervisorCreatedUnix,
	}

	cases := []struct {
		name            string
		sidMatch        bool
		sidErr          error
		want            bool
		wantErr         bool
		wantAlreadyGone bool
	}{
		{name: "same owner SID → kill proceeds", sidMatch: true, want: true},
		{name: "different owner SID → benign no-op (refused)", sidMatch: false, want: false},
		{
			name:     "unverifiable owner SID → reap failure",
			sidMatch: false,
			sidErr:   errors.New("OpenProcessToken: access denied"),
			want:     false,
			wantErr:  true,
		},
		{
			// pr301 r4 Finding 2: the supervisor exited between Gate 1's identity
			// probe and Gate 5's OpenProcess (TOCTOU). The SID gate returns
			// ErrProcessAlreadyExited; the reaper must return the typed already-gone
			// outcome so orchestration continues without claiming taskkill succeeded.
			name:            "target vanished mid-gate (ErrProcessAlreadyExited) → explicit already-gone",
			sidMatch:        false,
			sidErr:          process.ErrProcessAlreadyExited,
			want:            false,
			wantErr:         true,
			wantAlreadyGone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			swapProcessLookupForTest(t, liveIdent, nil)
			swapSupervisorReapInstallDirForTest(t, installDir)
			swapReapOwnerSIDForTest(t, tc.sidMatch, tc.sidErr)
			got, err := supervisorPIDIsLiveMcphubSupervisor(pid, liveSupervisorStartedAt)
			if got != tc.want {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor owner-SID gate = %v; want %v (err=%v)", got, tc.want, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("supervisorPIDIsLiveMcphubSupervisor owner-SID gate err = %v; wantErr %v", err, tc.wantErr)
			}
			if isAlreadyExitedError(err) != tc.wantAlreadyGone {
				t.Fatalf("owner-SID gate already-gone=%v; want %v (err=%v)", isAlreadyExitedError(err), tc.wantAlreadyGone, err)
			}
		})
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_SkipsReusedNonSupervisorPID pins that
// a reused non-supervisor PID is refused explicitly and never killed.
//
// Falsification on the unfixed code: ForceKillSupervisor would
// killPIDViaTaskkillFn(owner.PID) unconditionally → `killed` contains the
// reused PID.
func TestV5UpgradeDeps_ForceKillSupervisor_SkipsReusedNonSupervisorPID(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	const reusedPID = 31337
	body := `{"pid":31337,"started_at":"2026-05-17T00:00:00Z"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:    "node.exe",
		CommandLine: `C:\Program Files\nodejs\node.exe server.js`,
	}, nil)
	swapSupervisorReapInstallDirForTest(t, `C:\fixture-root\dev\.local\bin`)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("ForceKillSupervisor on reused non-supervisor PID error = %v, want explicit identity refusal", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("reused non-supervisor PID %d must NOT be force-killed; killed=%v", reusedPID, *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_KillsLiveSupervisor pins the positive
// path for the upgrade reaper.
func TestV5UpgradeDeps_ForceKillSupervisor_KillsLiveSupervisor(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	const supPID = 4242
	body := `{"pid":4242,"started_at":"` + liveSupervisorStartedAt + `"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:         "mcphub.exe",
		CommandLine:      `C:\fixture-root\dev\.local\bin\mcphub.exe supervise --strict-mode`,
		ExecutablePath:   `C:\fixture-root\dev\.local\bin\mcphub.exe`,
		CreationDateUnix: liveSupervisorCreatedUnix,
	}, nil)
	swapSupervisorReapInstallDirForTest(t, `C:\fixture-root\dev\.local\bin`)
	// Gate 5 (owner SID): same-user supervisor → pass, without opening a real
	// token for the fake PID 4242.
	swapReapOwnerSIDForTest(t, true, nil)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err != nil {
		t.Fatalf("ForceKillSupervisor on a live supervisor PID: %v", err)
	}
	if len(*killed) != 1 || (*killed)[0] != supPID {
		t.Fatalf("live supervisor PID %d must be force-killed exactly once; killed=%v", supPID, *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_SkipsPIDCreatedAfterSidecar is the
// Finding-1 regression for the upgrade reaper.
func TestV5UpgradeDeps_ForceKillSupervisor_SkipsPIDCreatedAfterSidecar(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	const reusedPID = 31337
	body := `{"pid":31337,"started_at":"` + liveSupervisorStartedAt + `"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))
	swapProcessLookupForTest(t, process.ProcessIdentity{
		Basename:         "mcphub.exe",
		CommandLine:      `C:\fixture-root\dev\.local\bin\mcphub.exe supervise`,
		ExecutablePath:   `C:\fixture-root\dev\.local\bin\mcphub.exe`,
		CreationDateUnix: reusedCreatedUnix,
	}, nil)
	swapSupervisorReapInstallDirForTest(t, `C:\fixture-root\dev\.local\bin`)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("ForceKillSupervisor on PID created after sidecar error = %v, want explicit identity refusal", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("reused PID %d (created after sidecar) must NOT be force-killed; killed=%v", reusedPID, *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_TransientProbeError_Propagates is the
// Finding-2 regression for the upgrade reaper: a transient probe error must
// propagate from ForceKillSupervisor so the strict RunInstallUpgrade
// orchestrator escalates rather than treating an unprovable supervisor as
// already-reaped.
func TestV5UpgradeDeps_ForceKillSupervisor_TransientProbeError_Propagates(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	body := `{"pid":4242,"started_at":"` + liveSupervisorStartedAt + `"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))
	swapProcessLookupForTest(t, process.ProcessIdentity{}, errors.New("simulated transient WMI stall"))
	swapSupervisorReapInstallDirForTest(t, `C:\fixture-root\dev\.local\bin`)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	err := d.ForceKillSupervisor("")
	if err == nil {
		t.Fatal("transient probe error must PROPAGATE from ForceKillSupervisor (not silent success); got nil")
	}
	if len(*killed) != 0 {
		t.Fatalf("an unprovable PID must NOT be force-killed; killed=%v", *killed)
	}
}

// TestV5UpgradeDeps_ForceKillSupervisor_NotFoundIsBenign pins the Finding-2
// boundary: ErrProcessNotFound (PID PROVEN gone) is the one lookup-error class
// that maps to benign no-op, distinct from the transient propagation above.
func TestV5UpgradeDeps_ForceKillSupervisor_NotFoundIsBenign(t *testing.T) {
	tmpDir := t.TempDir()
	lockDir := filepath.Join(tmpDir, "supervisor.lock")
	ownerPath := lockDir + ".owner.json"

	body := `{"pid":4242,"started_at":"` + liveSupervisorStartedAt + `"}`
	writeStateSidecarBytes(t, ownerPath, []byte(body))
	swapProcessLookupForTest(t, process.ProcessIdentity{}, process.ErrProcessNotFound)
	swapSupervisorReapInstallDirForTest(t, `C:\fixture-root\dev\.local\bin`)
	killed := swapKillPIDViaTaskkillForTest(t)

	d := &v5UpgradeDeps{supervisorLockDir: lockDir}
	if err := d.ForceKillSupervisor(""); !isAlreadyExitedError(err) {
		t.Fatalf("ErrProcessNotFound outcome = %v, want typed already-exited outcome", err)
	}
	if len(*killed) != 0 {
		t.Fatalf("a gone PID must NOT be force-killed; killed=%v", *killed)
	}
}

// TestSupervisorCommandLineSubcommand pins the argv-token parser (Finding 3)
// across image-quoting and substring-trap cases.
func TestSupervisorCommandLineSubcommand(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    string
	}{
		{"unquoted image + supervise", `C:\bin\mcphub.exe supervise --strict-mode`, "supervise"},
		{"quoted image with spaces + supervise", `"C:\Program Files\mcphub\mcphub.exe" supervise`, "supervise"},
		{"gui subcommand", `mcphub.exe gui --no-browser`, "gui"},
		{"daemon subcommand", `mcphub.exe daemon --server time --daemon default`, "daemon"},
		{"supervise only in a flag value", `mcphub.exe gui --log-dir C:\supervise\logs`, "gui"},
		{"image only, no argv1", `C:\bin\mcphub.exe`, ""},
		{"empty", ``, ""},
		{"unterminated quote", `"C:\bin\mcphub.exe supervise`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := supervisorCommandLineSubcommand(tc.cmdline); got != tc.want {
				t.Fatalf("supervisorCommandLineSubcommand(%q) = %q; want %q", tc.cmdline, got, tc.want)
			}
		})
	}
}
