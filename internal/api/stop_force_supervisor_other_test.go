//go:build !windows

package api

// POSIX-only (Linux beta / macOS preview) coverage for the supervisor-owned
// force-stop path. On non-Windows the Windows-only port-owner lookup hook
// (lookupProcess, status_enrich.go) is structurally nil — processes.go init only
// wires it when GOOS=="windows". These tests pin the two sites where a
// SUCCESSFUL or already-confirmed-dead trusted PID/tree kill must report a
// SUCCESS row even though the platform cannot supply a Windows-only port-release
// proof (bot PR #288 r35-1, sites 1 + 2).
//
// Both tests exercise forceKillOneSupervisorTarget directly over its function-
// pointer seams (stopForceKillPIDFn, forceKillByPortFn, lookupProcess,
// processIdentityByPID) and a caller-supplied pidByTask map. Nothing touches the
// real state dir, scheduler, supervisor IPC, or any real process — there is no
// real kill, no schtasks, no IPC, and no port bind.

import (
	"errors"
	"testing"
	"time"
)

// TestForceKillOneSupervisorTarget_POSIXSuccessfulPIDKillReportsWarningNotError
// is the FIX 1 falsifying test. With a descriptor port set, lookupProcess nil
// (the POSIX reality), and the trusted PID kill SUCCEEDING (stopForceKillPIDFn
// returns nil), the per-target row must be a SUCCESS (empty Err) carrying a
// non-empty Warning, and forceKillByPortFn must NOT be consulted (the trusted
// kill already achieved the force-stop goal).
//
// Pre-fix this FAILS: waitPortReleasedAfterPIDKill returned an errPortKillUnsupported
// error when lookupProcess == nil, so the row carried a non-empty Err.
func TestForceKillOneSupervisorTarget_POSIXSuccessfulPIDKillReportsWarningNotError(t *testing.T) {
	const (
		taskName = `\mcp-local-hub-time-default`
		pid      = 7711
		port     = 9315
	)

	origPID := stopForceKillPIDFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	origIdentity := processIdentityByPID
	t.Cleanup(func() {
		stopForceKillPIDFn = origPID
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
		processIdentityByPID = origIdentity
	})

	// POSIX reality: no port-owner lookup hook.
	lookupProcess = nil

	var killedPIDs []int
	stopForceKillPIDFn = func(got int) error {
		killedPIDs = append(killedPIDs, got)
		return nil // trusted SIGKILL of the process group SUCCEEDED
	}
	// The success path must never consult the port kill after a trusted PID kill.
	forceKillByPortFn = func(p int, _ time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for port %d after a successful trusted PID kill", p)
		return portKillNoListener, nil
	}
	// The pre-kill identity gate (requireMcphubPIDImage) must pass so we reach the
	// trusted kill.
	processIdentityByPID = func(got int) (string, string, bool) {
		if got != pid {
			t.Fatalf("processIdentityByPID pid = %d, want %d", got, pid)
		}
		return mcphubProcessImageName, mcphubProcessImageName, true
	}

	d := SupervisorDaemon{TaskName: taskName, Server: "time", Daemon: "default", Port: port}
	pidByTask := map[string]int{"mcp-local-hub-time-default": pid}

	result := forceKillOneSupervisorTarget(d, pidByTask)

	if len(killedPIDs) != 1 || killedPIDs[0] != pid {
		t.Fatalf("trusted PID kills = %v, want exactly [%d]", killedPIDs, pid)
	}
	if result.Err != "" {
		t.Fatalf("result.Err = %q, want empty — a successful trusted PID kill IS the proof on a host with no port-owner lookup", result.Err)
	}
	if result.Warning == "" {
		t.Fatalf("result.Warning is empty, want a warning noting the port-release proof was unavailable but the trusted kill succeeded")
	}
}

// TestForceKillOneSupervisorTarget_POSIXAlreadyGonePIDKillReportsSuccess is the
// FIX 2 falsifying test. With a descriptor port set, lookupProcess nil, and the
// trusted PID kill reporting the process ALREADY GONE ("no such process"), the
// daemon is confirmed dead so the per-target row must be a clean SUCCESS (empty
// Err). forceKillByPortFn must NOT be consulted — falling through to it on POSIX
// is exactly the bug: killDaemonByPortOutcome returns portKillLookupUnavailable
// when lookupProcess == nil, which the caller turned into a "no usable
// port-release proof" FAILED row.
//
// Pre-fix this FAILS: the already-gone branch returned clean success only when
// d.Port == 0; with d.Port != 0 it fell through to the port path and carried the
// port-proof error.
func TestForceKillOneSupervisorTarget_POSIXAlreadyGonePIDKillReportsSuccess(t *testing.T) {
	const (
		taskName = `\mcp-local-hub-time-default`
		pid      = 7712
		port     = 9316
	)

	origPID := stopForceKillPIDFn
	origForceKill := forceKillByPortFn
	origLookup := lookupProcess
	origIdentity := processIdentityByPID
	t.Cleanup(func() {
		stopForceKillPIDFn = origPID
		forceKillByPortFn = origForceKill
		lookupProcess = origLookup
		processIdentityByPID = origIdentity
	})

	lookupProcess = nil

	stopForceKillPIDFn = func(int) error {
		// pidKillAlreadyGoneError(...) matches "no such process".
		return errors.New("no such process")
	}
	forceKillByPortFn = func(p int, _ time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for port %d after an already-gone PID kill; the daemon is confirmed dead", p)
		return portKillNoListener, nil
	}
	processIdentityByPID = func(got int) (string, string, bool) {
		if got != pid {
			t.Fatalf("processIdentityByPID pid = %d, want %d", got, pid)
		}
		return mcphubProcessImageName, mcphubProcessImageName, true
	}

	d := SupervisorDaemon{TaskName: taskName, Server: "time", Daemon: "default", Port: port}
	pidByTask := map[string]int{"mcp-local-hub-time-default": pid}

	result := forceKillOneSupervisorTarget(d, pidByTask)

	if result.Err != "" {
		t.Fatalf("result.Err = %q, want empty — an already-gone daemon with a port is confirmed dead, so the force-stop goal already holds", result.Err)
	}
}
