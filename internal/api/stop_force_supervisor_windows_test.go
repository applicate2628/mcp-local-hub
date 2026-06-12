//go:build windows

package api

import (
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
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		forceKillByPortFn = origForcePort
		stopForceKillPIDFn = origPIDKill
		lookupProcess = origLookup
		taskkillProcessTreeByPIDFn = origTaskkill
	})

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
