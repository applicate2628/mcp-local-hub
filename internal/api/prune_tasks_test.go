package api

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeListScheduler is the narrow schedulerLister used by the
// pruneObsoleteServerTasks tests. It keeps per-task XML (so
// ExportXML/Delete/CreateXMLIfAbsent round-trips model the backend) and
// records every Delete/CreateXMLIfAbsent call so tests
// can assert rollback semantics without relying on log output.
type fakeListScheduler struct {
	tasks        map[string][]byte
	listErr      error
	exportErrFor map[string]error
	exportXMLFor map[string][]byte
	deleteErrFor map[string]error
	deletes      []string
	restores     []string
}

func newFakeListScheduler(t *testing.T, tasks map[string][]byte) *fakeListScheduler {
	t.Helper()
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	return &fakeListScheduler{
		tasks:        tasks,
		exportErrFor: map[string]error{},
		exportXMLFor: map[string][]byte{},
		deleteErrFor: map[string]error{},
	}
}

// List mirrors scheduler_windows.go's behavior: strip any leading
// backslash before prefix-matching, but return the raw stored name
// (with the backslash intact) so callers see what the real scheduler
// would return.
func (f *fakeListScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := []scheduler.TaskStatus{}
	for name := range f.tasks {
		if strings.HasPrefix(strings.TrimPrefix(name, "\\"), prefix) {
			out = append(out, scheduler.TaskStatus{Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// resolveTaskKey mirrors Windows schtasks flexibility: names can be
// looked up with or without a leading backslash. The helper strips the
// backslash before calling Delete/ExportXML, so the fake must not
// require callers to preserve it.
func (f *fakeListScheduler) resolveTaskKey(name string) (string, bool) {
	if _, ok := f.tasks[name]; ok {
		return name, true
	}
	if _, ok := f.tasks["\\"+name]; ok {
		return "\\" + name, true
	}
	return "", false
}

func (f *fakeListScheduler) Delete(name string) error {
	if err := f.deleteErrFor[name]; err != nil {
		return err
	}
	if key, ok := f.resolveTaskKey(name); ok {
		delete(f.tasks, key)
	}
	f.deletes = append(f.deletes, name)
	return nil
}

func (f *fakeListScheduler) ExportXML(name string) ([]byte, error) {
	if xml, ok := f.exportXMLFor[name]; ok {
		return xml, f.exportErrFor[name]
	}
	if err := f.exportErrFor[name]; err != nil {
		return nil, err
	}
	if key, ok := f.resolveTaskKey(name); ok {
		return f.tasks[key], nil
	}
	return nil, scheduler.ErrTaskNotFound
}

func (f *fakeListScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	f.restores = append(f.restores, name)
	if _, occupied := f.resolveTaskKey(name); occupied {
		return scheduler.ErrTaskAlreadyExists
	}
	if f.tasks == nil {
		f.tasks = map[string][]byte{}
	}
	f.tasks[name] = bytes.Clone(xml)
	return nil
}

// TestPruneObsoleteServerTasks_RemovesOnlyUnplanned is the core semantic:
// tasks that match the server prefix AND are in the plan must stay;
// tasks that match the prefix but are NOT in the plan must be deleted.
// Tasks outside the server prefix are ignored entirely (List only
// returns prefix matches anyway, but we defensively check in the helper).
func TestPruneObsoleteServerTasks_RemovesOnlyUnplanned(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{
		"mcp-local-hub-serena-claude":         []byte("<xml>claude</xml>"),
		"mcp-local-hub-serena-codex":          []byte("<xml>codex</xml>"),
		"mcp-local-hub-serena-weekly-refresh": []byte("<xml>weekly</xml>"), // obsolete
		"mcp-local-hub-other-daemon":          []byte("<xml>other</xml>"),  // different server
		"unrelated-task":                      []byte("<xml>unrelated</xml>"),
	})
	planned := map[string]struct{}{
		"mcp-local-hub-serena-claude": {},
		"mcp-local-hub-serena-codex":  {},
	}

	var buf bytes.Buffer
	rollbacks, err := pruneObsoleteServerTasks(sch, "serena", planned, &buf)
	if err != nil {
		t.Fatalf("prune: unexpected error: %v", err)
	}
	if len(rollbacks) != 1 {
		t.Fatalf("rollbacks = %d, want 1", len(rollbacks))
	}

	// serena-claude and serena-codex must be untouched.
	if _, ok := sch.tasks["mcp-local-hub-serena-claude"]; !ok {
		t.Error("serena-claude was unexpectedly deleted (in plan)")
	}
	if _, ok := sch.tasks["mcp-local-hub-serena-codex"]; !ok {
		t.Error("serena-codex was unexpectedly deleted (in plan)")
	}
	// weekly-refresh must be gone.
	if _, ok := sch.tasks["mcp-local-hub-serena-weekly-refresh"]; ok {
		t.Error("serena-weekly-refresh was not pruned (obsolete, not in plan)")
	}
	// Different server prefix: untouched (prune scoped to server).
	if _, ok := sch.tasks["mcp-local-hub-other-daemon"]; !ok {
		t.Error("mcp-local-hub-other-daemon was pruned, but it belongs to a different server")
	}
	// User-created unrelated task: untouched (outside prefix entirely).
	if _, ok := sch.tasks["unrelated-task"]; !ok {
		t.Error("unrelated-task was pruned despite not matching our prefix")
	}

	if want := "Scheduler task removed (obsolete): mcp-local-hub-serena-weekly-refresh"; !strings.Contains(buf.String(), want) {
		t.Errorf("output missing prune log line; got: %q", buf.String())
	}
}

// TestPruneObsoleteServerTasks_RollbackRestoresXML exercises the
// rollback path: after prune succeeds, invoking the returned closure
// must recreate the pruned task from its pre-delete XML snapshot. This
// is the piece that makes the helper install-rollback-safe — if a later
// install step fails and the runRollback stack unwinds, obsolete-task
// pruning gets undone together with the rest.
func TestPruneObsoleteServerTasks_RollbackRestoresXML(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{
		"mcp-local-hub-s-weekly-refresh": []byte("<xml>weekly-snapshot</xml>"),
	})
	planned := map[string]struct{}{} // nothing planned -> weekly gets pruned

	rollbacks, err := pruneObsoleteServerTasks(sch, "s", planned, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]; ok {
		t.Fatal("task should have been pruned before rollback runs")
	}

	// Run rollbacks in LIFO order like executeInstallTo would.
	for i := len(rollbacks) - 1; i >= 0; i-- {
		if err := rollbacks[i](); err != nil {
			t.Fatalf("restore pruned task: %v", err)
		}
	}

	got, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]
	if !ok {
		t.Fatal("rollback did not restore the pruned task")
	}
	if string(got) != "<xml>weekly-snapshot</xml>" {
		t.Errorf("rollback restored wrong XML: %q", got)
	}
	if len(sch.restores) != 1 || sch.restores[0] != "mcp-local-hub-s-weekly-refresh" {
		t.Errorf("CreateXMLIfAbsent record = %v, want [mcp-local-hub-s-weekly-refresh]", sch.restores)
	}
}

// A missing or unreliable snapshot must leave its task untouched while the
// next independently exportable task is pruned and remains rollback-capable.
func TestPruneObsoleteServerTasks_SkipsUnavailableSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name      string
		xml       []byte
		exportErr error
	}{
		{name: "export error", exportErr: errors.New("export unavailable")},
		{name: "bytes with error", xml: []byte("<partial/>"), exportErr: errors.New("partial export")},
		{name: "not found after list", exportErr: scheduler.ErrTaskNotFound},
		{name: "nil snapshot"},
		{name: "empty snapshot", xml: []byte{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const skippedName = "mcp-local-hub-s-unavailable"
			const prunedName = "mcp-local-hub-s-zobsolete"
			skippedXML := []byte("<xml>preserve-me</xml>")
			prunedXML := []byte("<xml>restore-me</xml>")
			sch := newFakeListScheduler(t, map[string][]byte{
				skippedName: skippedXML,
				prunedName:  prunedXML,
			})
			sch.exportXMLFor[skippedName] = tt.xml
			sch.exportErrFor[skippedName] = tt.exportErr

			var buf bytes.Buffer
			rollbacks, err := pruneObsoleteServerTasks(sch, "s", map[string]struct{}{}, &buf)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if len(sch.deletes) != 1 || sch.deletes[0] != prunedName {
				t.Fatalf("Delete calls = %v, want only the later task %s", sch.deletes, prunedName)
			}
			if len(rollbacks) != 1 {
				t.Fatalf("rollbacks = %d, want only the pruned task's rollback", len(rollbacks))
			}
			if _, ok := sch.tasks[prunedName]; ok {
				t.Fatal("independently exportable task was not pruned")
			}
			wantCause := "empty XML snapshot"
			if tt.exportErr != nil {
				wantCause = tt.exportErr.Error()
			}
			if !strings.Contains(buf.String(), "Skipped pruning obsolete scheduler task "+skippedName) ||
				!strings.Contains(buf.String(), wantCause) {
				t.Errorf("warning does not identify the skipped task and snapshot failure: %q", buf.String())
			}

			if err := rollbacks[0](); err != nil {
				t.Fatalf("restore pruned task: %v", err)
			}
			if len(sch.restores) != 1 || sch.restores[0] != prunedName {
				t.Fatalf("CreateXMLIfAbsent calls = %v, want only %s", sch.restores, prunedName)
			}
			if got, ok := sch.tasks[skippedName]; !ok || !bytes.Equal(got, skippedXML) {
				t.Errorf("task without a reliable snapshot changed: present=%v XML=%q", ok, got)
			}
			if got, ok := sch.tasks[prunedName]; !ok || !bytes.Equal(got, prunedXML) {
				t.Errorf("later task was not restored exactly: present=%v XML=%q", ok, got)
			}
		})
	}
}

// TestPruneObsoleteServerTasks_ListFailureIsFatal guards against
// silently pruning nothing when the backend cannot enumerate tasks.
// The helper must surface the error so the install path can log a
// clear "reconciliation skipped" warning instead of the install
// appearing to succeed while obsolete tasks keep running.
func TestPruneObsoleteServerTasks_ListFailureIsFatal(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{})
	sch.listErr = errors.New("schtasks /Query exploded")

	_, err := pruneObsoleteServerTasks(sch, "s", map[string]struct{}{}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error when List fails, got nil")
	}
	if !strings.Contains(err.Error(), "schtasks /Query exploded") {
		t.Errorf("error should wrap the underlying cause; got: %v", err)
	}
}

// TestPruneObsoleteServerTasks_DeleteFailureContinuesWithOthers: one
// per-task delete failure must not abort pruning of siblings. The
// helper logs the failure and moves on — an install that pruned 1 of 2
// stale tasks is still an improvement over aborting and pruning 0.
func TestPruneObsoleteServerTasks_DeleteFailureContinuesWithOthers(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{
		"mcp-local-hub-s-weekly-refresh": []byte("<xml>a</xml>"),
		"mcp-local-hub-s-z-stale-debug":  []byte("<xml>b</xml>"),
	})
	sch.deleteErrFor["mcp-local-hub-s-weekly-refresh"] = errors.New("access denied")

	var buf bytes.Buffer
	rollbacks, err := pruneObsoleteServerTasks(sch, "s", map[string]struct{}{}, &buf)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	// One delete succeeded, so one rollback.
	if len(rollbacks) != 1 {
		t.Errorf("rollbacks = %d, want 1", len(rollbacks))
	}
	// Stale task 1 is still present (delete failed).
	if _, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]; !ok {
		t.Error("weekly-refresh should be present (Delete induced to fail)")
	}
	// Stale task 2 is gone (delete succeeded).
	if _, ok := sch.tasks["mcp-local-hub-s-z-stale-debug"]; ok {
		t.Error("stale-debug should have been pruned")
	}
	if !strings.Contains(buf.String(), "Failed to prune obsolete scheduler task") {
		t.Errorf("log should report the failed delete; got: %q", buf.String())
	}
}

// TestPruneObsoleteServerTasks_SerenaWeeklyRefreshFlip is the regression
// scenario named by the reviewer: serena's manifest flipped weekly_refresh
// from true to false, so a reinstall must delete the legacy
// `mcp-local-hub-serena-weekly-refresh` task that prior installs created.
// Without this helper, existing installs would keep running the old weekly
// restart behavior silently.
func TestPruneObsoleteServerTasks_SerenaWeeklyRefreshFlip(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{
		"mcp-local-hub-serena-claude":         []byte("<xml>claude</xml>"),
		"mcp-local-hub-serena-codex":          []byte("<xml>codex</xml>"),
		"mcp-local-hub-serena-weekly-refresh": []byte("<xml>weekly</xml>"),
	})
	// New plan (WeeklyRefresh: false) covers only the two daemons.
	planned := map[string]struct{}{
		"mcp-local-hub-serena-claude": {},
		"mcp-local-hub-serena-codex":  {},
	}

	rollbacks, err := pruneObsoleteServerTasks(sch, "serena", planned, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(rollbacks) != 1 {
		t.Errorf("rollbacks = %d, want 1 (weekly-refresh)", len(rollbacks))
	}
	if _, still := sch.tasks["mcp-local-hub-serena-weekly-refresh"]; still {
		t.Fatal("serena-weekly-refresh must be pruned after weekly_refresh flip")
	}
}

// TestPruneObsoleteServerTasks_WindowsBackslashPrefix: Windows Task
// Scheduler prefixes task names with a backslash (the task-folder
// separator). The helper strips it before prefix/equality checks so
// "\mcp-local-hub-X-weekly-refresh" matches both the prune prefix and
// the planned-set key.
func TestPruneObsoleteServerTasks_WindowsBackslashPrefix(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{
		"\\mcp-local-hub-serena-claude":         []byte("<xml>claude</xml>"),
		"\\mcp-local-hub-serena-weekly-refresh": []byte("<xml>weekly</xml>"),
	})
	planned := map[string]struct{}{"mcp-local-hub-serena-claude": {}}

	_, err := pruneObsoleteServerTasks(sch, "serena", planned, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, still := sch.tasks["\\mcp-local-hub-serena-weekly-refresh"]; still {
		t.Error("leading-backslash task name not recognized — weekly-refresh still present")
	}
	if _, still := sch.tasks["\\mcp-local-hub-serena-claude"]; !still {
		t.Error("leading-backslash planned task was incorrectly pruned")
	}
}
