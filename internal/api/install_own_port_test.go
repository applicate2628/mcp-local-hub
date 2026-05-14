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
// lookupProcess + schedulerStatusForOwnPort, so we test all four
// combinations of (port-PID lookup result × scheduler Status result).
func TestPortHeldByOurDaemon(t *testing.T) {
	// Save originals; restore on every test exit.
	origLookup := lookupProcess
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		schedulerStatusForOwnPort = origSched
	})

	cases := []struct {
		name      string
		lookupOK  bool
		lookupPID int
		schState  string
		schErr    error
		want      bool
	}{
		{"matching task running → our daemon", true, 12345, "Running", nil, true},
		{"matching task Ready (not running) → not ours", true, 12345, "Ready", nil, false},
		{"matching task Disabled → not ours", true, 12345, "Disabled", nil, false},
		{"scheduler reports task-not-found → not ours (foreign PID)", true, 12345, "", scheduler.ErrTaskNotFound, false},
		{"port-to-PID lookup fails → not ours", false, 0, "Running", nil, false},
		{"port-to-PID returns 0 → not ours", true, 0, "Running", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookupProcess = func(port int) (int, uint64, int64, bool) {
				return tc.lookupPID, 0, 0, tc.lookupOK
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

// Sanity: if scheduler helper itself is unwired, portHeldByOurDaemon
// must fail-closed (return false → real collision path fires). Reuses
// the cleanup above by leaving it on the package init order.
func TestPortHeldByOurDaemon_NilSeam(t *testing.T) {
	origLookup := lookupProcess
	origSched := schedulerStatusForOwnPort
	t.Cleanup(func() {
		lookupProcess = origLookup
		schedulerStatusForOwnPort = origSched
	})
	lookupProcess = func(port int) (int, uint64, int64, bool) {
		return 12345, 0, 0, true
	}
	schedulerStatusForOwnPort = nil
	if got := portHeldByOurDaemon(9129, "gdb", "default"); got != false {
		t.Errorf("portHeldByOurDaemon with nil seam = true, want false (fail-closed)")
	}
}

// Suppress unused-import warning when running narrow test build subsets.
var _ = errors.New
