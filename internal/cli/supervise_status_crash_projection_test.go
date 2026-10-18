package cli

import (
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestSupervisorStatusDaemonsProjectsCrashesInWindow(t *testing.T) {
	const taskName = `\mcp-local-hub-crashprojection-default`
	stateDir := apitest.HardenedTempDir(t)
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "crashprojection",
			Daemon:   "default",
			Port:     9321,
		}},
	}); err != nil {
		t.Fatalf("seed isolated supervisor intent: %v", err)
	}

	tracker := NewDaemonRuntimeTracker()
	now := time.Now().UTC()
	if got := tracker.RecordCrashAndCountInWindow(taskName, now.Add(-2*time.Minute), respawnFailureWindow); got != 1 {
		t.Fatalf("first synthetic crash count = %d, want 1", got)
	}
	if got := tracker.RecordCrashAndCountInWindow(taskName, now.Add(-time.Minute), respawnFailureWindow); got != 2 {
		t.Fatalf("second synthetic crash count = %d, want 2", got)
	}

	// The tracker has no running entry, so the status row is idle. Disable the
	// global liveness probes as an additional guard against OS port, PID, or TCP
	// inspection; supervisorStatusDaemons must project the metric from memory.
	restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{})
	t.Cleanup(restore)

	rows, err := supervisorStatusDaemons(stateDir, tracker, nil)
	if err != nil {
		t.Fatalf("supervisorStatusDaemons: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("status rows = %#v, want exactly one", rows)
	}
	got, ok := rows[0]["crashes_in_window"]
	if !ok {
		t.Fatalf("status row omitted crashes_in_window after two synthetic crashes: %#v", rows[0])
	}
	count, ok := got.(int)
	if !ok {
		t.Fatalf("crashes_in_window type = %T, want int (row=%#v)", got, rows[0])
	}
	if count != 2 {
		t.Fatalf("crashes_in_window = %d, want 2", count)
	}
	if countAfter := tracker.CrashCountInWindow(taskName, now, respawnFailureWindow); countAfter != 2 {
		t.Fatalf("status observation mutated crash count: after=%d, want unchanged 2", countAfter)
	}
}

func TestSupervisorStatusDaemonsProjectsZeroCrashesInWindow(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracker *DaemonRuntimeTracker
	}{
		{name: "fresh tracker", tracker: NewDaemonRuntimeTracker()},
		{name: "nil tracker", tracker: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const taskName = `\mcp-local-hub-crashprojection-zero-default`
			stateDir := apitest.HardenedTempDir(t)
			if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{
				Version: 1,
				Daemons: []api.SupervisorDaemon{{
					TaskName: taskName,
					Server:   "crashprojection-zero",
					Daemon:   "default",
					Port:     9322,
				}},
			}); err != nil {
				t.Fatalf("seed isolated supervisor intent: %v", err)
			}

			restore := setSupervisorLivenessProbeForTest(supervisorLivenessProbe{})
			t.Cleanup(restore)
			rows, err := supervisorStatusDaemons(stateDir, tc.tracker, nil)
			if err != nil {
				t.Fatalf("supervisorStatusDaemons: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("status rows = %#v, want exactly one", rows)
			}
			got, ok := rows[0]["crashes_in_window"]
			if !ok {
				t.Fatalf("status row omitted zero crashes_in_window: %#v", rows[0])
			}
			count, ok := got.(int)
			if !ok || count != 0 {
				t.Fatalf("crashes_in_window = %#v (%T), want int(0)", got, got)
			}
		})
	}
}
