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
func TestEmitExitReasonEvent_WritesBoundedRow(t *testing.T) {
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
func TestEmitExitReasonEvent_EachReasonIsDistinct(t *testing.T) {
	stateDir := t.TempDir()
	restore := api.SetDaemonStateRootForTest(stateDir)
	t.Cleanup(restore)

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
		EmitExitReasonEvent(r, nil)
	}

	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read supervisor-events.log: %v", err)
	}
	body := string(raw)
	for _, r := range reasons {
		needle := `"reason":"` + string(r) + `"`
		if !strings.Contains(body, needle) {
			t.Fatalf("missing row for reason %q; log=%s", r, body)
		}
	}
}
