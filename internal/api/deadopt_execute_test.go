package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
	"mcp-local-hub/internal/secrets"

	"github.com/gofrs/flock"
)

func TestExecuteDeAdoptT1HappyPathClosesAllOwnedState(t *testing.T) {
	name := "deadopt-execute-happy"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("report = %+v, want one restored client", report)
	}
	got := mustReadFileForAdoptTest(t, codexPath)
	mutator, ok := clients.AsCASEntryMutator(clients.AllClients()["codex-cli"])
	if !ok {
		t.Fatal("codex-cli does not expose CASEntryMutator")
	}
	gotSubtree, gotPresent, gotErr := mutator.EntryRawSubtree(got, name)
	wantSubtree, wantPresent, wantErr := mutator.EntryRawSubtree(snapshot, name)
	if gotErr != nil || wantErr != nil || !gotPresent || !wantPresent || !reflect.DeepEqual(gotSubtree, wantSubtree) {
		t.Fatalf("restored entry is not functionally equivalent\ngot subtree: %#v (present=%v err=%v)\nwant subtree: %#v (present=%v err=%v)", gotSubtree, gotPresent, gotErr, wantSubtree, wantPresent, wantErr)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)

	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 0 {
		t.Fatalf("supervisor intent still has daemon rows: %+v", intent.Daemons)
	}
	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if !bytes.Contains(logBytes, []byte(`"source":"deadopt"`)) || !bytes.Contains(logBytes, []byte(`"event":"deadopt-executed"`)) {
		t.Fatalf("deadopt-executed event missing:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptAbsentClientUsesVerifiedManifestTimeoutAtPreviewAndCAS(t *testing.T) {
	name := "deadopt-absent-timeout"
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateAdopted, originalState: AdoptOriginalStateAbsent,
		liveConfig: deAdoptHubConfigWithToolTimeout(name, 30), manifestPresent: true,
		manifestBytes: deAdoptManagedManifest(name, 30),
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRemovePending {
		t.Fatalf("preview clients=%+v; want timeout-matched remove-pending", plan.Clients)
	}
	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("apply after matching preview: %v", err)
	}
	if len(report.Failed) != 0 || len(report.Restored) != 1 {
		t.Fatalf("apply report=%+v", report)
	}
	if live := mustReadFileForAdoptTest(t, codexPath); bytes.Contains(live, []byte(name)) {
		t.Fatalf("absent original hub entry remained after CAS removal:\n%s", live)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderRestoresOnlyAfterManagedStopSettlement(t *testing.T) {
	name := "provider-deadopt-settlement"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateAdopted, originalState: AdoptOriginalStatePresent,
		liveConfig: deAdoptHubConfig(name), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: true, manifestBytes: deAdoptManagedManifest(name, 0),
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled", ActivationEnabledPresent: true, ActivationEnabled: false, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func(_ context.Context, frozen SupervisorDaemon) (StoppedSettlement, error) {
		if frozen.Server != name || frozen.ManifestHash != rec.ExpectedManifestHash {
			t.Fatalf("frozen descriptor=%+v", frozen)
		}
		stopped = true
		return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}
	provider.onCAS = func() {
		if !stopped {
			t.Fatal("provider activation restored before managed stop settlement")
		}
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, nil, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider, stop: stop}})
	if err != nil {
		t.Fatalf("execute provider deadopt: %v", err)
	}
	if !stopped || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("stop=%t provider=%+v report=%+v", stopped, provider.entry, report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallMissingIntent(t *testing.T) {
	name := "provider-preinstall-missing-intent"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	stopCalls := 0
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
			stopCalls++
			return StoppedSettlement{}, errors.New("preinstall recovery must not stop a daemon")
		},
	}})
	if err != nil || report == nil {
		t.Fatalf("preinstall missing-intent recovery report=%+v err=%v", report, err)
	}
	if stopCalls != 0 || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("preinstall missing-intent side effects: stop=%d provider calls=%d enabled=%t", stopCalls, provider.calls, provider.entry.Enabled)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallEmptyIntent(t *testing.T) {
	name := "provider-preinstall-empty-intent"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	stopCalls := 0
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
			stopCalls++
			return StoppedSettlement{}, errors.New("preinstall recovery must not stop a daemon")
		},
	}})
	if err != nil || report == nil {
		t.Fatalf("preinstall empty-intent recovery report=%+v err=%v", report, err)
	}
	if stopCalls != 0 || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("preinstall empty-intent side effects: stop=%d provider calls=%d enabled=%t", stopCalls, provider.calls, provider.entry.Enabled)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallOwnerContradictionRefused(t *testing.T) {
	tests := []struct {
		name         string
		installState ProviderManagedInstallStateV1
		daemon       func(AdoptProvenanceRecord) SupervisorDaemon
		wantRefused  bool
		wantStop     int
		wantRetained bool
	}{
		{
			name: "direct identity with mismatching port and hash", installState: ProviderManagedInstallPreInstall, wantRefused: true,
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: `\unrelated-task`, Server: rec.ManifestName, Daemon: adoptDefaultDaemonName, Port: rec.Port + 1, ManifestHash: "mismatch"}
			},
		},
		{
			name: "direct exact live identity", installState: ProviderManagedInstallPreInstall, wantRefused: true,
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return providerRecoveryExactDaemon(rec)
			},
		},
		{
			name: "legacy exact task identity", installState: ProviderManagedInstallPreInstall, wantRefused: true,
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(rec.ManifestName, adoptDefaultDaemonName)), Port: rec.Port + 1, ManifestHash: "mismatch"}
			},
		},
		{
			name: "legacy different task identity", installState: ProviderManagedInstallPreInstall, wantRetained: true,
			daemon: func(AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon("unrelated-provider", adoptDefaultDaemonName))}
			},
		},
		{
			name: "may-have-applied exact owner uses stop", installState: ProviderManagedInstallMayHaveApplied, wantStop: 1,
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return providerRecoveryExactDaemon(rec)
			},
		},
		{
			name: "legacy unknown exact owner uses stop", installState: ProviderManagedInstallUnknown, wantStop: 1,
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return providerRecoveryExactDaemon(rec)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-owner-" + strings.ReplaceAll(tc.name, " ", "-")
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
			rec.ProviderSource.ManagedInstallState = tc.installState
			writeDeAdoptExecutorRecord(t, rec)
			intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{tc.daemon(rec)}}
			if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
				t.Fatal(err)
			}
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			stopCalls := 0
			closeCalls := 0
			report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: provider,
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
				},
				close: func(manifestName string) error {
					closeCalls++
					return CloseAdoptProvenance(manifestName)
				},
			}})
			if tc.wantRefused {
				if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED: preinstall_owner_present") {
					t.Fatalf("contradiction error=%v, want preinstall_owner_present", err)
				}
				if report != nil || stopCalls != 0 || provider.calls != 0 || closeCalls != 0 {
					t.Fatalf("contradiction side effects: report=%+v stop=%d provider=%d close=%d", report, stopCalls, provider.calls, closeCalls)
				}
				if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); statErr != nil {
					t.Fatalf("contradiction deleted manifest: %v", statErr)
				}
				persisted, found, readErr := ReadAdoptProvenance(name)
				if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != tc.installState {
					t.Fatalf("contradiction changed durable state: found=%t row=%+v err=%v", found, persisted, readErr)
				}
				persistedIntent, readErr := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
				if readErr != nil || len(persistedIntent.Daemons) != 1 {
					t.Fatalf("contradiction changed intent: intent=%+v err=%v", persistedIntent, readErr)
				}
				return
			}
			if err != nil || report == nil {
				t.Fatalf("non-contradiction report=%+v err=%v", report, err)
			}
			if stopCalls != tc.wantStop || provider.calls != 1 || closeCalls != 1 {
				t.Fatalf("non-contradiction side effects: stop=%d want=%d provider=%d close=%d", stopCalls, tc.wantStop, provider.calls, closeCalls)
			}
			assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
			if tc.wantRetained {
				persistedIntent, readErr := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
				if readErr != nil || len(persistedIntent.Daemons) != 1 || persistedIntent.Daemons[0].TaskName != intent.Daemons[0].TaskName {
					t.Fatalf("unrelated legacy intent row not retained: intent=%+v err=%v", persistedIntent, readErr)
				}
			}
		})
	}
}

func TestExecuteDeAdoptProviderPreInstallSimultaneousOwnedRowsRefuse(t *testing.T) {
	name := "provider-simultaneous-owned-rows"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
	exact := providerRecoveryExactDaemon(rec)
	stale := exact
	stale.Port++
	stale.ManifestHash = "stale-manifest-hash"
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{exact, stale}}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	stopCalls := 0
	closeCalls := 0
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
			stopCalls++
			return StoppedSettlement{}, errors.New("simultaneous owners must not be stopped")
		},
		close: func(manifestName string) error {
			closeCalls++
			return CloseAdoptProvenance(manifestName)
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED: preinstall_owner_present") {
		t.Fatalf("simultaneous-owner error=%v, want preinstall_owner_present", err)
	}
	if report != nil || stopCalls != 0 || provider.calls != 0 || closeCalls != 0 {
		t.Fatalf("simultaneous-owner side effects: report=%+v stop=%d provider=%d close=%d", report, stopCalls, provider.calls, closeCalls)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); statErr != nil {
		t.Fatalf("simultaneous-owner refusal deleted manifest: %v", statErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall {
		t.Fatalf("simultaneous-owner refusal changed durable state: found=%t row=%+v err=%v", found, persisted, readErr)
	}
	persistedIntent, readErr := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if readErr != nil || !reflect.DeepEqual(persistedIntent.Daemons, intent.Daemons) {
		t.Fatalf("simultaneous-owner refusal changed intent: got=%+v want=%+v err=%v", persistedIntent, intent, readErr)
	}
}

func TestProviderManagedInstallAndNoStopRecoveryAreLeaseExclusive(t *testing.T) {
	t.Run("managed install wins lease", func(t *testing.T) {
		name := "provider-install-wins-lease"
		plan, adoptProvider, observer := providerManagedInstallTestPlan(t, name)
		installEntered := make(chan struct{})
		releaseInstall := make(chan struct{})
		adoptSettled := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseInstall) }) }
		t.Cleanup(func() {
			release()
			select {
			case <-adoptSettled:
			case <-time.After(5 * time.Second):
				t.Errorf("adopt worker did not settle during cleanup")
			}
		})
		installErr := errors.New("injected install boundary stop")
		installCalls := 0
		adoptDone := make(chan error, 1)
		go func() {
			defer close(adoptSettled)
			adoptDone <- NewAPI().ExecuteAdoptWithOpts(plan, io.Discard, ExecuteAdoptOpts{
				providerDeps: providerTransactionDeps{source: adoptProvider, observer: observer},
				installFn: func(context.Context, *API, InstallOpts) error {
					installCalls++
					close(installEntered)
					<-releaseInstall
					return installErr
				},
			})
		}()
		select {
		case <-installEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("adopt did not reach the Install boundary")
		}

		recoveryPlan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatal(err)
		}
		recoveryProvider := &providerLifecycleFake{entry: adoptProvider.entry}
		stopCalls := 0
		report, recoveryErr := NewAPI().executeDeAdoptPlanWithOpts(recoveryPlan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
			source: recoveryProvider,
			stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
				stopCalls++
				return StoppedSettlement{}, errors.New("recovery must not stop while adopt owns the lease")
			},
		}})
		if recoveryErr == nil || !strings.Contains(recoveryErr.Error(), "concurrent operation") || report != nil {
			t.Fatalf("recovery while Install owns lease: report=%+v err=%v", report, recoveryErr)
		}
		if stopCalls != 0 || recoveryProvider.calls != 0 {
			t.Fatalf("recovery advanced while Install owned lease: stop=%d provider=%d", stopCalls, recoveryProvider.calls)
		}

		release()
		select {
		case adoptErr := <-adoptDone:
			if !errors.Is(adoptErr, installErr) || installCalls != 1 {
				t.Fatalf("adopt result=%v install calls=%d, want injected Install failure after one call", adoptErr, installCalls)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("adopt did not settle after releasing Install boundary")
		}
	})

	t.Run("no-stop recovery wins lease", func(t *testing.T) {
		name := "provider-recovery-wins-lease"
		manifestRoot, stateRoot, rec, recoveryProvider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil {
			t.Fatal(err)
		}
		hookEntered := make(chan struct{})
		releaseHook := make(chan struct{})
		recoverySettled := make(chan struct{})
		var releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(releaseHook) }) }
		previousHook := deAdoptBeforeManifestDeleteHook
		deAdoptBeforeManifestDeleteHook = func() {
			close(hookEntered)
			<-releaseHook
		}
		t.Cleanup(func() {
			release()
			select {
			case <-recoverySettled:
			case <-time.After(5 * time.Second):
				t.Errorf("recovery worker did not settle during cleanup")
			}
			deAdoptBeforeManifestDeleteHook = previousHook
		})

		stopCalls := 0
		closeCalls := 0
		type recoveryResult struct {
			report *DeAdoptReport
			err    error
		}
		recoveryDone := make(chan recoveryResult, 1)
		go func() {
			defer close(recoverySettled)
			report, executeErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: recoveryProvider,
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("preinstall recovery must not stop")
				},
				close: func(manifestName string) error {
					closeCalls++
					return CloseAdoptProvenance(manifestName)
				},
			}})
			recoveryDone <- recoveryResult{report: report, err: executeErr}
		}()
		select {
		case <-hookEntered:
		case <-time.After(5 * time.Second):
			t.Fatal("recovery did not reach the lease-held before-delete boundary")
		}

		installCalls := 0
		adoptErr := NewAPI().ExecuteAdoptWithOpts(&AdoptPlan{ManifestName: name}, io.Discard, ExecuteAdoptOpts{
			installFn: func(context.Context, *API, InstallOpts) error {
				installCalls++
				return errors.New("Install must not run while recovery owns the lease")
			},
		})
		if adoptErr == nil || !strings.Contains(adoptErr.Error(), adoptLeaseFailureBusy) || installCalls != 0 {
			t.Fatalf("adopt while recovery owns lease: err=%v install calls=%d", adoptErr, installCalls)
		}

		release()
		select {
		case got := <-recoveryDone:
			if got.err != nil || got.report == nil || stopCalls != 0 || recoveryProvider.calls != 1 || closeCalls != 1 {
				t.Fatalf("recovery result=%+v err=%v stop=%d provider=%d close=%d", got.report, got.err, stopCalls, recoveryProvider.calls, closeCalls)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("recovery did not settle after releasing before-delete boundary")
		}
		assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
	})
}

func TestExecuteDeAdoptProviderPreInstallLateForeignOwnerAfterClaimRefuses(t *testing.T) {
	tests := []struct {
		name   string
		daemon func(AdoptProvenanceRecord) SupervisorDaemon
	}{
		{
			name: "direct owner",
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{Server: rec.ManifestName, Daemon: adoptDefaultDaemonName, Port: rec.Port + 1, ManifestHash: "late-stale"}
			},
		},
		{
			name: "legacy task-only owner",
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(rec.ManifestName, adoptDefaultDaemonName))}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-late-owner-" + strings.ReplaceAll(tc.name, " ", "-")
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{tc.daemon(rec)}}
			intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
			previousHook := deAdoptBeforeManifestDeleteHook
			hookCalls := 0
			deAdoptBeforeManifestDeleteHook = func() {
				hookCalls++
				if err := WriteSupervisorIntent(intentPath, intent); err != nil {
					t.Fatalf("inject late owner: %v", err)
				}
			}
			t.Cleanup(func() { deAdoptBeforeManifestDeleteHook = previousHook })

			stopCalls := 0
			closeCalls := 0
			report, executeErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: provider,
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("late owner must not be stopped")
				},
				close: func(manifestName string) error {
					closeCalls++
					return CloseAdoptProvenance(manifestName)
				},
			}})
			if executeErr == nil || !strings.Contains(executeErr.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
				t.Fatalf("late-owner error=%v, want lifecycle refusal", executeErr)
			}
			if report != nil || hookCalls != 1 || stopCalls != 0 || provider.calls != 0 || closeCalls != 0 {
				t.Fatalf("late-owner side effects: report=%+v hook=%d stop=%d provider=%d close=%d", report, hookCalls, stopCalls, provider.calls, closeCalls)
			}
			if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); !os.IsNotExist(statErr) {
				t.Fatalf("late-owner refusal did not retain the claim's exact-manifest deletion: %v", statErr)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall {
				t.Fatalf("late-owner refusal changed durable state: found=%t row=%+v err=%v", found, persisted, readErr)
			}
			persistedIntent, readErr := ReadSupervisorIntent(intentPath)
			if readErr != nil || !reflect.DeepEqual(persistedIntent.Daemons, intent.Daemons) {
				t.Fatalf("late-owner refusal changed intent: got=%+v want=%+v err=%v", persistedIntent, intent, readErr)
			}
		})
	}
}

func TestExecuteDeAdoptProviderPreInstallIntentReadFailureBeforeE2Retries(t *testing.T) {
	name := "provider-preinstall-e2-interruption"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateAdopting)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("injected intent interruption after E2")
	stopCalls := 0
	_, firstErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		loadIntent: func() (*SupervisorIntentFile, error) {
			return nil, interrupted
		},
		stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
			stopCalls++
			return StoppedSettlement{}, errors.New("preinstall recovery must not stop a daemon")
		},
	}})
	if !errors.Is(firstErr, interrupted) {
		t.Errorf("first attempt error=%v, want injected post-E2 interruption", firstErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.ProviderSource == nil || persisted.OperationState != AdoptOperationStateAdopting || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall {
		t.Fatalf("state after pre-E2 intent read failure: found=%t row=%+v err=%v", found, persisted, readErr)
	}

	retryPlan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || retryPlan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("reconstructed retry plan=%+v err=%v", retryPlan, err)
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(retryPlan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
		source: provider,
		stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
			stopCalls++
			return StoppedSettlement{}, errors.New("preinstall retry must not stop a daemon")
		},
	}})
	if err != nil || report == nil {
		t.Fatalf("retry after E2 interruption report=%+v err=%v", report, err)
	}
	if stopCalls != 0 || provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("retry side effects: stop=%d provider=%d enabled=%t", stopCalls, provider.calls, provider.entry.Enabled)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallColdRetryAfterE2WithMissingManifest(t *testing.T) {
	name := "provider-preinstall-e2-cold-missing-manifest"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateAdopting)
	manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	initial, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || !initial.providerRecovery {
		t.Fatalf("initial missing-manifest recovery plan=%+v err=%v", initial, err)
	}
	if err := MarkAdoptProvenanceDeAdopting(name); err != nil {
		t.Fatalf("persist E2: %v", err)
	}

	coldRetry, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || coldRetry.Routing != DeAdoptRoutingResume || !coldRetry.providerRecovery {
		t.Fatalf("cold missing-manifest retry plan=%+v err=%v", coldRetry, err)
	}
	stopCalls := 0
	closeCalls := 0
	var report *DeAdoptReport
	var executeErr error
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		report, executeErr = NewAPI().executeDeAdoptPlanWithOpts(coldRetry, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
			source: provider,
			stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
				stopCalls++
				t.Fatal("preinstall missing-intent recovery must not stop a daemon")
				return StoppedSettlement{}, nil
			},
			close: func(manifestName string) error {
				closeCalls++
				return CloseAdoptProvenance(manifestName)
			},
		}})
	}()
	if recovered != nil {
		t.Fatalf("cold missing-intent recovery panicked: %v", recovered)
	}
	if executeErr != nil || report == nil || provider.calls != 1 || !provider.entry.Enabled || stopCalls != 0 || closeCalls != 1 {
		t.Fatalf("cold missing-intent retry report=%+v err=%v provider=%+v calls=%d stop=%d close=%d", report, executeErr, provider.entry, provider.calls, stopCalls, closeCalls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderPreInstallColdRetryAbsenceOwnerContract(t *testing.T) {
	readFailure := errors.New("injected cold supervisor intent read failure")
	tests := []struct {
		name        string
		daemon      func(AdoptProvenanceRecord) SupervisorDaemon
		readErr     error
		wantSuccess bool
	}{
		{name: "valid empty intent", wantSuccess: true},
		{
			name: "mismatching direct owner refuses",
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: `\unrelated-task`, Server: rec.ManifestName, Daemon: adoptDefaultDaemonName, Port: rec.Port + 1, ManifestHash: "mismatch"}
			},
		},
		{
			name: "legacy task-only owner refuses",
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return SupervisorDaemon{TaskName: canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(rec.ManifestName, adoptDefaultDaemonName))}
			},
		},
		{
			name: "exact owner refuses",
			daemon: func(rec AdoptProvenanceRecord) SupervisorDaemon {
				return providerRecoveryExactDaemon(rec)
			},
		},
		{name: "intent read error refuses", readErr: readFailure},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-cold-owner-" + strings.ReplaceAll(tc.name, " ", "-")
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateAdopting)
			manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
			if err := os.Remove(manifestPath); err != nil {
				t.Fatal(err)
			}
			keepPath := filepath.Join(filepath.Dir(manifestPath), "foreign.keep")
			if err := os.WriteFile(keepPath, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			intent := &SupervisorIntentFile{Version: 1}
			if tc.daemon != nil {
				intent.Daemons = []SupervisorDaemon{tc.daemon(rec)}
			}
			intentPath := filepath.Join(stateRoot, supervisorIntentFileLeaf)
			if err := WriteSupervisorIntent(intentPath, intent); err != nil {
				t.Fatal(err)
			}
			if err := MarkAdoptProvenanceDeAdopting(name); err != nil {
				t.Fatalf("persist E2: %v", err)
			}
			coldRetry, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil || coldRetry.Routing != DeAdoptRoutingResume || !coldRetry.providerRecovery {
				t.Fatalf("cold retry plan=%+v err=%v", coldRetry, err)
			}

			stopCalls := 0
			closeCalls := 0
			deps := providerTransactionDeps{
				source: provider,
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("cold preinstall recovery must not stop a daemon")
				},
				close: func(manifestName string) error {
					closeCalls++
					return CloseAdoptProvenance(manifestName)
				},
			}
			if tc.readErr != nil {
				deps.loadIntent = func() (*SupervisorIntentFile, error) { return nil, tc.readErr }
			}
			report, executeErr := NewAPI().executeDeAdoptPlanWithOpts(coldRetry, io.Discard, ExecuteDeAdoptOpts{providerDeps: deps})
			if tc.wantSuccess {
				if executeErr != nil || report == nil || stopCalls != 0 || provider.calls != 1 || closeCalls != 1 || !provider.entry.Enabled {
					t.Fatalf("valid empty cold retry report=%+v err=%v stop=%d provider=%d close=%d enabled=%t", report, executeErr, stopCalls, provider.calls, closeCalls, provider.entry.Enabled)
				}
				if got, err := os.ReadFile(keepPath); err != nil || string(got) != "preserve" {
					t.Fatalf("successful absence path changed foreign marker: %q err=%v", got, err)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
				return
			}

			if tc.readErr != nil {
				if !errors.Is(executeErr, tc.readErr) {
					t.Fatalf("intent read error=%v, want preserved cause %v", executeErr, tc.readErr)
				}
			} else if executeErr == nil || !strings.Contains(executeErr.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
				t.Fatalf("owner contradiction error=%v, want standard unsupported failure", executeErr)
			}
			if report != nil || stopCalls != 0 || provider.calls != 0 || closeCalls != 0 {
				t.Fatalf("refusal side effects: report=%+v stop=%d provider=%d close=%d", report, stopCalls, provider.calls, closeCalls)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted == nil || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall {
				t.Fatalf("refusal changed durable phase: found=%t row=%+v err=%v", found, persisted, readErr)
			}
			if got, err := os.ReadFile(keepPath); err != nil || string(got) != "preserve" {
				t.Fatalf("refusal deleted or changed foreign marker: %q err=%v", got, err)
			}
			persistedIntent, readErr := ReadSupervisorIntent(intentPath)
			if readErr != nil || len(persistedIntent.Daemons) != len(intent.Daemons) {
				t.Fatalf("refusal changed intent: got=%+v want=%+v err=%v", persistedIntent, intent, readErr)
			}
		})
	}
}

func TestExecuteDeAdoptProviderPreInstallIntentReadErrorRetries(t *testing.T) {
	tests := []struct {
		name         string
		installState ProviderManagedInstallStateV1
		wantComplete bool
	}{
		{"preinstall retries absence", ProviderManagedInstallPreInstall, true},
		{"legacy unknown remains refused", ProviderManagedInstallUnknown, false},
		{"may-have-applied remains refused", ProviderManagedInstallMayHaveApplied, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-intent-retry-" + strings.ReplaceAll(tc.name, " ", "-")
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateDeAdopting)
			rec.ProviderSource.ManagedInstallState = tc.installState
			writeDeAdoptExecutorRecord(t, rec)
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			transient := errors.New("injected supervisor intent read failure")
			stopCalls := 0
			_, firstErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: provider,
				loadIntent: func() (*SupervisorIntentFile, error) {
					return nil, transient
				},
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("intent uncertainty must not stop a daemon")
				},
			}})
			if !errors.Is(firstErr, transient) {
				t.Fatalf("first intent-read error=%v, want visible injected cause", firstErr)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != tc.installState {
				t.Fatalf("state after intent uncertainty: found=%t row=%+v err=%v", found, persisted, readErr)
			}

			retryPlan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil || retryPlan.Routing != DeAdoptRoutingResume {
				t.Fatalf("retry plan=%+v err=%v", retryPlan, err)
			}
			report, retryErr := NewAPI().executeDeAdoptPlanWithOpts(retryPlan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: provider,
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("absent intent must not stop a daemon")
				},
			}})
			if tc.wantComplete {
				if retryErr != nil || report == nil || stopCalls != 0 || provider.calls != 1 || !provider.entry.Enabled {
					t.Fatalf("preinstall retry report=%+v err=%v stop=%d provider=%d enabled=%t", report, retryErr, stopCalls, provider.calls, provider.entry.Enabled)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
				return
			}
			if retryErr == nil || !strings.Contains(retryErr.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") || stopCalls != 0 || provider.calls != 0 {
				t.Fatalf("non-preinstall absent retry report=%+v err=%v stop=%d provider=%d", report, retryErr, stopCalls, provider.calls)
			}
			if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); statErr != nil {
				t.Fatalf("non-preinstall refusal deleted manifest: %v", statErr)
			}
			persisted, found, readErr = ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "" || persisted.ProviderSource.ManagedInstallState != tc.installState {
				t.Fatalf("non-preinstall retry changed state: found=%t row=%+v err=%v", found, persisted, readErr)
			}
		})
	}
}

func TestExecuteDeAdoptProviderStopFailurePreservesDisabledRecovery(t *testing.T) {
	name := "provider-deadopt-stop-failure"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{state: AdoptOperationStateAdopted, originalState: AdoptOriginalStatePresent, liveConfig: deAdoptHubConfig(name), snapshotBytes: snapshot, writeSnapshot: true, manifestPresent: true, manifestBytes: deAdoptManagedManifest(name, 0)})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled", ActivationEnabledPresent: true, ActivationEnabled: false, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, nil, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider, stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
		return StoppedSettlement{State: StoppedSettlementFailed, Reason: StoppedSettlementReasonProcessAlive}, errors.New("managed still active")
	}}})
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
		t.Fatalf("error=%v", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatalf("provider restored despite failed managed settlement: %+v", provider.entry)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("manifest not preserved: %v", err)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("provider recovery row=%+v found=%t err=%v", persisted, found, readErr)
	}
}

func TestExecuteDeAdoptProviderRestoreCrashUsesReadOnlyCASFence(t *testing.T) {
	name := "provider-deadopt-restore-crash"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateDeAdopting, originalState: AdoptOriginalStatePresent,
		liveConfig: string(snapshot), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: false,
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied", DeAdoptPhase: "managed_removed"}
	writeDeAdoptExecutorRecord(t, rec)
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: true, ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	if err != nil {
		t.Fatalf("retry after restore-CAS crash: %v", err)
	}
	if provider.calls != 1 || report == nil {
		t.Fatalf("prior fingerprint retry calls=%d report=%+v, want one same-state CAS fence and completion", provider.calls, report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderManagedRemovalCrashRetriesWithoutDescriptorLookup(t *testing.T) {
	name := "provider-deadopt-removal-crash"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateDeAdopting, originalState: AdoptOriginalStatePresent,
		liveConfig: string(snapshot), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: false,
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied", DeAdoptPhase: "managed_stop_settled"}
	writeDeAdoptExecutorRecord(t, rec)
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled", ActivationEnabledPresent: true, ActivationEnabled: false, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider, stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
		t.Fatal("retry looked up/stopped a descriptor already removed")
		return StoppedSettlement{}, nil
	}}})
	if err != nil || report == nil || provider.calls != 1 {
		t.Fatalf("removal retry report=%+v calls=%d err=%v", report, provider.calls, err)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderSecretCleanupRetrySkipsAlreadyDeletedKey(t *testing.T) {
	name := "provider-deadopt-secret-retry"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateDeAdopting, originalState: AdoptOriginalStatePresent,
		liveConfig: string(snapshot), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: false,
	})
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied", DeAdoptPhase: "restore_applied"}
	rec.RoutedSecretKeys = []string{"provider-secret-first", "provider-secret-second"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{"provider-secret-second": "value"})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err != nil || report == nil {
		t.Fatalf("secret cleanup retry report=%+v err=%v", report, err)
	}
	if got := deAdoptVaultKeys(t); len(got) != 0 {
		t.Fatalf("remaining routed keys=%v, want second key deleted while absent first was accepted", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderCloseFailurePreservesRestorePhaseForRetry(t *testing.T) {
	name := "provider-deadopt-close-retry"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateDeAdopting, originalState: AdoptOriginalStatePresent,
		liveConfig: string(snapshot), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: false,
	})
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied", DeAdoptPhase: "restore_applied"}
	writeDeAdoptExecutorRecord(t, rec)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	closeFailure := errors.New("injected close failure")
	leaseFailure := errors.New("injected lease failure")
	previousUnlock := adoptLeaseUnlockFailureHook
	adoptLeaseUnlockFailureHook = func() error { return leaseFailure }
	t.Cleanup(func() { adoptLeaseUnlockFailureHook = previousUnlock })
	_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{close: func(string) error { return closeFailure }}})
	if err == nil || !errors.Is(err, closeFailure) || !errors.Is(err, leaseFailure) {
		t.Fatalf("joined close/lease error=%v close=%t lease=%t", err, errors.Is(err, closeFailure), errors.Is(err, leaseFailure))
	}
	adoptLeaseUnlockFailureHook = previousUnlock
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateDeAdopting || persisted.ProviderSource == nil || persisted.ProviderSource.DeAdoptPhase != "restore_applied" {
		t.Fatalf("close failure lost retry state: row=%+v found=%t err=%v", persisted, found, readErr)
	}
	plan, err = NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{}); err != nil {
		t.Fatalf("close retry: %v", err)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderDisableMarkCrashRestoresOnlyExplicitRecovery(t *testing.T) {
	name := "provider-disable-mark-crash"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateAdopting, originalState: AdoptOriginalStateAbsent,
		liveConfig: "[mcp_servers]\n", manifestPresent: false,
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.AdoptClients = nil
	rec.Clients = nil
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_planned"}
	writeDeAdoptExecutorRecord(t, rec)
	if err := initializeProviderInstallPhase(name); err != nil {
		t.Fatal(err)
	}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled", ActivationEnabledPresent: true, ActivationEnabled: false, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("explicit recovery plan=%+v err=%v", plan, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("explicit recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("explicit recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptProviderCrashAfterManifestBeforeInstall(t *testing.T) {
	name := "provider-manifest-before-install-crash"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: AdoptOperationStateAdopting, originalState: AdoptOriginalStateAbsent,
		liveConfig: "[mcp_servers]\n", manifestPresent: true,
	})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	rec.ProviderSource = &ProviderSourceProvenanceV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation", PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true, ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied"}
	writeDeAdoptExecutorRecord(t, rec)
	if err := initializeProviderInstallPhase(name); err != nil {
		t.Fatal(err)
	}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), &SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name, Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd, Scope: clients.ProviderMCPScopeUser, Enabled: false, ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled", ActivationEnabledPresent: true, ActivationEnabled: false, DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy"}}

	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh || !plan.providerRecovery {
		t.Fatalf("post-manifest recovery plan=%+v err=%v, want durable pre-Install recovery", plan, err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("post-manifest recovery apply: %v", err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatalf("post-manifest recovery provider=%+v calls=%d", provider.entry, provider.calls)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptT7RollForwardSkipsRestoreDoneAndCompletes(t *testing.T) {
	name := "deadopt-execute-resume"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	before := mustReadFileForAdoptTest(t, codexPath)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt resume: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("resume report = %+v", report)
	}
	if !strings.Contains(out.String(), "already restored; skipping") {
		t.Fatalf("resume output did not record the skip: %s", out.String())
	}
	after := mustReadFileForAdoptTest(t, codexPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("RESTORE-DONE client was rewritten\nbefore:\n%s\nafter:\n%s", before, after)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptReclassifiesStaleRestoreDonePlanAtE3(t *testing.T) {
	t.Run("StillHub is restored instead of silently skipped", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-still-hub"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte(deAdoptHubConfig(name)), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 StillHub drift: %v", err)
		}

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err != nil {
			t.Fatalf("execute stale plan: %v", err)
		}
		if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
			t.Fatalf("StillHub drift report = %+v", report)
		}
		live := mustReadFileForAdoptTest(t, codexPath)
		if !bytes.Contains(live, []byte("pre-adopt-native")) || bytes.Contains(live, []byte("127.0.0.1")) {
			t.Fatalf("StillHub drift was silently skipped instead of restored:\n%s", live)
		}
		assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
	})

	t.Run("genuine conflict fails and preserves recovery state", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-conflict"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte(deAdoptNativeConfig(name, "operator-conflict")), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 conflict drift: %v", err)
		}
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err != nil {
			t.Fatalf("execute stale conflict plan: %v", err)
		}
		if len(report.Failed) != 1 || report.Failed[0].Client != "codex-cli" || len(report.Restored) != 0 {
			t.Fatalf("conflict drift report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})

	t.Run("unreadable gate refuses before E2", func(t *testing.T) {
		name := "deadopt-execute-plan-drift-unreadable"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte("[mcp_servers.\n"), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 unreadable drift: %v", err)
		}
		before := mustReadFileForAdoptTest(t, codexPath)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
		if err == nil || !strings.Contains(err.Error(), "cannot prove gate-OFF") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
			t.Fatalf("execute stale unreadable plan = report=%+v err=%v", report, err)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
			t.Fatalf("unreadable-gate refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
		}
		persisted, found, readErr := ReadAdoptProvenance(name)
		if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
			t.Fatalf("unreadable-gate refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
		}
		if _, statErr := os.Stat(snapshotPath); statErr != nil {
			t.Fatalf("unreadable-gate refusal removed snapshot: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
			t.Fatalf("unreadable-gate refusal removed manifest: %v", statErr)
		}
	})

	t.Run("accept conflict cannot bypass unreadable gate", func(t *testing.T) {
		name := "deadopt-execute-accept-drift-unreadable"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      string(snapshot),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		plan, err := NewAPI().BuildDeAdoptPlan(name)
		if err != nil || len(plan.Clients) != 1 || plan.Clients[0].Disposition != DeAdoptClientRestoreDone {
			t.Fatalf("pre-drift plan = %+v err=%v", plan, err)
		}
		if err := os.WriteFile(codexPath, []byte("[mcp_servers.\n"), 0o600); err != nil {
			t.Fatalf("simulate plan-to-E3 unreadable drift: %v", err)
		}
		before := mustReadFileForAdoptTest(t, codexPath)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err == nil || !strings.Contains(err.Error(), "cannot prove gate-OFF") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
			t.Fatalf("execute stale unreadable acceptance plan = report=%+v err=%v", report, err)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
			t.Fatalf("unreadable-gate acceptance refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
		}
		persisted, found, readErr := ReadAdoptProvenance(name)
		if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
			t.Fatalf("unreadable-gate acceptance refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
		}
		if _, statErr := os.Stat(snapshotPath); statErr != nil {
			t.Fatalf("unreadable-gate acceptance refusal removed snapshot: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
			t.Fatalf("unreadable-gate acceptance refusal removed manifest: %v", statErr)
		}
	})
}

func TestExecuteDeAdoptCallerWriterFlushesAfterLeaseUnlock(t *testing.T) {
	name := "deadopt-execute-writer-after-unlock"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	writer := newBlockingDeAdoptWriter()
	type result struct {
		report *DeAdoptReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := NewAPI().ExecuteDeAdopt(name, writer)
		done <- result{report: report, err: err}
	}()

	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		close(writer.release)
		t.Fatal("timed out waiting for caller writer invocation")
	}

	lease, leased, leaseErr := tryAcquireAdoptManifestLease(name)
	if leased {
		_ = lease.Unlock()
	}
	close(writer.release)
	select {
	case got := <-done:
		if got.err != nil || got.report == nil {
			t.Fatalf("ExecuteDeAdopt after writer release = report=%+v err=%v", got.report, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteDeAdopt remained blocked after caller writer was released")
	}
	if leaseErr != nil || !leased {
		t.Fatalf("manifest lease remained held while caller writer blocked: leased=%v err=%v", leased, leaseErr)
	}
}

func TestExecuteDeAdoptEventsDoNotBlockCompletedCall(t *testing.T) {
	name := "deadopt-execute-events-nonblocking"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	eventLock := flock.New(filepath.Join(stateRoot, SupervisorEventLogFileLeaf) + supervisorEventLogLockSuffix)
	locked, lockErr := eventLock.TryLock()
	if lockErr != nil || !locked {
		t.Fatalf("hold supervisor event-log lock: locked=%v err=%v", locked, lockErr)
	}
	defer func() { _ = eventLock.Unlock() }()

	type result struct {
		report *DeAdoptReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := NewAPI().ExecuteDeAdopt(name, io.Discard)
		done <- result{report: report, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil || got.report == nil {
			t.Fatalf("ExecuteDeAdopt under event-log contention = report=%+v err=%v", got.report, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ExecuteDeAdopt blocked on best-effort de-adopt event emission")
	}
	lease, leased, leaseErr := tryAcquireAdoptManifestLease(name)
	if leased {
		_ = lease.Unlock()
	}
	if leaseErr != nil || !leased {
		t.Fatalf("manifest lease remained held after completed de-adopt: leased=%v err=%v", leased, leaseErr)
	}
}

type blockingDeAdoptWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingDeAdoptWriter() *blockingDeAdoptWriter {
	return &blockingDeAdoptWriter{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingDeAdoptWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestExecuteDeAdoptE1ConcurrentOperationRefusesBeforeMark(t *testing.T) {
	name := "deadopt-execute-concurrent"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	before := mustReadFileForAdoptTest(t, codexPath)
	lease, leased, err := tryAcquireAdoptManifestLease(name)
	if err != nil || !leased {
		t.Fatalf("hold manifest lease: leased=%v err=%v", leased, err)
	}
	t.Cleanup(func() { _ = lease.Unlock() })

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil || !strings.Contains(err.Error(), "concurrent operation") || report != nil {
		t.Fatalf("concurrent ExecuteDeAdopt = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("client config changed before E1 refusal\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
		t.Fatalf("E1 refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
		t.Fatalf("E1 refusal removed manifest: %v", err)
	}
}

func TestExecuteDeAdoptReprobesHubGateUnderLease(t *testing.T) {
	name := "deadopt-execute-gate-flip"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}

	gateOn := []byte("[mcp_servers.mcphub-hub]\nurl = \"http://127.0.0.1:1/mcp\"\n")
	if err := os.WriteFile(codexPath, gateOn, 0o600); err != nil {
		t.Fatalf("flip hub gate ON after planning: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "gate OFF first") || !strings.Contains(err.Error(), "codex-cli") || report != nil {
		t.Fatalf("execute gate-flipped plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("gate-flip refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
		t.Fatalf("gate-flip refusal changed provenance: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); statErr != nil {
		t.Fatalf("gate-flip refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptDerivesResumeRoutingFromLeasedRow(t *testing.T) {
	name := "deadopt-execute-fresh-plan-resume-row"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}

	rec.OperationState = AdoptOperationStateDeAdopting
	writeDeAdoptExecutorRecord(t, rec)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatalf("simulate completed manifest delete: %v", err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err != nil {
		t.Fatalf("execute FRESH plan against leased resume row: %v", err)
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("resume-derived report = %+v", report)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("already-restored resume client was rewritten\nbefore:\n%s\nafter:\n%s", before, got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptRereadsProvenanceUnderLease(t *testing.T) {
	name := "deadopt-execute-row-replaced"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	newer := rec
	newer.Port++
	writeDeAdoptExecutorRecord(t, newer)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "provenance changed under the lease") || report != nil {
		t.Fatalf("execute replaced-row plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("replaced-row refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted || persisted.Port != newer.Port {
		t.Fatalf("replaced row changed: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("replaced-row refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptRejectsReplacementAdoptionByCreatedAt(t *testing.T) {
	name := "deadopt-execute-replacement-created-at"
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	codexPath, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptHubConfig(name),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	rec.CreatedAt = time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	rec.UpdatedAt = rec.CreatedAt
	writeDeAdoptExecutorRecord(t, rec)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil || plan.Routing != DeAdoptRoutingFresh {
		t.Fatalf("BuildDeAdoptPlan = %+v err=%v", plan, err)
	}
	before := mustReadFileForAdoptTest(t, codexPath)

	// Model a completed de-adopt followed by an otherwise-identical re-adopt:
	// capture assigns the replacement row a new immutable creation identity.
	replacement := rec
	replacement.CreatedAt = rec.CreatedAt.Add(time.Second)
	replacement.UpdatedAt = replacement.CreatedAt
	writeDeAdoptExecutorRecord(t, replacement)

	report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{})
	if err == nil || !strings.Contains(err.Error(), "provenance changed under the lease") || report != nil {
		t.Fatalf("execute replacement-adoption plan = report=%+v err=%v", report, err)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
		t.Fatalf("replacement-adoption refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted || !persisted.CreatedAt.Equal(replacement.CreatedAt) {
		t.Fatalf("replacement adoption changed: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); statErr != nil {
		t.Fatalf("replacement-adoption refusal removed manifest: %v", statErr)
	}
}

func TestExecuteDeAdoptManifestReadinessRefusesBeforeE2E3(t *testing.T) {
	tests := []struct {
		name                 string
		manifestPresent      bool
		expectedHashOverride string
		wantReason           string
	}{
		{name: "fresh manifest absent", manifestPresent: false, wantReason: "manifest is absent"},
		{name: "manifest hash mismatch", manifestPresent: true, expectedHashOverride: strings.Repeat("a", 64), wantReason: "manifest hash does not match adopt provenance"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "deadopt-readiness-" + strings.ReplaceAll(tc.name, " ", "-")
			snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
			codexPath, _, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:                AdoptOperationStateAdopted,
				originalState:        AdoptOriginalStatePresent,
				liveConfig:           deAdoptHubConfig(name),
				snapshotBytes:        snapshot,
				writeSnapshot:        true,
				manifestPresent:      tc.manifestPresent,
				expectedHashOverride: tc.expectedHashOverride,
			})
			before := mustReadFileForAdoptTest(t, codexPath)
			snapshotPath := deAdoptSnapshotPathForTest(t, rec)

			report, err := NewAPI().ExecuteDeAdopt(name, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "is not delete-ready") || !strings.Contains(err.Error(), tc.wantReason) || report != nil {
				t.Fatalf("ExecuteDeAdopt readiness refusal = report=%+v err=%v", report, err)
			}
			if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, before) {
				t.Fatalf("readiness refusal mutated client config\nbefore:\n%s\nafter:\n%s", before, got)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || persisted.OperationState != AdoptOperationStateAdopted {
				t.Fatalf("readiness refusal changed row: found=%v rec=%+v err=%v", found, persisted, readErr)
			}
			if _, statErr := os.Stat(snapshotPath); statErr != nil {
				t.Fatalf("readiness refusal removed snapshot: %v", statErr)
			}
		})
	}
}

func TestExecuteDeAdoptT8RoutedSecretPrefilterSharedSkipAndClose(t *testing.T) {
	name := "deadopt-execute-secrets"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const (
		deleteKey = "DEADOPT_DELETE_KEY"
		goneKey   = "DEADOPT_ALREADY_GONE_KEY"
		sharedKey = "DEADOPT_SHARED_KEY"
	)
	rec.RoutedSecretKeys = []string{goneKey, sharedKey, deleteKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{
		deleteKey: "delete-secret-value",
		sharedKey: "shared-secret-value",
	})
	sharedManifestDir := filepath.Join(manifestRoot, "shared-secret-owner")
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: shared-secret-owner\nkind: global\ntransport: stdio-bridge\ncommand: test-command\nenv:\n  TOKEN: secret:" + sharedKey + "\ndaemons:\n  - name: default\n    port: 31991\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt secrets: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) {
		t.Fatalf("secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want only shared key", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if !bytes.Contains(logBytes, []byte(sharedKey)) || bytes.Contains(logBytes, []byte("shared-secret-value")) || bytes.Contains(logBytes, []byte("delete-secret-value")) {
		t.Fatalf("shared-key event missing or leaked a value:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedThroughExpandedEnv(t *testing.T) {
	name := "deadopt-execute-expanded-env-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "DEADOPT_EXPANDED_ENV_SHARED_KEY"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "expanded-env-secret-value"})
	t.Setenv("DEADOPT_SHARED_SECRET_REF", "secret:"+sharedKey)

	sharedManifestDir := filepath.Join(manifestRoot, "expanded-env-secret-owner")
	t.Cleanup(func() { _ = os.RemoveAll(sharedManifestDir) })
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: expanded-env-secret-owner\nkind: global\ntransport: stdio-bridge\ncommand: test-command\nenv:\n  TOKEN: \"${DEADOPT_SHARED_SECRET_REF}\"\ndaemons:\n  - name: default\n    port: 31992\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt expanded-env secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) || strings.Contains(out.String(), "could not be read") {
		t.Fatalf("expanded-env secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want expanded env shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedByEmbeddedManifest(t *testing.T) {
	name := "deadopt-execute-embedded-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "wolfram_app_id"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "embedded-manifest-secret-value"})
	// Exercise the production manifest source: the wolfram manifest exists only
	// in the binary's embedded catalog for this test and references sharedKey.
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt embedded-manifest secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) || strings.Contains(out.String(), "could not be read") {
		t.Fatalf("embedded-manifest secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want embedded-manifest shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptManifestDirReadFailurePreservesRoutedSecret(t *testing.T) {
	name := "deadopt-execute-manifest-list-failure"
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const routedKey = "DEADOPT_MANIFEST_LIST_FAILURE_KEY"
	rec.RoutedSecretKeys = []string{routedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{routedKey: "preserve-on-list-failure"})

	// Exercise the production embed+disk branch, not the test-only manifest-dir
	// override branch that already propagates non-ENOENT ReadDir failures.
	t.Setenv("MCPHUB_MANIFEST_DIR_OVERRIDE", "")
	originalReadDir := deAdoptManifestReadDir
	deAdoptManifestReadDir = func(path string) ([]os.DirEntry, error) {
		return nil, &os.PathError{Op: "readdir", Path: path, Err: os.ErrPermission}
	}
	t.Cleanup(func() { deAdoptManifestReadDir = originalReadDir })

	report, err := NewAPI().ExecuteDeAdopt(name, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "routed-secret prefilter") {
		t.Fatalf("ExecuteDeAdopt manifest-list failure = report=%+v err=%v", report, err)
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{routedKey}) {
		t.Fatalf("manifest-list failure deleted routed secret: %#v", got)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("manifest-list failure lost recovery row: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	if !bytes.Contains(logBytes, []byte(`"event":"deadopt-close-failed"`)) ||
		!bytes.Contains(logBytes, []byte(`"phase":"routed-secret-prefilter"`)) {
		t.Fatalf("routed-secret-prefilter close-failed event missing:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptPreservesRoutedSecretReferencedByRemoteHeader(t *testing.T) {
	name := "deadopt-execute-header-secret"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const sharedKey = "DEADOPT_REMOTE_HEADER_SHARED_KEY"
	rec.RoutedSecretKeys = []string{sharedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{sharedKey: "remote-header-secret-value"})

	sharedManifestDir := filepath.Join(manifestRoot, "remote-header-secret-owner")
	if err := os.MkdirAll(sharedManifestDir, 0o700); err != nil {
		t.Fatalf("mkdir shared manifest: %v", err)
	}
	sharedManifest := "name: remote-header-secret-owner\nkind: global\ntransport: remote-http\nurl: https://example.com/mcp\nheaders:\n  Authorization: \"Bearer ${secret:" + sharedKey + "}\"\n"
	if err := os.WriteFile(filepath.Join(sharedManifestDir, "manifest.yaml"), []byte(sharedManifest), 0o600); err != nil {
		t.Fatalf("write shared manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt remote-header secret: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), sharedKey) {
		t.Fatalf("remote-header secret cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{sharedKey}) {
		t.Fatalf("vault keys = %#v, want remote-header shared key preserved", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPreservesRoutedSecretWhenAnotherManifestIsUnreadable(t *testing.T) {
	name := "deadopt-execute-unreadable-secret-owner"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      deAdoptHubConfig(name),
		manifestPresent: true,
	})
	const routedKey = "DEADOPT_POSSIBLY_SHARED_KEY"
	rec.RoutedSecretKeys = []string{routedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{routedKey: "possibly-shared-secret-value"})

	const unreadableManifest = "unreadable-secret-owner"
	unreadableDir := filepath.Join(manifestRoot, unreadableManifest)
	t.Cleanup(func() { _ = os.RemoveAll(unreadableDir) })
	if err := os.MkdirAll(unreadableDir, 0o700); err != nil {
		t.Fatalf("mkdir unreadable manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(unreadableDir, "manifest.yaml"), []byte("name: [\n"), 0o600); err != nil {
		t.Fatalf("write unreadable manifest: %v", err)
	}

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt unreadable-manifest secret scan: %v\n%s", err, out.String())
	}
	if len(report.Failed) != 0 || !strings.Contains(out.String(), "preserved 1 key(s)") || !strings.Contains(out.String(), unreadableManifest) {
		t.Fatalf("unreadable-manifest cleanup report/output = %+v / %s", report, out.String())
	}
	if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{routedKey}) {
		t.Fatalf("vault keys = %#v, want routed key preserved after unreadable manifest", got)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestDeAdoptManifestVaultReferenceKeysCoversEnvURLAndHeaders(t *testing.T) {
	got := deAdoptManifestVaultReferenceKeys(&config.ServerManifest{
		Env: map[string]string{
			"TOKEN": "secret:ENV_KEY",
		},
		URL: "https://example.com/${secret:URL_KEY}/mcp",
		Headers: map[string]string{
			"Authorization": "Bearer ${secret:HEADER_KEY}",
			"X-Combined":    "${secret:FIRST_KEY}:${secret:SECOND_KEY}",
		},
	})
	want := map[string]bool{
		"ENV_KEY":    true,
		"URL_KEY":    true,
		"HEADER_KEY": true,
		"FIRST_KEY":  true,
		"SECOND_KEY": true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("manifest vault references = %#v, want %#v", got, want)
	}
}

func TestExecuteDeAdoptT9T10T14FailureBlocksWholeCloseAndRedacts(t *testing.T) {
	name := "deadopt-execute-blocked"
	const snapshotSecret = "canary-one"
	snapshot := []byte("[mcp_servers." + name + "]\ncommand = \"native-command\"\n[mcp_servers." + name + ".env]\nAPI_KEY = \"" + snapshotSecret + "\"\n")
	codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateAdopted,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      deAdoptNativeConfig(name, "operator-conflict"),
		snapshotBytes:   snapshot,
		writeSnapshot:   true,
		manifestPresent: true,
	})
	const routedKey = "DEADOPT_BLOCKED_KEY"
	const routedValue = "ROUTED_SECRET_MUST_NOT_LEAK"
	rec.RoutedSecretKeys = []string{routedKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)
	seedDeAdoptVault(t, map[string]string{routedKey: routedValue})
	liveBefore := mustReadFileForAdoptTest(t, codexPath)
	snapshotPath := deAdoptSnapshotPathForTest(t, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdopt(name, &out)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt partial failure returned setup error: %v", err)
	}
	if len(report.Failed) != 1 || report.Failed[0].Client != "codex-cli" || len(report.Restored) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("blocked report = %+v", report)
	}
	if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, liveBefore) {
		t.Fatalf("conflicting live config was changed\nbefore:\n%s\nafter:\n%s", liveBefore, got)
	}
	assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, routedKey)

	logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
	serializedReport, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatalf("marshal de-adopt report: %v", marshalErr)
	}
	joined := out.String() + string(logBytes) + string(serializedReport)
	for _, forbidden := range []string{snapshotSecret, routedValue, string(snapshot), string(liveBefore)} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("de-adopt output/event leaked forbidden content %q:\n%s", forbidden, joined)
		}
	}
	if !bytes.Contains(logBytes, []byte(`"event":"deadopt-close-ready-blocked"`)) {
		t.Fatalf("close-ready-blocked event missing:\n%s", logBytes)
	}
}

func TestExecuteDeAdoptCrashInsideCloseResumeWithoutManifestOrSnapshot(t *testing.T) {
	name := "deadopt-execute-crash-inside-close"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: false,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt crash-inside-close resume: %v", err)
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 {
		t.Fatalf("crash-inside-close report = %+v", report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptAcceptConflictCrashInsideCloseIsRestoreDone(t *testing.T) {
	name := "deadopt-accept-crash-inside-close"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: false,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	var out bytes.Buffer
	report, err := NewAPI().ExecuteDeAdoptWithOpts(name, &out, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
	if err != nil {
		t.Fatalf("ExecuteDeAdoptWithOpts crash-inside-close: %v\n%s", err, out.String())
	}
	if !reflect.DeepEqual(report.Restored, []string{"codex-cli"}) || len(report.Failed) != 0 || len(report.Accepted) != 0 {
		t.Fatalf("accept crash-inside-close report = %+v", report)
	}
	if !strings.Contains(out.String(), "already restored; --accept-conflict was not needed") {
		t.Fatalf("accept crash-inside-close did not narrate harmless no-op: %s", out.String())
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptPartialRoutedSecretDeleteRetry(t *testing.T) {
	name := "deadopt-execute-partial-secret-retry"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	const (
		alreadyDeletedKey = "DEADOPT_PARTIAL_ALREADY_DELETED"
		remainingKey      = "DEADOPT_PARTIAL_REMAINING"
	)
	rec.RoutedSecretKeys = []string{alreadyDeletedKey, remainingKey}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{remainingKey: "remaining-secret-value"})

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt partial routed-secret retry: %v", err)
	}
	if len(report.Failed) != 0 || len(deAdoptVaultKeys(t)) != 0 {
		t.Fatalf("partial routed-secret retry report/vault = %+v / %#v", report, deAdoptVaultKeys(t))
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptResumeWithAbsentVaultCompletesRoutedSecretCleanup(t *testing.T) {
	name := "deadopt-execute-absent-vault-resume"
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	rec.RoutedSecretKeys = []string{"DEADOPT_ABSENT_VAULT_KEY"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{"UNRELATED_KEY": "unrelated-value"})
	if err := os.Remove(secrets.DefaultVaultPath()); err != nil {
		t.Fatalf("remove vault before resume: %v", err)
	}

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt absent-vault resume: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("absent-vault resume report = %+v", report)
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
}

func TestExecuteDeAdoptResumeWithUnreadableVaultBlocksClose(t *testing.T) {
	name := "deadopt-execute-unreadable-vault-resume"
	_, manifestRoot, _, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStateAbsent,
		liveConfig:      "",
		manifestPresent: false,
	})
	rec.RoutedSecretKeys = []string{"DEADOPT_UNREADABLE_VAULT_KEY"}
	writeDeAdoptExecutorRecord(t, rec)
	seedDeAdoptVault(t, map[string]string{"DEADOPT_UNREADABLE_VAULT_KEY": "secret-value"})
	if err := os.WriteFile(secrets.DefaultVaultPath(), []byte("not-an-age-vault"), 0o600); err != nil {
		t.Fatalf("corrupt existing vault: %v", err)
	}

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil || !strings.Contains(err.Error(), "routed-secret prefilter") {
		t.Fatalf("ExecuteDeAdopt unreadable-vault resume = report=%+v err=%v", report, err)
	}
	if _, statErr := os.Stat(secrets.DefaultVaultPath()); statErr != nil {
		t.Fatalf("existing unreadable vault was removed: %v", statErr)
	}
	persisted, found, readErr := ReadAdoptProvenance(name)
	if readErr != nil || !found || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("unreadable-vault refusal lost recovery row: found=%v rec=%+v err=%v", found, persisted, readErr)
	}
	if _, statErr := os.Stat(filepath.Join(manifestRoot, name, "manifest.yaml")); !os.IsNotExist(statErr) {
		t.Fatalf("resume manifest unexpectedly present after refusal: %v", statErr)
	}
}

func TestExecuteDeAdoptResumeRechecksRecreatedManifestAtE4(t *testing.T) {
	for _, tc := range []struct {
		name        string
		recreated   []byte
		wantSuccess bool
	}{
		{name: "matching adopt content is deleted", recreated: []byte("name: deadopt-recreate-matching\n"), wantSuccess: true},
		{name: "different content is refused", recreated: []byte("name: foreign-replacement\n"), wantSuccess: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "deadopt-recreate-matching"
			adoptContent := []byte("name: " + name + "\n")
			_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
				state:           AdoptOperationStateDeAdopting,
				originalState:   AdoptOriginalStateAbsent,
				liveConfig:      "",
				manifestPresent: false,
				manifestBytes:   adoptContent,
			})
			seedDeAdoptSupervisorIntent(t, stateRoot, rec)
			manifestPath := filepath.Join(manifestRoot, name, "manifest.yaml")
			deAdoptBeforeManifestDeleteHook = func() {
				if err := os.MkdirAll(filepath.Dir(manifestPath), 0o700); err != nil {
					t.Fatalf("recreate manifest dir before E4: %v", err)
				}
				if err := os.WriteFile(manifestPath, tc.recreated, 0o600); err != nil {
					t.Fatalf("recreate manifest before E4: %v", err)
				}
			}
			t.Cleanup(func() { deAdoptBeforeManifestDeleteHook = nil })

			report, err := NewAPI().ExecuteDeAdopt(name, nil)
			if tc.wantSuccess {
				if err != nil || len(report.Failed) != 0 {
					t.Fatalf("ExecuteDeAdopt matching recreation = report=%+v err=%v", report, err)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
				return
			}
			if err == nil || !strings.Contains(err.Error(), "content hash changed") {
				t.Fatalf("ExecuteDeAdopt foreign recreation = report=%+v err=%v", report, err)
			}
			if got := mustReadFileForAdoptTest(t, manifestPath); !bytes.Equal(got, tc.recreated) {
				t.Fatalf("foreign manifest changed\ngot: %q\nwant: %q", got, tc.recreated)
			}
			assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, "", "")
		})
	}
}

func TestExecuteDeAdoptProviderRecoverySameNameManifestCreatePreservesRoutedSecret(t *testing.T) {
	name := "provider-recovery-same-name-recreate"
	const routedKey = "PROVIDER_RECOVERY_RECREATE_ROUTED_KEY"
	tests := []struct {
		name            string
		replacementName string
	}{
		{name: "same-name", replacementName: name},
		{name: "different-name", replacementName: name + "-replacement"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			replacementName := tt.replacementName
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			rec.RoutedSecretKeys = []string{routedKey}
			writeDeAdoptExecutorRecord(t, *rec)
			if err := os.Remove(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
				t.Fatalf("remove adopt-created manifest before recovery: %v", err)
			}
			seedDeAdoptVault(t, map[string]string{routedKey: "synthetic-recreated-owner-value"})

			replacement := []byte("name: " + replacementName + "\nkind: global\ntransport: stdio-bridge\ncommand: go\nbase_args: [version]\nenv:\n  TOKEN: secret:" + routedKey + "\ndaemons:\n  - name: default\n    port: 31994\nclient_bindings: []\n")
			validationRoot := t.TempDir()
			if err := NewAPI().ManifestCreateIn(validationRoot, replacementName, string(replacement)); err != nil {
				t.Fatalf("replacement is not a valid ManifestCreate input: %v", err)
			}
			validated, err := os.ReadFile(filepath.Join(validationRoot, replacementName, "manifest.yaml"))
			if err != nil || !bytes.Equal(validated, replacement) {
				t.Fatalf("replacement prevalidation bytes: equal=%t err=%v", bytes.Equal(validated, replacement), err)
			}
			parsed, err := config.ParseManifest(bytes.NewReader(validated))
			if err != nil || parsed.Env["TOKEN"] != "secret:"+routedKey {
				t.Fatalf("replacement prevalidation parse: env=%+v err=%v", parsed, err)
			}

			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil || !plan.providerRecovery {
				t.Fatalf("provider recovery plan: recovery=%t err=%v", plan != nil && plan.providerRecovery, err)
			}
			restoreEntered := make(chan struct{})
			releaseRestore := make(chan struct{})
			recoverySettled := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseRestore) }) }
			provider.onCAS = func() {
				close(restoreEntered)
				<-releaseRestore
			}
			t.Cleanup(func() {
				release()
				select {
				case <-recoverySettled:
				case <-time.After(5 * time.Second):
					t.Errorf("provider recovery did not settle during cleanup")
				}
			})

			type recoveryResult struct {
				report *DeAdoptReport
				err    error
			}
			recoveryDone := make(chan recoveryResult, 1)
			go func() {
				defer close(recoverySettled)
				report, executeErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
				recoveryDone <- recoveryResult{report: report, err: executeErr}
			}()
			select {
			case <-restoreEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider recovery did not reach restore after the absence probe")
			}

			paused, found, err := ReadAdoptProvenance(name)
			if err != nil || !found || paused.OperationState != AdoptOperationStateDeAdopting || paused.ProviderSource == nil || paused.ProviderSource.DeAdoptPhase != "managed_removed" || !reflect.DeepEqual(paused.RoutedSecretKeys, []string{routedKey}) {
				t.Fatalf("paused recovery is not a production-valid routed-key phase: found=%t rec=%+v err=%v", found, paused, err)
			}
			manifestPath := filepath.Join(manifestRoot, replacementName, "manifest.yaml")
			if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
				t.Fatalf("replacement target was not absent at create point: %v", err)
			}
			createErr := NewAPI().ManifestCreate(replacementName, string(replacement))
			if createErr == nil {
				created, err := os.ReadFile(manifestPath)
				if err != nil || !bytes.Equal(created, replacement) {
					t.Fatalf("ManifestCreate did not persist exact replacement: equal=%t err=%v", bytes.Equal(created, replacement), err)
				}
			}
			release()
			var result recoveryResult
			select {
			case result = <-recoveryDone:
			case <-time.After(5 * time.Second):
				t.Fatal("provider recovery did not settle after recreate")
			}
			if result.err != nil || result.report == nil || provider.calls != 1 || !provider.entry.Enabled {
				t.Fatalf("provider recovery result=%+v err=%v provider=%d enabled=%t", result.report, result.err, provider.calls, provider.entry.Enabled)
			}
			if createErr != nil {
				if replacementName != rec.ManifestName {
					t.Fatalf("different-name ManifestCreate refused during provider recovery: %v", createErr)
				}
				if _, err := os.Stat(manifestPath); !os.IsNotExist(err) {
					t.Fatalf("refused ManifestCreate left a manifest: %v", err)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
				return
			}

			created, err := os.ReadFile(manifestPath)
			if err != nil || !bytes.Equal(created, replacement) {
				t.Fatalf("successful replacement changed during recovery: equal=%t err=%v", bytes.Equal(created, replacement), err)
			}
			parsed, err = config.ParseManifest(bytes.NewReader(created))
			if err != nil || parsed.Env["TOKEN"] != "secret:"+routedKey {
				t.Fatalf("successful replacement is not functional: env=%+v err=%v", parsed, err)
			}
			if got := deAdoptVaultKeys(t); !reflect.DeepEqual(got, []string{routedKey}) {
				t.Fatalf("replacement lost its routed secret: got=%#v want=[%s]", got, routedKey)
			}
			if persisted, found, err := ReadAdoptProvenance(name); err != nil || found || persisted != nil {
				t.Fatalf("old recovery provenance was not closed: found=%t rec=%+v err=%v", found, persisted, err)
			}
		})
	}
}

func TestExecuteDeAdoptSnapshotDeletedManifestPresentRetryBlocksClose(t *testing.T) {
	name := "deadopt-execute-snapshot-deleted-manifest-present"
	snapshot := []byte(deAdoptNativeConfig(name, "already-restored"))
	_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:           AdoptOperationStateDeAdopting,
		originalState:   AdoptOriginalStatePresent,
		liveConfig:      string(snapshot),
		snapshotBytes:   snapshot,
		writeSnapshot:   false,
		manifestPresent: true,
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err != nil {
		t.Fatalf("ExecuteDeAdopt T14 retry: %v", err)
	}
	if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "snapshot") {
		t.Fatalf("T14 retry report = %+v", report)
	}
	assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, "", "")
}

func TestExecuteDeAdoptErrorAndReportSerializationRedactsSensitiveState(t *testing.T) {
	name := "deadopt-execute-error-serialization"
	const snapshotSecret = "canary-two"
	snapshot := []byte("[mcp_servers." + name + "]\ncommand = \"native-command\"\n[mcp_servers." + name + ".env]\nAPI_KEY = \"" + snapshotSecret + "\"\n")
	_, _, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state:                AdoptOperationStateAdopted,
		originalState:        AdoptOriginalStatePresent,
		liveConfig:           deAdoptHubConfig(name),
		snapshotBytes:        snapshot,
		writeSnapshot:        true,
		manifestPresent:      true,
		expectedHashOverride: strings.Repeat("a", 64),
	})
	seedDeAdoptSupervisorIntent(t, stateRoot, rec)

	report, err := NewAPI().ExecuteDeAdopt(name, nil)
	if err == nil {
		t.Fatalf("ExecuteDeAdopt hash mismatch = report=%+v err=nil", report)
	}
	wire, marshalErr := json.Marshal(struct {
		Report *DeAdoptReport `json:"report"`
		Error  string         `json:"error"`
	}{Report: report, Error: err.Error()})
	if marshalErr != nil {
		t.Fatalf("marshal error/report envelope: %v", marshalErr)
	}
	if bytes.Contains(wire, []byte(snapshotSecret)) || bytes.Contains(wire, snapshot) {
		t.Fatalf("serialized error/report leaked snapshot content: %s", wire)
	}
}

func TestExecuteDeAdoptT13AcceptConflictRequiresMutationPointProof(t *testing.T) {
	t.Run("genuine conflict accepted and snapshot destroyed at close", func(t *testing.T) {
		name := "deadopt-accept-genuine"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		codexPath, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      deAdoptNativeConfig(name, "operator-owned"),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		liveBefore := mustReadFileForAdoptTest(t, codexPath)

		var out bytes.Buffer
		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, &out, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts: %v\n%s", err, out.String())
		}
		if !reflect.DeepEqual(report.Accepted, []string{"codex-cli"}) || len(report.Restored) != 0 || len(report.Failed) != 0 {
			t.Fatalf("accepted report = %+v", report)
		}
		if got := mustReadFileForAdoptTest(t, codexPath); !bytes.Equal(got, liveBefore) {
			t.Fatalf("accepted client was mutated\nbefore:\n%s\nafter:\n%s", liveBefore, got)
		}
		assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
		if !strings.Contains(out.String(), deAdoptAcceptConflictWarning) {
			t.Fatalf("mandatory snapshot-destruction warning missing: %s", out.String())
		}
		logBytes := mustReadFileForAdoptTest(t, filepath.Join(stateRoot, SupervisorEventLogFileLeaf))
		if !bytes.Contains(logBytes, []byte(`"event":"deadopt-client-accepted"`)) || !bytes.Contains(logBytes, []byte(deAdoptAcceptConflictWarning)) {
			t.Fatalf("accepted warning event missing:\n%s", logBytes)
		}
	})

	t.Run("still hub rejects acceptance and preserves snapshot", func(t *testing.T) {
		name := "deadopt-accept-still-hub"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:           AdoptOperationStateAdopted,
			originalState:   AdoptOriginalStatePresent,
			liveConfig:      deAdoptHubConfig(name),
			snapshotBytes:   snapshot,
			writeSnapshot:   true,
			manifestPresent: true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, nil, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts still-hub: %v", err)
		}
		if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "still the hub entry") || len(report.Accepted) != 0 {
			t.Fatalf("still-hub report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})

	t.Run("unverified snapshot rejects before acceptance", func(t *testing.T) {
		name := "deadopt-accept-bad-snapshot"
		snapshot := []byte(deAdoptNativeConfig(name, "pre-adopt-native"))
		_, manifestRoot, stateRoot, rec := setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
			state:                AdoptOperationStateAdopted,
			originalState:        AdoptOriginalStatePresent,
			liveConfig:           deAdoptNativeConfig(name, "operator-owned"),
			snapshotBytes:        snapshot,
			writeSnapshot:        true,
			snapshotHashOverride: strings.Repeat("0", 64),
			manifestPresent:      true,
		})
		seedDeAdoptSupervisorIntent(t, stateRoot, rec)
		snapshotPath := deAdoptSnapshotPathForTest(t, rec)

		report, err := NewAPI().ExecuteDeAdoptWithOpts(name, nil, ExecuteDeAdoptOpts{AcceptConflictClients: []string{"codex-cli"}})
		if err != nil {
			t.Fatalf("ExecuteDeAdoptWithOpts bad snapshot: %v", err)
		}
		if len(report.Failed) != 1 || !strings.Contains(report.Failed[0].Reason, "snapshot hash does not match") || len(report.Accepted) != 0 {
			t.Fatalf("bad-snapshot report = %+v", report)
		}
		assertDeAdoptRecoveryStateIntact(t, manifestRoot, stateRoot, rec, snapshotPath, "")
	})
}

func writeDeAdoptExecutorRecord(t *testing.T, rec AdoptProvenanceRecord) {
	t.Helper()
	if err := writeAdoptedEntries(&AdoptedEntries{
		Version: adoptedEntriesSchemaVersion,
		Records: []AdoptProvenanceRecord{rec},
	}); err != nil {
		t.Fatalf("write de-adopt executor record: %v", err)
	}
}

func TestExecuteDeAdoptProviderAdoptingManagedInstallGate(t *testing.T) {
	tests := []struct {
		name         string
		installState ProviderManagedInstallStateV1
		wantComplete bool
	}{
		{"preinstall absence completes", ProviderManagedInstallPreInstall, true},
		{"legacy unknown refuses", ProviderManagedInstallUnknown, false},
		{"may-have-applied refuses", ProviderManagedInstallMayHaveApplied, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-adopting-recovery-" + strings.ReplaceAll(tc.name, " ", "-")
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallDeAdoptFixture(t, name, AdoptOperationStateAdopting)
			rec.ProviderSource.ManagedInstallState = tc.installState
			writeDeAdoptExecutorRecord(t, rec)
			before, found, err := ReadAdoptProvenance(name)
			if err != nil || !found || before.ProviderSource == nil {
				t.Fatalf("authoritative provenance before attempt: found=%t rec=%+v err=%v", found, before, err)
			}
			if err := os.Remove(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
				t.Fatalf("remove fixture-created manifest: %v", err)
			}
			codexCLI, ok := clients.AllClients()["codex-cli"]
			if !ok {
				t.Fatal("codex-cli adapter missing from clients.AllClients()")
			}
			configPath := codexCLI.ConfigPath()
			configBefore, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read codex-cli config before de-adopt: %v", err)
			}
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			stopCalls := 0
			report, execErr := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{
				source: provider,
				loadIntent: func() (*SupervisorIntentFile, error) {
					return &SupervisorIntentFile{Version: 1}, nil
				},
				stop: func(context.Context, SupervisorDaemon) (StoppedSettlement, error) {
					stopCalls++
					return StoppedSettlement{}, errors.New("absent daemon must not be stopped")
				},
			}})
			configAfter, readErr := os.ReadFile(configPath)
			if readErr != nil || !bytes.Equal(configAfter, configBefore) {
				t.Fatalf("codex-cli config changed: path=%q before=%d bytes after=%d bytes err=%v", configPath, len(configBefore), len(configAfter), readErr)
			}
			if tc.wantComplete {
				if execErr != nil || report == nil || stopCalls != 0 || provider.calls != 1 || !provider.entry.Enabled {
					t.Fatalf("preinstall adopting recovery report=%+v err=%v stop=%d provider=%d enabled=%t", report, execErr, stopCalls, provider.calls, provider.entry.Enabled)
				}
				assertDeAdoptClosed(t, manifestRoot, stateRoot, rec)
				return
			}
			if execErr == nil || stopCalls != 0 || provider.calls != 0 {
				t.Fatalf("non-preinstall adopting recovery report=%+v err=%v stop=%d provider=%d", report, execErr, stopCalls, provider.calls)
			}
			persisted, found, readErr := ReadAdoptProvenance(name)
			if readErr != nil || !found || !reflect.DeepEqual(persisted, before) {
				t.Fatalf("refusal changed durable provenance: found=%t before=%+v after=%+v err=%v", found, before, persisted, readErr)
			}
		})
	}
}

func setupProviderPreInstallDeAdoptFixture(t *testing.T, name string, state AdoptOperationState) (manifestRoot, stateRoot string, rec AdoptProvenanceRecord, provider *providerLifecycleFake) {
	t.Helper()
	snapshot := []byte(deAdoptNativeConfig(name, "native-command"))
	_, manifestRoot, stateRoot, rec = setupDeAdoptPlannerFixture(t, name, deAdoptPlannerFixture{
		state: state, originalState: AdoptOriginalStatePresent,
		liveConfig: string(snapshot), snapshotBytes: snapshot, writeSnapshot: true,
		manifestPresent: true, manifestBytes: deAdoptManagedManifest(name, 0),
	})
	rec.ProviderSource = &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_applied",
		ManagedInstallState: ProviderManagedInstallPreInstall,
	}
	writeDeAdoptExecutorRecord(t, rec)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	provider = &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: name,
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: false,
		ReceiptFingerprint: "receipt", ActivationFingerprint: "disabled",
		ActivationEnabledPresent: true, ActivationEnabled: false,
		DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone,
		PolicyFingerprint: "policy",
	}}
	return manifestRoot, stateRoot, rec, provider
}

func providerRecoveryExactDaemon(rec AdoptProvenanceRecord) SupervisorDaemon {
	return SupervisorDaemon{
		TaskName: canonicalIntentTaskKey(supervisorTaskNameForManifestDaemon(rec.ManifestName, adoptDefaultDaemonName)),
		Server:   rec.ManifestName, Daemon: adoptDefaultDaemonName,
		Port: rec.Port, ManifestHash: rec.ExpectedManifestHash,
	}
}

func seedDeAdoptSupervisorIntent(t *testing.T, stateRoot string, rec AdoptProvenanceRecord) {
	t.Helper()
	intent := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:     `\mcp-local-hub-` + rec.ManifestName + `-` + adoptDefaultDaemonName,
			Server:       rec.ManifestName,
			Daemon:       adoptDefaultDaemonName,
			Port:         rec.Port,
			ManifestHash: rec.ExpectedManifestHash,
		}},
	}
	if err := WriteSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}
}

func seedDeAdoptVault(t *testing.T, values map[string]string) {
	t.Helper()
	if _, err := NewAPI().SecretsInit(); err != nil {
		t.Fatalf("SecretsInit: %v", err)
	}
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	for key, value := range values {
		if err := vault.Set(key, value); err != nil {
			t.Fatalf("vault.Set(%s): %v", key, err)
		}
	}
}

func deAdoptVaultKeys(t *testing.T) []string {
	t.Helper()
	vault, err := secrets.OpenVault(secrets.DefaultKeyPath(), secrets.DefaultVaultPath())
	if err != nil {
		t.Fatalf("OpenVault: %v", err)
	}
	return vault.List()
}

func deAdoptSnapshotPathForTest(t *testing.T, rec AdoptProvenanceRecord) string {
	t.Helper()
	dir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		t.Fatalf("adoptSnapshotDir: %v", err)
	}
	return filepath.Join(dir, rec.Clients[0].Client+adoptSnapshotFileSuffix)
}

func assertDeAdoptClosed(t *testing.T, manifestRoot, stateRoot string, rec AdoptProvenanceRecord) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); !os.IsNotExist(err) {
		t.Fatalf("manifest still present after close: %v", err)
	}
	if persisted, found, err := ReadAdoptProvenance(rec.ManifestName); err != nil || found || persisted != nil {
		t.Fatalf("provenance not closed: found=%v rec=%+v err=%v", found, persisted, err)
	}
	snapshotDir, err := adoptSnapshotDir(rec.ManifestName)
	if err != nil {
		t.Fatalf("adoptSnapshotDir: %v", err)
	}
	if _, err := os.Stat(snapshotDir); !os.IsNotExist(err) {
		t.Fatalf("snapshot dir still present after close: %v", err)
	}
	_ = stateRoot
}

func assertDeAdoptRecoveryStateIntact(t *testing.T, manifestRoot, stateRoot string, rec AdoptProvenanceRecord, snapshotPath, routedKey string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(manifestRoot, rec.ManifestName, "manifest.yaml")); err != nil {
		t.Fatalf("manifest was not preserved: %v", err)
	}
	persisted, found, err := ReadAdoptProvenance(rec.ManifestName)
	if err != nil || !found || persisted == nil || persisted.OperationState != AdoptOperationStateDeAdopting {
		t.Fatalf("recoverable row missing: found=%v rec=%+v err=%v", found, persisted, err)
	}
	if snapshotPath != "" {
		if _, err := os.Stat(snapshotPath); err != nil {
			t.Fatalf("snapshot was not preserved: %v", err)
		}
	}
	intent, err := ReadSupervisorIntent(filepath.Join(stateRoot, supervisorIntentFileLeaf))
	if err != nil {
		t.Fatalf("ReadSupervisorIntent: %v", err)
	}
	if len(intent.Daemons) != 1 || intent.Daemons[0].Server != rec.ManifestName {
		t.Fatalf("supervisor intent was not preserved: %+v", intent.Daemons)
	}
	if routedKey != "" {
		keys := deAdoptVaultKeys(t)
		if !reflect.DeepEqual(keys, []string{routedKey}) {
			t.Fatalf("routed secret key was not preserved: %#v", keys)
		}
	}
}
