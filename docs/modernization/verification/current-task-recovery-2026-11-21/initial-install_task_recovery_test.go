package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

var errInstallRecoveryOccupied = errors.New("fixture: task name occupied")

// The shared scheduler fake models the optional create-only operation without
// routing through its replacing ImportXML operation.
func (f *fakeScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	if f.tasks[name] {
		return errInstallRecoveryOccupied
	}
	f.tasks[name] = true
	f.xml[name] = append([]byte(nil), xml...)
	return nil
}

type installRecoveryScheduler struct {
	*fakeScheduler
	createErr      error
	restoreErr     error
	createWrites   []byte
	restoreWrites  []byte
	createAttempts int
	restoreNames   []string
	beforeDelete   func()
}

func (s *installRecoveryScheduler) Create(spec scheduler.TaskSpec) error {
	s.createAttempts++
	if s.createErr != nil {
		if s.createWrites != nil {
			s.tasks[spec.Name], s.xml[spec.Name] = true, s.createWrites
		}
		return s.createErr
	}
	return s.fakeScheduler.Create(spec)
}

func (s *installRecoveryScheduler) Delete(name string) error {
	if s.beforeDelete != nil {
		s.beforeDelete()
	}
	return s.fakeScheduler.Delete(name)
}

func (s *installRecoveryScheduler) CreateXMLIfAbsent(name string, xml []byte) error {
	s.restoreNames = append(s.restoreNames, name)
	if s.tasks[name] {
		return errInstallRecoveryOccupied
	}
	if s.restoreWrites != nil {
		s.tasks[name], s.xml[name] = true, s.restoreWrites
	}
	if s.restoreErr != nil {
		return s.restoreErr
	}
	return s.fakeScheduler.CreateXMLIfAbsent(name, xml)
}

func installRecoveryPlan(t *testing.T, s scheduler.Scheduler, root, name string) *Plan {
	t.Helper()
	t.Cleanup(SetDaemonStateRootForTest(root))
	prior := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) { return s, nil }
	t.Cleanup(func() { schedulerFactoryFn = prior })
	exe := filepath.Join(root, "mcphub.exe")
	return &Plan{installRelayExePath: exe, SchedulerTasks: []ScheduledTaskPlan{{Name: name, Command: exe}}}
}

func readInstallRecoverySnapshot(t *testing.T, root, name string, xml []byte) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("RECOVERY-SNAPSHOT: got %d files, error %v; want one retained snapshot", len(paths), err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Version       int    `json:"version"`
		TaskName      string `json:"task_name"`
		CapturedAtUTC string `json:"captured_at_utc"`
		XML           []byte `json:"xml"`
		XMLSHA256     string `json:"xml_sha256"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(xml)
	if saved.Version != 1 || saved.TaskName != name || !bytes.Equal(saved.XML, xml) || saved.XMLSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("RECOVERY-SNAPSHOT: task, encoding or content identity differs")
	}
	if _, err := time.Parse(time.RFC3339Nano, saved.CapturedAtUTC); err != nil || !strings.HasSuffix(saved.CapturedAtUTC, "Z") {
		t.Fatalf("RECOVERY-SNAPSHOT: invalid UTC capture timestamp: %v", err)
	}
	return paths[0]
}

func TestExecuteInstallToCurrentTaskRecovery(t *testing.T) {
	const name = "mcp-local-hub-recovery-default"
	priorXML := []byte("<Task>prior-Привет-🙂</Task>")
	createCause := errors.New("fixture: create failed")
	restoreCause := errors.New("fixture: recovery reply unavailable")
	for _, tc := range []struct {
		name          string
		createWrites  []byte
		restoreWrites []byte
		restoreErr    error
		wantXML       []byte
		wantErr       error
	}{
		{name: "restored", wantXML: priorXML},
		{name: "external_occupant", createWrites: []byte("external"), wantXML: []byte("external"), wantErr: errInstallRecoveryOccupied},
		{name: "create_applied_reply_lost", createWrites: []byte("candidate"), wantXML: []byte("candidate"), wantErr: errInstallRecoveryOccupied},
		{name: "restore_rejected", restoreErr: restoreCause, wantErr: restoreCause},
		{name: "restore_applied_reply_lost", restoreWrites: priorXML, restoreErr: restoreCause, wantXML: priorXML, wantErr: restoreCause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			f := newInstallFakeScheduler()
			f.tasks[name], f.xml[name] = true, priorXML
			s := &installRecoveryScheduler{fakeScheduler: f, createErr: createCause, createWrites: tc.createWrites, restoreWrites: tc.restoreWrites, restoreErr: tc.restoreErr}
			plan := installRecoveryPlan(t, s, root, name)
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
			if !errors.Is(err, createCause) || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Errorf("RECOVERY-CAUSES: got %v, want original and recovery causes", err)
			}
			if !bytes.Equal(f.xml[name], tc.wantXML) || f.tasks[name] != (tc.wantXML != nil) || s.createAttempts != 1 || len(s.restoreNames) != 1 || s.restoreNames[0] != name || len(f.deleteNames) != 1 || f.deleteNames[0] != name || len(f.importNames) != 0 || f.runCount != 0 {
				t.Errorf("RECOVERY-STATE: current task or operation sequence differs; delete=%v restore=%v create=%d import=%v run=%d", f.deleteNames, s.restoreNames, s.createAttempts, f.importNames, f.runCount)
			}
			path := readInstallRecoverySnapshot(t, root, name, priorXML)
			if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(output.String(), "Scheduler task created") || strings.Contains(output.String(), "Install complete") {
				t.Errorf("RECOVERY-DIAGNOSTIC: failure lost snapshot reference or reported install success: %v", err)
			}
			if tc.wantErr != nil && strings.Contains(err.Error(), "prior definition registered again") {
				t.Error("RECOVERY-DIAGNOSTIC: uncertain restoration reported as acknowledged")
			}
		})
	}
}

func TestExecuteInstallToRecoveryCapabilityRequired(t *testing.T) {
	const name = "mcp-local-hub-recovery-no-capability"
	root := hardenedTempDir(t)
	f := newInstallFakeScheduler()
	priorXML := []byte("<Task>prior</Task>")
	f.tasks[name], f.xml[name] = true, priorXML
	// Expose only the existing Scheduler interface, hiding the optional method.
	s := struct{ scheduler.Scheduler }{f}
	plan := installRecoveryPlan(t, s, root, name)
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
	if !errors.Is(err, scheduler.ErrNotImplemented) || !bytes.Equal(f.xml[name], priorXML) || len(f.deleteNames) != 0 || f.createCount != 0 {
		t.Errorf("RECOVERY-CAPABILITY: unsupported backend changed prior task or lost typed cause: %v", err)
	}
	paths, _ := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
	if len(paths) != 0 {
		t.Error("RECOVERY-CAPABILITY: saved snapshot for unsupported backend")
	}
}

func TestExecuteInstallToRecoverySnapshotBeforeDelete(t *testing.T) {
	const name = "mcp-local-hub-recovery-order"
	priorXML := []byte("<Task>before delete</Task>")
	for _, failDelete := range []bool{false, true} {
		label := "replacement_succeeds"
		if failDelete {
			label = "delete_fails"
		}
		t.Run(label, func(t *testing.T) {
			root := hardenedTempDir(t)
			f := newInstallFakeScheduler()
			f.tasks[name], f.xml[name] = true, priorXML
			cause := errors.New("fixture: delete failed")
			if failDelete {
				f.failDeleteErr = cause
			}
			s := &installRecoveryScheduler{fakeScheduler: f}
			s.beforeDelete = func() { readInstallRecoverySnapshot(t, root, name, priorXML) }
			plan := installRecoveryPlan(t, s, root, name)
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
			path := readInstallRecoverySnapshot(t, root, name, priorXML)
			if failDelete {
				if !errors.Is(err, cause) || !strings.Contains(err.Error(), path) || s.createAttempts != 0 || !bytes.Equal(f.xml[name], priorXML) {
					t.Errorf("RECOVERY-DELETE: got %v; create attempts %d", err, s.createAttempts)
				}
			} else if err != nil || !strings.Contains(output.String(), path) || s.createAttempts != 1 {
				t.Errorf("RECOVERY-SUCCESS: lost retained snapshot or failed replacement: %v", err)
			}
			if len(s.restoreNames) != 0 || len(f.importNames) != 0 || f.runCount != 0 {
				t.Error("RECOVERY-ORDER: unexpected recovery, replacing import or Run")
			}
		})
	}
}

func TestExecuteInstallToRecoverySnapshotFailureStopsReplacement(t *testing.T) {
	const name = "mcp-local-hub-recovery-storage"
	root := filepath.Join(hardenedTempDir(t), "not-a-directory")
	if err := os.WriteFile(root, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := newInstallFakeScheduler()
	priorXML := []byte("<Task>prior</Task>")
	f.tasks[name], f.xml[name] = true, priorXML
	s := &installRecoveryScheduler{fakeScheduler: f}
	plan := installRecoveryPlan(t, s, root, name)
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
	if err == nil || !bytes.Equal(f.xml[name], priorXML) || len(f.deleteNames) != 0 || s.createAttempts != 0 || len(s.restoreNames) != 0 {
		t.Errorf("RECOVERY-STORAGE: unusable snapshot storage did not stop replacement: %v", err)
	}
}

func TestExecuteInstallToAbsentFailureHasNoRecovery(t *testing.T) {
	const name = "mcp-local-hub-recovery-fresh"
	root := hardenedTempDir(t)
	cause := errors.New("fixture: create absent task failed")
	s := &installRecoveryScheduler{fakeScheduler: newInstallFakeScheduler(), createErr: cause}
	plan := installRecoveryPlan(t, s, root, name)
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
	paths, _ := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
	if !errors.Is(err, cause) || s.createAttempts != 1 || len(s.deleteNames) != 0 || len(s.restoreNames) != 0 || len(paths) != 0 {
		t.Fatalf("RECOVERY-ABSENT: absent task failure attempted restoration: %v", err)
	}
}

func TestExecuteInstallToRejectsAliasedRecoveryNames(t *testing.T) {
	const name = "mcp-local-hub-recovery-alias"
	for _, alias := range []string{name, strings.ToUpper(name), `\` + name} {
		label := "exact"
		if alias == strings.ToUpper(name) {
			label = "case"
		} else if strings.HasPrefix(alias, `\`) {
			label = "root_prefix"
		}
		t.Run(label, func(t *testing.T) {
			root := hardenedTempDir(t)
			f := newInstallFakeScheduler()
			s := &installRecoveryScheduler{fakeScheduler: f}
			plan := installRecoveryPlan(t, s, root, name)
			plan.SchedulerTasks = append(plan.SchedulerTasks, ScheduledTaskPlan{Name: alias, Command: plan.installRelayExePath})
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
			if err == nil || !strings.Contains(err.Error(), "duplicate scheduler task name") || s.createAttempts != 0 || len(f.deleteNames) != 0 || len(s.restoreNames) != 0 {
				t.Errorf("RECOVERY-ALIASES: duplicate namespace reached task mutation: %v", err)
			}
			paths, _ := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
			if len(paths) != 0 {
				t.Error("RECOVERY-ALIASES: invalid plan retained recovery material")
			}
		})
	}
}
