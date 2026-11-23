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
	"strconv"
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

func TestExecuteInstallToExistingTaskRefusesBeforeRecovery(t *testing.T) {
	const name = "mcp-local-hub-recovery-default"
	prior := []byte("<Task>prior-Привет-🙂</Task>")
	createCause, restoreCause := errors.New("fixture: create reply lost"), errors.New("fixture: restore reply lost")
	for _, scenario := range []string{"plain", "create_applied_reply_lost", "restore_applied_reply_lost", "without_xml_creator"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			f := newInstallFakeScheduler()
			f.tasks[name], f.xml[name] = true, bytes.Clone(prior)
			s := &installRecoveryScheduler{fakeScheduler: f, createErr: createCause, restoreErr: restoreCause}
			if scenario == "create_applied_reply_lost" {
				s.createWrites = []byte("candidate")
			}
			if scenario == "restore_applied_reply_lost" {
				s.restoreWrites = bytes.Clone(prior)
			}
			var backend scheduler.Scheduler = s
			if scenario == "without_xml_creator" {
				backend = struct{ scheduler.Scheduler }{s}
			}
			plan := installRecoveryPlan(t, backend, root, name)
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "recovery"}, plan, 0, false, nil, true, false)
			var refused *SchedulerReplacementRefusedError
			if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &refused) || refused.TaskName != name || errors.Is(err, createCause) || errors.Is(err, restoreCause) {
				t.Fatalf("RECOVERY-REFUSAL: %v", err)
			}
			if !f.tasks[name] || !bytes.Equal(f.xml[name], prior) || s.createAttempts != 0 || len(s.restoreNames) != 0 || len(f.deleteNames) != 0 || len(f.importNames) != 0 || f.runCount != 0 {
				t.Fatalf("RECOVERY-STATE: delete=%v restore=%v create=%d", f.deleteNames, s.restoreNames, s.createAttempts)
			}
			path := readInstallRecoverySnapshot(t, root, name, prior)
			if refused.SnapshotPath != path || !strings.Contains(err.Error(), strconv.Quote(path)) || strings.Contains(output.String(), "Install complete") {
				t.Fatalf("RECOVERY-SNAPSHOT: err=%v path=%s", err, path)
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
