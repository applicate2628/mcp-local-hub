//go:build !windows

package api

// POSIX-only coverage for the supervisor-owned force-stop path. On non-Windows
// the Windows-only port-owner lookup hook is structurally nil. These tests pin
// the fail-closed behavior: a descriptor port must not be reported as stopped
// after a PID kill unless the port is proven released or safely classified.
//
// Both tests exercise forceKillOneSupervisorTarget directly over its function-
// pointer seams (stopForceKillPIDFn, forceKillByPortFn, lookupProcess,
// processIdentityByPID) and a caller-supplied pidByTask map. Nothing touches the
// real state dir, scheduler, supervisor IPC, or any real process — there is no
// real kill, no schtasks, no IPC, and no port bind.

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestForceKillOneSupervisorTarget_POSIXSuccessfulPIDKillRequiresPortProof
// verifies that a successful PID kill with a descriptor port still requires
// release proof when lookupProcess is unavailable on POSIX. The port-kill seam
// must not be used after a PID kill because the port could have been rebound by
// a foreign process; instead the result must fail closed.
func TestForceKillOneSupervisorTarget_POSIXSuccessfulPIDKillRequiresPortProof(t *testing.T) {
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
	if result.Err == "" {
		t.Fatal("result.Err is empty, want failure when POSIX port-release proof is unavailable after PID kill")
	}
	if result.Warning != "" {
		t.Fatalf("result.Warning = %q, want empty warning on failed unverified stop", result.Warning)
	}
}

// TestForceKillOneSupervisorTarget_POSIXAlreadyGonePIDKillRequiresPortProof
// verifies that an already-gone PID with a descriptor port falls through to the
// port classifier. On POSIX, an unavailable port classifier must surface a
// failed row instead of masking a potentially orphaned listener.
func TestForceKillOneSupervisorTarget_POSIXAlreadyGonePIDKillRequiresPortProof(t *testing.T) {
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
		if p != port {
			t.Fatalf("forceKillByPortFn port = %d, want %d", p, port)
		}
		return portKillLookupUnavailable, nil
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

	if result.Err == "" {
		t.Fatal("result.Err is empty, want failure when an already-gone PID has no port-release proof")
	}
	if !strings.Contains(result.Err, "no usable port-release proof") {
		t.Fatalf("result.Err = %q, want no usable port-release proof", result.Err)
	}
}
