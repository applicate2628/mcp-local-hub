package api

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"mcp-local-hub/internal/clients"
)

func setupPendingProviderAdmission(t *testing.T, name string) (string, string, string, *AdoptProvenanceRecord, *providerLifecycleFake, providerTransactionDeps) {
	t.Helper()
	clientPath, dir, state, plan, provider, deps := setupProviderInstallFailureFixture(t, name)
	a := NewAPI()
	if _, err := a.captureAdoptProvenance(plan); err != nil {
		t.Fatal(err)
	}
	provider.entry.Enabled = false
	provider.entry.ActivationEnabled = false
	provider.entry.ActivationFingerprint = plan.providerSource.ExpectedDisabledFingerprint
	if err := markProviderDisableApplied(name, plan.providerSource.ExpectedDisabledFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := a.ManifestCreate(name, plan.ManifestYAML); err != nil {
		t.Fatal(err)
	}
	rec, found, err := ReadAdoptProvenance(name)
	if err != nil || !found {
		t.Fatalf("pending receipt: found=%v err=%v", found, err)
	}
	return clientPath, dir, state, rec, provider, deps
}

// Only substitute the executable at the existing admission seam. The ordinary
// Install caller, process containment, protocol probe, wait/reap, client writes,
// rollback, phase persistence, and recovery all use their production owners.
func useProviderAdmissionHelper(t *testing.T, name, mode string, after func(error) error) {
	t.Helper()
	t.Setenv(stdioBridgeAdmissionHelperEnv, mode)
	frozenStdioBridgeAdmissionFn = func(ctx context.Context, requests []FrozenStdioBridgeAdmissionRequest) error {
		if len(requests) != 1 || requests[0].ManifestName != name {
			t.Fatalf("unexpected admission requests: %+v", requests)
		}
		if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
			t.Errorf("process-owning probe entered before started marker: phase=%q err=%v", phase, err)
		}
		t.Setenv(stdioBridgeAdmissionHelperPortEnv, strconv.Itoa(requests[0].Port))
		requests[0].Command = os.Args[0]
		requests[0].Args = []string{"-test.run=^TestFrozenStdioBridgeAdmissionHelper$"}
		requests[0].StartDeadline = 2 * time.Second
		err := admitFrozenStdioBridgeRequests(ctx, requests)
		if portInUse(requests[0].Port) {
			t.Error("real provisional helper left its port bound")
		}
		if after != nil {
			return after(err)
		}
		return err
	}
}

func assertProviderAdmissionRecovery(t *testing.T, name, dir, state string, rec *AdoptProvenanceRecord, provider *providerLifecycleFake, deps providerTransactionDeps) {
	t.Helper()
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatalf("BuildDeAdoptPlan after settled install: %v", err)
	}
	if !plan.providerRecovery || !plan.ProviderRecoveryReady {
		t.Fatalf("settled install did not admit provider recovery: %+v", plan)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: deps}); err != nil {
		t.Fatalf("provider recovery failed: %v", err)
	}
	if !provider.entry.Enabled {
		t.Fatal("recovery did not restore the disabled provider")
	}
	assertDeAdoptClosed(t, dir, state, *rec)
}

func assertProviderAdmissionRetained(t *testing.T, name, dir string, provider *providerLifecycleFake) {
	t.Helper()
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
		t.Errorf("unconfirmed install must retain started: phase=%q err=%v", phase, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); err != nil {
		t.Errorf("unconfirmed install lost manifest: %v", err)
	}
	if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
		t.Errorf("unconfirmed install lost provenance: found=%v err=%v", found, err)
	}
	if provider.entry.Enabled || provider.calls != 0 {
		t.Errorf("unconfirmed install restored provider: enabled=%v calls=%d", provider.entry.Enabled, provider.calls)
	}
}

func TestProviderAdmissionUnknownCleanupRetainsStarted(t *testing.T) {
	const name = "provider-admission-unknown"
	clientPath, dir, _, _, provider, deps := setupPendingProviderAdmission(t, name)
	original := string(mustReadFileForAdoptTest(t, clientPath))
	cause := errors.New("process owner could not confirm provisional cleanup")
	called := false
	frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error {
		called = true
		if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
			t.Errorf("process-owning probe entered before started marker: phase=%q err=%v", phase, err)
		}
		return cause // Deliberately no cleanup-error sentinel or bound port.
	}
	err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
	if !called || !errors.Is(err, cause) {
		t.Fatalf("uncertain admission not exercised: called=%v err=%v", called, err)
	}
	assertProviderAdmissionRetained(t, name, dir, provider)
	if got := string(mustReadFileForAdoptTest(t, clientPath)); got != original {
		t.Error("failed admission changed client config")
	}
	plan, planErr := NewAPI().BuildDeAdoptPlan(name)
	if planErr == nil {
		if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: deps}); err == nil {
			t.Error("provider recovery accepted unknown provisional cleanup")
		}
	}
	assertProviderAdmissionRetained(t, name, dir, provider)
}

func TestProviderAdmissionConfirmedCleanupPreservesRecovery(t *testing.T) {
	for _, mode := range []string{"legacy", "ready"} {
		t.Run(mode, func(t *testing.T) {
			const name = "provider-admission-cleaned"
			clientPath, dir, state, rec, provider, deps := setupPendingProviderAdmission(t, name)
			original := string(mustReadFileForAdoptTest(t, clientPath))
			useProviderAdmissionHelper(t, name, mode, nil)
			cause := errors.New("audit refused after confirmed admission cleanup")
			if mode == "ready" {
				previous := appendIntentAuditFn
				appendIntentAuditFn = func(IntentAuditEntry) error { return cause }
				t.Cleanup(func() { appendIntentAuditFn = previous })
			}
			err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
			if mode == "legacy" {
				var readiness *MCPReadinessError
				if !errors.As(err, &readiness) || readiness.Result.FailureID != "MCP_BACKING_PROTOCOL_UNSUPPORTED" {
					t.Fatalf("real helper did not reach protocol rejection: %v", err)
				}
			} else if !errors.Is(err, cause) {
				t.Fatalf("post-probe audit failure not reached: %v", err)
			}
			if strings.Contains(err.Error(), "E_PROVIDER_LIFECYCLE_UNSUPPORTED") {
				t.Errorf("known untouched provider acquired a spurious settlement failure: %v", err)
			}
			if got := string(mustReadFileForAdoptTest(t, clientPath)); got != original {
				t.Error("pre-client failure changed config")
			}
			assertProviderAdmissionRecovery(t, name, dir, state, rec, provider, deps)
		})
	}
}

func TestProviderAdmissionOrdinaryInstallRollbackSettlesAfterInnerRelease(t *testing.T) {
	const name = "provider-admission-ordinary-rollback"
	clientPath, dir, state, rec, provider, deps := setupPendingProviderAdmission(t, name)
	useProviderAdmissionHelper(t, name, "ready", nil)
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: {addEntry: addEntryFailMutated, restore: restoreSucceed}})
	lockPath := supervisorIntentLockPath(filepath.Join(state, supervisorIntentFileLeaf))
	previous := flockUnlockFn
	released := false
	flockUnlockFn = func(fl *flock.Flock) error {
		if !released && fl.Path() == lockPath && seam.writeN[filepath.Clean(clientPath)] == 2 {
			released = true
			if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
				t.Errorf("rollback settled before inner lock release: phase=%q err=%v", phase, err)
			}
		}
		return previous(fl)
	}
	t.Cleanup(func() { flockUnlockFn = previous })
	err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "induced mutated add-entry failure") || !released {
		t.Fatalf("real client rollback not exercised: released=%v writes=%v err=%v", released, seam.writeN, err)
	}
	entry, readErr := clients.AllClients()["codex-cli"].GetEntry(name)
	if readErr != nil || entry != nil {
		t.Fatalf("rollback did not restore client: entry=%+v err=%v", entry, readErr)
	}
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseManagedSettled {
		t.Errorf("ordinary Install lost completed rollback proof: phase=%q err=%v", phase, err)
	}
	if _, err := os.Stat(filepath.Join(dir, name, "manifest.yaml")); err != nil {
		t.Fatalf("ordinary Install discarded recovery manifest: %v", err)
	}
	if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
		t.Fatalf("ordinary Install discarded provenance: found=%v err=%v", found, err)
	}
	assertProviderAdmissionRecovery(t, name, dir, state, rec, provider, deps)
}

func TestProviderAdmissionPriorStartedHistoryIsNotErased(t *testing.T) {
	for _, mode := range []string{"ready", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			const name = "provider-admission-prior-started"
			clientPath, dir, _, _, provider, _ := setupPendingProviderAdmission(t, name)
			if err := markProviderInstallStartedForTask(name, supervisorTaskNameForManifestDaemon(name, adoptDefaultDaemonName)); err != nil {
				t.Fatal(err)
			}
			useProviderAdmissionHelper(t, name, mode, nil)
			seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: {addEntry: addEntryFailMutated, restore: restoreSucceed}})
			if err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard}); err == nil {
				t.Fatal("injected install failure was lost")
			}
			assertProviderAdmissionRetained(t, name, dir, provider)
		})
	}
}

func TestProviderAdmissionSuccessfulInstallAndDryRun(t *testing.T) {
	const name = "provider-admission-success"
	_, _, state, _, _, _ := setupPendingProviderAdmission(t, name)
	frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error {
		t.Fatal("dry run launched admission")
		return nil
	}
	opts := InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard, DryRun: true}
	if err := NewAPI().Install(opts); err != nil {
		t.Fatal(err)
	}
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseNotStarted {
		t.Fatalf("dry run changed marker: phase=%q err=%v", phase, err)
	}
	useProviderAdmissionHelper(t, name, "ready", nil)
	opts.DryRun = false
	if err := NewAPI().Install(opts); err != nil {
		t.Fatalf("successful real probe did not allow install: %v", err)
	}
	if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseStarted {
		t.Fatalf("published install incorrectly settled: phase=%q err=%v", phase, err)
	}
	intent, err := ReadSupervisorIntent(filepath.Join(state, supervisorIntentFileLeaf))
	if err != nil || len(intent.Daemons) != 1 || intent.Daemons[0].Server != name {
		t.Fatalf("successful install did not publish descriptor: intent=%+v err=%v", intent, err)
	}
}

// A positive cleanup result nested beside another error cannot certify the
// complete admission outcome. This exercises real reaping before injecting the
// additional uncertainty at the existing admission seam.
func TestProviderAdmissionJoinedUncertaintyRetainsStarted(t *testing.T) {
	const name = "provider-admission-joined-uncertainty"
	_, dir, _, _, provider, _ := setupPendingProviderAdmission(t, name)
	cause := errors.New("additional process-owner uncertainty")
	useProviderAdmissionHelper(t, name, "legacy", func(err error) error {
		var readiness *MCPReadinessError
		if !errors.As(err, &readiness) {
			t.Fatalf("real failing probe did not execute: %v", err)
		}
		return errors.Join(err, cause)
	})
	err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
	if !errors.Is(err, cause) {
		t.Fatalf("joined uncertainty was lost: %v", err)
	}
	assertProviderAdmissionRetained(t, name, dir, provider)
}

func TestProviderAdmissionUnknownMarkerBlocksProbe(t *testing.T) {
	for _, marker := range []string{"missing", "corrupt", providerInstallPhaseRecoveryClaimed, providerInstallPhaseManagedSettled} {
		t.Run(marker, func(t *testing.T) {
			const name = "provider-admission-unknown-marker"
			clientPath, _, _, _, _, _ := setupPendingProviderAdmission(t, name)
			path, err := providerInstallPhasePath(name)
			if err != nil {
				t.Fatal(err)
			}
			switch marker {
			case "missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := WriteStateFileBytesAtomic(path, []byte("{")); err != nil {
					t.Fatal(err)
				}
			default:
				if err := writeProviderInstallPhase(name, marker); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			frozenStdioBridgeAdmissionFn = func(context.Context, []FrozenStdioBridgeAdmissionRequest) error {
				called = true
				return errors.New("probe must not run")
			}
			before := string(mustReadFileForAdoptTest(t, clientPath))
			err = NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
			if err == nil || called {
				t.Errorf("unknown/claimed marker admitted a process: called=%v err=%v", called, err)
			}
			if got := string(mustReadFileForAdoptTest(t, clientPath)); got != before {
				t.Error("refused admission changed client config")
			}
		})
	}
}

func TestProviderAdmissionRollbackUnconfirmedInnerReleaseRetainsStarted(t *testing.T) {
	const name = "provider-admission-release-unconfirmed"
	clientPath, dir, state, _, provider, _ := setupPendingProviderAdmission(t, name)
	useProviderAdmissionHelper(t, name, "ready", nil)
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: {addEntry: addEntryFailMutated, restore: restoreSucceed}})
	lockPath := supervisorIntentLockPath(filepath.Join(state, supervisorIntentFileLeaf))
	previous := flockUnlockFn
	cause := errors.New("injected unpublished rollback lock release failure")
	var stranded *flock.Flock
	flockUnlockFn = func(fl *flock.Flock) error {
		if fl.Path() == lockPath && seam.writeN[filepath.Clean(clientPath)] == 2 {
			stranded = fl
			return cause
		}
		return previous(fl)
	}
	t.Cleanup(func() {
		flockUnlockFn = previous
		if stranded != nil {
			if err := stranded.Unlock(); err != nil {
				t.Errorf("test lock cleanup: %v", err)
			}
		}
		unconfirmedLockReleasesMu.Lock()
		delete(unconfirmedLockReleases, lockPath)
		unconfirmedLockReleasesMu.Unlock()
	})
	err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
	if !errors.Is(err, cause) || !errors.Is(err, ErrLockReleaseUnconfirmed) || stranded == nil {
		t.Fatalf("inner release failure not exercised: stranded=%v err=%v", stranded != nil, err)
	}
	entry, readErr := clients.AllClients()["codex-cli"].GetEntry(name)
	if readErr != nil || entry != nil {
		t.Fatalf("test did not complete the actual client rollback: entry=%+v err=%v", entry, readErr)
	}
	assertProviderAdmissionRetained(t, name, dir, provider)
}

func TestProviderAdmissionRetainedClientOutcomesDoNotSettle(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec clientWriteSpec
	}{
		{"incomplete", clientWriteSpec{addEntry: addEntryFailMutated, restore: restoreFail}},
		{"restore-release-unconfirmed", clientWriteSpec{addEntry: addEntryFailMutated, restore: restoreAppliedReleaseUnconfirmed}},
		{"forward-committed", clientWriteSpec{addEntry: addEntryAppliedReleaseUnconfirmed, restore: restoreSucceed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "provider-admission-retained-client"
			clientPath, dir, _, _, provider, _ := setupPendingProviderAdmission(t, name)
			useProviderAdmissionHelper(t, name, "ready", nil)
			seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: tc.spec})
			err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
			if err == nil || seam.writeN[filepath.Clean(clientPath)] == 0 {
				t.Fatalf("client failure not exercised: writes=%v err=%v", seam.writeN, err)
			}
			var rolledBack *providerInstallUnpublishedRollbackError
			if errors.As(err, &rolledBack) {
				t.Errorf("unsettled client outcome certified rollback: %v", err)
			}
			assertProviderAdmissionRetained(t, name, dir, provider)
		})
	}
}

func TestProviderAdmissionPreexistingIntentDoesNotCertifyRollback(t *testing.T) {
	const name = "provider-admission-prior-intent"
	clientPath, dir, state, rec, provider, _ := setupPendingProviderAdmission(t, name)
	// Existing descriptor ownership is independent of this invocation's client
	// rollback. The fixture's successful admission seam starts no process.
	prior := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{
		TaskName: supervisorTaskNameForManifestDaemon(name, adoptDefaultDaemonName),
		Server:   name, Daemon: adoptDefaultDaemonName, Port: rec.Port,
		ManifestHash: rec.ExpectedManifestHash,
	}}}
	if err := WriteSupervisorIntent(filepath.Join(state, supervisorIntentFileLeaf), prior); err != nil {
		t.Fatal(err)
	}
	seam := seedInstallWriteSeam(t, map[string]clientWriteSpec{clientPath: {addEntry: addEntryFailMutated, restore: restoreSucceed}})
	err := NewAPI().Install(InstallOpts{Server: name, ClientsInclude: []string{"codex-cli"}, Writer: io.Discard})
	if err == nil || seam.writeN[filepath.Clean(clientPath)] != 2 {
		t.Fatalf("real client rollback not exercised: writes=%v err=%v", seam.writeN, err)
	}
	var rolledBack *providerInstallUnpublishedRollbackError
	if errors.As(err, &rolledBack) {
		t.Errorf("preexisting supervisor ownership certified unpublished rollback: %v", err)
	}
	assertProviderAdmissionRetained(t, name, dir, provider)
}
