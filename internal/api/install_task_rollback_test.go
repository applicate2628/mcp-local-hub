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
	prior := []byte("<Task>prior § Привет 🙂</Task>")
	external := []byte("<Task>external replacement</Task>")
	installCause := errors.New("fixture: later install step failed")
	readCause := errors.New("fixture: export unavailable")
	deleteCause := errors.New("fixture: deletion reply unavailable")
	restoreCause := errors.New("fixture: restoration reply unavailable")
	for _, tc := range []struct {
		name           string
		priorAbsent    bool
		wantState      string
		wantDeletes    int
		wantRestores   int
		wantIncomplete bool
		wantCause      error
	}{
		{name: "restore_prior", wantState: "prior", wantDeletes: 2, wantRestores: 1},
		{name: "remove_new", priorAbsent: true, wantState: "absent", wantDeletes: 1},
		{name: "already_absent", wantState: "prior", wantDeletes: 1, wantRestores: 1},
		{name: "already_prior", wantState: "prior", wantDeletes: 1},
		{name: "external_replacement", wantState: "external", wantDeletes: 1, wantIncomplete: true},
		{name: "read_error", wantState: "created", wantDeletes: 1, wantIncomplete: true, wantCause: readCause},
		{name: "partial_read_error", wantState: "created", wantDeletes: 1, wantIncomplete: true, wantCause: readCause},
		{name: "empty_read", wantState: "created", wantDeletes: 1, wantIncomplete: true},
		{name: "delete_rejected", wantState: "created", wantDeletes: 2, wantIncomplete: true, wantCause: deleteCause},
		{name: "delete_applied_reply_lost", wantState: "absent", wantDeletes: 2, wantIncomplete: true, wantCause: deleteCause},
		{name: "delete_ack_without_effect", wantState: "created", wantDeletes: 2, wantIncomplete: true},
		{name: "absence_read_error", wantState: "absent", wantDeletes: 2, wantIncomplete: true, wantCause: readCause},
		{name: "restore_conflict", wantState: "external", wantDeletes: 2, wantRestores: 1, wantIncomplete: true, wantCause: scheduler.ErrTaskAlreadyExists},
		{name: "restore_rejected", wantState: "absent", wantDeletes: 2, wantRestores: 1, wantIncomplete: true, wantCause: restoreCause},
		{name: "restore_applied_reply_lost", wantState: "prior", wantDeletes: 2, wantRestores: 1, wantIncomplete: true, wantCause: restoreCause},
		{name: "restore_ack_wrong_definition", wantState: "external", wantDeletes: 2, wantRestores: 1, wantIncomplete: true},
		{name: "restored_read_error", wantState: "prior", wantDeletes: 2, wantRestores: 1, wantIncomplete: true, wantCause: readCause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := hardenedTempDir(t)
			s := newRollbackFixtureScheduler()
			if !tc.priorAbsent {
				s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			}
			plan := installRecoveryPlan(t, s, root, name)
			rollbackStarted, rollbackReads := false, 0
			s.exportHook = func(task string, _ int) ([]byte, error, bool) {
				if !rollbackStarted {
					return nil, nil, false
				}
				rollbackReads++
				switch {
				case rollbackReads == 1 && tc.name == "read_error":
					return nil, readCause, true
				case rollbackReads == 1 && tc.name == "partial_read_error":
					return []byte("<partial>"), readCause, true
				case rollbackReads == 1 && tc.name == "empty_read":
					return nil, nil, true
				case rollbackReads == 2 && tc.name == "absence_read_error":
					return nil, readCause, true
				case rollbackReads == 3 && tc.name == "restored_read_error":
					return nil, readCause, true
				}
				return nil, nil, false
			}
			s.deleteHook = func(task string, ordinal int) (error, bool) {
				if ordinal != 2 {
					return nil, false
				}
				switch tc.name {
				case "delete_rejected":
					return deleteCause, true
				case "delete_applied_reply_lost":
					delete(s.tasks, task)
					delete(s.xml, task)
					return deleteCause, true
				case "delete_ack_without_effect":
					return nil, true
				}
				return nil, false
			}
			s.restoreHook = func(task string, raw []byte) (error, bool) {
				if !bytes.Equal(raw, prior) {
					t.Error("ROLLBACK-INPUT: prior bytes changed before conditional registration")
				}
				switch tc.name {
				case "restore_conflict":
					s.tasks[task], s.xml[task] = true, bytes.Clone(external)
					return scheduler.ErrTaskAlreadyExists, true
				case "restore_rejected":
					return restoreCause, true
				case "restore_applied_reply_lost":
					s.tasks[task], s.xml[task] = true, bytes.Clone(raw)
					return restoreCause, true
				case "restore_ack_wrong_definition":
					s.tasks[task], s.xml[task] = true, bytes.Clone(external)
					return nil, true
				}
				return nil, false
			}
			intermediate := func() (func(), error) {
				rollbackStarted = true
				switch tc.name {
				case "already_absent":
					delete(s.tasks, name)
					delete(s.xml, name)
				case "already_prior":
					s.xml[name] = bytes.Clone(prior)
				case "external_replacement":
					s.xml[name] = bytes.Clone(external)
				}
				return nil, installCause
			}
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: "r22-rollback"}, plan, 0, false, intermediate, true, false)
			if !errors.Is(err, installCause) || (tc.wantCause != nil && !errors.Is(err, tc.wantCause)) {
				t.Errorf("ROLLBACK-CAUSES: got %v, want install and operation causes", err)
			}
			var incomplete *InstallSchedulerRollbackIncompleteError
			if got := errors.As(err, &incomplete); got != tc.wantIncomplete {
				t.Errorf("ROLLBACK-CLASS: incomplete=%v want %v; error=%v", got, tc.wantIncomplete, err)
			} else if got && !reflect.DeepEqual(incomplete.Tasks, []string{name}) {
				t.Errorf("ROLLBACK-CLASS: task names=%v", incomplete.Tasks)
			}
			wantXML := map[string][]byte{"prior": prior, "external": external, "created": s.createdXML[name]}[tc.wantState]
			if s.tasks[name] != (tc.wantState != "absent") || !bytes.Equal(s.xml[name], wantXML) {
				t.Errorf("ROLLBACK-STATE: present=%v XML=%q want %s", s.tasks[name], s.xml[name], tc.wantState)
			}
			if len(s.deleteAttempts) != tc.wantDeletes || len(s.restoreNames) != tc.wantRestores || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 || s.createCount != 1 {
				t.Errorf("ROLLBACK-EFFECTS: delete=%v restore=%v import=%v run=%d stop=%d create=%d", s.deleteAttempts, s.restoreNames, s.importNames, s.runCount, s.stopCalls, s.createCount)
			}
			if !tc.priorAbsent {
				path := readInstallRecoverySnapshot(t, root, name, prior)
				if tc.wantIncomplete && !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
					t.Error("ROLLBACK-SNAPSHOT: incomplete result lost retained recovery path")
				}
			}
			if strings.Contains(output.String(), "rollback: deleted scheduler task") || strings.Contains(output.String(), "Install complete") || (tc.wantIncomplete && strings.Contains(output.String(), "prior scheduler definition confirmed")) {
				t.Errorf("ROLLBACK-OUTPUT: unconfirmed result reported success: %s", output.String())
			}
		})
	}
}

func TestExecuteInstallToRollbackCaptureFailureRetainsCreatedTask(t *testing.T) {
	const name = "mcp-local-hub-r22-capture-default"
	prior := []byte("<Task>prior</Task>")
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
			s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			s.exportHook = func(_ string, _ int) ([]byte, error, bool) {
				return tc.raw, tc.err, s.createCount > 0
			}
			plan := installRecoveryPlan(t, s, root, name)
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-capture"}, plan, 0, false, nil, true, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			if err == nil || !errors.As(err, &incomplete) || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Errorf("CAPTURE-FAILURE: missing capture cause or armed incomplete rollback: %v", err)
			}
			if !s.tasks[name] || !bytes.Equal(s.xml[name], s.createdXML[name]) || len(s.deleteAttempts) != 1 || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Errorf("CAPTURE-FAILURE: unobserved candidate was compensated by name: deletes=%v restore=%v import=%v", s.deleteAttempts, s.restoreNames, s.importNames)
			}
			readInstallRecoverySnapshot(t, root, name, prior)
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
		// Mutate the exact borrowed array returned by the fake's post-Create
		// ExportXML. A retained alias would authorize deletion of these new bytes.
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
	root := hardenedTempDir(t)
	plan := installRecoveryPlan(t, s, root, first)
	plan.SchedulerTasks = append(plan.SchedulerTasks, ScheduledTaskPlan{Name: second, Command: plan.installRelayExePath})
	installCause := errors.New("fixture: later failure")
	firstCause, secondCause := errors.New("fixture: first delete"), errors.New("fixture: second delete")
	s.deleteHook = func(name string, _ int) (error, bool) {
		if name == first {
			return firstCause, true
		}
		return secondCause, true
	}
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "r22-many"}, plan, 0, false, func() (func(), error) { return nil, installCause }, true, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	if !errors.As(err, &incomplete) || !reflect.DeepEqual(incomplete.Tasks, []string{second, first}) || !errors.Is(err, installCause) || !errors.Is(err, firstCause) || !errors.Is(err, secondCause) {
		t.Fatalf("ROLLBACK-ALL-CAUSES: lost cause or reverse order: %v", err)
	}
	if !reflect.DeepEqual(s.deleteAttempts, []string{second, first}) || !s.tasks[first] || !s.tasks[second] || len(s.importNames) != 0 {
		t.Fatalf("ROLLBACK-CONTINUE: deletes=%v tasks=%v", s.deleteAttempts, s.tasks)
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
			installCause, deleteCause := errors.New("fixture: intermediate"), errors.New("fixture: task delete")
			s.deleteHook = func(string, int) (error, bool) { return deleteCause, true }
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: entry}, plan, 0, false, func() (func(), error) { return nil, installCause }, true, false)
			var taskErr *InstallSchedulerRollbackIncompleteError
			var clientErr *InstallClientRollbackIncompleteError
			if !errors.As(err, &taskErr) || errors.As(err, &clientErr) != tc.incomplete || !errors.Is(err, installCause) || !errors.Is(err, deleteCause) {
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
		if err != cause || errors.As(err, &incomplete) || len(f.tasks) != 0 || len(f.deleteNames) != 1 || len(f.importNames) != 0 {
			t.Fatalf("ROLLBACK-CAPABILITY: fresh task removal required XMLCreator: %v", err)
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

// Exercise both forward deletion sites through the install owner. The first
// export returns an owned snapshot while the live fake can already change;
// no native Scheduler, process, socket, or installed state is involved.
func TestExecuteInstallToPreDeleteRevalidation(t *testing.T) {
	const server = "predelete"
	const name = "mcp-local-hub-predelete-default"
	prior := []byte("<Task>prior-Привет-🙂</Task>")
	external := []byte("<Task>external replacement</Task>")
	readCause := errors.New("fixture: pre-delete current read unavailable")
	laterCause := errors.New("fixture: later install step failed")
	for _, flow := range []string{"replace", "prune"} {
		for _, scenario := range []string{"same_xml", "changed_xml", "missing", "empty", "read_error", "matching_xml_with_error"} {
			t.Run(flow+"_"+scenario, func(t *testing.T) {
				root := hardenedTempDir(t)
				t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
				s := newRollbackFixtureScheduler()
				s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
				plan := installRecoveryPlan(t, s, root, name)
				prune := flow == "prune"
				var intermediate intentWriteStep
				if prune {
					plan.SchedulerTasks = nil
					plan.FullInstall = true
					s.listSeed = []scheduler.TaskStatus{{Name: name}}
					// A refused prune must not acquire a compensation that could
					// recreate an absent task or overwrite an external definition.
					intermediate = func() (func(), error) { return nil, laterCause }
				}
				s.exportHook = func(task string, ordinal int) ([]byte, error, bool) {
					if task != name {
						t.Fatalf("unexpected export of %s", task)
					}
					if ordinal == 1 {
						switch scenario {
						case "changed_xml":
							s.xml[name] = bytes.Clone(external)
						case "missing":
							delete(s.tasks, name)
							delete(s.xml, name)
						}
						return bytes.Clone(prior), nil, true
					}
					if len(s.deleteAttempts) == 0 {
						switch scenario {
						case "empty":
							return nil, nil, true
						case "read_error":
							return nil, readCause, true
						case "matching_xml_with_error":
							return bytes.Clone(prior), readCause, true
						}
					}
					return nil, nil, false
				}
				s.deleteHook = func(task string, _ int) (error, bool) {
					if task != name {
						t.Fatalf("unexpected deletion of %s", task)
					}
					readInstallRecoverySnapshot(t, root, name, prior)
					return nil, false
				}
				var output bytes.Buffer
				err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, intermediate, !prune, false)
				allowed := scenario == "same_xml"
				if prune {
					if !errors.Is(err, laterCause) {
						t.Errorf("PREDELETE-PRUNE-CAUSE: got %v, want later failure", err)
					}
					if !allowed && (!strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+name) || strings.Contains(output.String(), "Scheduler task removed (obsolete)")) {
						t.Errorf("PREDELETE-PRUNE-OUTPUT: missing refusal or false removal claim: %s", output.String())
					}
				} else if allowed {
					if err != nil {
						t.Errorf("PREDELETE-MATCH: matching definition refused: %v", err)
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "revalidate task "+name+" before replacement") {
						t.Errorf("PREDELETE-REFUSAL: missing named pre-delete refusal: %v", err)
					}
					if (scenario == "read_error" || scenario == "matching_xml_with_error") && !errors.Is(err, readCause) {
						t.Errorf("PREDELETE-READ-CAUSE: original read failure lost: %v", err)
					}
					if scenario == "missing" && !errors.Is(err, scheduler.ErrTaskNotFound) {
						t.Errorf("PREDELETE-ABSENCE-CAUSE: observed absence lost: %v", err)
					}
				}
				var incomplete *InstallSchedulerRollbackIncompleteError
				if errors.As(err, &incomplete) {
					t.Errorf("PREDELETE-CLASS: unmutated or restored task acquired incomplete recovery: %v", err)
				}
				wantXML, wantPresent := prior, true
				switch scenario {
				case "changed_xml":
					wantXML = external
				case "missing":
					wantXML, wantPresent = nil, false
				}
				wantDeletes, wantCreates, wantRestores := 0, 0, 0
				if allowed {
					wantDeletes = 1
					if prune {
						wantRestores = 1
					} else {
						wantCreates = 1
						wantXML = s.createdXML[name]
					}
				}
				if s.tasks[name] != wantPresent || !bytes.Equal(s.xml[name], wantXML) {
					t.Errorf("PREDELETE-STATE: present=%v XML=%q; want present=%v XML=%q", s.tasks[name], s.xml[name], wantPresent, wantXML)
				}
				if len(s.deleteAttempts) != wantDeletes || s.createCount != wantCreates || len(s.restoreNames) != wantRestores || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 {
					t.Errorf("PREDELETE-EFFECTS: deletes=%v creates=%d restores=%v imports=%v runs=%d stops=%d", s.deleteAttempts, s.createCount, s.restoreNames, s.importNames, s.runCount, s.stopCalls)
				}
				path := readInstallRecoverySnapshot(t, root, name, prior)
				if !prune && !allowed && (err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", path))) {
					t.Errorf("PREDELETE-SNAPSHOT: refusal lost retained recovery path: %v", err)
				}
				if prune && !allowed {
					if !strings.Contains(output.String(), fmt.Sprintf("%q", path)) {
						t.Errorf("PREDELETE-PRUNE-SNAPSHOT: refusal lost retained recovery path: %s", output.String())
					}
					if (scenario == "read_error" || scenario == "matching_xml_with_error") && !strings.Contains(output.String(), readCause.Error()) {
						t.Errorf("PREDELETE-PRUNE-READ-CAUSE: warning lost original read failure: %s", output.String())
					}
				}
			})
		}
	}
}

func TestExecuteInstallToPreDeleteRefusalRollsBackEarlierTask(t *testing.T) {
	const first = "mcp-local-hub-predelete-earlier-first"
	const blocked = "mcp-local-hub-predelete-earlier-blocked"
	prior := []byte("<Task>prior blocked task</Task>")
	external := []byte("<Task>external blocked task</Task>")
	readCause := errors.New("fixture: pre-delete read failed")
	rollbackCause := errors.New("fixture: earlier rollback delete denied")
	for _, rollbackFails := range []bool{false, true} {
		label := "earlier_creation_removed"
		if rollbackFails {
			label = "earlier_rollback_failure_retained"
		}
		t.Run(label, func(t *testing.T) {
			root := hardenedTempDir(t)
			s := newRollbackFixtureScheduler()
			s.tasks[blocked], s.xml[blocked] = true, bytes.Clone(prior)
			plan := installRecoveryPlan(t, s, root, blocked)
			plan.SchedulerTasks = append([]ScheduledTaskPlan{{Name: first, Command: plan.installRelayExePath}}, plan.SchedulerTasks...)
			s.exportHook = func(task string, ordinal int) ([]byte, error, bool) {
				if task != blocked {
					return nil, nil, false
				}
				if ordinal == 1 {
					s.xml[blocked] = bytes.Clone(external)
					return bytes.Clone(prior), nil, true
				}
				if len(s.deleteAttempts) == 0 {
					return bytes.Clone(external), readCause, true
				}
				return nil, nil, false
			}
			s.deleteHook = func(task string, _ int) (error, bool) {
				if task == first && rollbackFails {
					return rollbackCause, true
				}
				return nil, false
			}
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "predelete-earlier"}, plan, 0, false, nil, true, false)
			if !errors.Is(err, readCause) || (rollbackFails && !errors.Is(err, rollbackCause)) {
				t.Errorf("PREDELETE-EARLIER-CAUSES: original read or rollback error lost: %v", err)
			}
			var incomplete *InstallSchedulerRollbackIncompleteError
			if got := errors.As(err, &incomplete); got != rollbackFails {
				t.Errorf("PREDELETE-EARLIER-CLASS: incomplete=%v want %v; err=%v", got, rollbackFails, err)
			} else if got && !reflect.DeepEqual(incomplete.Tasks, []string{first}) {
				t.Errorf("PREDELETE-EARLIER-CLASS: unmutated task entered recovery set: %v", incomplete.Tasks)
			}
			if !s.tasks[blocked] || !bytes.Equal(s.xml[blocked], external) || s.tasks[first] != rollbackFails || (!rollbackFails && len(s.xml[first]) != 0) || (rollbackFails && !bytes.Equal(s.xml[first], s.createdXML[first])) {
				t.Errorf("PREDELETE-EARLIER-STATE: first=%v blocked=%v XML=%v", s.tasks[first], s.tasks[blocked], s.xml)
			}
			if !reflect.DeepEqual(s.deleteAttempts, []string{first}) || s.createCount != 1 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Errorf("PREDELETE-EARLIER-EFFECTS: deletes=%v creates=%d restores=%v imports=%v runs=%d stops=%d", s.deleteAttempts, s.createCount, s.restoreNames, s.importNames, s.runCount, s.stopCalls)
			}
			path := readInstallRecoverySnapshot(t, root, blocked, prior)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q", path)) {
				t.Errorf("PREDELETE-EARLIER-SNAPSHOT: original snapshot location lost: %v", err)
			}
		})
	}
}
