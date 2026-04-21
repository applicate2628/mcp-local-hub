package api

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// fakeListScheduler is the narrow schedulerLister used by the
// pruneObsoleteServerTasks tests. It keeps per-task XML (so
// ExportXML/Delete/ImportXML round-trips behave like the real
// Task Scheduler) and records every Delete/ImportXML call so tests
// can assert rollback semantics without relying on log output.
type fakeListScheduler struct {
	tasks        map[string][]byte
	listErr      error
	exportErrFor map[string]error
	deleteErrFor map[string]error
	deletes      []string
	imports      []string
}

func newFakeListScheduler(tasks map[string][]byte) *fakeListScheduler {
	return &fakeListScheduler{
		tasks:        tasks,
		exportErrFor: map[string]error{},
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
	if err := f.exportErrFor[name]; err != nil {
		return nil, err
	}
	if key, ok := f.resolveTaskKey(name); ok {
		return f.tasks[key], nil
	}
	return nil, scheduler.ErrTaskNotFound
}

func (f *fakeListScheduler) ImportXML(name string, xml []byte) error {
	if f.tasks == nil {
		f.tasks = map[string][]byte{}
	}
	f.tasks[name] = xml
	f.imports = append(f.imports, name)
	return nil
}

// TestPruneObsoleteServerTasks_RemovesOnlyUnplanned is the core semantic:
// tasks that match the server prefix AND are in the plan must stay;
// tasks that match the prefix but are NOT in the plan must be deleted.
// Tasks outside the server prefix are ignored entirely (List only
// returns prefix matches anyway, but we defensively check in the helper).
func TestPruneObsoleteServerTasks_RemovesOnlyUnplanned(t *testing.T) {
	sch := newFakeListScheduler(map[string][]byte{
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
	sch := newFakeListScheduler(map[string][]byte{
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
		rollbacks[i]()
	}

	got, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]
	if !ok {
		t.Fatal("rollback did not restore the pruned task")
	}
	if string(got) != "<xml>weekly-snapshot</xml>" {
		t.Errorf("rollback restored wrong XML: %q", got)
	}
	if len(sch.imports) != 1 || sch.imports[0] != "mcp-local-hub-s-weekly-refresh" {
		t.Errorf("ImportXML record = %v, want [mcp-local-hub-s-weekly-refresh]", sch.imports)
	}
}

// TestPruneObsoleteServerTasks_RollbackNoopWhenExportFailed covers the
// degraded-but-safe path: if ExportXML failed before the Delete (e.g.
// platform doesn't support export), Delete still runs so the stale task
// does not keep executing, but rollback has no XML to restore from. The
// rollback closure must tolerate that and log rather than panic or
// silently succeed.
func TestPruneObsoleteServerTasks_RollbackNoopWhenExportFailed(t *testing.T) {
	sch := newFakeListScheduler(map[string][]byte{
		"mcp-local-hub-s-weekly-refresh": []byte("<xml>w</xml>"),
	})
	sch.exportErrFor["mcp-local-hub-s-weekly-refresh"] = errors.New("export not supported on this platform")

	var buf bytes.Buffer
	rollbacks, err := pruneObsoleteServerTasks(sch, "s", map[string]struct{}{}, &buf)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]; ok {
		t.Fatal("task should have been pruned even when ExportXML failed")
	}

	buf.Reset()
	for _, rb := range rollbacks {
		rb()
	}
	// Rollback must not restore a task without a snapshot.
	if _, ok := sch.tasks["mcp-local-hub-s-weekly-refresh"]; ok {
		t.Error("rollback recreated task despite no XML snapshot available")
	}
	if !strings.Contains(buf.String(), "no XML snapshot") {
		t.Errorf("rollback log should mention missing snapshot; got: %q", buf.String())
	}
}

// TestPruneObsoleteServerTasks_ListFailureIsFatal guards against
// silently pruning nothing when the backend cannot enumerate tasks.
// The helper must surface the error so the install path can log a
// clear "reconciliation skipped" warning instead of the install
// appearing to succeed while obsolete tasks keep running.
func TestPruneObsoleteServerTasks_ListFailureIsFatal(t *testing.T) {
	sch := newFakeListScheduler(map[string][]byte{})
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
	sch := newFakeListScheduler(map[string][]byte{
		"mcp-local-hub-s-weekly-refresh": []byte("<xml>a</xml>"),
		"mcp-local-hub-s-stale-debug":    []byte("<xml>b</xml>"),
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
	if _, ok := sch.tasks["mcp-local-hub-s-stale-debug"]; ok {
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
	sch := newFakeListScheduler(map[string][]byte{
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
	sch := newFakeListScheduler(map[string][]byte{
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
