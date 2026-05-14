package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// TestPortHeldByOurDaemon pins bug-bash A6 (#6) closure: portInUse
// must distinguish "our own running daemon" (idempotent reinstall;
// tolerate) from "a foreign process stole the port" (real collision;
// fail). The helper is purely a function-pointer seam wired through
// lookupProcess + processImageByPID + schedulerStatusForOwnPort, so
// we exercise the full three-part identity gate (bot r1 P1 closure
// on PR #180): port-to-PID lookup × image basename match × scheduler
// task state.
func TestPortHeldByOurDaemon(t *testing.T) {
	origLookup := lookupProcess
	origImage := processImageByPID
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		processImageByPID = origImage
		schedulerStatusForOwnPort = origSched
	})

	cases := []struct {
		name      string
		lookupOK  bool
		lookupPID int
		imageOK   bool
		image     string
		schState  string
		schErr    error
		want      bool
	}{
		// Happy path: every gate aligns.
		{"mcphub.exe + task Running → ours", true, 12345, true, "mcphub.exe", "Running", nil, true},
		// Case-insensitive image match (Windows is case-insensitive on
		// filenames; wmic Name output may render any casing).
		{"MCPHUB.EXE (uppercase) + task Running → ours", true, 12345, true, "MCPHUB.EXE", "Running", nil, true},
		{"MCPHub.exe (mixed) + task Running → ours", true, 12345, true, "MCPHub.exe", "Running", nil, true},
		// Foreign image with matching task state → real collision.
		// This is the bot r1 P1 scenario: a stale orphan or attacker
		// process holds the port while our same-named task is also
		// Running (recovery race, watchdog restart in flight).
		{"foreign python.exe + task Running → NOT ours (collision)", true, 12345, true, "python.exe", "Running", nil, false},
		{"foreign notepad.exe + task Running → NOT ours (collision)", true, 12345, true, "notepad.exe", "Running", nil, false},
		// Image lookup itself fails — fail-closed (treat as collision)
		// rather than trust the scheduler signal alone.
		{"image lookup fails → fail-closed (not ours)", true, 12345, false, "", "Running", nil, false},
		{"empty image string → fail-closed (not ours)", true, 12345, true, "", "Running", nil, false},
		// Scheduler-state gate (existing cases).
		{"mcphub.exe + task Ready (not Running) → not ours", true, 12345, true, "mcphub.exe", "Ready", nil, false},
		{"mcphub.exe + task Disabled → not ours", true, 12345, true, "mcphub.exe", "Disabled", nil, false},
		{"mcphub.exe + task-not-found → not ours (foreign owner)", true, 12345, true, "mcphub.exe", "", scheduler.ErrTaskNotFound, false},
		// Port lookup gate.
		{"port-to-PID lookup fails → not ours", false, 0, true, "mcphub.exe", "Running", nil, false},
		{"port-to-PID returns 0 → not ours", true, 0, true, "mcphub.exe", "Running", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookupProcess = func(port int) (int, uint64, int64, bool) {
				return tc.lookupPID, 0, 0, tc.lookupOK
			}
			processImageByPID = func(pid int) (string, bool) {
				return tc.image, tc.imageOK
			}
			schedulerStatusForOwnPort = func(taskName string) (scheduler.TaskStatus, error) {
				return scheduler.TaskStatus{Name: taskName, State: tc.schState}, tc.schErr
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
	origImage := processImageByPID
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		processImageByPID = origImage
		schedulerStatusForOwnPort = origSched
	})

	t.Run("nil lookupProcess → fail-closed", func(t *testing.T) {
		lookupProcess = nil
		processImageByPID = func(pid int) (string, bool) { return "mcphub.exe", true }
		schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
			return scheduler.TaskStatus{State: "Running"}, nil
		}
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil lookupProcess = true, want false")
		}
	})

	t.Run("nil processImageByPID → fail-closed", func(t *testing.T) {
		lookupProcess = func(port int) (int, uint64, int64, bool) { return 12345, 0, 0, true }
		processImageByPID = nil
		schedulerStatusForOwnPort = func(string) (scheduler.TaskStatus, error) {
			return scheduler.TaskStatus{State: "Running"}, nil
		}
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil processImageByPID = true, want false")
		}
	})

	t.Run("nil schedulerStatusForOwnPort → fail-closed", func(t *testing.T) {
		lookupProcess = func(port int) (int, uint64, int64, bool) { return 12345, 0, 0, true }
		processImageByPID = func(pid int) (string, bool) { return "mcphub.exe", true }
		schedulerStatusForOwnPort = nil
		if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
			t.Errorf("nil schedulerStatusForOwnPort = true, want false")
		}
	})
}

// Suppress unused-import warning when running narrow test build subsets.
var _ = errors.New
