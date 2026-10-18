package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func providerManagedInstallTestPlan(t *testing.T, entry string) (*AdoptPlan, *providerLifecycleFake, providerDirectProcessObserver) {
	t.Helper()
	setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n")
	cwd := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entry,
		Transport: clients.ProviderMCPTransportStdio, Command: exe, WorkingDir: &cwd,
		Scope: clients.ProviderMCPScopeUser, Enabled: true,
		ReceiptFingerprint: "receipt", ActivationFingerprint: "activation",
		ActivationEnabledPresent: true, ActivationEnabled: true,
		DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone,
		PolicyFingerprint: "policy",
	}}
	providerSource := &ProviderSourceProvenanceV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entry,
		Scope: "user", ReceiptFingerprint: "receipt", ActivationFingerprint: "activation",
		PolicyFingerprint: "policy", PriorEnabledPresent: true, PriorEnabled: true,
		ExpectedDisabledFingerprint: "disabled", DisablePhase: "disable_planned",
	}
	plan := &AdoptPlan{
		EntryName: entry, SourceClient: "codex-cli", ManifestName: entry, Port: deAdoptTestPort,
		AdoptClients: []string{"codex-cli"}, ManifestYAML: string(deAdoptManagedManifest(entry, 0)),
		TargetEntryNames: map[string]string{"codex-cli": entry}, presentAtBuild: []string{"codex-cli"},
		providerSource: providerSource,
	}
	observer := func(context.Context, providerDirectProcessIdentityV1, []providerProcessGenerationV1) providerDirectProcessObservationV1 {
		return providerDirectProcessObservationV1{State: providerProcessObservationComplete}
	}
	return plan, provider, observer
}

func TestProviderManagedInstallStateAdvancesBeforeInstall(t *testing.T) {
	t.Run("persisted before install call", func(t *testing.T) {
		entry := "provider-install-state-before-call"
		plan, provider, observer := providerManagedInstallTestPlan(t, entry)
		installCalls := 0
		installErr := errors.New("injected install failure")
		err := NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{
			providerDeps: providerTransactionDeps{source: provider, observer: observer},
			installFn: func(_ context.Context, _ *API, _ InstallOpts) error {
				installCalls++
				persisted, found, readErr := ReadAdoptProvenance(entry)
				if readErr != nil || !found || persisted.ProviderSource == nil {
					t.Fatalf("read provenance at Install boundary: found=%t row=%+v err=%v", found, persisted, readErr)
				}
				if got := persisted.ProviderSource.ManagedInstallState; got != ProviderManagedInstallMayHaveApplied {
					t.Fatalf("managed install state at Install boundary=%q, want install_may_have_applied", got)
				}
				return installErr
			},
		})
		if err == nil || installCalls != 1 {
			t.Fatalf("ExecuteAdoptWithOpts err=%v install calls=%d, want injected failure after one call", err, installCalls)
		}
	})

	t.Run("stale durable state prevents install call", func(t *testing.T) {
		entry := "provider-install-state-cas-refusal"
		plan, provider, observer := providerManagedInstallTestPlan(t, entry)
		provider.onCAS = func() {
			if provider.calls != 1 {
				return
			}
			persisted, found, err := ReadAdoptProvenance(entry)
			if err != nil || !found || persisted.ProviderSource == nil {
				t.Fatalf("read provenance before injected conflict: found=%t row=%+v err=%v", found, persisted, err)
			}
			persisted.ProviderSource.ManagedInstallState = ProviderManagedInstallUnknown
			if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{*persisted}}); err != nil {
				t.Fatalf("inject durable state conflict: %v", err)
			}
		}
		installCalls := 0
		err := NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{
			providerDeps: providerTransactionDeps{source: provider, observer: observer},
			installFn: func(_ context.Context, _ *API, _ InstallOpts) error {
				installCalls++
				return errors.New("install invoked despite durable state conflict")
			},
		})
		if err == nil || installCalls != 0 {
			t.Fatalf("ExecuteAdoptWithOpts err=%v install calls=%d, want durable CAS refusal before Install", err, installCalls)
		}
	})
}

func TestCaptureAdoptProvenanceProviderStartsPreInstall(t *testing.T) {
	entry := "provider-preinstall-capture"
	setupAdoptTestEnv(t, entry, "[mcp_servers."+entry+"]\ncommand = \"go\"\nargs = [\"version\"]\n")
	provider := sampleProviderSource()
	provider.ServerName = entry
	plan := &AdoptPlan{
		EntryName:        entry,
		SourceClient:     "codex-cli",
		ManifestName:     entry,
		Port:             deAdoptTestPort,
		AdoptClients:     []string{"codex-cli"},
		ManifestYAML:     "name: " + entry + "\n",
		TargetEntryNames: map[string]string{"codex-cli": entry},
		presentAtBuild:   []string{"codex-cli"},
		providerSource:   provider,
	}

	rec, err := NewAPI().captureAdoptProvenance(plan)
	if err != nil {
		t.Fatalf("capture provider provenance: %v", err)
	}
	if rec.ProviderSource == nil {
		t.Fatal("captured provider provenance is nil")
	}
	if rec.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall {
		t.Fatalf("captured managed install state=%q, want preinstall", rec.ProviderSource.ManagedInstallState)
	}
	persisted, found, err := ReadAdoptProvenance(entry)
	if err != nil || !found || persisted.ProviderSource == nil {
		t.Fatalf("reload captured provider provenance: found=%t row=%+v err=%v", found, persisted, err)
	}
	if got := persisted.ProviderSource.ManagedInstallState; got != ProviderManagedInstallPreInstall {
		t.Fatalf("persisted managed install state=%q, want preinstall", got)
	}
}

func TestProviderManagedInstallStatePersistsAcrossReadWrite(t *testing.T) {
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ProviderSource = sampleProviderSource()

	raw, err := json.Marshal(AdoptedEntries{Version: adoptedEntriesSchemaV2, Records: []AdoptProvenanceRecord{rec}})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	record := wire["records"].([]any)[0].(map[string]any)
	provider := record["provider_source"].(map[string]any)
	provider["managed_install_state"] = "preinstall"
	raw, err = json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeHubMcpStateFile(adoptedEntriesFileLeaf, raw); err != nil {
		t.Fatalf("seed provider provenance: %v", err)
	}

	persisted, found, err := ReadAdoptProvenance(rec.ManifestName)
	if err != nil || !found || persisted.ProviderSource == nil {
		t.Fatalf("read provider provenance: found=%t row=%+v err=%v", found, persisted, err)
	}
	state := reflect.ValueOf(persisted.ProviderSource).Elem().FieldByName("ManagedInstallState")
	if !state.IsValid() || state.Kind() != reflect.String {
		t.Fatalf("provider provenance dropped managed_install_state during reload")
	}
	if got := state.String(); got != "preinstall" {
		t.Fatalf("managed install state after reload=%q, want preinstall", got)
	}

	state.SetString("install_may_have_applied")
	if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{*persisted}}); err != nil {
		t.Fatalf("rewrite provider provenance: %v", err)
	}
	reloaded, found, err := ReadAdoptProvenance(rec.ManifestName)
	if err != nil || !found || reloaded.ProviderSource == nil {
		t.Fatalf("reload rewritten provider provenance: found=%t row=%+v err=%v", found, reloaded, err)
	}
	if got := reflect.ValueOf(reloaded.ProviderSource).Elem().FieldByName("ManagedInstallState").String(); got != "install_may_have_applied" {
		t.Fatalf("managed install state after rewrite=%q, want install_may_have_applied", got)
	}
}

func TestProviderManagedInstallStateLegacyZeroAndInvalidAreFailClosed(t *testing.T) {
	t.Run("missing wire field remains unknown", func(t *testing.T) {
		isolateStateDir(t)
		rec := sampleAdoptRecord()
		rec.ProviderSource = sampleProviderSource()
		if err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}}); err != nil {
			t.Fatalf("write legacy provider provenance: %v", err)
		}
		persisted, found, err := ReadAdoptProvenance(rec.ManifestName)
		if err != nil || !found || persisted.ProviderSource == nil {
			t.Fatalf("read legacy provider provenance: found=%t row=%+v err=%v", found, persisted, err)
		}
		if got := persisted.ProviderSource.ManagedInstallState; got != ProviderManagedInstallUnknown {
			t.Fatalf("legacy managed install state=%q, want unknown", got)
		}
	})

	t.Run("unrecognized wire value is rejected", func(t *testing.T) {
		isolateStateDir(t)
		rec := sampleAdoptRecord()
		rec.ProviderSource = sampleProviderSource()
		rec.ProviderSource.ManagedInstallState = ProviderManagedInstallStateV1("future-state")
		err := writeAdoptedEntries(&AdoptedEntries{Records: []AdoptProvenanceRecord{rec}})
		if err == nil || !strings.Contains(err.Error(), `managed_install_state "future-state" is invalid`) {
			t.Fatalf("invalid managed install state error=%v", err)
		}
	})
}
