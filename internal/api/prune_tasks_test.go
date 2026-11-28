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
// pruneObsoleteServerTasks tests. It keeps per-task XML and records attempted
// Delete/CreateXMLIfAbsent calls to distinguish refusal from compensation.
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

// Obsolete-task classification is still exact even though deletion is refused.
// Planned, unrelated and sibling tasks must not acquire warnings or snapshots.
func TestPruneObsoleteServerTasks_ReportsOnlyUnplanned(t *testing.T) {
	const obsolete = "mcp-local-hub-serena-weekly-refresh"
	sch := newFakeListScheduler(t, map[string][]byte{
		"mcp-local-hub-serena-claude": []byte("<xml>claude</xml>"),
		"mcp-local-hub-serena-codex":  []byte("<xml>codex</xml>"),
		obsolete:                      []byte("<xml>weekly</xml>"),
		"mcp-local-hub-other-daemon":  []byte("<xml>other</xml>"),
		"unrelated-task":              []byte("<xml>unrelated</xml>"),
	})
	planned := map[string]struct{}{"mcp-local-hub-serena-claude": {}, "mcp-local-hub-serena-codex": {}}
	var output bytes.Buffer
	if err := pruneObsoleteServerTasks(sch, "serena", planned, &output); err != nil {
		t.Fatal(err)
	}
	if len(sch.tasks) != 5 || len(sch.deletes) != 0 || len(sch.restores) != 0 {
		t.Fatalf("prune changed registrations: %v deletes=%v", sch.tasks, sch.deletes)
	}
	if strings.Count(output.String(), "Skipped pruning obsolete scheduler task ") != 1 || !strings.Contains(output.String(), obsolete) || !strings.Contains(output.String(), ErrSchedulerReplacementUnowned.Error()) {
		t.Fatalf("classification warning: %s", output.String())
	}
}

func TestPruneObsoleteServerTasks_SkipsUnavailableSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name string
		xml  []byte
		err  error
	}{
		{"export error", nil, errors.New("export unavailable")},
		{"bytes with error", []byte("<partial/>"), errors.New("partial export")},
		{"not found after list", nil, scheduler.ErrTaskNotFound},
		{"nil snapshot", nil, nil}, {"empty snapshot", []byte{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const unavailable, later = "mcp-local-hub-s-unavailable", "mcp-local-hub-s-zobsolete"
			prior := []byte("<xml>preserve</xml>")
			sch := newFakeListScheduler(t, map[string][]byte{unavailable: bytes.Clone(prior), later: bytes.Clone(prior)})
			sch.exportXMLFor[unavailable], sch.exportErrFor[unavailable] = tc.xml, tc.err
			var output bytes.Buffer
			if err := pruneObsoleteServerTasks(sch, "s", nil, &output); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(sch.tasks[unavailable], prior) || !bytes.Equal(sch.tasks[later], prior) || len(sch.deletes) != 0 || len(sch.restores) != 0 {
				t.Fatalf("snapshot error allowed mutation: deletes=%v", sch.deletes)
			}
			cause := "empty XML snapshot"
			if tc.err != nil {
				cause = tc.err.Error()
			}
			if !strings.Contains(output.String(), cause) || !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+unavailable) || !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+later) {
				t.Fatalf("warning lost cause or later task: %s", output.String())
			}
		})
	}
}

func TestPruneObsoleteServerTasks_ListFailureIsFatal(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{"mcp-local-hub-s-default": []byte("<Task/>")})
	cause := errors.New("fixture: list unavailable")
	sch.listErr = cause
	var output bytes.Buffer
	err := pruneObsoleteServerTasks(sch, "s", nil, &output)
	if !errors.Is(err, cause) || len(sch.tasks) != 1 || len(sch.deletes) != 0 || len(sch.restores) != 0 || output.Len() != 0 {
		t.Fatalf("list failure: %v tasks=%v output=%s", err, sch.tasks, output.String())
	}
}

func TestPruneObsoleteServerTasks_SerenaWeeklyRefreshFlip(t *testing.T) {
	const obsolete = "mcp-local-hub-serena-weekly-refresh"
	sch := newFakeListScheduler(t, map[string][]byte{obsolete: []byte("<xml>weekly</xml>"), "mcp-local-hub-serena-codex": []byte("<xml>codex</xml>")})
	var output bytes.Buffer
	if err := pruneObsoleteServerTasks(sch, "serena", map[string]struct{}{"mcp-local-hub-serena-codex": {}}, &output); err != nil {
		t.Fatal(err)
	}
	if len(sch.tasks) != 2 || len(sch.deletes) != 0 || len(sch.restores) != 0 || !strings.Contains(output.String(), obsolete) || !strings.Contains(output.String(), ErrSchedulerReplacementUnowned.Error()) {
		t.Fatalf("obsolete weekly task was silently removed or retained: %s", output.String())
	}
}

func TestPruneObsoleteServerTasks_WindowsBackslashPrefix(t *testing.T) {
	sch := newFakeListScheduler(t, map[string][]byte{`\mcp-local-hub-serena-claude`: []byte("<xml>claude</xml>"), `\mcp-local-hub-serena-weekly-refresh`: []byte("<xml>weekly</xml>")})
	var output bytes.Buffer
	if err := pruneObsoleteServerTasks(sch, "serena", map[string]struct{}{"mcp-local-hub-serena-claude": {}}, &output); err != nil {
		t.Fatal(err)
	}
	if len(sch.tasks) != 2 || len(sch.deletes) != 0 || len(sch.restores) != 0 || strings.Count(output.String(), "Skipped pruning obsolete scheduler task ") != 1 || !strings.Contains(output.String(), "mcp-local-hub-serena-weekly-refresh") || strings.Contains(output.String(), "mcp-local-hub-serena-claude") {
		t.Fatalf("leading-backslash classification: %s", output.String())
	}
}
