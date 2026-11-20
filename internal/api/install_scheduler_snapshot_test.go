package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

type installSnapshotScheduler struct {
	*fakeScheduler
	name      string
	snapshot  []byte
	exportErr error
	deleteErr error
}

func (s *installSnapshotScheduler) ExportXML(name string) ([]byte, error) {
	if name == s.name {
		return s.snapshot, s.exportErr
	}
	return s.fakeScheduler.ExportXML(name)
}

func (s *installSnapshotScheduler) Delete(name string) error {
	if name == s.name && s.deleteErr != nil {
		s.deleteNames = append(s.deleteNames, name)
		return s.deleteErr
	}
	return s.fakeScheduler.Delete(name)
}

func installSnapshotPlan(t *testing.T, s *installSnapshotScheduler, names ...string) *Plan {
	t.Helper()
	t.Cleanup(SetDaemonStateRootForTest(hardenedTempDir(t)))
	prior := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return s, nil }
	t.Cleanup(func() { schedulerFactoryFn = prior })
	p := &Plan{installRelayExePath: filepath.Join(t.TempDir(), "mcphub.exe")}
	for _, name := range names {
		p.SchedulerTasks = append(p.SchedulerTasks, ScheduledTaskPlan{Name: name, Command: p.installRelayExePath})
	}
	return p
}

func TestExecuteInstallToSnapshotFailurePreservesTask(t *testing.T) {
	const name = "mcp-local-hub-snapshot-default"
	priorXML := []byte("<Task>original</Task>")
	for _, tc := range []struct {
		name     string
		snapshot []byte
		cause    error
	}{
		{"unavailable", nil, scheduler.ErrUnavailable},
		{"partial_permission_error", []byte("<Task>partial"), scheduler.ErrPermissionDenied},
		{"unsupported", nil, scheduler.ErrNotImplemented},
		{"nil_success", nil, nil},
		{"empty_success", []byte{}, nil},
		{"conflicting_absence", priorXML, scheduler.ErrTaskNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFakeScheduler()
			f.tasks[name], f.xml[name] = true, priorXML
			s := &installSnapshotScheduler{fakeScheduler: f, name: name, snapshot: tc.snapshot, exportErr: tc.cause}
			plan := installSnapshotPlan(t, s, name)
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "snapshot"}, plan, 0, false, nil, true, false)
			if err == nil || !strings.Contains(err.Error(), "snapshot task "+name) || (tc.cause != nil && !errors.Is(err, tc.cause)) {
				t.Errorf("snapshot refusal = %v, want named error preserving %v", err, tc.cause)
			}
			if !f.tasks[name] || !bytes.Equal(f.xml[name], priorXML) || len(f.deleteNames) != 0 || f.createCount != 0 || len(f.importNames) != 0 || f.runCount != 0 {
				t.Errorf("unsafe snapshot changed task: tasks=%v xml=%q delete=%v create=%d import=%v run=%d", f.tasks, f.xml[name], f.deleteNames, f.createCount, f.importNames, f.runCount)
			}
			if strings.Contains(output.String(), "Scheduler task created") || strings.Contains(output.String(), "Install complete") {
				t.Errorf("snapshot refusal reported success: %s", output.String())
			}
		})
	}
}

func TestExecuteInstallToAbsentSnapshotCreatesWithoutDelete(t *testing.T) {
	const name = "mcp-local-hub-snapshot-new"
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"direct", scheduler.ErrTaskNotFound},
		{"wrapped", fmt.Errorf("lookup: %w", scheduler.ErrTaskNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newInstallFakeScheduler()
			s := &installSnapshotScheduler{fakeScheduler: f, name: name, exportErr: tc.cause}
			plan := installSnapshotPlan(t, s, name)
			if err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "snapshot"}, plan, 0, false, nil, true, false); err != nil {
				t.Fatal(err)
			}
			if !f.tasks[name] || len(f.xml[name]) == 0 || f.createCount != 1 || len(f.deleteNames) != 0 || len(f.importNames) != 0 || f.runCount != 0 {
				t.Fatalf("fresh task not created without deletion: tasks=%v delete=%v create=%d import=%v run=%d", f.tasks, f.deleteNames, f.createCount, f.importNames, f.runCount)
			}
		})
	}
}

func TestExecuteInstallToAbsentSnapshotCreateConflictPreservesTask(t *testing.T) {
	const name = "mcp-local-hub-snapshot-conflict"
	otherXML := []byte("<Task>external replacement</Task>")
	f := newInstallFakeScheduler()
	// Model a task that appeared after the absent snapshot: Create refuses the
	// conflict while Delete would erase the external definition.
	f.tasks[name], f.xml[name], f.failCreateForName = true, otherXML, name
	s := &installSnapshotScheduler{fakeScheduler: f, name: name, exportErr: scheduler.ErrTaskNotFound}
	plan := installSnapshotPlan(t, s, name)
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "snapshot"}, plan, 0, false, nil, true, false)
	if err == nil || !strings.Contains(err.Error(), "create task "+name) {
		t.Fatalf("create conflict = %v, want failure", err)
	}
	if !f.tasks[name] || !bytes.Equal(f.xml[name], otherXML) || len(f.deleteNames) != 0 || len(f.importNames) != 0 {
		t.Fatalf("create conflict erased external task: tasks=%v xml=%q delete=%v import=%v", f.tasks, f.xml[name], f.deleteNames, f.importNames)
	}
}

func TestExecuteInstallToDeleteFailurePreservesTask(t *testing.T) {
	const name = "mcp-local-hub-snapshot-delete"
	priorXML := []byte("<Task>original</Task>")
	cause := errors.New("delete denied")
	f := newInstallFakeScheduler()
	f.tasks[name], f.xml[name] = true, priorXML
	s := &installSnapshotScheduler{fakeScheduler: f, name: name, snapshot: priorXML, deleteErr: cause}
	plan := installSnapshotPlan(t, s, name)
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "snapshot"}, plan, 0, false, nil, true, false)
	if !errors.Is(err, cause) || !strings.Contains(err.Error(), "delete task "+name) {
		t.Errorf("delete refusal = %v, want named cause", err)
	}
	if !f.tasks[name] || !bytes.Equal(f.xml[name], priorXML) || len(f.deleteNames) != 1 || f.deleteNames[0] != name || f.createCount != 0 || len(f.importNames) != 0 || f.runCount != 0 {
		t.Errorf("failed delete continued replacement: tasks=%v xml=%q delete=%v create=%d import=%v run=%d", f.tasks, f.xml[name], f.deleteNames, f.createCount, f.importNames, f.runCount)
	}
}

func TestExecuteInstallToSnapshotRefusalRollsBackEarlierTask(t *testing.T) {
	const first = "mcp-local-hub-snapshot-first"
	const blocked = "mcp-local-hub-snapshot-blocked"
	priorXML := []byte("<Task>prior first task</Task>")
	blockedXML := []byte("<Task>blocked task</Task>")
	cause := errors.New("replacement blocked")
	for _, phase := range []string{"export", "delete"} {
		t.Run(phase, func(t *testing.T) {
			f := newInstallFakeScheduler()
			f.tasks[first], f.xml[first] = true, priorXML
			f.tasks[blocked], f.xml[blocked] = true, blockedXML
			s := &installSnapshotScheduler{fakeScheduler: f, name: blocked, snapshot: blockedXML}
			if phase == "export" {
				s.exportErr = cause
			} else {
				s.deleteErr = cause
			}
			plan := installSnapshotPlan(t, s, first, blocked)
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "snapshot"}, plan, 0, false, nil, true, false)
			if !errors.Is(err, cause) {
				t.Errorf("blocked replacement = %v, want original cause", err)
			}
			if !f.tasks[first] || !bytes.Equal(f.xml[first], priorXML) || !f.tasks[blocked] || !bytes.Equal(f.xml[blocked], blockedXML) || f.createCount != 1 || len(f.importNames) != 1 || f.importNames[0] != first || f.runCount != 0 {
				t.Errorf("earlier task not restored or blocked task changed: tasks=%v xml=%q create=%d import=%v run=%d", f.tasks, f.xml, f.createCount, f.importNames, f.runCount)
			}
		})
	}
}
