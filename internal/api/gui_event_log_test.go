// Package api — tests for G9 gui-events.log persistence.

package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendGUIEventLog_HappyPath(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	err := a.AppendGUIEventLog(GUIEventEntry{
		Type:   "daemon-state",
		Source: "poller",
		Body: map[string]any{
			"server": "memory",
			"state":  "Running",
		},
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(root, guiEventLogFileLeaf))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := splitJSONLines(raw)
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d", len(lines))
	}

	var got GUIEventEntry
	if err := json.Unmarshal(lines[0], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != GUIEventLogSchemaVersion {
		t.Errorf("schema_version = %q, want %q", got.SchemaVersion, GUIEventLogSchemaVersion)
	}
	if got.Type != "daemon-state" {
		t.Errorf("type = %q, want daemon-state", got.Type)
	}
	if got.Source != "poller" {
		t.Errorf("source = %q, want poller", got.Source)
	}
	if got.Severity != GUIEventSeverityInfo {
		t.Errorf("severity = %q, want %q (auto-filled default)", got.Severity, GUIEventSeverityInfo)
	}
	if got.TS.IsZero() {
		t.Error("ts is zero — AppendGUIEventLog should auto-fill")
	}
	if got.Body["server"] != "memory" || got.Body["state"] != "Running" {
		t.Errorf("body = %v, want server/state preserved", got.Body)
	}
}

func TestAppendGUIEventLog_MissingType_Rejected(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	err := a.AppendGUIEventLog(GUIEventEntry{
		Source: "poller",
		Body:   map[string]any{"x": 1},
	})
	if !errors.Is(err, ErrGUIEventLogMissingType) {
		t.Errorf("got %v, want ErrGUIEventLogMissingType", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, guiEventLogFileLeaf)); !os.IsNotExist(statErr) {
		t.Errorf("missing-type append should not create log file; stat = %v", statErr)
	}
}

func TestAppendGUIEventLog_PreservesCallerTS(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	fixed := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	if err := a.AppendGUIEventLog(GUIEventEntry{
		Type: "test",
		TS:   fixed,
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	tail := a.ReadGUIEventLogTail(1)
	if len(tail) != 1 {
		t.Fatalf("want 1 entry, got %d", len(tail))
	}
	if !tail[0].TS.Equal(fixed) {
		t.Errorf("ts = %v, want %v (caller TS must be preserved, not overwritten)", tail[0].TS, fixed)
	}
}

func TestAppendGUIEventLog_CallerSchemaVersionOverride(t *testing.T) {
	// Future migration scenario: a caller writes a v2 entry that the
	// current binary marshals through but didn't auto-fill.
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	if err := a.AppendGUIEventLog(GUIEventEntry{
		Type:          "test",
		SchemaVersion: "2",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	tail := a.ReadGUIEventLogTail(1)
	if tail[0].SchemaVersion != "2" {
		t.Errorf("schema_version = %q, want 2 (caller override must survive)", tail[0].SchemaVersion)
	}
}

func TestAppendGUIEventLog_Rotation(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	path := filepath.Join(root, guiEventLogFileLeaf)
	rotated := path + guiEventLogRotatedSuffix

	// Seed file at exactly the rotation threshold so the NEXT append
	// triggers the rotation branch.
	if err := os.WriteFile(path, []byte(strings.Repeat("x", int(GUIEventLogRotateSizeBytes))), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := a.AppendGUIEventLog(GUIEventEntry{
		Type: "test-rotate",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// .log.1 should now exist with the seeded contents.
	rotBytes, err := os.ReadFile(rotated)
	if err != nil {
		t.Fatalf("read rotated: %v", err)
	}
	if int64(len(rotBytes)) != GUIEventLogRotateSizeBytes {
		t.Errorf(".log.1 size = %d, want %d (seeded)", len(rotBytes), GUIEventLogRotateSizeBytes)
	}
	// Active log should contain only the new entry.
	tail := a.ReadGUIEventLogTail(10)
	if len(tail) != 1 || tail[0].Type != "test-rotate" {
		t.Errorf("active log tail = %+v, want single test-rotate entry", tail)
	}
}

func TestReadGUIEventLogTail_LimitedToN(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	for i := 0; i < 5; i++ {
		if err := a.AppendGUIEventLog(GUIEventEntry{
			Type: "test",
			Body: map[string]any{"i": i},
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	tail := a.ReadGUIEventLogTail(3)
	if len(tail) != 3 {
		t.Fatalf("tail len = %d, want 3", len(tail))
	}
	// Tail is the last 3 of 5 appends → i=2,3,4.
	for k, want := range []float64{2, 3, 4} {
		got := tail[k].Body["i"].(float64)
		if got != want {
			t.Errorf("tail[%d].body.i = %v, want %v", k, got, want)
		}
	}
}

func TestReadGUIEventLogTail_NoFile_EmptySlice(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	tail := a.ReadGUIEventLogTail(10)
	if len(tail) != 0 {
		t.Errorf("tail on missing file = %d entries, want 0", len(tail))
	}
	// Codex P3 on PR #150 line 236: non-happy paths must return an
	// empty slice (not nil) so JSON encoders emit `[]` not `null`.
	if tail == nil {
		t.Errorf("tail on missing file is nil; want []GUIEventEntry{} so JSON renders as []")
	}
}

// TestReadGUIEventLogTail_ZeroN_EmptySlice guards Codex P3 on PR #150
// line 236 for the n<=0 branch.
func TestReadGUIEventLogTail_ZeroN_EmptySlice(t *testing.T) {
	root := t.TempDir()
	restore := SetDaemonStateRootForTest(root)
	t.Cleanup(restore)

	a := NewAPI()
	if tail := a.ReadGUIEventLogTail(0); tail == nil {
		t.Errorf("n=0 returned nil; want []GUIEventEntry{}")
	}
	if tail := a.ReadGUIEventLogTail(-5); tail == nil {
		t.Errorf("n=-5 returned nil; want []GUIEventEntry{}")
	}
}
