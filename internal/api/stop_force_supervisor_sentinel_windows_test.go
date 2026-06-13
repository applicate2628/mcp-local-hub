//go:build windows

// stop_force_supervisor_sentinel_windows_test.go — Windows-only falsifying
// regressions for pr301 r5 Finding 2 (the `mcphub stop --force` owner-SID arm
// must treat the ErrProcessAlreadyExited sentinel as an already-gone SUCCESS,
// not an unverifiable-owner FAILURE).
//
// The owner-SID arm (processOwnerSIDMatchesCurrentFn) can return the canonical
// process.ErrProcessAlreadyExited sentinel when the target PID exits between the
// caller's image/start-time identity probe and the gate's OpenProcess (a TOCTOU
// window). A gone target is the force-stop SUCCESS condition; the prior code
// classified the sentinel as a fail-closed unverifiable-owner error, reporting a
// FAILED row for an already-dead daemon. These tests pin both stop-force sites:
//   - forceKillOneSupervisorTarget (PID-direct path, especially a PORTLESS
//     descriptor with no port fallback),
//   - killDaemonByPortOutcome (port-owner taskkill path).
// Each test also pins that a LIVE-unverifiable (non-sentinel) error STILL fails
// closed, so the sentinel handling does not weaken the SEC-F3 guarantee.

package api

import (
	"errors"
	"testing"
	"time"

	"mcp-local-hub/internal/process"
)

// TestForceKillOneSupervisorTarget_OwnerSIDSentinel_PortlessReportsSuccess is the
// FALSIFYING CORE of Finding 2 for the worst case: a PORTLESS supervisor-owned
// descriptor (d.Port == 0) whose PID vanished mid-gate. The owner-SID arm returns
// ErrProcessAlreadyExited; forceKillOneSupervisorTarget must report a clean
// SUCCESS row (empty Err) and must NOT touch the PID-kill primitive (the target
// is already gone). Pre-fix the sentinel fell through to the fail-closed
// pidKillErr path, and with no port fallback the row was a FAILED stop for an
// already-dead daemon.
func TestForceKillOneSupervisorTarget_OwnerSIDSentinel_PortlessReportsSuccess(t *testing.T) {
	const (
		taskName = `\mcp-local-hub-memory-default`
		pid      = 51010
	)

	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	origPIDKill := stopForceKillPIDFn
	origForcePort := forceKillByPortFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
		stopForceKillPIDFn = origPIDKill
		forceKillByPortFn = origForcePort
	})

	// Image gate passes so the owner-SID arm is reached.
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}
	// Owner-SID arm: target vanished mid-gate → the already-exited sentinel.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, process.ErrProcessAlreadyExited
	}
	// The PID-kill primitive must NEVER run: the target is already gone.
	stopForceKillPIDFn = func(int) error {
		t.Fatal("stopForceKillPIDFn called for an already-gone PID; the owner-SID sentinel " +
			"means the target is dead — there is nothing to kill")
		return nil
	}
	// No port path exists for a portless descriptor; guard anyway.
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
		t.Fatal("forceKillByPortFn called for a portless already-gone descriptor")
		return portKillNoListener, nil
	}

	d := SupervisorDaemon{TaskName: taskName, Server: "memory", Daemon: "default"} // Port == 0
	result := forceKillOneSupervisorTarget(d, map[string]int{"mcp-local-hub-memory-default": pid})

	if result.Err != "" {
		t.Fatalf("pr301 r5 Finding 2 regression: an already-gone PORTLESS descriptor must "+
			"report a clean SUCCESS row; got Err=%q (the pre-fix fail-closed path that reports "+
			"a FAILED stop for an already-dead daemon)", result.Err)
	}
}

// TestForceKillOneSupervisorTarget_OwnerSIDLiveUnverifiable_StillFailsClosed is
// the SECURITY-PRESERVATION control. A LIVE process whose owner cannot be
// verified (a NON-sentinel error, e.g. ACCESS_DENIED) must STILL fail closed —
// the row carries an Err and no kill is attempted. The sentinel handling must
// not relax a genuinely unverifiable live target.
func TestForceKillOneSupervisorTarget_OwnerSIDLiveUnverifiable_StillFailsClosed(t *testing.T) {
	const (
		taskName = `\mcp-local-hub-memory-default`
		pid      = 51011
	)

	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	origPIDKill := stopForceKillPIDFn
	origForcePort := forceKillByPortFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
		stopForceKillPIDFn = origPIDKill
		forceKillByPortFn = origForcePort
	})

	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}
	// LIVE but unverifiable owner: a non-sentinel error → fail closed.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	stopForceKillPIDFn = func(int) error {
		t.Fatal("stopForceKillPIDFn called despite an unverifiable LIVE owner; SEC-F3 requires " +
			"refusing the kill")
		return nil
	}
	forceKillByPortFn = func(int, time.Duration) (portKillOutcome, error) {
		t.Fatal("forceKillByPortFn called for a portless unverifiable-owner descriptor")
		return portKillNoListener, nil
	}

	d := SupervisorDaemon{TaskName: taskName, Server: "memory", Daemon: "default"} // Port == 0
	result := forceKillOneSupervisorTarget(d, map[string]int{"mcp-local-hub-memory-default": pid})

	if result.Err == "" {
		t.Fatal("SEC-F3 regression: a LIVE process whose owner cannot be verified must FAIL " +
			"CLOSED (non-empty Err); got a success row — the sentinel handling must not relax a " +
			"genuinely unverifiable live target")
	}
}

// TestKillDaemonByPortOutcome_OwnerSIDSentinel_ReportsNoListener pins Finding 2
// for the port-owner taskkill path. The PID owning the port vanished between
// lookupProcess and the owner-SID gate; killDaemonByPortOutcome must return the
// benign portKillNoListener outcome with NO error (the listener is gone),
// instead of the pre-fix portKillIdentityMismatch FAILURE.
func TestKillDaemonByPortOutcome_OwnerSIDSentinel_ReportsNoListener(t *testing.T) {
	const (
		port = 33992
		pid  = 51012
	)

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	// A listener exists at probe time.
	lookupProcess = func(int) (int, uint64, int64, bool) { return pid, 0, 0, true }
	// Image gate passes so the owner-SID arm is reached.
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}
	// Owner-SID arm: the port owner vanished mid-gate → the already-exited sentinel.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, process.ErrProcessAlreadyExited
	}
	taskkillProcessTreeByPIDFn = func(int) error {
		t.Fatal("taskkillProcessTreeByPIDFn called for an already-gone port owner; there is " +
			"nothing to kill")
		return nil
	}

	outcome, err := killDaemonByPortOutcome(port, 1*time.Second)
	if err != nil {
		t.Fatalf("pr301 r5 Finding 2 regression: an already-gone port owner must yield a benign "+
			"no-error outcome; got err=%v", err)
	}
	if outcome != portKillNoListener {
		t.Fatalf("an already-gone port owner must report portKillNoListener (the listener is "+
			"gone); got outcome=%v", outcome)
	}
}

// TestKillDaemonByPortOutcome_OwnerSIDLiveUnverifiable_StillFailsClosed is the
// port-path SECURITY-PRESERVATION control: a LIVE unverifiable port owner (a
// non-sentinel error) must STILL fail closed — portKillIdentityMismatch with a
// non-nil error, and no taskkill.
func TestKillDaemonByPortOutcome_OwnerSIDLiveUnverifiable_StillFailsClosed(t *testing.T) {
	const (
		port = 33993
		pid  = 51013
	)

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	lookupProcess = func(int) (int, uint64, int64, bool) { return pid, 0, 0, true }
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	taskkillProcessTreeByPIDFn = func(int) error {
		t.Fatal("taskkillProcessTreeByPIDFn called despite an unverifiable LIVE port owner; " +
			"SEC-F3 requires refusing the kill")
		return nil
	}

	outcome, err := killDaemonByPortOutcome(port, 1*time.Second)
	if err == nil {
		t.Fatal("SEC-F3 regression: a LIVE unverifiable port owner must FAIL CLOSED (non-nil " +
			"error); got nil")
	}
	if outcome != portKillIdentityMismatch {
		t.Fatalf("a LIVE unverifiable port owner must report portKillIdentityMismatch; got %v", outcome)
	}
}
