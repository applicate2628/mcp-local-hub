package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/scheduler"
)

// Exercise pruning through the install owner, using its existing scheduler and
// protected-state fixtures. These tests intentionally do not depend on the
// prune helper's compensation signature, so they also compile before the fix.
func TestExecuteInstallToPruneSnapshotBeforeDelete(t *testing.T) {
	const server = "prune-snapshot"
	const name = "mcp-local-hub-prune-snapshot-weekly-refresh"
	// The scheduler owns the encoding: retain these UTF-16 bytes verbatim.
	prior := []byte{0xff, 0xfe, '<', 0, 'T', 0, 'a', 0, 's', 0, 'k', 0, '/', 0, '>', 0}
	for _, laterFailure := range []bool{false, true} {
		label := "successful_prune"
		if laterFailure {
			label = "later_failure_restores_owned_bytes"
		}
		t.Run(label, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
			s := newRollbackFixtureScheduler()
			borrowed := bytes.Clone(prior)
			s.tasks[name], s.xml[name] = true, borrowed
			s.listSeed = []scheduler.TaskStatus{{Name: name}}
			plan := installRecoveryPlan(t, s, root, name)
			plan.SchedulerTasks = nil
			plan.FullInstall = true
			s.deleteHook = func(task string, _ int) (error, bool) {
				if task != name {
					t.Fatalf("unexpected deletion of %s", task)
				}
				readInstallRecoverySnapshot(t, root, name, prior)
				// ExportXML returned borrowed backend storage. Reusing it after
				// Delete must not change either retained recovery representation.
				for i := range borrowed {
					borrowed[i] = 'x'
				}
				return nil, false
			}
			cause := errors.New("fixture: later install step failed")
			var intermediate intentWriteStep
			if laterFailure {
				intermediate = func() (func(), error) { return nil, cause }
			}
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: server}, plan, 0, false, intermediate, false, false)
			if laterFailure {
				var incomplete *InstallSchedulerRollbackIncompleteError
				if !errors.Is(err, cause) || errors.As(err, &incomplete) || !s.tasks[name] || !bytes.Equal(s.xml[name], prior) || !reflect.DeepEqual(s.restoreNames, []string{name}) {
					t.Fatalf("PRUNE-OWNED-RECOVERY: original bytes not restored through create-only registration: err=%v present=%v XML=%q restores=%v", err, s.tasks[name], s.xml[name], s.restoreNames)
				}
			} else if err != nil || s.tasks[name] || len(s.restoreNames) != 0 {
				t.Fatalf("PRUNE-COMMIT: successful prune did not remain applied: err=%v present=%v restores=%v", err, s.tasks[name], s.restoreNames)
			}
			readInstallRecoverySnapshot(t, root, name, prior)
			if !reflect.DeepEqual(s.deleteAttempts, []string{name}) || len(s.importNames) != 0 || s.createCount != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Errorf("PRUNE-RECOVERY-EFFECTS: deletes=%v imports=%v creates=%d runs=%d stops=%d", s.deleteAttempts, s.importNames, s.createCount, s.runCount, s.stopCalls)
			}
		})
	}
}

func TestExecuteInstallToPruneRequiresRecoveryBeforeDelete(t *testing.T) {
	const server = "prune-refusal"
	const name = "mcp-local-hub-prune-refusal-weekly-refresh"
	prior := []byte("<Task>must remain present</Task>")
	for _, scenario := range []string{"snapshot_storage_unavailable", "no_create_only_capability"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
			stateRoot := root
			s := newRollbackFixtureScheduler()
			s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			s.listSeed = []scheduler.TaskStatus{{Name: name}}
			var backend scheduler.Scheduler = s
			if scenario == "snapshot_storage_unavailable" {
				stateRoot = filepath.Join(root, "not-a-directory")
				if err := os.WriteFile(stateRoot, []byte("occupied"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				backend = struct{ scheduler.Scheduler }{s}
			}
			plan := installRecoveryPlan(t, backend, stateRoot, name)
			plan.SchedulerTasks = nil
			plan.FullInstall = true
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, nil, false, false)
			if err != nil || !s.tasks[name] || !bytes.Equal(s.xml[name], prior) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Fatalf("PRUNE-REFUSAL: optional pruning changed an unrecoverable task: err=%v present=%v deletes=%v restores=%v imports=%v", err, s.tasks[name], s.deleteAttempts, s.restoreNames, s.importNames)
			}
			if !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+name) || strings.Contains(output.String(), "Scheduler task removed (obsolete)") {
				t.Errorf("PRUNE-REFUSAL-DIAGNOSTIC: missing named warning or false deletion claim: %q", output.String())
			}
		})
	}
}

func TestExecuteInstallToPruneDeleteUnconfirmedStopsAndRollsBack(t *testing.T) {
	const server = "prune-unconfirmed"
	const planned = "mcp-local-hub-prune-unconfirmed-default"
	const earlier = "mcp-local-hub-prune-unconfirmed-a-earlier"
	const failed = "mcp-local-hub-prune-unconfirmed-b-failed"
	const later = "mcp-local-hub-prune-unconfirmed-c-later"
	prior := []byte("<Task>prior obsolete definition</Task>")
	for _, scenario := range []string{"delete_applied_reply_lost", "partial_observation_error", "earlier_restore_failed"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
			s := newRollbackFixtureScheduler()
			for _, name := range []string{earlier, failed, later} {
				s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
				s.listSeed = append(s.listSeed, scheduler.TaskStatus{Name: name})
			}
			cause := &os.PathError{Op: "delete", Path: "synthetic-obsolete-task", Err: errors.New("fixture: deletion reply unavailable")}
			readCause := errors.New("fixture: observation unavailable")
			restoreCause := errors.New("fixture: earlier prune restoration failed")
			failedDelete := false
			s.deleteHook = func(name string, _ int) (error, bool) {
				if name != failed {
					return nil, false
				}
				failedDelete = true
				if scenario != "partial_observation_error" {
					delete(s.tasks, name)
					delete(s.xml, name)
				}
				return cause, true
			}
			s.exportHook = func(name string, _ int) ([]byte, error, bool) {
				if name == failed && failedDelete && scenario == "partial_observation_error" {
					return bytes.Clone(prior), readCause, true
				}
				return nil, nil, false
			}
			if scenario == "earlier_restore_failed" {
				s.restoreHook = func(name string, _ []byte) (error, bool) {
					if name == earlier {
						return restoreCause, true
					}
					return nil, false
				}
			}
			plan := installRecoveryPlan(t, s, root, planned)
			plan.FullInstall = true
			continued := false
			intermediate := func() (func(), error) { continued = true; return nil, nil }
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, intermediate, false, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			var typedCause *os.PathError
			wantNames := []string{failed}
			if scenario == "earlier_restore_failed" {
				wantNames = []string{earlier, failed}
			}
			if !errors.Is(err, cause) || !errors.As(err, &typedCause) || typedCause != cause || !errors.As(err, &incomplete) || !reflect.DeepEqual(incomplete.Tasks, wantNames) || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), wantNames) {
				t.Fatalf("PRUNE-UNCONFIRMED-OWNER: failed deletion lost its cause or incomplete classification: %v", err)
			}
			if scenario == "partial_observation_error" && !errors.Is(err, readCause) {
				t.Errorf("PRUNE-UNCONFIRMED-CAUSE: observation error lost: %v", err)
			}
			if scenario == "earlier_restore_failed" && !errors.Is(err, restoreCause) {
				t.Errorf("PRUNE-UNCONFIRMED-CAUSE: earlier prune recovery error lost: %v", err)
			}
			wantEarlierPresent := scenario != "earlier_restore_failed"
			if continued || s.tasks[earlier] != wantEarlierPresent || (wantEarlierPresent && !bytes.Equal(s.xml[earlier], prior)) || !s.tasks[later] || !bytes.Equal(s.xml[later], prior) || s.tasks[planned] {
				t.Errorf("PRUNE-UNCONFIRMED-STATE: earlier work not rolled back or later work continued: continued=%v tasks=%v", continued, s.tasks)
			}
			wantFailedPresent := scenario == "partial_observation_error"
			if s.tasks[failed] != wantFailedPresent || (wantFailedPresent && !bytes.Equal(s.xml[failed], prior)) {
				t.Error("PRUNE-UNCONFIRMED-CURRENT: uncertain current task was changed by compensation")
			}
			if !reflect.DeepEqual(s.deleteAttempts, []string{earlier, failed, planned}) || !reflect.DeepEqual(s.restoreNames, []string{earlier}) || len(s.importNames) != 0 || s.createCount != 1 || s.runCount != 0 || s.stopCalls != 0 {
				t.Errorf("PRUNE-UNCONFIRMED-EFFECTS: deletes=%v restores=%v imports=%v creates=%d runs=%d stops=%d", s.deleteAttempts, s.restoreNames, s.importNames, s.createCount, s.runCount, s.stopCalls)
			}
			if strings.Contains(output.String(), "Install complete") {
				t.Error("PRUNE-UNCONFIRMED-OUTPUT: uncertain deletion reported install success")
			}
		})
	}
}

func TestExecuteInstallToPruneRollbackObservation(t *testing.T) {
	const server = "prune-observation"
	const name = "mcp-local-hub-prune-observation-weekly-refresh"
	prior := []byte("<Task>prior obsolete definition</Task>")
	external := []byte("<Task>external occupant</Task>")
	for _, scenario := range []string{"prior_already_present", "external_already_present", "observation_unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
			s := newRollbackFixtureScheduler()
			s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			s.listSeed = []scheduler.TaskStatus{{Name: name}}
			cause := errors.New("fixture: later install failure")
			readCause := errors.New("fixture: rollback observation unavailable")
			rollbackStage := false
			s.exportHook = func(task string, _ int) ([]byte, error, bool) {
				if task == name && rollbackStage && scenario == "observation_unavailable" {
					// Even apparently matching bytes cannot prove restoration
					// when returned together with an observation error.
					return bytes.Clone(prior), readCause, true
				}
				return nil, nil, false
			}
			wantXML := external
			if scenario == "prior_already_present" {
				wantXML = prior
			}
			intermediate := func() (func(), error) {
				s.tasks[name], s.xml[name] = true, bytes.Clone(wantXML)
				rollbackStage = true
				return nil, cause
			}
			plan := installRecoveryPlan(t, s, root, name)
			plan.SchedulerTasks = nil
			plan.FullInstall = true
			err := executeInstallTo(io.Discard, &config.ServerManifest{Name: server}, plan, 0, false, intermediate, false, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			wantIncomplete := scenario != "prior_already_present"
			if !errors.Is(err, cause) || errors.As(err, &incomplete) != wantIncomplete {
				t.Fatalf("PRUNE-OBSERVATION-OWNER: incorrect recovery classification: err=%v wantIncomplete=%v", err, wantIncomplete)
			}
			if wantIncomplete && !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{name}) {
				t.Errorf("PRUNE-OBSERVATION-NAME: affected task missing or duplicated: %v", installSchedulerRecoveryTasks(err))
			}
			if scenario == "observation_unavailable" && !errors.Is(err, readCause) {
				t.Errorf("PRUNE-OBSERVATION-CAUSE: read error lost: %v", err)
			}
			if !s.tasks[name] || !bytes.Equal(s.xml[name], wantXML) || !reflect.DeepEqual(s.deleteAttempts, []string{name}) || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Fatalf("PRUNE-OBSERVATION-STATE: occupied or uncertain task was changed: tasks=%v XML=%q deletes=%v restores=%v imports=%v", s.tasks, s.xml[name], s.deleteAttempts, s.restoreNames, s.importNames)
			}
		})
	}
}

func TestExecuteInstallToPruneRollbackPreservesExternalOccupant(t *testing.T) {
	const server = "prune-conflict"
	const name = "mcp-local-hub-prune-conflict-weekly-refresh"
	root := hardenedTempDir(t)
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
	prior := []byte("<Task>prior obsolete definition</Task>")
	external := []byte("<Task>external occupant</Task>")
	cause := errors.New("fixture: later install failure")
	s := newRollbackFixtureScheduler()
	s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
	s.listSeed = []scheduler.TaskStatus{{Name: name}}
	s.restoreHook = func(task string, raw []byte) (error, bool) {
		if task != name || !bytes.Equal(raw, prior) {
			t.Fatalf("PRUNE-CONFLICT-INPUT: wrong restoration task or bytes: %s %q", task, raw)
		}
		// Occupancy changes at the native create-only boundary, after any
		// preceding absence observation. ImportXML would overwrite this task.
		s.tasks[task], s.xml[task] = true, bytes.Clone(external)
		return scheduler.ErrTaskAlreadyExists, true
	}
	plan := installRecoveryPlan(t, s, root, name)
	plan.SchedulerTasks = nil
	plan.FullInstall = true
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: server}, plan, 0, false, func() (func(), error) { return nil, cause }, false, false)
	var incomplete *InstallSchedulerRollbackIncompleteError
	if !errors.Is(err, cause) || !errors.Is(err, scheduler.ErrTaskAlreadyExists) || !errors.As(err, &incomplete) || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{name}) {
		t.Fatalf("PRUNE-CONFLICT-OWNER: install/recovery causes or affected task lost: %v", err)
	}
	if !s.tasks[name] || !bytes.Equal(s.xml[name], external) || !reflect.DeepEqual(s.deleteAttempts, []string{name}) || !reflect.DeepEqual(s.restoreNames, []string{name}) || len(s.importNames) != 0 {
		t.Fatalf("PRUNE-CONFLICT-STATE: external definition overwritten or retried: tasks=%v XML=%q deletes=%v restores=%v imports=%v", s.tasks, s.xml[name], s.deleteAttempts, s.restoreNames, s.importNames)
	}
	path := readInstallRecoverySnapshot(t, root, name, prior)
	if !strings.Contains(err.Error(), strconv.Quote(path)) {
		t.Error("PRUNE-CONFLICT-SNAPSHOT: retained recovery path missing from error")
	}
}

// Keep the real prune loop inside the existing adopt install boundary; the
// generic adopt tests already cover event redaction and combined error domains.
func TestExecuteAdoptPruneRecoveryPreservesProvenance(t *testing.T) {
	for _, scenario := range []string{"delete_unconfirmed", "restore_conflict"} {
		t.Run(scenario, func(t *testing.T) {
			entry := "prune-adopt-" + strings.ReplaceAll(scenario, "_", "-")
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
				t.Fatal("fixture needs routed keys to prove preservation")
			}
			planned := "mcp-local-hub-" + entry + "-default"
			obsolete := "mcp-local-hub-" + entry + "-weekly-refresh"
			prior := []byte("<Task>prior obsolete definition</Task>")
			external := []byte("<Task>external occupant</Task>")
			cause := errors.New("fixture: prune transaction failed")
			s := newRollbackFixtureScheduler()
			s.tasks[obsolete], s.xml[obsolete] = true, bytes.Clone(prior)
			s.listSeed = []scheduler.TaskStatus{{Name: obsolete}}
			var intermediate intentWriteStep
			if scenario == "delete_unconfirmed" {
				s.deleteHook = func(name string, _ int) (error, bool) {
					if name != obsolete {
						return nil, false
					}
					delete(s.tasks, name)
					delete(s.xml, name)
					return cause, true
				}
			} else {
				intermediate = func() (func(), error) { return nil, cause }
				s.restoreHook = func(name string, _ []byte) (error, bool) {
					if name != obsolete {
						return nil, false
					}
					s.tasks[name], s.xml[name] = true, bytes.Clone(external)
					return scheduler.ErrTaskAlreadyExists, true
				}
			}
			installPlan := installRecoveryPlan(t, s, stateRoot, planned)
			installPlan.FullInstall = true
			installCalls := 0
			var output bytes.Buffer
			err = api.ExecuteAdoptWithOpts(plan, &output, ExecuteAdoptOpts{
				installFn: func(_ context.Context, _ *API, opts InstallOpts) error {
					installCalls++
					return executeInstallTo(opts.Writer, &config.ServerManifest{Name: entry}, installPlan, 0, false, intermediate, false, false)
				},
			})
			var staged *AdoptStageError
			var incomplete *InstallSchedulerRollbackIncompleteError
			if installCalls != 1 || !errors.Is(err, cause) || !errors.As(err, &incomplete) || !errors.As(err, &staged) || staged.CommitState != "committed_unverified" || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{obsolete}) {
				t.Fatalf("ADOPT-PRUNE-OWNER: uncertain prune recovery did not reach adopt: calls=%d err=%v", installCalls, err)
			}
			if scenario == "restore_conflict" && (!errors.Is(err, scheduler.ErrTaskAlreadyExists) || !s.tasks[obsolete] || !bytes.Equal(s.xml[obsolete], external)) {
				t.Errorf("ADOPT-PRUNE-CONFLICT: external occupant or recovery cause lost: err=%v XML=%q", err, s.xml[obsolete])
			}
			assertPreservedProvenance(t, entry, plan, manifestRoot, priorClients)
			readInstallRecoverySnapshot(t, stateRoot, obsolete, prior)
			logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
			if ev, _ := findSupervisorEventByName(t, logPath, "adopt-scheduler-recovery-preserved"); ev == nil {
				t.Error("ADOPT-PRUNE-EVENT: scheduler preservation event missing")
			}
			if ev, _ := findSupervisorEventByName(t, logPath, "adopt-executed"); ev != nil || strings.Contains(output.String(), "Adopted ") || strings.Contains(output.String(), "Install complete") {
				t.Error("ADOPT-PRUNE-OUTPUT: uncertain recovery reported success")
			}
			if s.tasks[planned] || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Errorf("ADOPT-PRUNE-EFFECTS: earlier task remained or unsafe scheduler effects occurred: tasks=%v imports=%v runs=%d stops=%d", s.tasks, s.importNames, s.runCount, s.stopCalls)
			}
		})
	}
}
