package api

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
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

// TestWeeklyRefreshAll_KillsStaleProxyBeforeRun verifies each enabled entry's
// live proxy is killed by port BEFORE sch.Run is invoked. Without this
// kill-then-run sequence, Task Scheduler's MultipleInstancesPolicy=IgnoreNew
// makes a second Run on an already-running task a no-op, leaving the stale
// proxy in place.
func TestWeeklyRefreshAll_KillsStaleProxyBeforeRun(t *testing.T) {
	h := newRegisterHarness(t)
	defer h.restore()

	origKill := killByPortFn
	defer func() { killByPortFn = origKill }()
	// Record each (taskName, action, tIndex) tuple so we can assert ordering.
	var mu sync.Mutex
	var events []string
	killByPortFn = func(port int, timeout time.Duration) error {
		mu.Lock()
		events = append(events, fmt.Sprintf("kill:%d", port))
		mu.Unlock()
		return nil
	}

	reg := NewRegistry(h.regPath)
	reg.Put(WorkspaceEntry{WorkspaceKey: "a", Language: "python", TaskName: "tA", WeeklyRefresh: true, Port: 9200, Backend: "mcp-language-server"})
	reg.Put(WorkspaceEntry{WorkspaceKey: "b", Language: "rust", TaskName: "tB", WeeklyRefresh: true, Port: 9201, Backend: "mcp-language-server"})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	// Wrap the harness scheduler factory so Run invocations are captured
	// into the same event slice. The existing fake's Run already records
	// runNames; we additionally mirror those into events with a prefix so
	// we can test interleaving against kill events.
	origFactory := testSchedulerFactory
	recordingSch := &recordingScheduler{inner: h.fakeSch, onRun: func(name string) {
		mu.Lock()
		events = append(events, "run:"+name)
		mu.Unlock()
	}}
	testSchedulerFactory = func() (testScheduler, error) { return recordingSch, nil }
	defer func() { testSchedulerFactory = origFactory }()

	a := NewAPI()
	report, err := a.WeeklyRefreshAll()
	if err != nil {
		t.Fatalf("WeeklyRefreshAll: %v", err)
	}
	if len(report.Restarted) != 2 {
		t.Fatalf("expected 2 restarts, got %d: %+v", len(report.Restarted), report.Restarted)
	}
	// Each entry must kill BEFORE run. Iterate through events in order and
	// require every kill event for a port precedes the run event for the
	// matching task name.
	portForTask := map[string]int{"tA": 9200, "tB": 9201}
	for task, port := range portForTask {
		killIdx, runIdx := -1, -1
		for i, e := range events {
			if e == fmt.Sprintf("kill:%d", port) && killIdx < 0 {
				killIdx = i
			}
			if e == "run:"+task && runIdx < 0 {
				runIdx = i
			}
		}
		if killIdx < 0 || runIdx < 0 {
			t.Fatalf("missing kill/run events for %s: %v", task, events)
		}
		if killIdx >= runIdx {
			t.Errorf("kill for port %d must precede run for task %s; events=%v",
				port, task, events)
		}
	}
}

// recordingScheduler wraps a testScheduler and fires a callback on Run so
// tests can observe scheduler call ordering without modifying the shared
// fakeScheduler.
type recordingScheduler struct {
	inner testScheduler
	onRun func(name string)
}

func (r *recordingScheduler) Create(spec scheduler.TaskSpec) error { return r.inner.Create(spec) }
func (r *recordingScheduler) Delete(name string) error             { return r.inner.Delete(name) }
func (r *recordingScheduler) Run(name string) error {
	if r.onRun != nil {
		r.onRun(name)
	}
	return r.inner.Run(name)
}
func (r *recordingScheduler) ExportXML(name string) ([]byte, error) {
	return r.inner.ExportXML(name)
}
func (r *recordingScheduler) ImportXML(name string, xml []byte) error {
	return r.inner.ImportXML(name, xml)
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
