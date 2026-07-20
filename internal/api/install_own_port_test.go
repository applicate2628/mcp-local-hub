package api

import (
	"context"
	"errors"
	"fmt"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// TestPortHeldByOurDaemon pins bug-bash A6 (#6) closure: portInUse
// must distinguish "our own running daemon" (idempotent reinstall;
// tolerate) from "a foreign process stole the port" (real collision;
// fail). The helper is purely a function-pointer seam wired through
// lookupProcess + processIdentityByPID + schedulerStatusForOwnPort,
// so we exercise the full three-part identity gate (bot r1 P1 + r2 P1
// closure on PR #180): port-to-PID lookup × image OR parent-image
// matches mcphub.exe × scheduler task state.
func TestPortHeldByOurDaemon(t *testing.T) {
	current := "root"
	if u, err := user.Current(); err == nil && u != nil && u.Username != "" {
		current = u.Username
	}
	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		schedulerStatusForOwnPort = origSched
	})

	cases := []struct {
		name        string
		lookupOK    bool
		lookupPID   int
		identOK     bool
		image       string
		parentImage string
		schState    string
		schOwner    string
		schErr      error
		want        bool
	}{
		// Happy paths: image matches.
		{"mcphub.exe + task Running → ours (stdio-bridge / native-http external)", true, 12345, true, "mcphub.exe", "svchost.exe", "Running", current, nil, true},
		{"MCPHUB.EXE (uppercase) + task Running → ours", true, 12345, true, "MCPHUB.EXE", "svchost.exe", "Running", current, nil, true},
		{"MCPHub.exe (mixed) + task Running → ours", true, 12345, true, "MCPHub.exe", "svchost.exe", "Running", current, nil, true},

		// Happy paths: parent image matches (native-http internal port,
		// upstream child spawned by mcphub.exe).
		{"python.exe child of mcphub.exe + task Running → ours (native-http internal)", true, 12345, true, "python.exe", "mcphub.exe", "Running", current, nil, true},
		{"node.exe child of MCPHUB.EXE (case insensitive parent) → ours", true, 12345, true, "node.exe", "MCPHUB.EXE", "Running", current, nil, true},
		{"empty image + parent mcphub.exe → ours (parent saves it)", true, 12345, true, "", "mcphub.exe", "Running", current, nil, true},

		// Foreign image AND foreign parent → real collision. This is
		// the bot r1 P1 scenario: a stale orphan or attacker process
		// holds the port while our same-named task is also Running.
		{"foreign python.exe (no mcphub parent) + task Running → NOT ours", true, 12345, true, "python.exe", "explorer.exe", "Running", current, nil, false},
		{"foreign notepad.exe (no mcphub parent) + task Running → NOT ours", true, 12345, true, "notepad.exe", "cmd.exe", "Running", current, nil, false},
		{"foreign python.exe + empty parent → NOT ours", true, 12345, true, "python.exe", "", "Running", current, nil, false},

		// Identity lookup itself fails → fail-closed.
		{"identity lookup fails → fail-closed (not ours)", true, 12345, false, "", "", "Running", current, nil, false},
		{"empty image AND empty parent → fail-closed (not ours)", true, 12345, true, "", "", "Running", current, nil, false},

		// Scheduler-state gate (image + parent both match, but task
		// not running → still not ours, e.g. orphan after task disabled).
		{"mcphub.exe + task Ready (not Running) → not ours", true, 12345, true, "mcphub.exe", "svchost.exe", "Ready", current, nil, false},
		{"mcphub.exe + task Disabled → not ours", true, 12345, true, "mcphub.exe", "svchost.exe", "Disabled", current, nil, false},
		{"mcphub.exe + task-not-found → not ours (foreign owner)", true, 12345, true, "mcphub.exe", "svchost.exe", "", current, scheduler.ErrTaskNotFound, false},
		{"mcphub.exe + task Running owned by other user → not ours", true, 12345, true, "mcphub.exe", "svchost.exe", "Running", "ATTACKER\\mallory", nil, false},

		// Port lookup gate.
		{"port-to-PID lookup fails → not ours", false, 0, true, "mcphub.exe", "svchost.exe", "Running", current, nil, false},
		{"port-to-PID returns 0 → not ours", true, 0, true, "mcphub.exe", "svchost.exe", "Running", current, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookupProcess = func(port int) (int, uint64, int64, bool) {
				return tc.lookupPID, 0, 0, tc.lookupOK
			}
			processIdentityByPID = func(pid int) (string, string, bool) {
				return tc.image, tc.parentImage, tc.identOK
			}
			schedulerStatusForOwnPort = func(taskName string) (scheduler.TaskStatus, error) {
				return scheduler.TaskStatus{Name: taskName, State: tc.schState, Owner: tc.schOwner}, tc.schErr
			}
			got := portHeldByOurDaemon(9129, "gdb", "default")
			if got != tc.want {
				t.Errorf("portHeldByOurDaemon = %v, want %v", got, tc.want)
			}
		})
	}
}

// Sanity: if any of the three seams is unwired, portHeldByOurDaemon
// must fail-closed (return false → real collision path fires). One
// case per nil seam confirms no seam grants implicit ownership.
func TestPortHeldByOurDaemon_NilSeams(t *testing.T) {
	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		schedulerStatusForOwnPort = origSched
	})

	t.Run("nil lookupProcess → fail-closed", func(t *testing.T) {
		lookupProcess = nil
		processIdentityByPID = func(pid int) (string, string, bool) { return "mcphub.exe", "svchost.exe", true }
		schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
			return scheduler.TaskStatus{State: "Running"}, nil
		}
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil lookupProcess = true, want false")
		}
	})

	t.Run("nil processIdentityByPID → fail-closed", func(t *testing.T) {
		lookupProcess = func(port int) (int, uint64, int64, bool) { return 12345, 0, 0, true }
		processIdentityByPID = nil
		schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
			return scheduler.TaskStatus{State: "Running"}, nil
		}
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil processIdentityByPID = true, want false")
		}
	})

	t.Run("nil schedulerStatusForOwnPort → fail-closed", func(t *testing.T) {
		lookupProcess = func(port int) (int, uint64, int64, bool) { return 12345, 0, 0, true }
		processIdentityByPID = func(pid int) (string, string, bool) { return "mcphub.exe", "svchost.exe", true }
		schedulerStatusForOwnPort = nil
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil schedulerStatusForOwnPort = true, want false")
		}
	})
}

func TestPreflight_AllowsSameSupervisorOwnedPortAndRejectsForeignIntentRow(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)

	const (
		port    = 9311
		portPID = 55221
	)
	taskName := `\mcp-local-hub-demo-alpha`
	m := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportStdioBridge,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: port}},
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
	preflightPortInUse = func(p int) bool { return p == port }
	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == port {
			return portPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == portPID {
			return "mcphub.exe", "svchost.exe", true
		}
		return "", "", false
	}
	// Post-v0.6 supervisor-owned installs no longer have a per-daemon scheduler
	// task, so the legacy scheduler proof must not be required for this path.
	schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
		return scheduler.TaskStatus{}, scheduler.ErrTaskNotFound
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: portPID, State: "Running"}}, nil
	}

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     port,
		}},
	}); err != nil {
		t.Fatalf("seed same-server supervisor intent: %v", err)
	}
	if err := Preflight(m, "alpha"); err != nil {
		t.Fatalf("Preflight should accept the same supervisor-owned daemon port: %v", err)
	}

	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: `\mcp-local-hub-other-alpha`,
			Server:   "other",
			Daemon:   "alpha",
			Command:  "go",
			Port:     port,
		}},
	}); err != nil {
		t.Fatalf("seed foreign supervisor intent: %v", err)
	}
	err := Preflight(m, "alpha")
	if err == nil {
		t.Fatal("Preflight accepted a foreign supervisor-intent row on the same port; want port collision")
	}
	if !strings.Contains(err.Error(), "port 9311 already in use") {
		t.Fatalf("Preflight error = %v, want port collision for foreign owner", err)
	}
}

func TestPortHeldBySupervisorIntentDaemonExternalRequiresPIDProof(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	const (
		port    = 33011
		portPID = 66011
	)
	const taskName = `\mcp-local-hub-demo-alpha`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     port,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	origLookup := lookupProcess
	origStatus := supervisorIPCStatusFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		supervisorIPCStatusFn = origStatus
	})

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: portPID, State: "Running"}}, nil
	}
	lookupProcess = nil
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); got {
		t.Fatal("intent row with unavailable port-owner lookup and live supervisor PID = true, want false")
	}

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return nil, errors.New("supervisor IPC unavailable")
	}
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); got {
		t.Fatal("intent row with unavailable port-owner lookup and unreachable supervisor IPC = true, want false")
	}

	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == port {
			return portPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return nil, errors.New("supervisor IPC unavailable")
	}
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); got {
		t.Fatal("intent row with unreachable supervisor IPC = true, want false")
	}

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: portPID, State: "Running"}}, nil
	}
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); !got {
		t.Fatal("intent row with matching port PID and live supervisor PID = false, want true")
	}

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: portPID + 1, State: "Running"}}, nil
	}
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); got {
		t.Fatal("intent row with mismatched port PID and live supervisor PID = true, want false")
	}

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, State: "Running"}}, nil
	}
	if got := portHeldBySupervisorIntentDaemon(port, "demo", "alpha"); got {
		t.Fatal("intent row without live supervisor PID = true, want false")
	}

	lookupProcess = nil
	supervisorIPCStatusFn = nil
	if got := portHeldBySupervisorIntentDaemon(port+config.NativeHTTPInternalPortOffset, "demo", "alpha"); got {
		t.Fatal("native-http internal port with unreachable supervisor IPC = true, want false")
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: portPID, State: "Running"}}, nil
	}
	if got := portHeldBySupervisorIntentDaemon(port+config.NativeHTTPInternalPortOffset, "demo", "alpha"); !got {
		t.Fatal("native-http internal port with matching descriptor row and live supervisor PID = false, want true")
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, State: "Running"}}, nil
	}
	if got := portHeldBySupervisorIntentDaemon(port+config.NativeHTTPInternalPortOffset, "demo", "alpha"); got {
		t.Fatal("native-http internal port without live supervisor PID = true, want false")
	}
}

func TestPortHeldBySupervisorIntentDaemonInternalPortRequiresWrapperAncestry(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	const (
		externalPort = 33021
		internalPort = externalPort + config.NativeHTTPInternalPortOffset
		listenerPID  = 88021
		uvxPID       = 88022
		wrapperPID   = 77021
		foreignPID   = 99021
	)
	const taskName = `\mcp-local-hub-demo-alpha`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     externalPort,
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	origLookup := lookupProcess
	origStatus := supervisorIPCStatusFn
	origNameParent := processNameAndParentByPID
	t.Cleanup(func() {
		lookupProcess = origLookup
		supervisorIPCStatusFn = origStatus
		processNameAndParentByPID = origNameParent
	})

	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: wrapperPID, State: "Running"}}, nil
	}
	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == internalPort {
			return listenerPID, 0, 0, true
		}
		return 0, 0, 0, false
	}

	processNameAndParentByPID = fakeProcessNameAndParent(map[int]struct {
		image  string
		parent int
	}{
		listenerPID: {image: "python.exe", parent: uvxPID},
		uvxPID:      {image: "uvx.exe", parent: wrapperPID},
		wrapperPID:  {image: "mcphub.exe", parent: 1},
	})
	if got := portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha"); !got {
		t.Fatal("native-http internal listener whose parent chain reaches the supervisor wrapper PID = false, want true")
	}

	processNameAndParentByPID = fakeProcessNameAndParent(map[int]struct {
		image  string
		parent int
	}{
		listenerPID: {image: "python.exe", parent: uvxPID},
		uvxPID:      {image: "uvx.exe", parent: foreignPID},
		foreignPID:  {image: "node.exe", parent: 1},
	})
	if got := portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha"); got {
		t.Fatal("native-http internal listener whose resolvable parent chain does not reach the wrapper PID = true, want false")
	}

	lookupProcess = nil
	processNameAndParentByPID = nil
	if got := portHeldBySupervisorIntentDaemon(internalPort, "demo", "alpha"); !got {
		t.Fatal("native-http internal port with no port-owner lookup should keep the live-wrapper-PID downgrade; got false")
	}
}

func TestPreflight_ExternalPortDoesNotMatchSupervisorIntentInternalPort(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	preparePreflightBinaryChecks(t)

	const (
		externalPort = 33031
		internalPort = externalPort + config.NativeHTTPInternalPortOffset
		listenerPID  = 88031
		wrapperPID   = 77031
	)
	const taskName = `\mcp-local-hub-demo-alpha`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "demo",
			Daemon:   "alpha",
			Command:  "go",
			Port:     externalPort,
			RuntimeSpec: &DaemonRuntimeSpec{
				SpecVersion:  DaemonRuntimeSpecVersion,
				ExternalPort: externalPort,
				UpstreamPort: internalPort,
			},
		}},
	}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}

	origPortInUse := preflightPortInUse
	origLookup := lookupProcess
	origIdent := processIdentityByPID
	origSched := schedulerStatusForOwnPort
	origStatus := supervisorIPCStatusFn
	origNameParent := processNameAndParentByPID
	t.Cleanup(func() {
		preflightPortInUse = origPortInUse
		lookupProcess = origLookup
		processIdentityByPID = origIdent
		schedulerStatusForOwnPort = origSched
		supervisorIPCStatusFn = origStatus
		processNameAndParentByPID = origNameParent
	})

	preflightPortInUse = func(port int) bool { return port == internalPort }
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		if port == internalPort {
			return listenerPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	processIdentityByPID = func(pid int) (string, string, bool) {
		if pid == listenerPID {
			return "python.exe", "mcphub.exe", true
		}
		return "", "", false
	}
	schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
		return scheduler.TaskStatus{}, scheduler.ErrTaskNotFound
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: wrapperPID, State: "Running"}}, nil
	}
	processNameAndParentByPID = fakeProcessNameAndParent(map[int]struct {
		image  string
		parent int
	}{
		listenerPID: {image: "python.exe", parent: wrapperPID},
		wrapperPID:  {image: "mcphub.exe", parent: 1},
	})

	movedExternalPort := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: internalPort}},
	}
	err := Preflight(movedExternalPort, "alpha")
	if err == nil {
		t.Fatal("Preflight accepted an external-port collision by matching a prior descriptor's internal port; want collision")
	}
	if !strings.Contains(err.Error(), "port 43031 already in use") {
		t.Fatalf("Preflight error = %v, want external-port collision", err)
	}

	unchangedExternalPort := &config.ServerManifest{
		Name:      "demo",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "go",
		Daemons:   []config.DaemonSpec{{Name: "alpha", Port: externalPort}},
	}
	if err := Preflight(unchangedExternalPort, "alpha"); err != nil {
		t.Fatalf("Preflight should still accept the matching internal port for the unchanged descriptor: %v", err)
	}
}

// fakeProcessNameAndParent models a probe surface that ANSWERS: an absent row
// is a definitive "no such process" (errProcessNotFound), NOT a timeout. Use
// fakeProcessNameAndParentTimingOut for the did-not-answer case — the two drive
// opposite ownership decisions.
func fakeProcessNameAndParent(rows map[int]struct {
	image  string
	parent int
}) func(int) (string, int, error) {
	return func(pid int) (string, int, error) {
		row, ok := rows[pid]
		if !ok {
			return "", 0, errProcessNotFound
		}
		return row.image, row.parent, nil
	}
}

// fakeProcessNameAndParentTimingOut models the WMI-congested host: the probe
// ran and was cut by its deadline, so ownership is UNKNOWN.
func fakeProcessNameAndParentTimingOut() func(int) (string, int, error) {
	return func(pid int) (string, int, error) {
		return "", 0, fmt.Errorf("identity probe for pid %d: %w", pid, ErrProbeTimeout)
	}
}

// TestParseNameParent pins the CSV parser used by both wmic and
// PowerShell paths in procNameAndParent (bot r2 P2 closure). The parser
// must accept both Windows shapes and reject malformed rows.
func TestParseNameParent(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		wantName  string
		wantPID   int
		wantParse bool
	}{
		{
			name: "wmic CSV shape — single row",
			input: "Node,Name,ParentProcessId\r\n" +
				"\r\n" +
				"HOST,mcphub.exe,4200\r\n",
			wantName:  "mcphub.exe",
			wantPID:   4200,
			wantParse: true,
		},
		{
			name: "PowerShell shape — header prepended",
			input: "Node,Name,ParentProcessId\n" +
				"HOST,python.exe,12345\n",
			wantName:  "python.exe",
			wantPID:   12345,
			wantParse: true,
		},
		{
			name:      "empty output → no parse",
			input:     "",
			wantParse: false,
		},
		{
			name:      "header only → no parse",
			input:     "Node,Name,ParentProcessId\n",
			wantParse: false,
		},
		{
			name: "malformed row (2 cols) → no parse",
			input: "Node,Name,ParentProcessId\n" +
				"HOST,foo\n",
			wantParse: false,
		},
		{
			name: "empty Name field → skip and no parse",
			input: "Node,Name,ParentProcessId\n" +
				"HOST,,4200\n",
			wantParse: false,
		},
		{
			name: "non-numeric parent PID → name parsed, PID=0",
			input: "Node,Name,ParentProcessId\n" +
				"HOST,mcphub.exe,not-a-number\n",
			wantName:  "mcphub.exe",
			wantPID:   0,
			wantParse: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, pp, parsed := parseNameParent(tc.input)
			if parsed != tc.wantParse {
				t.Errorf("parsed = %v, want %v", parsed, tc.wantParse)
				return
			}
			if !parsed {
				return
			}
			if name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
			if pp != tc.wantPID {
				t.Errorf("parentPID = %d, want %d", pp, tc.wantPID)
			}
		})
	}
}

// TestSupervisorIntentDaemonForPort_Port0StdioMatchedByIdentity pins the
// matcher fix: a stdio-bridge descriptor records Port==0 (its HTTP bridge port
// is assigned at spawn, never written to the row), so the matcher must decide
// identity FIRST and treat a Port==0 row as an external-port candidate — a
// port-first match would skip it and a running stdio server could never
// recognize its own reinstall. Real external + internal-offset matches must
// keep their prior semantics.
func TestSupervisorIntentDaemonForPort_Port0StdioMatchedByIdentity(t *testing.T) {
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0},
			{TaskName: `\mcp-local-hub-serena-unified`, Server: "serena", Daemon: "unified", Port: 9121},
			{TaskName: `\mcp-local-hub-demo-alpha`, Server: "demo", Daemon: "alpha", Port: 9300},
		},
	}
	cases := []struct {
		name        string
		port        int
		server      string
		daemon      string
		allowIntern bool
		wantOK      bool
		wantIntern  bool // matchedInternalPort
	}{
		{"port0 stdio matched by identity (the bug)", 9123, "memory", "default", true, true, false},
		{"port0 stdio matched even when internal-match disabled", 9123, "memory", "default", false, true, false},
		{"real external port still matches", 9121, "serena", "unified", true, true, false},
		{"internal offset still matches when allowed", 9300 + config.NativeHTTPInternalPortOffset, "demo", "alpha", true, true, true},
		{"internal offset NOT matched when disabled", 9300 + config.NativeHTTPInternalPortOffset, "demo", "alpha", false, false, false},
		{"port0 row NOT claimed for a different server", 9123, "other", "default", true, false, false},
		{"non-zero row NOT claimed for an unrelated probed port", 7777, "demo", "alpha", true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row, matchedIntern, ok := supervisorIntentDaemonForPort(intent, tc.port, tc.server, tc.daemon, tc.allowIntern)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (row=%+v)", ok, tc.wantOK, row)
			}
			if ok && matchedIntern != tc.wantIntern {
				t.Errorf("matchedInternalPort = %v, want %v", matchedIntern, tc.wantIntern)
			}
		})
	}
}

// TestSupervisorIntentRowMatchesServerDaemon_ArgvDisambiguatesAmbiguousCanonical
// is the bot PR #505 r4 guard: a blank-field row whose ARGS carry
// --server demo --daemon alpha-beta matches demo/alpha-beta and NOT the sibling
// demo-alpha/beta, even though both reconstruct the same canonical task name
// \mcp-local-hub-demo-alpha-beta. F5 no longer heals the fields, so the argv is the
// authoritative disambiguator (else a sibling's preflight claims this daemon's port).
func TestSupervisorIntentRowMatchesServerDaemon_ArgvDisambiguatesAmbiguousCanonical(t *testing.T) {
	row := SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Args:     []string{"daemon", "--server", "demo", "--daemon", "alpha-beta"}, // identity in args
	}
	if !supervisorIntentRowMatchesServerDaemon(row, "demo", "alpha-beta") {
		t.Fatal("row with args --server demo --daemon alpha-beta must match demo/alpha-beta")
	}
	if supervisorIntentRowMatchesServerDaemon(row, "demo-alpha", "beta") {
		t.Fatal("row must NOT match the sibling demo-alpha/beta (argv proves it is demo/alpha-beta)")
	}
	// An argv-less blank row still falls back to the canonical task-name match (r26).
	bare := SupervisorDaemon{TaskName: `\mcp-local-hub-demo-alpha-beta`}
	if !supervisorIntentRowMatchesServerDaemon(bare, "demo", "alpha-beta") {
		t.Fatal("argv-less blank row must still match via canonical task name (r26 fallback)")
	}
	// bot PR #505 r6: a PARTIAL/corrupt global argv (`daemon --server demo`, no
	// --daemon) is owner-rejected. It must NOT fall back to the ambiguous canonical
	// task-name match — otherwise a sibling's preflight claims the live daemon's port
	// as self-owned and bypasses a real port collision. Fail closed for BOTH siblings.
	corrupt := SupervisorDaemon{
		TaskName: `\mcp-local-hub-demo-alpha-beta`,
		Args:     []string{"daemon", "--server", "demo"}, // partial: no --daemon
	}
	if supervisorIntentRowMatchesServerDaemon(corrupt, "demo", "alpha-beta") {
		t.Fatal("corrupt/partial global argv must NOT match demo/alpha-beta via task name (fail closed)")
	}
	if supervisorIntentRowMatchesServerDaemon(corrupt, "demo-alpha", "beta") {
		t.Fatal("corrupt/partial global argv must NOT match sibling demo-alpha/beta either")
	}
	// commission PR #505 r6 P1: a FULLY-POPULATED row whose fields CONTRADICT its
	// launch argv (a lying cache) must fail closed — the match no longer trusts the
	// stale field. Fields say memory/default; argv says time/default.
	lying := SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "time", "--daemon", "default"},
	}
	if supervisorIntentRowMatchesServerDaemon(lying, "memory", "default") {
		t.Fatal("populated field/argv-contradicting row must NOT match its stale field memory/default (fail closed, r6 P1)")
	}
	if supervisorIntentRowMatchesServerDaemon(lying, "time", "default") {
		t.Fatal("populated field/argv-contradicting row must NOT match the argv identity either (corrupt → no match)")
	}
	// A well-formed populated row still matches (common-path neutral).
	wellFormed := SupervisorDaemon{
		TaskName: `\mcp-local-hub-memory-default`,
		Server:   "memory", Daemon: "default",
		Args: []string{"daemon", "--server", "memory", "--daemon", "default"},
	}
	if !supervisorIntentRowMatchesServerDaemon(wellFormed, "memory", "default") {
		t.Fatal("well-formed populated row must still match memory/default (dropping the blank-field gate must be common-path neutral)")
	}
	// A populated row with EMPTY Args (legacy) still matches on its fields.
	legacyNoArgs := SupervisorDaemon{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default"}
	if !supervisorIntentRowMatchesServerDaemon(legacyNoArgs, "memory", "default") {
		t.Fatal("populated empty-Args legacy row must still match on its fields")
	}
}

// TestPortHeldBySupervisorIntentDaemon_Port0StdioBridgeRecognized is the
// end-to-end regression guard for the live bug: a running stdio-bridge global
// daemon (memory/time/wolfram/gdb/…) whose descriptor records Port==0 must be
// recognized as self-owned on its own port so a reinstall does NOT spuriously
// fail with port-in-use. The live-PID-owns-the-probed-port proof (supervisor
// IPC PID == netstat port owner) both authorizes the self-owned case and
// rejects a foreign PID holding the same port.
func TestPortHeldBySupervisorIntentDaemon_Port0StdioBridgeRecognized(t *testing.T) {
	stateDir := daemonIntentTestHelper(t)
	const (
		bridgePort = 9123
		daemonPID  = 103948
	)
	const taskName = `\mcp-local-hub-memory-default`

	intentPath := filepath.Join(stateDir, supervisorIntentFileLeaf)
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName: taskName,
			Server:   "memory",
			Daemon:   "default",
			Command:  "mcphub",
			Port:     0,
		}},
	}); err != nil {
		t.Fatalf("seed port-0 stdio supervisor intent: %v", err)
	}

	origLookup := lookupProcess
	origStatus := supervisorIPCStatusFn
	t.Cleanup(func() {
		lookupProcess = origLookup
		supervisorIPCStatusFn = origStatus
	})
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return []DaemonStatus{{TaskName: taskName, PID: daemonPID, State: "Running"}}, nil
	}
	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == bridgePort {
			return daemonPID, 0, 0, true
		}
		return 0, 0, 0, false
	}
	if got := portHeldBySupervisorIntentDaemon(bridgePort, "memory", "default"); !got {
		t.Fatal("running stdio-bridge daemon (Port==0 descriptor) not recognized as self-owned; its reinstall would spuriously fail port-in-use")
	}

	// Over-claim guard: a DIFFERENT live PID owning the probed port must NOT be
	// claimed as ours, even though the descriptor row matches the server.
	lookupProcess = func(p int) (int, uint64, int64, bool) {
		if p == bridgePort {
			return daemonPID + 1, 0, 0, true // a foreign process holds the port
		}
		return 0, 0, 0, false
	}
	if got := portHeldBySupervisorIntentDaemon(bridgePort, "memory", "default"); got {
		t.Fatal("foreign PID holding the bridge port was claimed as ours; the live-PID proof must reject it")
	}
}

// Suppress unused-import warning when running narrow test build subsets.
var _ = errors.New
