package api

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func seedProviderProvenanceCleanup(t *testing.T, state AdoptOperationState, phase string) (AdoptProvenanceRecord, string, []byte) {
	t.Helper()
	isolateStateDir(t)
	rec := sampleAdoptRecord()
	rec.ManifestName = "provider-cleanup"
	rec.OperationState = state
	rec.AdoptClients = nil
	rec.Clients = nil
	rec.ProviderSource = sampleProviderSource()
	dir := seedAdoptProvenanceMutatorRecord(t, rec)
	// Keep noncanonical whitespace to detect a cleanup that rewrites history.
	marker := []byte(fmt.Sprintf("{\n  \"version\": 1, \"phase\": %q\n}\n", phase))
	if err := WriteStateFileBytesAtomic(filepath.Join(dir, providerInstallPhaseLeaf), marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "partial-capture"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "partial-capture", "secret.tmp"), []byte("synthetic secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, acquired, err := tryAcquireAdoptManifestLease(rec.ManifestName)
	if err != nil || !acquired {
		t.Fatalf("acquire cleanup lease: acquired=%t err=%v", acquired, err)
	}
	t.Cleanup(func() {
		if err := lease.Unlock(); err != nil {
			t.Errorf("release cleanup lease: %v", err)
		}
	})
	return rec, dir, marker
}

func assertProviderCleanupSecretsGone(t *testing.T, dir string) {
	t.Helper()
	for _, leaf := range []string{"codex-cli.snapshot", "partial-capture"} {
		if _, err := os.Lstat(filepath.Join(dir, leaf)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("secret-bearing %s survived cleanup: %v", leaf, err)
		}
	}
}

func assertProviderCleanupMarker(t *testing.T, dir string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, providerInstallPhaseLeaf))
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("provider phase was removed or rewritten before row deletion committed: err=%v", err)
	}
}

func TestProviderProvenanceCleanupStoreWriteFailure(t *testing.T) {
	for _, operation := range []string{"abort", "reap", "close"} {
		for _, phase := range []string{providerInstallPhaseNotStarted, providerInstallPhaseStarted, providerInstallPhaseRecoveryClaimed, providerInstallPhaseManagedSettled} {
			t.Run(operation+"/"+phase, func(t *testing.T) {
				state := AdoptOperationStateAdopting
				if operation == "close" {
					state = AdoptOperationStateDeAdopting
				}
				rec, dir, marker := seedProviderProvenanceCleanup(t, state, phase)
				cleanup := func() error {
					switch operation {
					case "abort":
						// Capture failures pass only the name: provider identity must
						// come from the live store, not the caller's partial record.
						return abortAdoptProvenance(&AdoptProvenanceRecord{ManifestName: rec.ManifestName})
					case "reap":
						return reapAdoptProvenanceRow(rec.ManifestName, rec.OperationState, rec.UpdatedAt)
					default:
						return CloseAdoptProvenance(rec.ManifestName)
					}
				}
				before, found, err := ReadAdoptProvenance(rec.ManifestName)
				if err != nil || !found {
					t.Fatalf("read seeded row: found=%t err=%v", found, err)
				}
				originalWriter := writeAdoptedEntriesFn
				t.Cleanup(func() { writeAdoptedEntriesFn = originalWriter })
				writeFailure := errors.New("injected provenance store-write failure")
				calls := 0
				writeAdoptedEntriesFn = func(store *AdoptedEntries) error {
					calls++
					assertProviderCleanupSecretsGone(t, dir)
					assertProviderCleanupMarker(t, dir, marker)
					for _, candidate := range store.Records {
						if candidate.ManifestName == rec.ManifestName {
							t.Error("cleanup writer was not given the row deletion")
						}
					}
					return writeFailure
				}
				if err := cleanup(); !errors.Is(err, writeFailure) {
					t.Errorf("cleanup error=%v, want injected store-write failure", err)
				}
				if calls != 1 {
					t.Errorf("store-write attempts=%d, want 1", calls)
				}
				assertProviderCleanupSecretsGone(t, dir)
				assertProviderCleanupMarker(t, dir, marker)
				after, found, err := ReadAdoptProvenance(rec.ManifestName)
				if err != nil || !found || !reflect.DeepEqual(after, before) {
					t.Fatalf("failed cleanup must retain the unchanged row: found=%t err=%v", found, err)
				}
				if got, err := readProviderInstallPhase(rec.ManifestName); err != nil || got != phase {
					t.Errorf("retained recovery phase=%q err=%v, want %q", got, err, phase)
				}
				originalManifestExists := adoptManifestExistsFn
				t.Cleanup(func() { adoptManifestExistsFn = originalManifestExists })
				adoptManifestExistsFn = func(string) (bool, error) { return false, nil }
				if state == AdoptOperationStateAdopting && classifyDeadAdoptingRow(*after) != adoptRowRecoveryKeep {
					t.Error("failed provider abort must remain in explicit recovery")
				}
				writeAdoptedEntriesFn = originalWriter
				if err := cleanup(); err != nil {
					t.Fatalf("retry cleanup: %v", err)
				}
				if _, found, err := ReadAdoptProvenance(rec.ManifestName); err != nil || found {
					t.Errorf("row after retry: found=%t err=%v", found, err)
				}
				if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("snapshot/phase residue after retry: %v", err)
				}
				if err := cleanup(); err != nil {
					t.Errorf("idempotent cleanup: %v", err)
				}
			})
		}
	}
}

func TestProviderProvenanceCleanupDoesNotFollowChildSymlink(t *testing.T) {
	rec, dir, _ := seedProviderProvenanceCleanup(t, AdoptOperationStateAdopting, providerInstallPhaseNotStarted)
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.snapshot")
	if err := os.WriteFile(target, []byte("unrelated snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linked-snapshots")); err != nil {
		t.Skipf("directory symlink creation unavailable: %v", err)
	}
	if err := abortAdoptProvenance(&rec); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "unrelated snapshot" {
		t.Fatalf("cleanup traversed a child symlink: %v", err)
	}
}

func TestProviderProvenanceCleanupDoesNotFollowSnapshotDirSymlink(t *testing.T) {
	rec, dir, _ := seedProviderProvenanceCleanup(t, AdoptOperationStateAdopting, providerInstallPhaseNotStarted)
	// Preserve the real fixture and point its old name at unrelated data.
	if err := os.Rename(dir, dir+"-saved"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.snapshot")
	if err := os.WriteFile(target, []byte("unrelated snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Skipf("directory symlink creation unavailable: %v", err)
	}
	// Refusal or unlinking the link are both safe; traversing it is not.
	_ = abortAdoptProvenance(&rec)
	if got, err := os.ReadFile(target); err != nil || string(got) != "unrelated snapshot" {
		t.Fatalf("cleanup traversed the snapshot directory symlink: %v", err)
	}
}
