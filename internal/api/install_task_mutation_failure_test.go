package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// A failed response is independent of its side effect and the next observation.
// Reuse the existing fake for successful operations and later compensation.
type installMutationFailureScheduler struct {
	*rollbackFixtureScheduler
	name             string
	operation        string
	cause            error
	mutate           func()
	observe          func() ([]byte, error)
	failed           bool
	postFailureReads int
}

func (s *installMutationFailureScheduler) Create(spec scheduler.TaskSpec) error {
	if s.operation != "create" || spec.Name != s.name {
		return s.rollbackFixtureScheduler.Create(spec)
	}
	s.createCount++
	if s.mutate != nil {
		s.mutate()
	}
	s.failed = true
	return s.cause
}

func (s *installMutationFailureScheduler) Delete(name string) error {
	if s.operation != "delete" || name != s.name {
		return s.rollbackFixtureScheduler.Delete(name)
	}
	s.deleteAttempts = append(s.deleteAttempts, name)
	s.deleteNames = append(s.deleteNames, name)
	if s.mutate != nil {
		s.mutate()
	}
	s.failed = true
	return s.cause
}

func (s *installMutationFailureScheduler) ExportXML(name string) ([]byte, error) {
	if s.failed && name == s.name {
		s.postFailureReads++
		if s.observe != nil {
			return s.observe()
		}
	}
	return s.rollbackFixtureScheduler.ExportXML(name)
}

func TestExecuteInstallToCurrentMutationFailureObservation(t *testing.T) {
	const name = "mcp-local-hub-r23-current-default"
	prior := []byte("<Task>prior</Task>")
	candidate := []byte("<Task>candidate</Task>")
	readCause := errors.New("fixture: observation unavailable")
	for _, tc := range []struct {
		name       string
		operation  string
		final      string
		read       string
		incomplete bool
	}{
		{name: "delete_rejected_prior_confirmed", operation: "delete", final: "prior"},
		{name: "delete_applied_reply_lost", operation: "delete", final: "absent", incomplete: true},
		{name: "delete_external_definition", operation: "delete", final: "candidate", incomplete: true},
		{name: "delete_partial_observation_error", operation: "delete", final: "prior", read: "partial_error", incomplete: true},
		{name: "delete_empty_success", operation: "delete", final: "prior", read: "empty", incomplete: true},
		{name: "create_rejected_absence_confirmed", operation: "create", final: "absent"},
		{name: "create_wrapped_absence", operation: "create", final: "absent", read: "wrapped_absence"},
		{name: "create_applied_reply_lost", operation: "create", final: "candidate", incomplete: true},
		{name: "create_observation_error", operation: "create", final: "absent", read: "error", incomplete: true},
		{name: "create_partial_absence", operation: "create", final: "absent", read: "partial_absence", incomplete: true},
		{name: "create_empty_success", operation: "create", final: "absent", read: "empty", incomplete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			cause := &os.PathError{Op: tc.operation, Path: "synthetic-task", Err: errors.New("fixture: response unavailable")}
			s := &installMutationFailureScheduler{
				rollbackFixtureScheduler: newRollbackFixtureScheduler(),
				name:                     name, operation: tc.operation, cause: cause,
			}
			if tc.operation == "delete" {
				s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			}
			s.mutate = func() {
				switch tc.final {
				case "absent":
					delete(s.tasks, name)
					delete(s.xml, name)
				case "candidate":
					s.tasks[name], s.xml[name] = true, bytes.Clone(candidate)
				}
			}
			switch tc.read {
			case "error":
				s.observe = func() ([]byte, error) { return nil, readCause }
			case "partial_error":
				s.observe = func() ([]byte, error) { return bytes.Clone(prior), readCause }
			case "partial_absence":
				s.observe = func() ([]byte, error) { return []byte("<partial>"), scheduler.ErrTaskNotFound }
			case "empty":
				s.observe = func() ([]byte, error) { return nil, nil }
			case "wrapped_absence":
				s.observe = func() ([]byte, error) { return nil, fmt.Errorf("lookup: %w", scheduler.ErrTaskNotFound) }
			}
			plan := installRecoveryPlan(t, s, root, name)
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "r23-current"}, plan, 0, false, nil, true, false)
			var typedCause *os.PathError
			if !errors.Is(err, cause) || !errors.As(err, &typedCause) || typedCause != cause {
				t.Fatalf("CURRENT-MUTATION-CAUSE: original typed cause lost: %v", err)
			}
			if (tc.read == "error" || tc.read == "partial_error") && !errors.Is(err, readCause) {
				t.Errorf("CURRENT-MUTATION-OBSERVATION: read cause lost: %v", err)
			}
			var incomplete *InstallSchedulerRollbackIncompleteError
			if got := errors.As(err, &incomplete); got != tc.incomplete {
				t.Fatalf("CURRENT-MUTATION-CLASS: incomplete=%v want %v; error=%v", got, tc.incomplete, err)
			}
			if tc.incomplete && !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{name}) {
				t.Errorf("CURRENT-MUTATION-OWNER: missing affected task: %v", err)
			}
			wantXML := map[string][]byte{"prior": prior, "candidate": candidate}[tc.final]
			if !bytes.Equal(s.xml[name], wantXML) || s.tasks[name] != (tc.final != "absent") {
				t.Errorf("CURRENT-MUTATION-STATE: observation changed the task: present=%v XML=%q", s.tasks[name], s.xml[name])
			}
			wantCreates, wantDeletes := 1, 0
			if tc.operation == "delete" {
				wantCreates, wantDeletes = 0, 1
				path := readInstallRecoverySnapshot(t, root, name, prior)
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
					t.Error("CURRENT-MUTATION-SNAPSHOT: retained snapshot reference lost")
				}
			}
			if !s.failed || s.postFailureReads != 1 || s.createCount != wantCreates || len(s.deleteAttempts) != wantDeletes || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Errorf("CURRENT-MUTATION-EFFECTS: failed=%v reads=%d create=%d deletes=%v restores=%v imports=%v run=%d stop=%d", s.failed, s.postFailureReads, s.createCount, s.deleteAttempts, s.restoreNames, s.importNames, s.runCount, s.stopCalls)
			}
			if strings.Contains(output.String(), "Scheduler task created") || strings.Contains(output.String(), "Install complete") {
				t.Errorf("CURRENT-MUTATION-OUTPUT: failed mutation reported success: %s", output.String())
			}
		})
	}
}

func TestExecuteInstallToCurrentAndEarlierRecoveryNames(t *testing.T) {
	const first, current = "mcp-local-hub-r23-earlier", "mcp-local-hub-r23-current"
	currentCause, earlierCause := errors.New("fixture: create reply lost"), errors.New("fixture: earlier delete failed")
	s := &installMutationFailureScheduler{
		rollbackFixtureScheduler: newRollbackFixtureScheduler(),
		name:                     current, operation: "create", cause: currentCause,
	}
	s.mutate = func() { s.tasks[current], s.xml[current] = true, []byte("<Task>unconfirmed</Task>") }
	s.deleteHook = func(name string, _ int) (error, bool) {
		if name == first {
			return earlierCause, true
		}
		return nil, false
	}
	plan := installRecoveryPlan(t, s, hardenedTempDir(t), first)
	plan.SchedulerTasks = append(plan.SchedulerTasks, ScheduledTaskPlan{Name: current, Command: plan.installRelayExePath})
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r23-many"}, plan, 0, false, nil, true, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	wantNames := []string{first, current}
	if !errors.Is(err, currentCause) || !errors.Is(err, earlierCause) || !errors.As(err, &incomplete) || !reflect.DeepEqual(incomplete.Tasks, wantNames) || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), wantNames) {
		t.Fatalf("CURRENT-AND-EARLIER: lost cause or affected name: %v", err)
	}
	if !s.tasks[first] || !s.tasks[current] || !reflect.DeepEqual(s.deleteAttempts, []string{first}) || s.postFailureReads != 1 || len(s.importNames) != 0 {
		t.Fatalf("CURRENT-AND-EARLIER: current task was compensated by name or earlier rollback skipped: deletes=%v tasks=%v", s.deleteAttempts, s.tasks)
	}
}

// Traverse the real install loop inside the existing outer install boundary.
// This verifies the error classification reaches durable adopt records; it does
// not replace the product's separate plan-building or native scheduler tests.
func TestExecuteAdoptCurrentMutationFailurePreservesRecovery(t *testing.T) {
	for _, operation := range []string{"delete", "create"} {
		t.Run(operation, func(t *testing.T) {
			entry := "r23-adopt-" + operation
			codexPath, cursorPath, manifestRoot, stateRoot := seedTwoPresentClients(t, entry)
			api := NewAPI()
			if _, err := api.SecretsInit(); err != nil {
				t.Fatal(err)
			}
			priorClients := map[string][]byte{
				filepath.Clean(codexPath):  mustReadFileForAdoptTest(t, codexPath),
				filepath.Clean(cursorPath): mustReadFileForAdoptTest(t, cursorPath),
			}
			plan, err := api.BuildAdoptPlan(AdoptOpts{
				EntryName: entry, Client: "codex-cli", ManifestName: entry,
				Port:     nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()),
				Clients:  []string{"codex-cli", "cursor"},
				ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath},
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.SecretRoutedKeys) == 0 {
				t.Fatal("fixture requires routed keys to test preservation")
			}
			name := "mcp-local-hub-" + entry + "-default"
			cause := errors.New("fixture: applied " + operation + " lost its reply")
			s := &installMutationFailureScheduler{
				rollbackFixtureScheduler: newRollbackFixtureScheduler(),
				name:                     name, operation: operation, cause: cause,
			}
			if operation == "delete" {
				s.tasks[name], s.xml[name] = true, []byte("<Task>prior</Task>")
				s.mutate = func() { delete(s.tasks, name); delete(s.xml, name) }
			} else {
				s.mutate = func() { s.tasks[name], s.xml[name] = true, []byte("<Task>candidate</Task>") }
			}
			installPlan := installRecoveryPlan(t, s, stateRoot, name)
			installCalls := 0
			var output bytes.Buffer
			err = api.ExecuteAdoptWithOpts(plan, &output, ExecuteAdoptOpts{
				installFn: func(_ context.Context, _ *API, opts InstallOpts) error {
					installCalls++
					return executeInstallTo(opts.Writer, &config.ServerManifest{Name: entry}, installPlan, 0, false, nil, true, false)
				},
			})
			var staged *AdoptStageError
			if installCalls != 1 || !errors.Is(err, cause) || !errors.As(err, &staged) || staged.CommitState != "committed_unverified" || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{name}) {
				t.Fatalf("ADOPT-CURRENT-MUTATION: lost uncertain outcome: calls=%d err=%v", installCalls, err)
			}
			assertPreservedProvenance(t, entry, plan, manifestRoot, priorClients)
			logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
			ev, _ := findSupervisorEventByName(t, logPath, "adopt-scheduler-recovery-preserved")
			if ev == nil {
				t.Fatal("ADOPT-CURRENT-MUTATION: missing preservation event")
			}
			if success, _ := findSupervisorEventByName(t, logPath, "adopt-executed"); success != nil || strings.Contains(output.String(), "Adopted ") {
				t.Error("ADOPT-CURRENT-MUTATION: failed installation reported success")
			}
			if s.postFailureReads != 1 || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Errorf("ADOPT-CURRENT-MUTATION: unexpected retry or recovery: reads=%d restores=%v imports=%v", s.postFailureReads, s.restoreNames, s.importNames)
			}
		})
	}
}
