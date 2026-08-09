package gui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestEmitExitReasonEvent_WritesBoundedRow proves EmitExitReasonEvent writes
// the expected gui-exit-reason row (event name + machine-filterable reason
// field) to supervisor-events.log, redirected to a temp state dir via the
// same api.SetDaemonStateRootForTest seam supervise_ensure_alive_test.go
// uses — this test never touches the developer's real %LOCALAPPDATA%.
//
// Resets the process-wide first-trigger-wins guard (P2-5 review fix) before
// and after so this test's outcome never depends on whether an earlier test
// in this binary already emitted a (dedup-consuming) row.
func TestEmitExitReasonEvent_WritesBoundedRow(t *testing.T) {
	ResetExitReasonDedupForTest()
	t.Cleanup(ResetExitReasonDedupForTest)

	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	EmitExitReasonEvent(GUIExitReasonTrayQuit, map[string]any{"extra_field": "present"})

	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"event":"gui-exit-reason"`) {
		t.Fatalf("missing gui-exit-reason event: %s", body)
	}
	if !strings.Contains(body, `"reason":"tray-quit"`) {
		t.Fatalf("missing tray-quit reason: %s", body)
	}
	if !strings.Contains(body, `"extra_field":"present"`) {
		t.Fatalf("caller-supplied body field was dropped: %s", body)
	}
}

// TestEmitExitReasonEvent_EachReasonIsDistinct is a light sweep over the
// full GUIExitReason vocabulary confirming each constant round-trips as its
// own distinct reason string (guards against two constants accidentally
// sharing a value, which would make supervisor-events.log un-attributable).
//
// Each reason gets its own subtest with its own reset of the first-trigger-
// wins guard (P2-5 review fix): EmitExitReasonEvent now writes AT MOST ONE
// row per process, so testing five DIFFERENT reasons round-trip
// independently requires resetting the guard between them — a single
// process-wide sweep (the pre-fix shape of this test) would otherwise only
// ever observe the first reason's row.
func TestEmitExitReasonEvent_EachReasonIsDistinct(t *testing.T) {
	reasons := []GUIExitReason{
		GUIExitReasonSIGINT,
		GUIExitReasonSIGTERM,
		GUIExitReasonTrayQuit,
		GUIExitReasonTrayQuitAndStopAll,
		GUIExitReasonSelfRestart,
	}
	seen := map[GUIExitReason]bool{}
	for _, r := range reasons {
		if seen[r] {
			t.Fatalf("duplicate GUIExitReason value %q", r)
		}
		seen[r] = true

		t.Run(string(r), func(t *testing.T) {
			ResetExitReasonDedupForTest()
			t.Cleanup(ResetExitReasonDedupForTest)

			stateDir := t.TempDir()
			restore := api.SetDaemonStateRootForTest(stateDir)
			t.Cleanup(restore)

			EmitExitReasonEvent(r, nil)

			raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
			if err != nil {
				t.Fatalf("read supervisor-events.log: %v", err)
			}
			needle := `"reason":"` + string(r) + `"`
			if !strings.Contains(string(raw), needle) {
				t.Fatalf("missing row for reason %q; log=%s", r, raw)
			}
		})
	}
}

// TestEmitExitReasonEvent_FirstTriggerWins reproduces the P2-5 review
// finding directly: a signal racing a tray Quit (or any other pair of
// triggers) must never produce two conflicting exit-reason rows for one
// process's shutdown. Two DIFFERENT reasons are emitted back-to-back with
// NO reset between them (simulating the race — whichever trigger's
// EmitExitReasonEvent call happens to run first inside the real process);
// only the first must land.
//
// MUTATION: remove the exitReasonOnce.Do wrapper in EmitExitReasonEvent
// (call emitExitReasonEventOnce directly) — both reasons would then be
// written and this test's "second reason must be ABSENT" assertion fails.
func TestEmitExitReasonEvent_FirstTriggerWins(t *testing.T) {
	ResetExitReasonDedupForTest()
	t.Cleanup(ResetExitReasonDedupForTest)

	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

	EmitExitReasonEvent(GUIExitReasonSIGINT, nil)
	EmitExitReasonEvent(GUIExitReasonTrayQuit, nil) // races the first trigger; must be a no-op

	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"reason":"sigint"`) {
		t.Fatalf("first trigger's row is missing; log=%s", body)
	}
	if strings.Contains(body, `"reason":"tray-quit"`) {
		t.Fatalf("second (racing) trigger ALSO wrote a row — first-trigger-wins dedup did not fire; log=%s", body)
	}
	// Exactly one gui-exit-reason line, not two.
	if got := strings.Count(body, `"event":"gui-exit-reason"`); got != 1 {
		t.Fatalf("gui-exit-reason row count = %d, want exactly 1; log=%s", got, body)
	}
}
