package api

import (
	"errors"
	"os/user"
	"testing"

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

// Suppress unused-import warning when running narrow test build subsets.
var _ = errors.New
