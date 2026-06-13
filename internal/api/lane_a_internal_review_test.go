package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

func setupLaneAState(t *testing.T) string {
	t.Helper()
	stateDir := apitest.HardenedTempDir(t)
	restoreState := SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restoreState)
	t.Setenv("LOCALAPPDATA", stateDir)
	t.Setenv("XDG_STATE_HOME", stateDir)
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed empty supervisor-intent.json: %v", err)
	}
	return stateDir
}

func TestRestartAllSkipsHubInfrastructureSchedulerTasks(t *testing.T) {
	setupLaneAState(t)

	const daemonTask = `\mcp-local-hub-laneatest-default`
	fake := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: daemonTask},
		{Name: `\mcp-local-hub-supervisor`},
		{Name: `\mcp-local-hub-liveness`},
		{Name: `\mcp-local-hub-workspace-weekly-refresh`},
	}}

	origFactory := restartSchedulerFactory
	restartSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	t.Cleanup(func() { restartSchedulerFactory = origFactory })

	results, err := NewAPI().RestartAll()
	if err != nil {
		t.Fatalf("RestartAll: %v", err)
	}
	if len(fake.stopNames) != 1 || fake.stopNames[0] != daemonTask {
		t.Fatalf("scheduler Stop calls = %v, want only [%s]", fake.stopNames, daemonTask)
	}
	if len(fake.runNames) != 1 || fake.runNames[0] != daemonTask {
		t.Fatalf("scheduler Run calls = %v, want only [%s]", fake.runNames, daemonTask)
	}
	if len(results) != 1 || results[0].TaskName != daemonTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one daemon success row", results)
	}
}

func TestStopAllSkipsHubInfrastructureSchedulerTasks(t *testing.T) {
	setupLaneAState(t)
	installRecordingAudit(t, &recordingAuditWriter{})

	const daemonTask = `\mcp-local-hub-laneatest-default`
	fake := &restartAllFakeScheduler{tasks: []scheduler.TaskStatus{
		{Name: daemonTask},
		{Name: `\mcp-local-hub-supervisor`},
		{Name: `\mcp-local-hub-liveness`},
		{Name: `\mcp-local-hub-workspace-weekly-refresh`},
	}}

	origFactory := stopSchedulerFactory
	stopSchedulerFactory = func() (scheduler.Scheduler, error) { return fake, nil }
	t.Cleanup(func() { stopSchedulerFactory = origFactory })

	var killPorts []int
	origKill := killByPortFn
	killByPortFn = func(port int, timeout time.Duration) error {
		killPorts = append(killPorts, port)
		return nil
	}
	t.Cleanup(func() { killByPortFn = origKill })

	results, err := NewAPI().StopAll()
	if err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	if len(killPorts) != 1 {
		t.Fatalf("killByPortFn calls = %d ports=%v, want one daemon kill only", len(killPorts), killPorts)
	}
	if len(fake.stopNames) != 1 || fake.stopNames[0] != daemonTask {
		t.Fatalf("scheduler Stop calls = %v, want only [%s]", fake.stopNames, daemonTask)
	}
	if len(results) != 1 || results[0].TaskName != daemonTask || results[0].Err != "" {
		t.Fatalf("results = %+v, want one daemon success row", results)
	}
}

func TestPreflight_AllowsSupervisorIntentNativeHTTPInternalPortAndRejectsRowlessPort(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)

	const (
		externalPort = 33001
		internalPort = externalPort + config.NativeHTTPInternalPortOffset
		internalPID  = 61001
		daemonPID    = 61002
	)
	const taskName = `\mcp-local-hub-lanehttp-alpha`
	m := &config.ServerManifest{
		Name:      "lanehttp",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: externalPort}},
	}

	origPortInUse := preflightPortInUse
	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSched := schedulerStatusForOwnPort
	origStatus := supervisorIPCStatusFn
	t.Cleanup(func() {
		preflightPortInUse = origPortInUse
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		schedulerStatusForOwnPort = origSched
		supervisorIPCStatusFn = origStatus
	})

	preflightPortInUse = func(port int) bool { return port == internalPort }
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port == internalPort {
			return internalPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == internalPID {
			return "python.exe", "mcphub.exe", true
		}
		return "", "", false
	}
	schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
		return scheduler.TaskStatus{}, scheduler.ErrTaskNotFound
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: daemonPID, State: "Running"}}, nil
	}

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "lanehttp",
			Daemon:   "alpha",
			Command:  "go",
			Port:     externalPort,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}
	if err := Preflight(m, "alpha"); err != nil {
		t.Fatalf("Preflight should accept native-http internal port from matching supervisor intent row: %v", err)
	}

	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("clear supervisor intent: %v", err)
	}
	err := Preflight(m, "alpha")
	if err == nil {
		t.Fatal("Preflight accepted a rowless native-http internal port; want collision")
	}
	if !strings.Contains(err.Error(), "internal port 43001 already in use") {
		t.Fatalf("Preflight error = %v, want rowless internal-port collision", err)
	}
}

func TestStopForceUsesSupervisorPIDThenWaitsOnDescriptorPort(t *testing.T) {
	const taskName = `\mcp-local-hub-laneatest-default`

	origIdent := processIdentityByPID
	origForcePort := forceKillByPortFn
	origPIDKill := stopForceKillPIDFn
	origLookup := lookupProcess
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		forceKillByPortFn = origForcePort
		stopForceKillPIDFn = origPIDKill
		lookupProcess = origLookup
	})

	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == 62001 {
			return "mcphub.exe", "svchost.exe", true
		}
		return "", "", false
	}
	var portKills []int
	forceKillByPortFn = func(port int, timeout time.Duration) (portKillOutcome, error) {
		portKills = append(portKills, port)
		t.Fatalf("forceKillByPortFn called for port %d after successful PID kill", port)
		return portKillNoListener, nil
	}
	var portLookups int
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		portLookups++
		if port != 33002 {
			t.Fatalf("lookupProcess port = %d, want 33002", port)
		}
		return 0, 0, 0, false
	}
	var pidKills []int
	stopForceKillPIDFn = func(pid int) error {
		pidKills = append(pidKills, pid)
		return nil
	}

	result := forceKillOneSupervisorTarget(
		SupervisorDaemon{TaskName: taskName, Port: 33002},
		map[string]int{strings.TrimPrefix(taskName, `\`): 62001},
	)
	if result.Err != "" {
		t.Fatalf("forceKillOneSupervisorTarget error = %q, want success", result.Err)
	}
	if len(pidKills) != 1 || pidKills[0] != 62001 {
		t.Fatalf("PID kills = %v, want [62001]", pidKills)
	}
	if portLookups == 0 {
		t.Fatal("lookupProcess was not consulted to wait for descriptor-port release after PID kill")
	}
	if len(portKills) != 0 {
		t.Fatalf("port kills = %v, want none after successful PID kill", portKills)
	}
}

func TestStopForcePIDIdentityRefusalFallsThroughToPortClassifier(t *testing.T) {
	const taskName = `\mcp-local-hub-laneatest-default`
	const bareTask = "mcp-local-hub-laneatest-default"
	const stalePID = 62101

	t.Run("port unbound returns already-not-running success", func(t *testing.T) {
		const port = 33005
		origLookup := lookupProcess
		origIdent := processIdentityByPID
		origForcePort := forceKillByPortFn
		origPIDKill := stopForceKillPIDFn
		t.Cleanup(func() {
			lookupProcess = origLookup
			processIdentityByPID = origIdent
			forceKillByPortFn = origForcePort
			stopForceKillPIDFn = origPIDKill
		})

		forceKillByPortFn = killDaemonByPortOutcome
		processIdentityByPID = func(pid int) (string, string, bool) {
			if pid != stalePID {
				t.Fatalf("processIdentityByPID pid = %d, want stale PID %d", pid, stalePID)
			}
			return "node.exe", "explorer.exe", true
		}
		var portLookups int
		lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
			portLookups++
			if gotPort != port {
				t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
			}
			return 0, 0, 0, false
		}
		stopForceKillPIDFn = func(pid int) error {
			t.Fatalf("stopForceKillPIDFn called for refused foreign PID %d", pid)
			return nil
		}

		result := forceKillOneSupervisorTarget(
			SupervisorDaemon{TaskName: taskName, Port: port},
			map[string]int{bareTask: stalePID},
		)
		if result.Err != "" {
			t.Fatalf("forceKillOneSupervisorTarget error = %q, want success after port classifier reports no listener", result.Err)
		}
		if portLookups == 0 {
			t.Fatal("port classifier was not consulted after PID identity refusal")
		}
	})

	t.Run("port rebound by mcphub is killed by port path", func(t *testing.T) {
		const port = 33006
		const portOwnerPID = 62102
		origLookup := lookupProcess
		origIdent := processIdentityByPID
		origForcePort := forceKillByPortFn
		origTaskkill := taskkillProcessTreeByPIDFn
		origPIDKill := stopForceKillPIDFn
		t.Cleanup(func() {
			lookupProcess = origLookup
			processIdentityByPID = origIdent
			forceKillByPortFn = origForcePort
			taskkillProcessTreeByPIDFn = origTaskkill
			stopForceKillPIDFn = origPIDKill
		})

		forceKillByPortFn = killDaemonByPortOutcome
		killed := false
		lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
			if gotPort != port {
				t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
			}
			if killed {
				return 0, 0, 0, false
			}
			return portOwnerPID, 0, 0, true
		}
		processIdentityByPID = func(pid int) (string, string, bool) {
			switch pid {
			case stalePID:
				return "node.exe", "explorer.exe", true
			case portOwnerPID:
				return "mcphub.exe", "svchost.exe", true
			default:
				t.Fatalf("processIdentityByPID pid = %d, want stale PID %d or port owner PID %d", pid, stalePID, portOwnerPID)
				return "", "", false
			}
		}
		var taskkillPIDs []int
		taskkillProcessTreeByPIDFn = func(pid int) error {
			taskkillPIDs = append(taskkillPIDs, pid)
			killed = true
			return nil
		}
		stopForceKillPIDFn = func(pid int) error {
			t.Fatalf("stopForceKillPIDFn called for refused foreign PID %d", pid)
			return nil
		}

		result := forceKillOneSupervisorTarget(
			SupervisorDaemon{TaskName: taskName, Port: port},
			map[string]int{bareTask: stalePID},
		)
		if result.Err != "" {
			t.Fatalf("forceKillOneSupervisorTarget error = %q, want port-path kill success", result.Err)
		}
		if len(taskkillPIDs) != 1 || taskkillPIDs[0] != portOwnerPID {
			t.Fatalf("taskkill PIDs = %v, want [%d]", taskkillPIDs, portOwnerPID)
		}
	})
}

// TestStopForceAlreadyGonePIDKillFallsThroughToPortClassifier verifies that an
// already-gone PID kill result is not unconditional success when a descriptor
// port exists. The port classifier must still prove that the port has no
// listener (or otherwise handle/report it) before the row can be successful.
func TestStopForceAlreadyGonePIDKillFallsThroughToPortClassifier(t *testing.T) {
	const taskName = `\mcp-local-hub-laneatest-default`
	const bareTask = "mcp-local-hub-laneatest-default"
	const pid = 62201
	const port = 33007

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origForcePort := forceKillByPortFn
	origPIDKill := stopForceKillPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		forceKillByPortFn = origForcePort
		stopForceKillPIDFn = origPIDKill
	})

	var portLookups int
	forceKillByPortFn = func(p int, _ time.Duration) (portKillOutcome, error) {
		portLookups++
		if p != port {
			t.Fatalf("forceKillByPortFn port = %d, want %d", p, port)
		}
		return portKillNoListener, nil
	}
	processIdentityByPID = func(gotPID int) (string, string, bool) {
		if gotPID != pid {
			t.Fatalf("processIdentityByPID pid = %d, want %d", gotPID, pid)
		}
		return "mcphub.exe", "svchost.exe", true
	}
	stopForceKillPIDFn = func(gotPID int) error {
		if gotPID != pid {
			t.Fatalf("stopForceKillPIDFn pid = %d, want %d", gotPID, pid)
		}
		return errors.New("synthetic stale PID already gone")
	}
	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		t.Fatalf("lookupProcess called directly for port %d; want forceKillByPortFn seam", gotPort)
		return 0, 0, 0, false
	}

	result := forceKillOneSupervisorTarget(
		SupervisorDaemon{TaskName: taskName, Port: port},
		map[string]int{bareTask: pid},
	)
	if result.Err != "" {
		t.Fatalf("forceKillOneSupervisorTarget error = %q, want clean success when the port classifier reports no listener", result.Err)
	}
	if portLookups != 1 {
		t.Fatalf("port classifier consulted %d times after an already-gone PID kill; want 1", portLookups)
	}
}

func TestStopForcePortKillRejectsForeignProcessOwner(t *testing.T) {
	const port = 33003

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		if gotPort != port {
			t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
		}
		return 63001, 0, 0, true
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid != 63001 {
			t.Fatalf("processIdentityByPID pid = %d, want 63001", pid)
		}
		return "python.exe", "explorer.exe", true
	}
	taskkillProcessTreeByPIDFn = func(pid int) error {
		t.Fatalf("taskkillProcessTreeByPIDFn called for foreign pid %d", pid)
		return nil
	}

	result := forceKillOneSupervisorTarget(SupervisorDaemon{TaskName: `\mcp-local-hub-laneatest-default`, Port: port}, nil)
	if result.Err == "" {
		t.Fatal("forceKillOneSupervisorTarget returned success for foreign port owner; want explicit error")
	}
	if !strings.Contains(result.Err, "port owned by foreign process") {
		t.Fatalf("error = %q, want foreign-process owner message", result.Err)
	}
}

func TestStopForcePortKillAllowsMcphubProcessOwner(t *testing.T) {
	const port = 33004

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	killed := false
	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		if gotPort != port {
			t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
		}
		if killed {
			return 0, 0, 0, false
		}
		return 64001, 0, 0, true
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid != 64001 {
			t.Fatalf("processIdentityByPID pid = %d, want 64001", pid)
		}
		return "mcphub.exe", "svchost.exe", true
	}
	var killedPIDs []int
	taskkillProcessTreeByPIDFn = func(pid int) error {
		killedPIDs = append(killedPIDs, pid)
		killed = true
		return nil
	}

	result := forceKillOneSupervisorTarget(SupervisorDaemon{TaskName: `\mcp-local-hub-laneatest-default`, Port: port}, nil)
	if result.Err != "" {
		t.Fatalf("forceKillOneSupervisorTarget error = %q, want success", result.Err)
	}
	if len(killedPIDs) != 1 || killedPIDs[0] != 64001 {
		t.Fatalf("taskkill PIDs = %v, want [64001]", killedPIDs)
	}
}

func TestStopForcePortKillRejectsIdentityLookupFailure(t *testing.T) {
	const port = 33008
	const ownerPID = 65001

	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origTaskkill := taskkillProcessTreeByPIDFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		taskkillProcessTreeByPIDFn = origTaskkill
	})

	lookupProcess = func(gotPort int) (int, uint64, int64, bool) {
		if gotPort != port {
			t.Fatalf("lookupProcess port = %d, want %d", gotPort, port)
		}
		return ownerPID, 0, 0, true
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid != ownerPID {
			t.Fatalf("processIdentityByPID pid = %d, want %d", pid, ownerPID)
		}
		return "", "", false
	}
	taskkillProcessTreeByPIDFn = func(pid int) error {
		t.Fatalf("taskkillProcessTreeByPIDFn called after identity lookup failure for pid %d", pid)
		return nil
	}

	outcome, err := killDaemonByPortOutcome(port, 5*time.Second)
	if err == nil {
		t.Fatal("killDaemonByPortOutcome returned nil error after identity lookup failure; want fail-closed error")
	}
	if outcome != portKillIdentityMismatch {
		t.Fatalf("outcome = %v, want %v", outcome, portKillIdentityMismatch)
	}
	if !strings.Contains(err.Error(), "process identity lookup failed") {
		t.Fatalf("error = %q, want identity lookup failure", err.Error())
	}
}

func TestStopForceSupervisorPIDRejectsIdentityLookupFailure(t *testing.T) {
	const taskName = `\mcp-local-hub-laneatest-default`
	const bareTask = "mcp-local-hub-laneatest-default"
	const stalePID = 65002

	origIdent := processIdentityByPID
	origPIDKill := stopForceKillPIDFn
	t.Cleanup(func() {
		processIdentityByPID = origIdent
		stopForceKillPIDFn = origPIDKill
	})

	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid != stalePID {
			t.Fatalf("processIdentityByPID pid = %d, want %d", pid, stalePID)
		}
		return "", "", false
	}
	stopForceKillPIDFn = func(pid int) error {
		t.Fatalf("stopForceKillPIDFn called after identity lookup failure for pid %d", pid)
		return nil
	}

	result := forceKillOneSupervisorTarget(
		SupervisorDaemon{TaskName: taskName, Port: 0},
		map[string]int{bareTask: stalePID},
	)
	if result.Err == "" {
		t.Fatal("forceKillOneSupervisorTarget returned success after PID identity lookup failure; want fail-closed error")
	}
	if !strings.Contains(result.Err, "process identity lookup failed") {
		t.Fatalf("error = %q, want identity lookup failure", result.Err)
	}
}
