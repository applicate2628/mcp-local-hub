package api

import (
	"fmt"
	"strings"
	"testing"
)

// TestWeeklyRefreshAll_RestartsEnabledEntries seeds three registry rows and
// asserts WeeklyRefreshAll restarts only the two with WeeklyRefresh=true.
func TestWeeklyRefreshAll_RestartsEnabledEntries(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	reg := NewRegistry(h.regPath)
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", TaskName: "tA", WeeklyRefresh: true, Port: 9200, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "typescript", TaskName: "tB", WeeklyRefresh: false, Port: 9201, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "c", Language: "rust", TaskName: "tC", WeeklyRefresh: true, Port: 9202, Backend: "mcp-language-server"})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	report, err := a.WeeklyRefreshAll()
	if err != nil {
		t.Fatalf("WeeklyRefreshAll: %v", err)
	}
	if len(report.Restarted) != 2 {
		t.Fatalf("expected 2 restarts, got %d: %+v", len(report.Restarted), report.Restarted)
	}
	wantSet := map[string]bool{"tA": true, "tC": true}
	for _, r := range report.Restarted {
		if !wantSet[r] {
			t.Errorf("unexpected task restarted: %s", r)
		}
	}
}

// TestWeeklyRefreshAll_SkipsDisabledEntries verifies an all-disabled registry
// produces an empty Restarted list with no errors.
func TestWeeklyRefreshAll_SkipsDisabledEntries(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	reg := NewRegistry(h.regPath)
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", TaskName: "tA", WeeklyRefresh: false, Port: 9200, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "typescript", TaskName: "tB", WeeklyRefresh: false, Port: 9201, Backend: "mcp-language-server"})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	a := NewAPI()
	report, err := a.WeeklyRefreshAll()
	if err != nil {
		t.Fatalf("WeeklyRefreshAll: %v", err)
	}
	if len(report.Restarted) != 0 {
		t.Errorf("expected 0 restarts (all disabled), got %d: %+v", len(report.Restarted), report.Restarted)
	}
	if len(report.Warnings) != 0 {
		t.Errorf("expected 0 warnings, got %d: %+v", len(report.Warnings), report.Warnings)
	}
}

// TestWeeklyRefreshAll_AggregatesPartialFailures verifies per-entry failures
// (sch.Run returning an error) are captured in Warnings and do not abort the
// remaining entries.
func TestWeeklyRefreshAll_AggregatesPartialFailures(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	// Seed three enabled entries. Middle one's Run call will fail.
	reg := NewRegistry(h.regPath)
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", TaskName: "tA", WeeklyRefresh: true, Port: 9200, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "typescript", TaskName: "tB", WeeklyRefresh: true, Port: 9201, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "c", Language: "rust", TaskName: "tC", WeeklyRefresh: true, Port: 9202, Backend: "mcp-language-server"})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	h.fakeSch.failRunForTask = "tB"
	a := NewAPI()
	report, err := a.WeeklyRefreshAll()
	if err != nil {
		t.Fatalf("WeeklyRefreshAll: %v", err)
	}
	if len(report.Restarted) != 2 {
		t.Fatalf("expected 2 successful restarts, got %d: %+v", len(report.Restarted), report.Restarted)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("expected at least 1 warning for tB failure")
	}
	joined := strings.Join(report.Warnings, "\n")
	if !strings.Contains(joined, "tB") {
		t.Errorf("warnings should mention tB: %s", joined)
	}
}

// TestEnsureWeeklyRefreshTask_Idempotent verifies EnsureWeeklyRefreshTask can
// be called repeatedly without error and produces exactly one shared task.
func TestEnsureWeeklyRefreshTask_Idempotent(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	a := NewAPI()
	if err := a.EnsureWeeklyRefreshTask(); err != nil {
		t.Fatalf("first EnsureWeeklyRefreshTask: %v", err)
	}
	if err := a.EnsureWeeklyRefreshTask(); err != nil {
		t.Fatalf("second EnsureWeeklyRefreshTask: %v", err)
	}
	// Exactly one shared task by the expected name.
	count := 0
	var spec string
	for _, s := range h.fakeSch.createdSpecs {
		if s.Name == WeeklyRefreshTaskName {
			count++
			spec = fmt.Sprintf("%+v", s.Args)
		}
	}
	if count != 2 {
		// fakeScheduler tracks every Create call; the second Ensure should
		// Delete-then-Create again, so we expect 2 Create entries.
		t.Errorf("expected 2 Create calls for %s (idempotent replace), got %d", WeeklyRefreshTaskName, count)
	}
	// The args should invoke the hidden `workspace-weekly-refresh` CLI path
	// that M5 Task 17 will wire. Until M5 this task fails at runtime; that
	// is deliberate — creating the task now lets the shared trigger exist
	// so a single install-time setup creates all scheduler state.
	if !strings.Contains(spec, "workspace-weekly-refresh") {
		t.Errorf("task args %s should invoke workspace-weekly-refresh", spec)
	}
}
