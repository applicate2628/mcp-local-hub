package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"
)

type providerInventoryLockReviewSource struct {
	*providerLifecycleFake
	list func(context.Context) ([]clients.ProviderMCPEntryV1, error)
}

func (s *providerInventoryLockReviewSource) ListProviderMCPEntries(ctx context.Context) ([]clients.ProviderMCPEntryV1, error) {
	return s.list(ctx)
}

func setupProviderInventoryLockReview(t *testing.T) (string, *AdoptProvenanceRecord, *providerLifecycleFake) {
	t.Helper()
	name := "provider-inventory-lock-review"
	manifestRoot, stateRoot, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateDeAdopting)
	if err := os.Remove(filepath.Join(manifestRoot, name, "manifest.yaml")); err != nil {
		t.Fatal(err)
	}
	rec.ProviderSource.DeAdoptPhase = "managed_removed"
	writeDeAdoptExecutorRecord(t, *rec)
	if err := writeProviderInstallPhase(name, providerInstallPhaseManagedSettled); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := tryAcquireAdoptManifestLease(name)
	if err != nil || !acquired {
		t.Fatalf("acquire fixture manifest lease: acquired=%t err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Unlock(); err != nil {
			t.Errorf("release fixture manifest lease: %v", err)
		}
	})
	return filepath.Join(stateRoot, supervisorIntentFileLeaf), rec, provider
}

func TestProviderInventoryLockReviewUnrelatedIntentProgress(t *testing.T) {
	for _, restored := range []bool{false, true} {
		label := "disabled"
		if restored {
			label = "already-restored"
		}
		t.Run(label, func(t *testing.T) {
			intentPath, rec, provider := setupProviderInventoryLockReview(t)
			if restored {
				provider.entry.Enabled = true
				provider.entry.ActivationEnabled = true
				provider.entry.ActivationFingerprint = rec.ProviderSource.ActivationFingerprint
			}
			entered, resume := make(chan struct{}), make(chan struct{})
			var resumeOnce sync.Once
			unblock := func() { resumeOnce.Do(func() { close(resume) }) }
			source := &providerInventoryLockReviewSource{providerLifecycleFake: provider}
			source.list = func(ctx context.Context) ([]clients.ProviderMCPEntryV1, error) {
				entry := provider.entry
				close(entered)
				select {
				case <-resume:
					return []clients.ProviderMCPEntryV1{entry}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			recoveryDone := make(chan struct{})
			var recoveryErr error
			go func() {
				defer close(recoveryDone)
				recoveryErr = recoverProviderActivation(ctx, source, rec.ProviderSource)
			}()
			t.Cleanup(func() {
				unblock()
				select {
				case <-recoveryDone:
				case <-time.After(12 * time.Second):
					t.Error("recovery fixture did not terminate")
				}
			})
			select {
			case <-entered:
			case <-recoveryDone:
				t.Fatalf("recovery ended before inventory: %v", recoveryErr)
			case <-ctx.Done():
				t.Fatal("recovery never reached inventory")
			}

			mutationDone := make(chan struct{})
			var mutationErr error
			go func() {
				defer close(mutationDone)
				mutationErr = MutateSupervisorIntentIfChanged(intentPath, func(intent *SupervisorIntentFile) (bool, error) {
					intent.StrictMode = true
					return true, nil
				})
			}()
			t.Cleanup(func() {
				unblock()
				select {
				case <-mutationDone:
				case <-time.After(12 * time.Second):
					t.Error("intent mutation fixture did not terminate")
				}
			})
			select {
			case <-mutationDone:
				if mutationErr != nil {
					t.Fatalf("unrelated intent mutation: %v", mutationErr)
				}
				t.Log("unrelated intent mutation completed while provider inventory remained suspended")
			case <-time.After(time.Second):
				t.Error("unrelated supervisor intent mutation blocked by suspended provider inventory")
			}
			unblock()
			select {
			case <-recoveryDone:
				if recoveryErr != nil {
					t.Fatalf("recovery after inventory resumed: %v", recoveryErr)
				}
			case <-ctx.Done():
				t.Fatal("recovery did not finish after inventory resumed")
			}
			select {
			case <-mutationDone:
				if mutationErr != nil {
					t.Fatal(mutationErr)
				}
			case <-ctx.Done():
				t.Fatal("intent mutation did not finish after inventory resumed")
			}
			intent, err := ReadSupervisorIntent(intentPath)
			if err != nil || !intent.StrictMode {
				t.Fatalf("unrelated mutation was not persisted: intent=%+v err=%v", intent, err)
			}
			wantCAS := 1
			if provider.calls != wantCAS || !provider.entry.Enabled {
				t.Fatalf("restore calls=%d enabled=%t, want calls=%d enabled=true", provider.calls, provider.entry.Enabled, wantCAS)
			}
		})
	}
}

func TestProviderInventoryLockReviewRechecksFinalGuards(t *testing.T) {
	for _, restored := range []bool{false, true} {
		label := "disabled"
		if restored {
			label = "already-restored"
		}
		for _, change := range []string{"missing-provenance", "provenance-phase", "managed-owner", "install-started", "client-binding", "manifest"} {
			t.Run(label+"/"+change, func(t *testing.T) {
				intentPath, rec, provider := setupProviderInventoryLockReview(t)
				if restored {
					provider.entry.Enabled = true
					provider.entry.ActivationEnabled = true
					provider.entry.ActivationFingerprint = rec.ProviderSource.ActivationFingerprint
				}
				// Keep the caller's frozen provenance independent of injected disk drift.
				provenance := *rec.ProviderSource
				source := &providerInventoryLockReviewSource{providerLifecycleFake: provider}
				source.list = func(context.Context) ([]clients.ProviderMCPEntryV1, error) {
					entry := provider.entry
					switch change {
					case "missing-provenance":
						if err := writeAdoptedEntries(&AdoptedEntries{Version: adoptedEntriesSchemaVersion}); err != nil {
							t.Fatal(err)
						}
					case "provenance-phase":
						rec.ProviderSource.DeAdoptPhase = "managed_stop_settled"
						writeDeAdoptExecutorRecord(t, *rec)
					case "managed-owner":
						if err := MutateSupervisorIntentIfChanged(intentPath, func(intent *SupervisorIntentFile) (bool, error) {
							intent.Daemons = append(intent.Daemons, SupervisorDaemon{
								TaskName: "\\mcp-local-hub-" + rec.ManifestName + "-" + adoptDefaultDaemonName,
								Server:   rec.ManifestName, Daemon: adoptDefaultDaemonName,
								Port: rec.Port, ManifestHash: rec.ExpectedManifestHash,
							})
							return true, nil
						}); err != nil {
							t.Fatal(err)
						}
					case "install-started":
						if err := writeProviderInstallPhase(rec.ManifestName, providerInstallPhaseStarted); err != nil {
							t.Fatal(err)
						}
					case "client-binding":
						adapter := clients.AllClients()["codex-cli"]
						if adapter == nil {
							t.Fatal("fixture codex-cli adapter unavailable")
						}
						if err := os.WriteFile(adapter.ConfigPath(), []byte(deAdoptHubConfigWithToolTimeout(rec.SourceEntryName, 0)), 0o600); err != nil {
							t.Fatal(err)
						}
					case "manifest":
						path := filepath.Join(adoptCommittedManifestDir(), rec.ManifestName, "manifest.yaml")
						if err := os.WriteFile(path, deAdoptManagedManifest(rec.ManifestName, 0), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					return []clients.ProviderMCPEntryV1{entry}, nil
				}
				wantError := "E_PROVIDER_LIFECYCLE_UNSUPPORTED"
				if change == "missing-provenance" || change == "provenance-phase" {
					wantError = "E_PROVIDER_SOURCE_CHANGED"
				}
				err := recoverProviderActivation(context.Background(), source, &provenance)
				if err == nil || !strings.Contains(err.Error(), wantError) {
					t.Fatalf("recovery after %s: %v, want %s", change, err, wantError)
				}
				if provider.calls != 0 || provider.entry.Enabled != restored {
					t.Fatalf("final guard changed activation: calls=%d enabled=%t", provider.calls, provider.entry.Enabled)
				}
				if change == "managed-owner" {
					intent, err := ReadSupervisorIntent(intentPath)
					if err != nil || len(intent.Daemons) != 1 || intent.Daemons[0].Server != rec.ManifestName {
						t.Fatalf("late owner not preserved: intent=%+v err=%v", intent, err)
					}
				}
			})
		}
	}
}

func TestProviderInventoryLockReviewAlreadyRestoredCASRejectsDrift(t *testing.T) {
	intentPath, rec, provider := setupProviderInventoryLockReview(t)
	provider.entry.Enabled = true
	provider.entry.ActivationEnabled = true
	provider.entry.ActivationFingerprint = rec.ProviderSource.ActivationFingerprint

	source := &providerInventoryLockReviewSource{providerLifecycleFake: provider}
	source.list = func(context.Context) ([]clients.ProviderMCPEntryV1, error) {
		entry := provider.entry
		provider.entry.ActivationFingerprint = "concurrent-restored-activation"
		return []clients.ProviderMCPEntryV1{entry}, nil
	}
	provider.onCAS = func() {
		release, acquired, err := tryLockSupervisorIntent(intentPath)
		if err != nil {
			t.Errorf("probe restored CAS lock: %v", err)
		}
		if acquired {
			if err := release(); err != nil {
				t.Errorf("release unexpected restored CAS probe lock: %v", err)
			}
			t.Error("already-restored activation fence ran without the supervisor-intent lock")
		}
	}

	err := recoverProviderActivation(context.Background(), source, rec.ProviderSource)
	if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_RECOVERY_REQUIRED") {
		t.Fatalf("stale restored inventory accepted concurrent drift: err=%v", err)
	}
	if provider.calls != 1 || provider.entry.ActivationFingerprint != "concurrent-restored-activation" {
		t.Fatalf("restored fence calls=%d entry=%+v", provider.calls, provider.entry)
	}
}

func TestProviderInventoryLockReviewCASStaysLockedAndRejectsDrift(t *testing.T) {
	for _, drift := range []string{"none", "activation", "policy"} {
		t.Run(drift, func(t *testing.T) {
			intentPath, rec, provider := setupProviderInventoryLockReview(t)
			source := &providerInventoryLockReviewSource{providerLifecycleFake: provider}
			source.list = func(context.Context) ([]clients.ProviderMCPEntryV1, error) {
				entry := provider.entry
				if drift == "activation" {
					provider.entry.ActivationFingerprint = "concurrent-activation"
				}
				if drift == "policy" {
					provider.entry.PolicyFingerprint = "concurrent-policy"
				}
				return []clients.ProviderMCPEntryV1{entry}, nil
			}
			provider.onCAS = func() {
				release, acquired, err := tryLockSupervisorIntent(intentPath)
				if err != nil {
					t.Errorf("probe CAS lock: %v", err)
				}
				if acquired {
					if err := release(); err != nil {
						t.Errorf("release unexpected CAS probe lock: %v", err)
					}
					t.Error("activation CAS ran without the supervisor-intent lock")
				}
			}
			err := recoverProviderActivation(context.Background(), source, rec.ProviderSource)
			if drift == "none" {
				if err != nil || !provider.entry.Enabled {
					t.Fatalf("restore: enabled=%t err=%v", provider.entry.Enabled, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "E_PROVIDER_RECOVERY_REQUIRED") || provider.entry.Enabled {
				t.Fatalf("stale inventory overwrote %s drift: enabled=%t err=%v", drift, provider.entry.Enabled, err)
			}
			if provider.calls != 1 {
				t.Fatalf("CAS calls=%d, want 1", provider.calls)
			}
		})
	}
}
