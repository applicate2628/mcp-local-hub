package api

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestProviderRecoveryAuditResumeAfterE2(t *testing.T) {
	for _, claimBeforeE2 := range []bool{false, true} {
		label := "old-sequence-not-started"
		if claimBeforeE2 {
			label = "claimed-before-e2"
		}
		t.Run(label, func(t *testing.T) {
			name := "provider-audit-e2-retry"
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
				t.Fatal(err)
			}
			if claimBeforeE2 {
				claimed, err := providerInstallNeverStarted(rec)
				if err != nil || !claimed {
					t.Fatalf("claim=%t err=%v", claimed, err)
				}
			}
			if err := MarkAdoptProvenanceDeAdopting(name); err != nil {
				t.Fatal(err)
			}
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Routing != DeAdoptRoutingResume || !plan.providerRecovery {
				t.Fatalf("retry lost provider recovery: %+v", plan)
			}
			if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
				t.Fatal(err)
			}
			if provider.calls != 1 || !provider.entry.Enabled {
				t.Fatalf("restore calls=%d enabled=%t", provider.calls, provider.entry.Enabled)
			}
			assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
		})
	}
}

func TestProviderRecoveryAuditMissingMarkerDoesNotInventPhase(t *testing.T) {
	name := "provider-audit-unknown-history"
	manifestRoot, _, _, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err == nil {
		t.Fatal("unknown pre-install history was accepted")
	}
	current, found, err := ReadAdoptProvenance(name)
	if err != nil || !found || current.ProviderSource == nil {
		t.Fatalf("recovery evidence lost: found=%t err=%v", found, err)
	}
	if current.OperationState != AdoptOperationStateAdopting || current.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("refusal invented a durable transition: %+v", current)
	}
	if _, err := os.Lstat(marker); !os.IsNotExist(err) {
		t.Fatalf("unknown history was rewritten: %v", err)
	}
	if provider.calls != 0 || provider.entry.Enabled {
		t.Fatal("provider restored without historical proof")
	}
}

func TestProviderRecoveryAuditStalePlanCannotBypassStartedMarker(t *testing.T) {
	name := "provider-audit-stale-plan"
	manifestRoot, _, _, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.providerRecovery {
		t.Fatal("fixture did not enter provider recovery")
	}
	if err := markProviderInstallStartedForTask(name, "mcp-local-hub-"+name+"-"+adoptDefaultDaemonName); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err == nil {
		t.Fatal("stale recovery plan bypassed Install admission")
	}
	current, found, err := ReadAdoptProvenance(name)
	if err != nil || !found || current.ProviderSource == nil || current.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("stale-plan refusal lost the original receipt: found=%t err=%v", found, err)
	}
	if current.OperationState != AdoptOperationStateAdopting || provider.calls != 0 || provider.entry.Enabled {
		t.Fatal("stale recovery plan crossed a mutation boundary")
	}
}

func TestProviderRecoveryAuditResumeAfterManifestDeleteCrash(t *testing.T) {
	name := "provider-audit-delete-crash"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	previous := deAdoptAfterManifestDeleteHook
	t.Cleanup(func() { deAdoptAfterManifestDeleteHook = previous })
	const crash = "audit crash after delete"
	deAdoptAfterManifestDeleteHook = func() { panic(crash) }
	func() {
		defer func() {
			if got := recover(); got != crash {
				t.Fatalf("panic=%v, want injected crash", got)
			}
		}()
		_, _ = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
	}()
	deAdoptAfterManifestDeleteHook = previous
	current, found, err := ReadAdoptProvenance(name)
	if err != nil || !found || current.ProviderSource == nil || current.ProviderSource.DeAdoptPhase != "" {
		t.Fatalf("unexpected crash boundary: found=%t err=%v", found, err)
	}
	phase, err := readProviderInstallPhase(name)
	if err != nil || phase != providerInstallPhaseRecoveryClaimed {
		t.Fatalf("crash lost recovery claim: phase=%q err=%v", phase, err)
	}
	retry, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Routing != DeAdoptRoutingFresh || !retry.providerRecovery {
		t.Fatal("post-delete retry lost recovery routing")
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(retry, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 1 || !provider.entry.Enabled {
		t.Fatal("retry did not restore provider exactly once")
	}
	assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
}
