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

func TestExecuteInstallToPruneRetainsObservedDefinition(t *testing.T) {
	const server, name = "prune-snapshot", "mcp-local-hub-prune-snapshot-weekly-refresh"
	// Scheduler XML encoding is opaque: retain these UTF-16 bytes verbatim.
	prior := []byte{0xff, 0xfe, '<', 0, 'T', 0, 'a', 0, 's', 0, 'k', 0, '/', 0, '>', 0}
	for _, scenario := range []string{"install_succeeds", "later_failure", "external_absence_before_later_failure"} {
		t.Run(scenario, func(t *testing.T) {
			root := hardenedTempDir(t)
			t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", root)
			s := newRollbackFixtureScheduler()
			s.tasks[name], s.xml[name] = true, bytes.Clone(prior)
			s.listSeed = []scheduler.TaskStatus{{Name: name}}
			plan := installRecoveryPlan(t, s, root, name)
			plan.SchedulerTasks = nil
			plan.FullInstall = true
			cause := errors.New("fixture: later install step failed")
			var intermediate intentWriteStep
			if scenario != "install_succeeds" {
				intermediate = func() (func(), error) {
					if scenario == "external_absence_before_later_failure" {
						delete(s.tasks, name)
						delete(s.xml, name)
					}
					return nil, cause
				}
			}
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, intermediate, false, false)
			var incomplete *InstallSchedulerRollbackIncompleteError
			if (scenario == "install_succeeds" && err != nil) || (scenario != "install_succeeds" && !errors.Is(err, cause)) || errors.As(err, &incomplete) {
				t.Fatalf("PRUNE-REFUSAL-CLASS: %v", err)
			}
			wantPresent := scenario != "external_absence_before_later_failure"
			if s.tasks[name] != wantPresent || (wantPresent && !bytes.Equal(s.xml[name], prior)) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.createCount != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Fatalf("PRUNE-REFUSAL-EFFECTS: tasks=%v deletes=%v restores=%v", s.tasks, s.deleteAttempts, s.restoreNames)
			}
			path := readInstallRecoverySnapshot(t, root, name, prior)
			if !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+name) || !strings.Contains(output.String(), ErrSchedulerReplacementUnowned.Error()) || !strings.Contains(output.String(), strconv.Quote(path)) || strings.Contains(output.String(), "Scheduler task removed (obsolete)") {
				t.Fatalf("PRUNE-REFUSAL-OUTPUT: %s", output.String())
			}
		})
	}
}

func TestExecuteInstallToPruneSnapshotAndCreatorCannotAuthorizeDeletion(t *testing.T) {
	const server, name = "prune-refusal", "mcp-local-hub-prune-refusal-weekly-refresh"
	prior := []byte("<Task>must remain present</Task>")
	for _, scenario := range []string{"snapshot_storage_unavailable", "no_create_only_capability", "create_only_capability"} {
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
			} else if scenario == "no_create_only_capability" {
				backend = struct{ scheduler.Scheduler }{s}
			}
			plan := installRecoveryPlan(t, backend, stateRoot, name)
			plan.SchedulerTasks = nil
			plan.FullInstall = true
			var output bytes.Buffer
			err := executeInstallTo(&output, &config.ServerManifest{Name: server}, plan, 0, false, nil, false, false)
			if err != nil || !s.tasks[name] || !bytes.Equal(s.xml[name], prior) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 {
				t.Fatalf("PRUNE-REFUSAL: err=%v deletes=%v", err, s.deleteAttempts)
			}
			if !strings.Contains(output.String(), "Skipped pruning obsolete scheduler task "+name) || !strings.Contains(output.String(), ErrSchedulerReplacementUnowned.Error()) || strings.Contains(output.String(), "Scheduler task removed (obsolete)") {
				t.Fatalf("PRUNE-REFUSAL-OUTPUT: %s", output.String())
			}
			if scenario == "snapshot_storage_unavailable" && !strings.Contains(output.String(), "retain observed definition") {
				t.Error("storage cause lost")
			}
		})
	}
}

// The prune warning and a refused rollback belong to separate task domains.
// Keep the real install owner inside adopt so provenance retention is exercised.
func TestExecuteAdoptPruneRefusalPreservesCreatedTaskRecovery(t *testing.T) {
	for _, scenario := range []string{"unchanged", "external_same_xml", "external_changed_xml"} {
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
			plan, err := api.BuildAdoptPlan(AdoptOpts{EntryName: entry, Client: "codex-cli", ManifestName: entry,
				Port: nextBindableAdoptPortForTest(t, collectUsedAdoptPorts()), Clients: []string{"codex-cli", "cursor"},
				ScanOpts: ScanOpts{CodexConfigPath: codexPath, CursorConfigPath: cursorPath}})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.SecretRoutedKeys) == 0 {
				t.Fatal("fixture needs routed keys")
			}
			planned, obsolete := "mcp-local-hub-"+entry+"-default", "mcp-local-hub-"+entry+"-weekly-refresh"
			prior := []byte("<Task>obsolete</Task>")
			s := newRollbackFixtureScheduler()
			s.tasks[obsolete], s.xml[obsolete] = true, bytes.Clone(prior)
			s.listSeed = []scheduler.TaskStatus{{Name: obsolete}}
			cause := errors.New("fixture: later install step failed")
			var wanted []byte
			intermediate := func() (func(), error) {
				if scenario == "external_same_xml" {
					s.xml[planned] = bytes.Clone(s.createdXML[planned])
				}
				if scenario == "external_changed_xml" {
					s.xml[planned] = []byte("<Task>external</Task>")
				}
				wanted = bytes.Clone(s.xml[planned])
				return nil, cause
			}
			installPlan := installRecoveryPlan(t, s, stateRoot, planned)
			installPlan.FullInstall = true
			calls := 0
			var output bytes.Buffer
			err = api.ExecuteAdoptWithOpts(plan, &output, ExecuteAdoptOpts{installFn: func(_ context.Context, _ *API, opts InstallOpts) error {
				calls++
				return executeInstallTo(opts.Writer, &config.ServerManifest{Name: entry}, installPlan, 0, false, intermediate, false, false)
			}})
			var staged *AdoptStageError
			if calls != 1 || !errors.Is(err, cause) || !errors.Is(err, ErrSchedulerReplacementUnowned) || !errors.As(err, &staged) || staged.CommitState != "committed_unverified" || !reflect.DeepEqual(installSchedulerRecoveryTasks(err), []string{planned}) {
				t.Fatalf("ADOPT-REFUSAL-OWNER: calls=%d err=%v", calls, err)
			}
			assertPreservedProvenance(t, entry, plan, manifestRoot, priorClients)
			if !s.tasks[planned] || !bytes.Equal(s.xml[planned], wanted) || !s.tasks[obsolete] || !bytes.Equal(s.xml[obsolete], prior) || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 || len(s.importNames) != 0 || s.runCount != 0 || s.stopCalls != 0 {
				t.Fatalf("ADOPT-REFUSAL-STATE: tasks=%v deletes=%v", s.tasks, s.deleteAttempts)
			}
			logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
			if ev, _ := findSupervisorEventByName(t, logPath, "adopt-scheduler-recovery-preserved"); ev == nil {
				t.Error("preservation event missing")
			}
			if ev, _ := findSupervisorEventByName(t, logPath, "adopt-executed"); ev != nil || strings.Contains(output.String(), "Adopted ") || strings.Contains(output.String(), "Install complete") {
				t.Error("incomplete recovery reported success")
			}
		})
	}
}

// A refused optional prune has no compensating action to execute after another
// owner fails. This remains true when the task disappears before the failure.
func TestExecuteInstallToEmptyPruneHasNoCompensation(t *testing.T) {
	s := newRollbackFixtureScheduler()
	cause := errors.New("fixture: later install failure")
	plan := installRecoveryPlan(t, s, hardenedTempDir(t), "unused")
	plan.SchedulerTasks = nil
	plan.FullInstall = true
	err := executeInstallTo(io.Discard, &config.ServerManifest{Name: "empty-prune"}, plan, 0, false, func() (func(), error) { return nil, cause }, false, false)
	if err != cause || len(s.deleteAttempts) != 0 || len(s.restoreNames) != 0 {
		t.Fatalf("empty prune acquired rollback: %v", err)
	}
}
