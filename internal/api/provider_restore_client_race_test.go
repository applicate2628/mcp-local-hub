package api

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
)

func TestProviderRestoreGateRejectsReinstalledHubBinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		alias     bool
		recreated bool
	}{
		{name: "legacy source", recreated: true},
		{name: "recreated alias", alias: true, recreated: true},
		{name: "removed alias", alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-restore-target-key"
			manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
			if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
				t.Fatal(err)
			}
			if len(rec.Clients) != 1 || rec.Clients[0].Client != "codex-cli" {
				t.Fatalf("unexpected fixture client provenance: %+v", rec.Clients)
			}
			target := rec.SourceEntryName
			if tc.alias {
				target += "-http"
				rec.Clients[0].TargetEntryName = target
			}
			rec.Clients[0].ToolTimeoutSec = 30
			rec.ProviderSource.DeAdoptPhase = "managed_removed"
			writeDeAdoptExecutorRecord(t, *rec)
			if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
				t.Fatal(err)
			}
			config := deAdoptNativeConfig(rec.SourceEntryName, "native-command")
			if tc.recreated {
				config = deAdoptHubConfigWithToolTimeout(target, 30)
				if tc.alias {
					config += deAdoptNativeConfig(rec.SourceEntryName, "native-command")
				}
			}
			adapter := clients.AllClients()["codex-cli"]
			if adapter == nil {
				t.Fatal("codex-cli adapter unavailable")
			}
			if err := os.WriteFile(adapter.ConfigPath(), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			if allowed := providerInstallPhaseAllowsRestore(name); allowed == tc.recreated {
				t.Fatalf("restore allowed=%t for target %q, recreated=%t", allowed, target, tc.recreated)
			}
			if got := mustReadFileForAdoptTest(t, adapter.ConfigPath()); string(got) != config {
				t.Fatal("read-only restore guard changed the client configuration")
			}
		})
	}
}

func TestProviderPreInstallRecoveryCompletesWithoutClientAdapter(t *testing.T) {
	for _, manifestPresent := range []bool{false, true} {
		label := "before-manifest"
		if manifestPresent {
			label = "after-manifest"
		}
		t.Run(label, func(t *testing.T) {
			name := "provider-recovery-unavailable-client"
			manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			if !manifestPresent {
				if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
					t.Fatal(err)
				}
			}
			// Install never crossed its mutation barrier, so this recorded
			// client's availability must not block provider-only recovery.
			rec.AdoptClients = []string{"provider-client-probe-unavailable"}
			rec.Clients = nil
			writeDeAdoptExecutorRecord(t, *rec)
			plan, err := NewAPI().BuildDeAdoptPlan(name)
			if err != nil {
				t.Fatal(err)
			}
			if !plan.providerRecovery {
				t.Fatalf("durable pre-Install recovery was not selected: %+v", plan)
			}
			if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
				t.Fatalf("provider recovery depended on an untouched client: %v", err)
			}
			if provider.calls != 1 || !provider.entry.Enabled {
				t.Fatalf("restore calls=%d enabled=%t", provider.calls, provider.entry.Enabled)
			}
			assertDeAdoptClosed(t, manifestRoot, stateRoot, *rec)
		})
	}
}

func TestProviderManagedSettlementStillRequiresReadableClient(t *testing.T) {
	name := "provider-settled-unavailable-client"
	manifestRoot, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.AdoptClients = []string{"provider-client-probe-unavailable"}
	rec.Clients = nil
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, *rec)
	if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
		t.Fatal(err)
	}
	if providerInstallPhaseAllowsRestore(name) {
		t.Fatal("managed settlement bypassed the unreadable-client guard")
	}
}

func TestProviderRecoveryExecutableRequiresPositiveManifestProof(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readiness DeAdoptManifestReadiness
		want      bool
	}{
		{name: "unknown", readiness: DeAdoptManifestReadiness{}},
		{name: "unreadable", readiness: DeAdoptManifestReadiness{Reason: "manifest could not be read"}},
		{name: "hash mismatch", readiness: DeAdoptManifestReadiness{Present: true}},
		{name: "absent", readiness: DeAdoptManifestReadiness{AlreadyAbsent: true}, want: true},
		{name: "verified", readiness: DeAdoptManifestReadiness{Present: true, HashReady: true}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := &DeAdoptPlan{Routing: DeAdoptRoutingFresh, providerRecovery: true, Manifest: tc.readiness}
			if got := plan.ProviderRecoveryExecutable(); got != tc.want {
				t.Fatalf("executable=%t, want %t for readiness %+v", got, tc.want, tc.readiness)
			}
		})
	}
}

func TestProviderClaimedRestoreRequiresAbsentManifest(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exists  bool
		err     error
		allowed bool
	}{
		{name: "absent", allowed: true},
		{name: "recreated", exists: true},
		{name: "unreadable", err: os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "provider-claimed-restore-manifest"
			_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
			rec.ProviderSource.DeAdoptPhase = "managed_removed"
			rec.AdoptClients = []string{"provider-client-probe-unavailable"}
			rec.Clients = nil
			writeDeAdoptExecutorRecord(t, *rec)
			if err := writeProviderInstallPhase(name, providerInstallPhaseRecoveryClaimed); err != nil {
				t.Fatal(err)
			}
			previous := adoptManifestExistsFn
			adoptManifestExistsFn = func(string) (bool, error) { return tc.exists, tc.err }
			t.Cleanup(func() { adoptManifestExistsFn = previous })
			if allowed := providerRestoreBindingsClear(name); allowed != tc.allowed {
				t.Fatalf("claimed restore allowed=%t, want %t", allowed, tc.allowed)
			}
		})
	}
}

func TestProviderManagedRecoveryRetryRepairsRecreatedHubBinding(t *testing.T) {
	for _, phase := range []string{"managed_removed", "restore_applied"} {
		for _, alias := range []bool{false, true} {
			label := phase + "/source"
			if alias {
				label = phase + "/alias"
			}
			t.Run(label, func(t *testing.T) {
				const name = "provider-retry-recreated-binding"
				dir, stateDir, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
				if err := os.Remove(filepath.Join(dir, name, "manifest.yaml")); err != nil {
					t.Fatal(err)
				}
				target := rec.SourceEntryName
				if alias {
					target += "-http"
					rec.Clients[0].TargetEntryName = target
				}
				rec.Clients[0].ToolTimeoutSec = 30
				rec.ProviderSource.DeAdoptPhase = phase
				writeDeAdoptExecutorRecord(t, *rec)
				if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
					t.Fatal(err)
				}
				if phase == "restore_applied" {
					provider.entry.Enabled = true
					provider.entry.ActivationEnabled = true
					provider.entry.ActivationFingerprint = "activation"
				}
				body := deAdoptHubConfigWithToolTimeout(target, 30)
				if alias {
					body += deAdoptNativeConfig(rec.SourceEntryName, "native-command")
				}
				adapter := clients.AllClients()["codex-cli"]
				if err := os.WriteFile(adapter.ConfigPath(), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				plan, err := NewAPI().BuildDeAdoptPlan(name)
				if err != nil || plan.Routing != DeAdoptRoutingResume {
					t.Fatalf("retry plan=%+v err=%v", plan, err)
				}
				report, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
				if err != nil {
					t.Fatalf("retry did not repair the recreated managed binding: %v", err)
				}
				if report == nil || len(report.Failed) != 0 || len(report.Restored) != 1 {
					t.Fatalf("retry report=%+v, want one restored client", report)
				}
				entry, err := adapter.GetEntry(target)
				if err != nil || entry != nil {
					t.Fatalf("recreated hub target remains: entry=%+v err=%v", entry, err)
				}
				if alias {
					source, err := adapter.GetEntry(rec.SourceEntryName)
					if err != nil || source == nil || source.URL != "" || !strings.Contains(string(mustReadFileForAdoptTest(t, adapter.ConfigPath())), "native-command") {
						t.Fatalf("retry changed unrelated native source: entry=%+v err=%v", source, err)
					}
				}
				if !provider.entry.Enabled {
					t.Fatal("provider remains disabled after retry")
				}
				assertDeAdoptClosed(t, dir, stateDir, *rec)
			})
		}
	}
}

func TestProviderManagedRecoveryRetryRechecksManifestBeforeSecretCleanup(t *testing.T) {
	for _, phase := range []string{"managed_removed", "restore_applied"} {
		for _, changed := range []bool{false, true} {
			label := phase + "/exact"
			if changed {
				label = phase + "/changed"
			}
			t.Run(label, func(t *testing.T) {
				const name = "provider-retry-recreated-manifest"
				dir, stateDir, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
				rec.ProviderSource.DeAdoptPhase = phase
				rec.RoutedSecretKeys = []string{"retry-secret"}
				writeDeAdoptExecutorRecord(t, *rec)
				if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
					t.Fatal(err)
				}
				seedDeAdoptVault(t, map[string]string{"retry-secret": "value"})
				path := filepath.Join(dir, name, "manifest.yaml")
				if changed {
					body := append(mustReadFileForAdoptTest(t, path), []byte("\n# operator change\n")...)
					if err := os.WriteFile(path, body, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				plan, err := NewAPI().BuildDeAdoptPlan(name)
				if err != nil {
					t.Fatal(err)
				}
				_, err = NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
				if changed {
					if err == nil {
						t.Fatal("changed manifest accepted on retry")
					}
					if len(deAdoptVaultKeys(t)) != 1 {
						t.Fatal("failed retry deleted routed secrets")
					}
					if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
						t.Fatalf("failed retry lost provenance: found=%v err=%v", found, err)
					}
					return
				}
				if err != nil {
					t.Fatalf("exact recreation did not converge: %v", err)
				}
				if len(deAdoptVaultKeys(t)) != 0 {
					t.Fatal("successful cleanup retained unshared secret")
				}
				assertDeAdoptClosed(t, dir, stateDir, *rec)
			})
		}
	}
}

func TestProviderRestoreAppliedCleanupPreservesUnexpectedManagedOwner(t *testing.T) {
	const name = "provider-restored-owned-row"
	dir, stateDir, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(dir, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.ProviderSource.DeAdoptPhase = "restore_applied"
	rec.RoutedSecretKeys = []string{"retry-secret"}
	writeDeAdoptExecutorRecord(t, *rec)
	seedDeAdoptVault(t, map[string]string{"retry-secret": "value"})
	task := "\\mcp-local-hub-" + name + "-" + adoptDefaultDaemonName
	intent := &SupervisorIntentFile{Version: 1, Daemons: []SupervisorDaemon{{TaskName: task, Server: name, Daemon: adoptDefaultDaemonName, Port: rec.Port, ManifestHash: rec.ExpectedManifestHash}}}
	if err := WriteSupervisorIntent(filepath.Join(stateDir, supervisorIntentFileLeaf), intent); err != nil {
		t.Fatal(err)
	}
	plan, err := NewAPI().BuildDeAdoptPlan(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAPI().executeDeAdoptPlanWithOpts(plan, io.Discard, ExecuteDeAdoptOpts{}); err == nil {
		t.Fatal("cleanup trusted stale restored phase despite current managed owner")
	}
	if len(deAdoptVaultKeys(t)) != 1 {
		t.Fatal("unsafe cleanup removed routed secret")
	}
	if _, found, err := ReadAdoptProvenance(name); err != nil || !found {
		t.Fatalf("unsafe cleanup removed provenance: found=%v err=%v", found, err)
	}
}
