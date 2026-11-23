package api

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// Exercise the public per-server upgrade and its workspace-proxy branch with
// private manifest/registry paths and an in-memory Scheduler. An untrusted
// snapshot must preserve the original task; observed absence must not authorize
// Delete, including when a competing task occupies the name before Create.
func TestSchedulerUpgradeSnapshotAdmission(t *testing.T) {
	priorXML := []byte("<Task>original</Task>")
	externalXML := []byte("<Task>external replacement</Task>")
	for _, kind := range []string{"server", "lazy_proxy"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				snapshot  []byte
				exportErr error
				initial   []byte
				createErr error
				deleteErr error
				wantErr   string
				refused   bool
				uncertain bool
				deletes   int
				creates   int
			}{
				{name: "permission_denied", exportErr: scheduler.ErrPermissionDenied, initial: priorXML, createErr: scheduler.ErrUnavailable, wantErr: "export: " + scheduler.ErrPermissionDenied.Error()},
				{name: "unavailable", exportErr: scheduler.ErrUnavailable, initial: priorXML, wantErr: "export: " + scheduler.ErrUnavailable.Error()},
				{name: "canceled", exportErr: context.Canceled, initial: priorXML, wantErr: "export: " + context.Canceled.Error()},
				{name: "partial_failure", snapshot: []byte("<Task>partial"), exportErr: scheduler.ErrPermissionDenied, initial: priorXML, wantErr: "export: " + scheduler.ErrPermissionDenied.Error()},
				{name: "unsupported", exportErr: scheduler.ErrNotImplemented, initial: priorXML, wantErr: "export: " + scheduler.ErrNotImplemented.Error()},
				{name: "nil_success", initial: priorXML, wantErr: "export: " + scheduler.ErrTaskCorrupt.Error() + ": empty XML snapshot"},
				{name: "empty_success", snapshot: []byte{}, initial: priorXML, wantErr: "export: " + scheduler.ErrTaskCorrupt.Error() + ": empty XML snapshot"},
				{name: "conflicting_absence", snapshot: priorXML, exportErr: scheduler.ErrTaskNotFound, initial: priorXML, wantErr: "export: " + scheduler.ErrTaskNotFound.Error()},
				{name: "existing", snapshot: priorXML, initial: priorXML, refused: true},
				{name: "absent", exportErr: scheduler.ErrTaskNotFound, creates: 1},
				{name: "wrapped_absent", exportErr: fmt.Errorf("lookup: %w", scheduler.ErrTaskNotFound), creates: 1},
				{name: "absent_conflict", exportErr: scheduler.ErrTaskNotFound, initial: externalXML, wantErr: "create: " + scheduler.ErrTaskAlreadyExists.Error(), creates: 1, uncertain: true},
				{name: "unreached_delete_denial", snapshot: priorXML, initial: priorXML, deleteErr: scheduler.ErrPermissionDenied, refused: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
					name := `\mcp-local-hub-upgradesnapshot-alpha`
					if kind == "lazy_proxy" {
						name = `\mcp-local-hub-lsp-snapshot-python`
					}
					f := &upgradeSnapshotScheduler{
						t: t, name: name, snapshot: tc.snapshot, exportErr: tc.exportErr,
						createErr: tc.createErr, deleteErr: tc.deleteErr,
						xml: make(map[string][]byte), deletes: make(map[string]int),
						creates: make(map[string]int), imports: make(map[string]int),
					}
					if tc.initial != nil {
						f.xml[name] = bytes.Clone(tc.initial)
					}
					canonical := filepath.Join(t.TempDir(), mcphubShortName)
					var result SchedulerUpgradeResult
					if kind == "server" {
						result = runServerUpgradeSnapshotFixture(t, f, canonical)
					} else {
						bare := strings.TrimPrefix(name, `\`)
						got := upgradeLazyProxyTask(f, name, bare, canonical, map[string]WorkspaceEntry{
							bare: {WorkspacePath: filepath.Join(t.TempDir(), "workspace"), Language: "python", Port: 19321},
						})
						if got == nil {
							t.Fatal("missing workspace-proxy upgrade result")
						}
						result = *got
					}
					matchesError := result.Err == tc.wantErr
					if tc.refused {
						matchesError = strings.Contains(result.Err, ErrSchedulerReplacementUnowned.Error()) && strings.Contains(result.Err, "snapshot retained at")
					} else if tc.uncertain {
						matchesError = strings.Contains(result.Err, tc.wantErr) && strings.Contains(result.Err, "rollback incomplete")
					}
					if result.TaskName != name || !matchesError {
						t.Errorf("upgrade result = %+v, want task %q error %q", result, name, tc.wantErr)
					}
					if f.deletes[name] != tc.deletes || f.creates[name] != tc.creates || f.imports[name] != 0 {
						t.Errorf("task mutations delete/create/import = %d/%d/%d, want %d/%d/0", f.deletes[name], f.creates[name], f.imports[name], tc.deletes, tc.creates)
					}
					if tc.wantErr != "" || tc.refused {
						if result.NewCmd != "" || !bytes.Equal(f.xml[name], tc.initial) {
							t.Errorf("refused upgrade lost the original task or reported success: result=%+v current=%q want=%q", result, f.xml[name], tc.initial)
						}
					} else if result.NewCmd != canonical || !bytes.Equal(f.xml[name], []byte("<Task>upgraded</Task>")) {
						t.Errorf("admitted upgrade did not install the new task: result=%+v current=%q", result, f.xml[name])
					}
				})
			}
		})
	}
}

func runServerUpgradeSnapshotFixture(t *testing.T, f *upgradeSnapshotScheduler, canonical string) SchedulerUpgradeResult {
	t.Helper()
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	seedInstalledServerManifestsWithDaemons(t, map[string][]config.DaemonSpec{
		"upgradesnapshot": {{Name: "alpha", Port: 19321}, {Name: "beta", Port: 19322}},
	})
	if err := os.WriteFile(canonical, []byte("stub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(SetTestCanonicalMcphubPath(canonical))
	previousScheduler, previousRegistry := schedulerFactoryFn, defaultRegistryPathFn
	registryPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return f, nil }
	defaultRegistryPathFn = func() (string, error) { return registryPath, nil }
	t.Cleanup(func() {
		schedulerFactoryFn, defaultRegistryPathFn = previousScheduler, previousRegistry
	})
	const sibling = `\mcp-local-hub-upgradesnapshot-beta`
	// A listed sibling can disappear before its export. Its clean absence
	// remains independently eligible for Create after the first row is refused.
	f.tasks = []scheduler.TaskStatus{{Name: f.name}, {Name: sibling}}
	results, err := NewAPI().SchedulerUpgrade()
	if err != nil || len(results) != 2 {
		t.Fatalf("SchedulerUpgrade = %+v, %v; want both per-task results", results, err)
	}
	if results[1].TaskName != sibling || results[1].Err != "" || results[1].NewCmd != canonical ||
		f.creates[sibling] != 1 || f.deletes[sibling] != 0 || !bytes.Equal(f.xml[sibling], []byte("<Task>upgraded</Task>")) {
		t.Errorf("snapshot refusal blocked independent fresh creation: result=%+v current=%q", results[1], f.xml[sibling])
	}
	return results[0]
}

// This fake models Create's documented conflict refusal. All task state lives
// in memory; unexpected runtime actions fail immediately instead of falling
// through to the native scheduler, a process query, or a network listener.
type upgradeSnapshotScheduler struct {
	t             *testing.T
	name          string
	snapshot      []byte
	exportErr     error
	createErr     error
	deleteErr     error
	xml           map[string][]byte
	tasks         []scheduler.TaskStatus
	deletes       map[string]int
	creates       map[string]int
	imports       map[string]int
	createWrites  []byte
	restoreWrites []byte
	restoreErr    error
	restores      int
	beforeDelete  func(string)
	beforeRestore func(string)
	exports       int
	reexport      func() ([]byte, error)
}

func (f *upgradeSnapshotScheduler) ExportXML(name string) ([]byte, error) {
	if name == f.name {
		f.exports++
		if (f.exports > 1 || f.creates[name] > 0) && f.reexport != nil {
			return f.reexport()
		}
		if f.creates[name] > 0 {
			if raw, ok := f.xml[name]; ok {
				return bytes.Clone(raw), nil
			}
			return nil, scheduler.ErrTaskNotFound
		}
		return f.snapshot, f.exportErr
	}
	if xml, ok := f.xml[name]; ok {
		return bytes.Clone(xml), nil
	}
	return nil, scheduler.ErrTaskNotFound
}

func (f *upgradeSnapshotScheduler) Delete(name string) error {
	f.deletes[name]++
	if f.beforeDelete != nil {
		f.beforeDelete(name)
	}
	if name == f.name && f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.xml, name)
	return nil
}

func (f *upgradeSnapshotScheduler) Create(spec scheduler.TaskSpec) error {
	f.creates[spec.Name]++
	if spec.Name == f.name && f.createErr != nil {
		if f.createWrites != nil {
			f.xml[spec.Name] = bytes.Clone(f.createWrites)
		}
		return f.createErr
	}
	if _, exists := f.xml[spec.Name]; exists {
		return scheduler.ErrTaskAlreadyExists
	}
	f.xml[spec.Name] = []byte("<Task>upgraded</Task>")
	return nil
}

func (f *upgradeSnapshotScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	f.restores++
	if f.beforeRestore != nil {
		f.beforeRestore(name)
	}
	if _, exists := f.xml[name]; exists {
		return scheduler.ErrTaskAlreadyExists
	}
	if f.restoreWrites != nil {
		f.xml[name] = bytes.Clone(f.restoreWrites)
	}
	if f.restoreErr != nil {
		return f.restoreErr
	}
	f.xml[name] = bytes.Clone(xml)
	return nil
}

func (f *upgradeSnapshotScheduler) ImportXML(name string, xml []byte) error {
	f.imports[name]++
	f.xml[name] = bytes.Clone(xml)
	return nil
}

func (f *upgradeSnapshotScheduler) List(prefix string) ([]scheduler.TaskStatus, error) {
	if prefix != "mcp-local-hub-" {
		f.t.Fatalf("unexpected task prefix %q", prefix)
	}
	return f.tasks, nil
}

func (f *upgradeSnapshotScheduler) Run(name string) error {
	f.t.Fatalf("unexpected Run(%q)", name)
	return nil
}

func (f *upgradeSnapshotScheduler) Stop(name string) error {
	f.t.Fatalf("unexpected Stop(%q)", name)
	return nil
}

func (f *upgradeSnapshotScheduler) Status(name string) (scheduler.TaskStatus, error) {
	f.t.Fatalf("unexpected Status(%q)", name)
	return scheduler.TaskStatus{}, nil
}
