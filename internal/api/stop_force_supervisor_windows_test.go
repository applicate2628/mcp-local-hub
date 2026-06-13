//go:build windows

package api

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStopForceSupervisorPIDPathUsesTaskkillTreePrimitive(t *testing.T) {
	const taskName = `\mcp-local-hub-laneatest-default`
	const pid = 62301
	const port = 33008

	origIdent := processIdentityByPID
	origForcePort := forceKillByPortFn
	origPIDKill := stopForceKillPIDFn
	origLookup := lookupProcess
	origTaskkill := taskkillProcessTreeByPIDFn
	origSID := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		forceKillByPortFn = origForcePort
		stopForceKillPIDFn = origPIDKill
		lookupProcess = origLookup
		taskkillProcessTreeByPIDFn = origTaskkill
		processOwnerSIDMatchesCurrentFn = origSID
	})

	// SEC-F3 Gate: same-user owner → pass, without opening a real token for the
	// fake PID. The owner-SID arm is exercised in its own dedicated test below.
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }

	processIdentityByPID = func(gotPID int) (string, string, bool) {
		if gotPID != pid {
			t.Fatalf("processIdentityByPID pid = %d, want %d", gotPID, pid)
		}
		return "mcphub.exe", "svchost.exe", true
	}
	forceKillByPortFn = func(gotPort int, timeout time.Duration) (portKillOutcome, error) {
		t.Fatalf("forceKillByPortFn called for port %d after successful PID tree kill", gotPort)
		return portKillNoListener, nil
	}
	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		if gotPort != port {
			t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
		}
		return 0, 0, 0, false
	}
	var treeKilled []int
	taskkillProcessTreeByPIDFn = func(gotPID int) error {
		treeKilled = append(treeKilled, gotPID)
		return nil
	}
	stopForceKillPIDFn = stopForceKillSupervisorPIDTree

	result := forceKillOneSupervisorTarget(
		SupervisorDaemon{TaskName: taskName, Port: port},
		map[string]int{strings.TrimPrefix(taskName, `\`): pid},
	)
	if result.Err != "" {
		t.Fatalf("forceKillOneSupervisorTarget error = %q, want success", result.Err)
	}
	if len(treeKilled) != 1 || treeKilled[0] != pid {
		t.Fatalf("taskkill tree PIDs = %v, want [%d]", treeKilled, pid)
	}
}

// TestRequireMcphubPIDImage_OwnerSIDArm is the SEC-F3 falsifying test for the
// `mcphub stop --force` PID-direct kill gate. With image-identity already
// verified (processIdentityByPID returns mcphub.exe), the owner-SID arm is the
// only remaining decision:
//
//   - same-SID    → requireMcphubPIDImage returns nil (kill may proceed).
//   - different   → requireMcphubPIDImage returns an error (kill REFUSED) —
//     pre-fix this returned nil and authorized killing another
//     user's mcphub.exe.
//   - unverifiable → requireMcphubPIDImage returns an error (fail closed).
func TestRequireMcphubPIDImage_OwnerSIDArm(t *testing.T) {
	const pid = 51001

	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
	})
	// Image gate passes so the SID arm is reached.
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	if err := requireMcphubPIDImage(pid); err != nil {
		t.Fatalf("same-owner SID must pass requireMcphubPIDImage; got %v", err)
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return false, nil }
	if err := requireMcphubPIDImage(pid); err == nil {
		t.Fatal("different-owner SID must FAIL requireMcphubPIDImage (refuse the kill); got nil")
	} else if !strings.Contains(err.Error(), "owner-SID gate") {
		t.Fatalf("error must name the owner-SID gate; got %v", err)
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	if err := requireMcphubPIDImage(pid); err == nil {
		t.Fatal("unverifiable owner SID must FAIL requireMcphubPIDImage (fail closed); got nil")
	} else if !strings.Contains(err.Error(), "unverifiable") {
		t.Fatalf("error must name unverifiable ownership; got %v", err)
	}
}

// TestRequireMcphubPortOwnerPID_OwnerSIDArm is the SEC-F3 falsifying test for
// the port-owner taskkill gate (killDaemonByPortOutcome → requireMcphubPortOwnerPID
// → taskkill). Same tri-state contract as the PID-direct gate.
func TestRequireMcphubPortOwnerPID_OwnerSIDArm(t *testing.T) {
	const (
		port = 33991
		pid  = 51002
	)

	origIdent := processIdentityByPID
	origSID := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		processOwnerSIDMatchesCurrentFn = origSID
	})
	processIdentityByPID = func(int) (string, string, bool) {
		return mcphubProcessImageName, "svchost.exe", true
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
	if err := requireMcphubPortOwnerPID(port, pid); err != nil {
		t.Fatalf("same-owner SID must pass requireMcphubPortOwnerPID; got %v", err)
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return false, nil }
	if err := requireMcphubPortOwnerPID(port, pid); err == nil {
		t.Fatal("different-owner SID must FAIL requireMcphubPortOwnerPID (refuse the kill); got nil")
	} else if !strings.Contains(err.Error(), "owner-SID gate") {
		t.Fatalf("error must name the owner-SID gate; got %v", err)
	}

	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) {
		return false, errors.New("OpenProcessToken: access denied")
	}
	if err := requireMcphubPortOwnerPID(port, pid); err == nil {
		t.Fatal("unverifiable owner SID must FAIL requireMcphubPortOwnerPID (fail closed); got nil")
	}
}
