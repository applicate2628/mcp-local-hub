package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// This fixture scripts native observations independently from mutation replies.
// A nil Delete/CreateXMLIfAbsent reply need not imply the expected final state.
type rollbackFixtureScheduler struct {
	*fakeScheduler
	exportCalls    map[string]int
	deleteAttempts []string
	restoreNames   []string
	createdXML     map[string][]byte
	stopCalls      int
	exportHook     func(string, int) ([]byte, error, bool)
	deleteHook     func(string, int) (error, bool)
	restoreHook    func(string, []byte) (error, bool)
}

func newRollbackFixtureScheduler() *rollbackFixtureScheduler {
	return &rollbackFixtureScheduler{
		fakeScheduler: newInstallFakeScheduler(), exportCalls: map[string]int{}, createdXML: map[string][]byte{},
	}
}

func (s *rollbackFixtureScheduler) Create(spec scheduler.TaskSpec) error {
	if err := s.fakeScheduler.Create(spec); err != nil {
		return err
	}
	s.createdXML[spec.Name] = bytes.Clone(s.xml[spec.Name])
	return nil
}

func (s *rollbackFixtureScheduler) ExportXML(name string) ([]byte, error) {
	s.exportCalls[name]++
	if s.exportHook != nil {
		if raw, err, handled := s.exportHook(name, s.exportCalls[name]); handled {
			return raw, err
		}
	}
	return s.fakeScheduler.ExportXML(name)
}

func (s *rollbackFixtureScheduler) Delete(name string) error {
	s.deleteAttempts = append(s.deleteAttempts, name)
	if s.deleteHook != nil {
		if err, handled := s.deleteHook(name, len(s.deleteAttempts)); handled {
			return err
		}
	}
	return s.fakeScheduler.Delete(name)
}

func (s *rollbackFixtureScheduler) CreateXMLIfAbsent(name string, raw []byte) error {
	s.restoreNames = append(s.restoreNames, name)
	if s.restoreHook != nil {
		if err, handled := s.restoreHook(name, raw); handled {
			return err
		}
	}
	if s.tasks[name] {
		return scheduler.ErrTaskAlreadyExists
	}
	s.tasks[name], s.xml[name] = true, bytes.Clone(raw)
	return nil
}

func (s *rollbackFixtureScheduler) Stop(string) error {
	s.stopCalls++
	return nil
}

func TestExecuteInstallToEarlierTaskRollback(t *testing.T) {
	const name = "mcp-local-hub-r22-rollback-default"
	installCause := errors.New("fixture: later install step failed")
	readCause := errors.New("fixture: rollback observation unavailable")
	for _, scenario := range []string{"unchanged", "external_same_xml", "external_changed_xml", "already_absent", "read_error", "partial_read_error", "empty_read", "partial_absence"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			s := newRollbackFixtureScheduler()
			plan := installRecoveryPlan(t, s, root, name)
			rollbackStarted := false
			generation := 1
			s.exportHook = func(_ string, _ int) ([]byte, error, bool) {
				if !rollbackStarted {
					return nil, nil, false
				}
				switch scenario {
				case "read_error":
					return nil, readCause, true
				case "partial_read_error":
					return []byte("<partial>"), readCause, true
				case "empty_read":
					return nil, nil, true
				case "partial_absence":
					return []byte("<partial>"), scheduler.ErrTaskNotFound, true
				}
				return nil, nil, false
			}
			var wanted []byte
			intermediate := func() (func(), error) {
				rollbackStarted = true
				switch scenario {
				case "already_absent":
					delete(s.tasks, name)
					delete(s.xml, name)
				case "external_same_xml":
					generation++
					s.xml[name] = bytes.Clone(s.createdXML[name])
				case "external_changed_xml":
					generation++
					s.xml[name] = []byte("<Task>external</Task>")
				}
				wanted = bytes.Clone(s.xml[name])
				return nil, installCause
			}
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "r22-rollback"}, plan, 0, false, intermediate, true, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			wantIncomplete := scenario != "already_absent"
			if !errors.Is(err, installCause) || errors.As(err, &incomplete) != wantIncomplete {
				t.Fatalf("ROLLBACK-CLASS: cause or recovery classification lost: %v", err)
			}
			if wantIncomplete && (!errors.Is(err, ErrSchedulerReplacementUnowned) || !reflect.DeepEqual(incomplete.Tasks, []string{name})) {
				t.Errorf("ROLLBACK-OWNER: current generation was treated as owned: %v", err)
			}
			if (scenario == "read_error" || scenario == "partial_read_error") && !errors.Is(err, readCause) {
				t.Errorf("ROLLBACK-CAUSE: %v", err)
			}
			if scenario == "partial_absence" && !errors.Is(err, scheduler.ErrTaskNotFound) {
				t.Errorf("ROLLBACK-CAUSE: %v", err)
			}
			if s.tasks[name] != wantIncomplete || !bytes.Equal(s.xml[name], wanted) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.createCount != 1 || s.runCount != 0 || s.stopCalls != 0 {
				t.Fatalf("ROLLBACK-GENERATION: task changed by name: generation=%d deletes=%v restores=%v err=%v", generation, s.deleteAttempts, s.restoreNames, err)
			}
			if strings.HasPrefix(scenario, "external_") && generation != 2 {
				t.Fatal("external writer did not run")
			}
			if scenario == "unchanged" || strings.HasPrefix(scenario, "external_") {
				path := readInstallRecoverySnapshot(t, root, name, wanted)
				if !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
					t.Error("recovery snapshot path lost")
				}
			}
			if strings.Contains(output.String(), "Install complete") || (wantIncomplete && strings.Contains(output.String(), "task absence confirmed")) {
				t.Fatalf("ROLLBACK-OUTPUT: %s", output.String())
			}
		})
	}
}

func TestExecuteInstallToRollbackCaptureFailureRetainsCreatedTask(t *testing.T) {
	const name = "mcp-local-hub-r22-capture-default"
	cause := errors.New("fixture: capture unavailable")
	for _, tc := range []struct {
		name string
		raw  []byte
		err  error
	}{
		{name: "unavailable", err: cause},
		{name: "partial_error", raw: []byte("<partial>"), err: cause},
		{name: "empty_success"},
		{name: "not_found", err: scheduler.ErrTaskNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			s := newRollbackFixtureScheduler()
			s.exportHook = func(_ string, _ int) ([]byte, error, bool) {
				if s.createCount > 0 && tc.name == "not_found" {
					delete(s.tasks, name)
					delete(s.xml, name)
				}
				return tc.raw, tc.err, s.createCount > 0
			}
			plan := installRecoveryPlan(t, s, root, name)
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-capture"}, plan, 0, false, nil, true, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			if err == nil || errors.As(err, &incomplete) != (tc.name != "not_found") || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Errorf("CAPTURE-FAILURE: missing capture cause or armed incomplete rollback: %v", err)
			}
			if s.tasks[name] != (tc.name != "not_found") || (s.tasks[name] && !bytes.Equal(s.xml[name], s.createdXML[name])) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Errorf("CAPTURE-FAILURE: unobserved candidate was compensated by name: deletes=%v restore=%v import=%v", s.deleteAttempts, s.restoreNames, s.importNames)
			}
		})
	}
}

func TestExecuteInstallToRollbackOwnsCapturedBytes(t *testing.T) {
	const name = "mcp-local-hub-r22-clone-default"
	s := newRollbackFixtureScheduler()
	root := hardenedTempDir(t)
	plan := installRecoveryPlan(t, s, root, name)
	installCause := errors.New("fixture: later step")
	var external []byte
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-clone"}, plan, 0, false, func() (func(), error) {
		// Mutate the borrowed array returned by post-Create ExportXML. Neither
		// the captured nor the current bytes authorize deleting this occupant.
		s.xml[name][0] = '!'
		external = bytes.Clone(s.xml[name])
		return nil, installCause
	}, true, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	if !errors.Is(err, installCause) || !errors.As(err, &incomplete) || !s.tasks[name] || !bytes.Equal(s.xml[name], external) || len(s.deleteAttempts) != 0 || len(s.importNames) != 0 {
		t.Fatalf("ROLLBACK-OWNED-BYTES: mutation of borrowed export authorized deletion: %v deletes=%v", err, s.deleteAttempts)
	}
}

func TestExecuteInstallToRollbackContinuesAndPreservesEveryCause(t *testing.T) {
	const first, second = "mcp-local-hub-r22-first", "mcp-local-hub-r22-second"
	s := newRollbackFixtureScheduler()
	plan := installRecoveryPlan(t, s, hardenedTempDir(t), first)
	plan.SchedulerTasks = append(plan.SchedulerTasks, ScheduledTaskPlan{Name: second, Command: plan.installRelayExePath})
	installCause := errors.New("fixture: later failure")
	firstCause, secondCause := errors.New("fixture: first observation"), errors.New("fixture: second observation")
	rollbackStarted := false
	var observations []string
	s.exportHook = func(name string, _ int) ([]byte, error, bool) {
		if !rollbackStarted {
			return nil, nil, false
		}
		observations = append(observations, name)
		if name == first {
			return nil, firstCause, true
		}
		return nil, secondCause, true
	}
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-many"}, plan, 0, false, func() (func(), error) { rollbackStarted = true; return nil, installCause }, true, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	if !errors.As(err, &incomplete) || !reflect.DeepEqual(incomplete.Tasks, []string{second, first}) || !errors.Is(err, installCause) || !errors.Is(err, firstCause) || !errors.Is(err, secondCause) || !errors.Is(err, ErrSchedulerReplacementUnowned) {
		t.Fatalf("ROLLBACK-ALL-CAUSES: lost cause or reverse order: %v", err)
	}
	if !reflect.DeepEqual(observations, []string{second, first}) || len(s.deleteAttempts) != 0 || !s.tasks[first] || !s.tasks[second] || len(s.importNames) != 0 || len(s.restoreNames) != 0 {
		t.Fatalf("ROLLBACK-CONTINUE: observations=%v tasks=%v", observations, s.tasks)
	}
}

func TestExecuteInstallToRollbackPreservesClientErrorDomains(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restore    restoreOutcome
		incomplete bool
	}{
		{name: "client_incomplete", restore: restoreFail, incomplete: true},
		{name: "client_release_unconfirmed", restore: restoreAppliedReleaseUnconfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := "r22-client-" + tc.name
			codexPath, _, root := setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"original\"]\n")
			seedInstallWriteSeam(t, map[string]clientWriteSpec{codexPath: {addEntry: addEntrySucceed, restore: tc.restore}})
			if tc.restore == restoreAppliedReleaseUnconfirmed {
				classifyInstallReleaseForTest(t, inducedRestoreReleaseUnconfirmed)
			}
			s := newRollbackFixtureScheduler()
			name := "mcp-local-hub-" + entry + "-default"
			plan := installRecoveryPlan(t, s, root, name)
			plan.ClientUpdates = []ClientUpdatePlan{{Client: "codex-cli", URL: "http://127.0.0.1:9313/mcp"}}
			installCause, readCause := errors.New("fixture: intermediate"), errors.New("fixture: task observation")
			rollbackStarted := false
			s.exportHook = func(string, int) ([]byte, error, bool) { return nil, readCause, rollbackStarted }
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: entry}, plan, 0, false, func() (func(), error) { rollbackStarted = true; return nil, installCause }, true, false)
			var taskErr *InstallSchedulerRollbackIncompleteError
			var clientErr *InstallClientRollbackIncompleteError
			if !errors.As(err, &taskErr) || errors.As(err, &clientErr) != tc.incomplete || !errors.Is(err, installCause) || !errors.Is(err, readCause) {
				t.Fatalf("ROLLBACK-DOMAINS: task/client/original cause lost: %v", err)
			}
			if tc.incomplete && !reflect.DeepEqual(clientErr.Clients, []string{"codex-cli"}) {
				t.Errorf("ROLLBACK-DOMAINS: task leaked into client names: %v", clientErr.Clients)
			}
			if !tc.incomplete && (!errors.Is(err, inducedRestoreReleaseUnconfirmed) || !errors.Is(err, clients.ErrConfigLockReleaseUnconfirmed)) {
				t.Errorf("ROLLBACK-DOMAINS: applied client restore lost release errors: %v", err)
			}
		})
	}
}

func TestInstallSchedulerRecoveryTasksClassifiesCurrentAndLater(t *testing.T) {
	createCause, restoreCause := errors.New("create"), errors.New("restore")
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{name: "plain", err: createCause},
		{name: "current_acknowledged", err: &InstallTaskRecoveryError{TaskName: "current", Cause: createCause}},
		{name: "current_uncertain", err: &InstallTaskRecoveryError{TaskName: "current", Cause: createCause, Recovery: restoreCause}, want: []string{"current"}},
		{name: "later_and_current", err: errors.Join(&InstallSchedulerRollbackIncompleteError{Tasks: []string{"earlier"}, cause: createCause}, &InstallTaskRecoveryError{TaskName: "current", Cause: createCause, Recovery: restoreCause}), want: []string{"earlier", "current"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := installSchedulerRecoveryTasks(fmt.Errorf("outer: %w", tc.err))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("RECOVERY-CLASSIFICATION: got %v want %v", got, tc.want)
			}
		})
	}
}

func TestExecuteInstallToRollbackBackendCompatibility(t *testing.T) {
	t.Run("fresh_task_without_xml_creator", func(t *testing.T) {
		root := hardenedTempDir(t)
		f := newInstallFakeScheduler()
		withoutCreator := struct{ scheduler.Scheduler }{f}
		plan := installRecoveryPlan(t, withoutCreator, root, "mcp-local-hub-r22-no-creator")
		cause := errors.New("later step")
		err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-compat"}, plan, 0, false, func() (func(), error) { return nil, cause }, true, false)
		var incomplete *InstallSchedulerRollbackIncompleteError
		if !errors.Is(err, cause) || !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &incomplete) || len(f.tasks) != 1 || len(f.deleteNames) != 0 || len(f.importNames) != 0 {
			t.Fatalf("ROLLBACK-CAPABILITY: an unowned task was removed: %v", err)
		}
	})
	t.Run("skipped_scheduler", func(t *testing.T) {
		previous := schedulerFactoryFn
		calls := 0
		schedulerFactoryFn = func() (scheduler.Scheduler, error) { calls++; return nil, scheduler.ErrNotImplemented }
		t.Cleanup(func() { schedulerFactoryFn = previous })
		plan := &Plan{installRelayExePath: filepath.Join(t.TempDir(), "mcphub.exe"), SchedulerTasks: []ScheduledTaskPlan{{Name: "unused"}}}
		if err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-skipped"}, plan, 0, false, nil, true, true); err != nil || calls != 0 {
			t.Fatalf("ROLLBACK-SKIP: scheduler touched on skipped/non-Windows path: calls=%d err=%v", calls, err)
		}
	})
	t.Run("noop_rejects_before_capture", func(t *testing.T) {
		t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
		s, err := scheduler.New()
		if err != nil {
			t.Fatal(err)
		}
		plan := installRecoveryPlan(t, s, hardenedTempDir(t), "mcp-local-hub-r22-noop")
		err = executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-noop"}, plan, 0, false, nil, true, false)
		var incomplete *InstallSchedulerRollbackIncompleteError
		if err == nil || !strings.Contains(err.Error(), "mutations rejected") || errors.As(err, &incomplete) {
			t.Fatalf("ROLLBACK-NOOP: rejected creation became acknowledged rollback: %v", err)
		}
	})
	t.Run("unsupported_platform", func(t *testing.T) {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			t.Skip("native backend is supported on this platform")
		}
		t.Setenv("MCPHUB_E2E_SCHEDULER", "")
		if _, err := scheduler.New(); !errors.Is(err, scheduler.ErrNotImplemented) {
			t.Fatalf("ROLLBACK-PLATFORM: unsupported backend contract changed: %v", err)
		}
	})
}

// Exercise both forward refusal sites through the install owner. The first
// export can already be stale, including a different generation with the same
// XML. No native Scheduler, process, socket, or installed state is involved.
func TestExecuteInstallToGenerationOwnershipRefusal(t *testing.T) {
	const server, name = "generation", "mcp-local-hub-generation-default"
	prior := []byte("<Task>prior-Привет-🙂</Task>")
	for _, flow := range []string{"replacement", "prune"} {
		for _, scenario := range []string{"unchanged", "external_same_xml", "external_changed_xml", "external_absence"} {
			t.Run(flow+"_"+scenario, func(t *testing.T) {
				root := hardenedTempDir(t)
				t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
				s := newRollbackFixtureScheduler()
				s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
				generation := 1
				var wanted []byte
				s.exportHook = func(task string, ordinal int) ([]byte, error, bool) {
					if task != name || ordinal != 1 {
						return nil, nil, false
					}
					switch scenario {
					case "external_same_xml":
						generation++
						s.xml[name] = bytes.Clone(prior)
					case "external_changed_xml":
						generation++
						s.xml[name] = []byte("<Task>external</Task>")
					case "external_absence":
						generation++
						delete(s.tasks, name)
						delete(s.xml, name)
					}
					wanted = bytes.Clone(s.xml[name])
					return bytes.Clone(prior), nil, true
				}
				plan := installRecoveryPlan(t, s, root, name)
				prune := flow == "prune"
				if prune {
					plan.SchedulerTasks = nil
					plan.FullInstall = true
					s.listSeed = []scheduler.TaskStatus{{Name: name}}
				}
				var output bytes.Buffer
				err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, nil, !prune, false)
				if prune {
					if err != nil || !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+name) {
						t.Fatalf("PRUNE-REFUSAL: err=%v output=%s", err, output.String())
					}
				} else if !errors.Is(err, ErrSchedulerReplacementUnowned) {
					t.Fatalf("REPLACEMENT-REFUSAL: %v", err)
				}
				var incomplete *InstallSchedulerRollbackIncompleteError
				if errors.As(err, &incomplete) {
					t.Fatalf("unmutated refusal acquired incomplete rollback: %v", err)
				}
				if s.tasks[name] != (scenario != "external_absence") || !bytes.Equal(s.xml[name], wanted) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.createCount != 0 || s.runCount != 0 || s.stopCalls != 0 {
					t.Fatalf("GENERATION-REFUSAL: changed occupant: generation=%d delete=%v restore=%v create=%d", generation, s.deleteAttempts, s.restoreNames, s.createCount)
				}
				if strings.HasPrefix(scenario, "external_") && generation != 2 {
					t.Fatal("external writer did not run")
				}
				path := readInstallRecoverySnapshot(t, root, name, prior)
				if !prune && !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
					t.Error("snapshot reference lost")
				}
				if strings.Contains(output.String(), "Scheduler task removed (obsolete)") || (!prune && strings.Contains(output.String(), "Install complete")) {
					t.Fatalf("false success: %s", output.String())
				}
			})
		}
	}
}

func TestExecuteInstallToReplacementRefusalRetainsEarlierTask(t *testing.T) {
	const first, blocked = "mcp-local-hub-refusal-first", "mcp-local-hub-refusal-blocked"
	root := hardenedTempDir(t)
	s := newRollbackFixtureScheduler()
	prior := []byte("<Task>blocked</Task>")
	s.tasks[blocked], s.xml[blocked] = true, bytes.Clone(prior)
	plan := installRecoveryPlan(t, s, root, first)
	plan.SchedulerTasks = append(plan.SchedulerTasks, ScheduledTaskPlan{Name: blocked, Command: plan.installRelayExePath})
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "refusal"}, plan, 0, false, nil, true, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	if !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &incomplete) || !reflect.DeepEqual(incomplete.Tasks, []string{first}) {
		t.Fatalf("REFUSAL-RECOVERY: %v", err)
	}
	if !s.tasks[first] || !bytes.Equal(s.xml[first], s.createdXML[first]) || !s.tasks[blocked] || !bytes.Equal(s.xml[blocked], prior) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.createCount != 1 {
		t.Fatalf("REFUSAL-STATE: tasks=%v deletes=%v", s.tasks, s.deleteAttempts)
	}
	paths, pathErr := filepath.Glob(filepath.Join(root, "scheduler-recovery-*.json"))
	if pathErr != nil || len(paths) != 2 {
		t.Fatalf("REFUSAL-SNAPSHOTS: paths=%v err=%v", paths, pathErr)
	}
}
