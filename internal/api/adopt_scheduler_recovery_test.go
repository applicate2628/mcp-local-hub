package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/secrets"
)

type schedulerRecoveryLeaseOwner struct {
	cause   error
	settled *int
}

func (o schedulerRecoveryLeaseOwner) AcquireAdoptLease(name string) (AdoptLease, bool, error) {
	lease, acquired, err := NewAdoptLeaseOwner().AcquireAdoptLease(name)
	if err != nil || !acquired {
		return lease, acquired, err
	}
	return schedulerRecoveryLease{AdoptLease: lease, cause: o.cause, settled: o.settled}, true, nil
}

type schedulerRecoveryLease struct {
	AdoptLease
	cause   error
	settled *int
}

func (l schedulerRecoveryLease) ReleaseAndRemove() error {
	(*l.settled)++
	return errors.Join(l.AdoptLease.ReleaseAndRemove(), l.cause)
}

// The core rollback tests execute the real install loop. This table instead
// exercises the outer transaction owner's existing installFn boundary: real
// provenance, manifest, routed vault keys, events and lease settlement.
func TestExecuteAdoptSchedulerRecoveryPreservesProvenance(t *testing.T) {
	for _, scenario := range []string{"later", "current_uncertain", "current_acknowledged", "client_and_scheduler", "lease_release_failure"} {
		t.Run(scenario, func(t *testing.T) {
			entry := "r22-adopt-" + strings.ReplaceAll(scenario, "_", "-")
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
				t.Fatal("fixture needs exact routed keys to distinguish preservation from abort")
			}
			installCause := errors.New("fixture: install failed")
			recoveryCause := errors.New("fixture: scheduler recovery failed")
			name := "mcp-local-hub-" + entry + "-default"
			var installErr error = &InstallSchedulerRollbackIncompleteError{Tasks: []string{name}, cause: errors.Join(installCause, recoveryCause)}
			if scenario == "current_uncertain" || scenario == "current_acknowledged" {
				current := &InstallTaskRecoveryError{TaskName: name, Cause: installCause, SnapshotPath: filepath.Join(stateRoot, "private-recovery.json")}
				if scenario == "current_uncertain" {
					current.Recovery = recoveryCause
				}
				installErr = current
			}
			if scenario == "client_and_scheduler" {
				installErr = &InstallClientRollbackIncompleteError{Clients: []string{"codex-cli"}, cause: installErr}
			}
			promoteCalls := 0
			previousPromote := promoteAdoptProvenanceFn
			promoteAdoptProvenanceFn = func(string) error { promoteCalls++; return nil }
			t.Cleanup(func() { promoteAdoptProvenanceFn = previousPromote })
			installCalls := 0
			opts := ExecuteAdoptOpts{installFn: func(context.Context, *API, InstallOpts) error {
				installCalls++
				return installErr
			}}
			releaseCause := errors.New("fixture: release after real lease settlement")
			settled := 0
			if scenario == "lease_release_failure" {
				opts.LeaseOwner = schedulerRecoveryLeaseOwner{cause: releaseCause, settled: &settled}
			}
			var output bytes.Buffer
			err = api.ExecuteAdoptWithOpts(plan, &output, opts)
			if installCalls != 1 || !errors.Is(err, installCause) || promoteCalls != 0 || strings.Contains(output.String(), "Adopted ") || strings.Contains(output.String(), "Install complete") {
				t.Fatalf("ADOPT-SCHEDULER-OUTCOME: calls=%d promote=%d error=%v output=%q", installCalls, promoteCalls, err, output.String())
			}
			logPath := filepath.Join(stateRoot, SupervisorEventLogFileLeaf)
			if ev, _ := findSupervisorEventByName(t, logPath, "adopt-executed"); ev != nil {
				t.Error("ADOPT-SCHEDULER-EVENT: failed installation emitted adopt success")
			}
			if scenario == "current_acknowledged" {
				assertNoAdoptProvenanceResidue(t, entry)
				if _, statErr := os.Stat(filepath.Join(manifestRoot, entry, "manifest.yaml")); !os.IsNotExist(statErr) {
					t.Errorf("ADOPT-CURRENT-ACK: ordinary abort did not remove manifest: %v", statErr)
				}
				vault, vaultErr := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
				if vaultErr != nil || len(vault.List()) != 0 {
					t.Fatalf("ADOPT-CURRENT-ACK: ordinary abort retained routed keys: %v", vaultErr)
				}
				if ev, _ := findSupervisorEventByName(t, logPath, "adopt-scheduler-recovery-preserved"); ev != nil {
					t.Error("ADOPT-CURRENT-ACK: acknowledged recovery misclassified as incomplete")
				}
				return
			}
			if !errors.Is(err, recoveryCause) {
				t.Errorf("ADOPT-SCHEDULER-CAUSE: recovery cause lost: %v", err)
			}
			if scenario == "lease_release_failure" && (settled != 1 || !errors.Is(err, releaseCause)) {
				t.Errorf("ADOPT-SCHEDULER-LEASE: settlement=%d or release cause lost: %v", settled, err)
			}
			var staged *AdoptStageError
			if !errors.As(err, &staged) || staged.CommitState != "committed_unverified" {
				t.Errorf("ADOPT-SCHEDULER-STAGE: uncertain effects presented as unapplied: %v", err)
			}
			if scenario == "client_and_scheduler" {
				var clientErr *InstallClientRollbackIncompleteError
				var taskErr *InstallSchedulerRollbackIncompleteError
				if !errors.As(err, &clientErr) || !errors.As(err, &taskErr) {
					t.Errorf("ADOPT-SCHEDULER-JOIN: error domain lost: %v", err)
				}
				if !strings.Contains(err.Error(), "reconcile both client configurations and scheduler tasks") || strings.Contains(err.Error(), "then remove manifest") {
					t.Errorf("ADOPT-SCHEDULER-GUIDANCE: combined failure permits cleanup after client-only recovery: %v", err)
				}
				assertPreservedEvent(t, entry, []string{"codex-cli"})
			}
			assertPreservedProvenance(t, entry, plan, manifestRoot, priorClients)
			ev, _ := findSupervisorEventByName(t, logPath, "adopt-scheduler-recovery-preserved")
			if ev == nil {
				t.Fatal("ADOPT-SCHEDULER-EVENT: missing scheduler preservation event")
			}
			body, _ := ev["body"].(map[string]any)
			tasks, _ := body["tasks"].([]any)
			if ev["source"] != "adopt" || body["manifest"] != entry || body["task_count"] != float64(1) || len(tasks) != 1 || tasks[0] != name || len(body) != 3 {
				t.Errorf("ADOPT-SCHEDULER-EVENT: incorrect task domain: %#v", ev)
			}
			raw, marshalErr := json.Marshal(ev)
			if marshalErr != nil || bytes.Contains(raw, []byte("literal-secret-value")) || bytes.Contains(raw, []byte("private-recovery.json")) || bytes.Contains(raw, []byte(recoveryCause.Error())) {
				t.Errorf("ADOPT-SCHEDULER-REDACTION: event leaked recovery details: %v", marshalErr)
			}
		})
	}
}

// Inject the lower error at the real client write seam before managed intent
// publication. This traverses the actual provider unpublished-rollback gate and
// adopt owner; a bare installFn error would not exercise that proof boundary.
func TestProviderInstallSchedulerRecoveryIsNotUnpublishedRollback(t *testing.T) {
	for _, current := range []bool{false, true} {
		label := "later"
		if current {
			label = "current"
		}
		t.Run(label, func(t *testing.T) {
			name := "r22-provider-" + label
			clientPath, manifestRoot, stateRoot, plan, provider, deps := setupProviderInstallFailureFixture(t, name)
			cause := errors.New("fixture: scheduler effect unconfirmed")
			var schedulerErr error = &InstallSchedulerRollbackIncompleteError{Tasks: []string{"mcp-local-hub-" + name + "-default"}, cause: cause}
			if current {
				schedulerErr = &InstallTaskRecoveryError{TaskName: "mcp-local-hub-" + name + "-default", Cause: errors.New("fixture: create"), Recovery: cause}
			}
			previousWrite := clients.WriteConfigFile
			writeCalls := 0
			clients.WriteConfigFile = func(path string, raw []byte) error {
				if filepath.Clean(path) == filepath.Clean(clientPath) {
					writeCalls++
					return schedulerErr
				}
				return previousWrite(path, raw)
			}
			t.Cleanup(func() { clients.WriteConfigFile = previousWrite })
			err := NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{providerDeps: deps})
			if !errors.Is(err, cause) || writeCalls != 1 || len(installSchedulerRecoveryTasks(err)) == 0 {
				t.Fatalf("PROVIDER-SCHEDULER-CAUSE: seam not reached or cause lost: writes=%d err=%v", writeCalls, err)
			}
			var clientIncomplete *InstallClientRollbackIncompleteError
			if errors.As(err, &clientIncomplete) {
				t.Fatalf("PROVIDER-SCHEDULER-FIXTURE: client rollback failure masks the scheduler proof gate: %v", err)
			}
			var proof *providerInstallUnpublishedRollbackError
			if errors.As(err, &proof) {
				t.Errorf("PROVIDER-SCHEDULER-PROOF: unconfirmed scheduler effect certified as rolled back: %v", err)
			}
			if provider.entry.Enabled {
				t.Error("PROVIDER-SCHEDULER-RESTORE: direct provider re-enabled beside uncertain managed state")
			}
			if phase, phaseErr := readProviderInstallPhase(name); phaseErr != nil || phase != providerInstallPhaseStarted {
				t.Errorf("PROVIDER-SCHEDULER-PHASE: uncertain effect settled: phase=%q err=%v", phase, phaseErr)
			}
			if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
				t.Errorf("PROVIDER-SCHEDULER-MANIFEST: recovery manifest missing: %v", statErr)
			}
			if rec, found, readErr := ReadAdoptProvenance(name); readErr != nil || !found || rec.OperationState != AdoptOperationStateAdopting {
				t.Errorf("PROVIDER-SCHEDULER-PROVENANCE: recovery state changed: found=%v err=%v", found, readErr)
			}
			if _, statErr := os.Stat(filepath.Join(stateRoot, supervisorIntentFileLeaf)); !os.IsNotExist(statErr) {
				t.Errorf("PROVIDER-SCHEDULER-FIXTURE: expected pre-intent failure: %v", statErr)
			}
		})
	}
}
