package cli

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

// normExeForTest mirrors the normalization daemonExpectedIdentityExe (and the
// process package's normalizeWindowsExecutablePath / normalizeExpectedExecutablePath)
// apply: filepath.Abs + EvalSymlinks + Clean. EvalSymlinks is best-effort (a
// non-existent path leaves the value unchanged), so the test computes the
// expected value the same way rather than hard-coding a platform path.
func normExeForTest(command string) string {
	exe := command
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Clean(exe)
}

// TestDaemonExpectedIdentityExe_UsesCommandNotSupervisorPath is the unit guard
// for the helper: a non-empty configured Command resolves to that command's
// normalized path (the daemon's install path), NOT the supervisor's own
// canonicalMcphubPath(). An empty Command falls back to canonicalMcphubPath()
// for defense-in-depth on legacy intent rows.
func TestDaemonExpectedIdentityExe_UsesCommandNotSupervisorPath(t *testing.T) {
	// A daemon install path deliberately different from this test binary's
	// os.Executable() (the "supervisor" path in production terms).
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	got := daemonExpectedIdentityExe(daemonCmd)
	want := normExeForTest(daemonCmd)
	if got != want {
		t.Fatalf("daemonExpectedIdentityExe(%q) = %q, want %q", daemonCmd, got, want)
	}
	if got == canonicalMcphubPath() {
		t.Fatalf("daemonExpectedIdentityExe returned the SUPERVISOR path %q; must use the daemon's configured command", got)
	}

	// Empty command falls back to the supervisor's own binary.
	if fb := daemonExpectedIdentityExe(""); fb != canonicalMcphubPath() {
		t.Fatalf("daemonExpectedIdentityExe(\"\") = %q, want canonicalMcphubPath() %q", fb, canonicalMcphubPath())
	}

	// A BARE command name (no directory) is resolved via PATH like exec.Command,
	// NOT filepath.Abs'd to <cwd>/<name>. A name not on PATH is unspawnable, so
	// it falls back to the supervisor's own binary (NOT a CWD-prefixed path).
	// This is the Codex bot #270 P2 regression guard.
	const bareNotOnPath = "mcphub-definitely-not-on-path-xyz123.exe"
	if got := daemonExpectedIdentityExe(bareNotOnPath); got != canonicalMcphubPath() {
		t.Fatalf("daemonExpectedIdentityExe(%q) = %q, want canonicalMcphubPath() %q (a bare name off PATH must fall back, not get CWD-prefixed)", bareNotOnPath, got, canonicalMcphubPath())
	}
}

// TestSupervisorDaemonEntryLive_IdentityUsesDaemonCommandNotSupervisorPath is
// the liveness-site guard (supervise_liveness.go). A daemon whose configured
// Command differs from the supervisor's os.Executable() must have its PID
// identity verified against the daemon's OWN command. The pre-fix code passed
// canonicalMcphubPath() (the supervisor's binary), which made
// process.VerifyPIDIdentity return ErrProcessIdentityMismatch for every live
// daemon when the supervisor ran from a different path — the fleet-wide false
// pid_identity_mismatch incident on 2026-06-09. Here the injected PIDIdentity
// probe asserts it received the DAEMON's command path and returns nil (verified),
// so the daemon is reported live with no mismatch.
func TestSupervisorDaemonEntryLive_IdentityUsesDaemonCommandNotSupervisorPath(t *testing.T) {
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	wantExe := normExeForTest(daemonCmd)
	if wantExe == canonicalMcphubPath() {
		t.Skip("test daemon command unexpectedly equals the supervisor path; mismatch is unobservable")
	}

	var sawExe string
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(proof process.PIDIdentityProof) error {
			sawExe = proof.ExecutablePath
			if proof.ExecutablePath != wantExe {
				t.Fatalf("PIDIdentity ExecutablePath = %q, want daemon command %q (NOT supervisor path %q)",
					proof.ExecutablePath, wantExe, canonicalMcphubPath())
			}
			return nil
		},
		// Port 0 short-circuits to live after the identity check; keeps the
		// test focused on the identity proof path.
	})
	defer restore()

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Command: daemonCmd, Port: 0}
	entry := DaemonRuntimeEntry{
		State:      daemonRuntimeStateRunning,
		CurrentPID: 22036,
		StartedAt:  time.Now().UTC().Add(-time.Minute),
	}
	live, reason := supervisorDaemonEntryLive(d, entry, time.Now().UTC())
	if !live {
		t.Fatalf("daemon with command != supervisor path reported not-live (reason %q); identity should verify against d.Command", reason)
	}
	if sawExe == "" {
		t.Fatal("PIDIdentity probe never ran; identity check was skipped")
	}
}

// TestSupervisorDaemonEntryLive_GenuineForeignExeStillMismatches is the
// negative control: when the live process is genuinely a DIFFERENT executable
// (the probe returns ErrProcessIdentityMismatch), the fix must NOT mask it —
// the reason is still pid_identity_mismatch. The fix only changes WHICH expected
// path is compared, not whether a real mismatch is honored.
func TestSupervisorDaemonEntryLive_GenuineForeignExeStillMismatches(t *testing.T) {
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(proof process.PIDIdentityProof) error {
			if proof.ExecutablePath != normExeForTest(daemonCmd) {
				t.Fatalf("PIDIdentity ExecutablePath = %q, want daemon command %q", proof.ExecutablePath, normExeForTest(daemonCmd))
			}
			// A real foreign process: identity does not match the expected exe.
			return process.ErrProcessIdentityMismatch
		},
	})
	defer restore()

	d := api.SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Command: daemonCmd, Port: 0}
	entry := DaemonRuntimeEntry{
		State:      daemonRuntimeStateRunning,
		CurrentPID: 22036,
		StartedAt:  time.Now().UTC().Add(-time.Minute),
	}
	live, reason := supervisorDaemonEntryLive(d, entry, time.Now().UTC())
	if live {
		t.Fatal("genuine foreign-exe process reported live; a real identity mismatch must not be masked")
	}
	if reason != supervisorLivenessReasonPIDIdentityMismatch {
		t.Fatalf("reason = %q, want %q", reason, supervisorLivenessReasonPIDIdentityMismatch)
	}
}

// TestProductionTerminateFn_IdentityProofUsesDaemonCommand is the terminate-site
// guard (supervise.go makeProductionTerminateFnWithStatePath). The terminate
// path verifies (and then kills) the target PID against an identity proof; that
// proof's ExecutablePath must be the DAEMON's configured Command, not the
// supervisor's canonicalMcphubPath(). Pre-fix, a dev-build supervisor could not
// VERIFY — and therefore could not terminate — release-path daemons, which
// worsened the orphan/port-fight. Here the injected verify fn asserts it received
// the daemon command and the terminate fn returns already-exited to short-circuit
// (no real kill needed); the absence of a mismatch-refusal proves the fix.
func TestProductionTerminateFn_IdentityProofUsesDaemonCommand(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()
	statePath := filepath.Join(tmpHome, "supervisor-state.json")

	const taskName = `\mcp-local-hub-memory-default`
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	wantExe := normExeForTest(daemonCmd)
	if wantExe == canonicalMcphubPath() {
		t.Skip("test daemon command unexpectedly equals the supervisor path; mismatch is unobservable")
	}

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(taskName, 4242, time.Unix(1700000000, 0).UTC())

	prevQuery := productionQueryPIDStateFn
	prevVerify := productionVerifyPIDIdentityFn
	prevTerminate := productionTerminatePIDWithIdentityFn
	productionQueryPIDStateFn = func(pid int) (process.PIDState, error) {
		return process.PIDStateAlive, nil
	}
	var sawVerifyExe, sawTerminateExe string
	productionVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		sawVerifyExe = proof.ExecutablePath
		if proof.ExecutablePath != wantExe {
			t.Fatalf("VerifyPIDIdentity ExecutablePath = %q, want daemon command %q (NOT supervisor path %q)",
				proof.ExecutablePath, wantExe, canonicalMcphubPath())
		}
		return nil
	}
	productionTerminatePIDWithIdentityFn = func(proof process.PIDIdentityProof) error {
		sawTerminateExe = proof.ExecutablePath
		// Short-circuit the kill cleanly: already-exited returns success
		// without invoking finishProductionTerminate.
		return process.ErrProcessAlreadyExited
	}
	t.Cleanup(func() {
		productionQueryPIDStateFn = prevQuery
		productionVerifyPIDIdentityFn = prevVerify
		productionTerminatePIDWithIdentityFn = prevTerminate
	})

	terminateFn := makeProductionTerminateFnWithStatePath(events, map[string]runningProcessIdentity{
		taskName: {
			PID:           4242,
			PIDGeneration: 1,
			StartedAt:     time.Unix(1700000000, 0).UTC().Format(time.RFC3339Nano),
		},
	}, tracker, statePath)

	// d.Command carries the daemon's install path — the closure must thread it
	// into the identity proof.
	if err := terminateFn(api.SupervisorDaemon{TaskName: taskName, Command: daemonCmd}); err != nil {
		t.Fatalf("terminate fn returned error (false identity mismatch-refusal?): %v", err)
	}
	if sawVerifyExe == "" {
		t.Fatal("verify identity fn never ran")
	}
	if sawTerminateExe != wantExe {
		t.Fatalf("terminate identity ExecutablePath = %q, want daemon command %q", sawTerminateExe, wantExe)
	}
}

// TestLoadSupervisorCurrentRunning_IdentityUsesIntentCommandNotSupervisorPath is
// the startup-site guard (supervise.go loadSupervisorCurrentRunning). The
// warm-restart scan validates each recorded running PID's identity. The expected
// exe must come from the daemon's configured Command in supervisor-intent.json
// (the daemon's install path), NOT the supervisor's canonicalMcphubPath().
// Pre-fix, a supervisor running from a different binary marked every live daemon
// stale on a spurious identity mismatch, dropping it from currentRunning so the
// startup reconcile spawned a port-fighting duplicate. Here the injected verify
// fn asserts it received the intent Command and returns nil; the daemon stays
// current-running.
func TestLoadSupervisorCurrentRunning_IdentityUsesIntentCommandNotSupervisorPath(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const taskName = `\mcp-local-hub-memory-default`
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	wantExe := normExeForTest(daemonCmd)
	if wantExe == canonicalMcphubPath() {
		t.Skip("test daemon command unexpectedly equals the supervisor path; mismatch is unobservable")
	}

	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Command:  daemonCmd,
			Port:     0, // 0 skips the port-liveness stage; isolate the identity proof
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	var sawExe string
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		sawExe = proof.ExecutablePath
		if proof.ExecutablePath != wantExe {
			t.Fatalf("currentRunning VerifyPIDIdentity ExecutablePath = %q, want intent command %q (NOT supervisor path %q)",
				proof.ExecutablePath, wantExe, canonicalMcphubPath())
		}
		return nil
	}
	currentRunningIsPIDAliveFn = func(pid int) bool { return pid == 22036 }
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	got, gotPIDs, err := loadSupervisorCurrentRunning(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if sawExe == "" {
		t.Fatal("identity verify never ran; the running row was filtered before the identity check")
	}
	if !got[taskName] {
		t.Fatalf("daemon with intent command != supervisor path was dropped from currentRunning (false stale): %v", got)
	}
	if gotPIDs[taskName].PID != 22036 {
		t.Fatalf("running PID snapshot = %+v, want live pid 22036", gotPIDs[taskName])
	}

	// The state row must NOT have been rewritten to idle (no spurious stale clear).
	after, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		t.Fatalf("read supervisor-state.json: %v", err)
	}
	row := after.Daemons[taskName]
	if row.State != "running" || row.CurrentPID != 22036 {
		t.Fatalf("live daemon row was cleared to idle on a spurious identity mismatch: %+v", row)
	}
}

// TestLoadSupervisorCurrentRunning_PortBearingInnerRecheckUsesIntentCommand
// guards the INNER port-liveness re-check inside loadSupervisorCurrentRunning
// (the `if port > 0` block). That re-check calls supervisorDaemonEntryLive with
// a synthesized descriptor; its PID-identity probe must also compare against the
// daemon's configured Command, not canonicalMcphubPath(). Without threading the
// Command into the synthetic descriptor, a port-bearing daemon whose install
// path differs from the supervisor binary would false-mismatch here (reason
// pid_identity_mismatch, NOT a live-PID reason) and be cleared as stale —
// re-introducing the 2026-06-09 bug on the startup path. Here both the outer
// (currentRunningVerifyPIDIdentityFn) and inner (liveness probe PIDIdentity)
// identity checks assert they received the daemon command and pass; the daemon
// stays current-running.
func TestLoadSupervisorCurrentRunning_PortBearingInnerRecheckUsesIntentCommand(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const taskName = `\mcp-local-hub-memory-default`
	const daemonCmd = `C:\Users\someone\.local\bin\mcphub.exe`
	wantExe := normExeForTest(daemonCmd)
	if wantExe == canonicalMcphubPath() {
		t.Skip("test daemon command unexpectedly equals the supervisor path; mismatch is unobservable")
	}

	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Command:  daemonCmd,
			Port:     9123, // non-zero → exercises the inner port-liveness re-check
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		if proof.ExecutablePath != wantExe {
			t.Fatalf("outer VerifyPIDIdentity ExecutablePath = %q, want daemon command %q", proof.ExecutablePath, wantExe)
		}
		return nil
	}
	currentRunningIsPIDAliveFn = func(pid int) bool { return pid == 22036 }

	// Inner port-liveness re-check probe: assert the identity proof carries the
	// daemon command and the PID owns its port (live).
	var sawInnerExe string
	restoreProbe := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{
		PIDAlive: func(pid int) bool { return pid == 22036 },
		PIDIdentity: func(proof process.PIDIdentityProof) error {
			sawInnerExe = proof.ExecutablePath
			if proof.ExecutablePath != wantExe {
				t.Fatalf("inner re-check PIDIdentity ExecutablePath = %q, want daemon command %q (NOT supervisor path %q)",
					proof.ExecutablePath, wantExe, canonicalMcphubPath())
			}
			return nil
		},
		PortOwnerPID: func(port int) (int, bool, error) {
			if port != 9123 {
				t.Fatalf("PortOwnerPID port = %d, want 9123", port)
			}
			return 22036, true, nil // the tracked PID owns its port → live
		},
	})
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
		restoreProbe()
	})

	got, _, err := loadSupervisorCurrentRunning(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if sawInnerExe == "" {
		t.Fatal("inner port-liveness re-check identity probe never ran")
	}
	if !got[taskName] {
		t.Fatalf("port-bearing daemon (command != supervisor path) dropped from currentRunning on a spurious inner mismatch: %v", got)
	}
	after, err := api.ReadSupervisorState(filepath.Join(stateDir, "supervisor-state.json"))
	if err != nil {
		t.Fatalf("read supervisor-state.json: %v", err)
	}
	if row := after.Daemons[taskName]; row.State != "running" || row.CurrentPID != 22036 {
		t.Fatalf("live port-bearing daemon row cleared to idle on inner spurious mismatch: %+v", row)
	}
}

// TestLoadSupervisorCurrentRunning_EmptyIntentCommandFallsBackToSupervisorPath
// guards the defense-in-depth fallback: a legacy intent row whose Command is
// empty (pre-Command supervisor-intent.json) falls back to canonicalMcphubPath()
// via daemonExpectedIdentityExe(""), preserving the prior behavior for such rows.
func TestLoadSupervisorCurrentRunning_EmptyIntentCommandFallsBackToSupervisorPath(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	const taskName = `\mcp-local-hub-memory-default`

	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			// Command intentionally empty (legacy row).
			Port: 0,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	startedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := api.WriteSupervisorState(filepath.Join(stateDir, "supervisor-state.json"), &api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {
				State:         "running",
				CurrentPID:    22036,
				PIDGeneration: 7,
				StartedAt:     startedAt,
			},
		},
	}); err != nil {
		t.Fatalf("seed supervisor-state.json: %v", err)
	}

	prevVerify := currentRunningVerifyPIDIdentityFn
	prevAlive := currentRunningIsPIDAliveFn
	var sawExe string
	currentRunningVerifyPIDIdentityFn = func(proof process.PIDIdentityProof) error {
		sawExe = proof.ExecutablePath
		if proof.ExecutablePath != canonicalMcphubPath() {
			t.Fatalf("empty intent command: ExecutablePath = %q, want canonicalMcphubPath() fallback %q",
				proof.ExecutablePath, canonicalMcphubPath())
		}
		return process.ErrProcessIdentityUnsupported
	}
	currentRunningIsPIDAliveFn = func(pid int) bool { return pid == 22036 }
	t.Cleanup(func() {
		currentRunningVerifyPIDIdentityFn = prevVerify
		currentRunningIsPIDAliveFn = prevAlive
	})

	got, _, err := loadSupervisorCurrentRunning(stateDir)
	if err != nil {
		t.Fatalf("loadSupervisorCurrentRunning: %v", err)
	}
	if sawExe == "" {
		t.Fatal("identity verify never ran")
	}
	if !got[taskName] {
		t.Fatalf("legacy empty-command daemon (alive, unsupported identity) was dropped: %v", got)
	}
}
