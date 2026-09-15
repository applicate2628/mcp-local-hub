package api

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// The row deletion may commit before final phase-file removal is observed.
// Never convert that residue into new never-started evidence automatically.
func TestProviderCleanupCommittedRowDeleteExplainsExplicitResume(t *testing.T) {
	for _, initialPhase := range []string{providerInstallPhaseNotStarted, providerInstallPhaseManagedSettled} {
		t.Run(initialPhase, func(t *testing.T) {
			name := "provider-cleanup-resume"
			_, _, rec, provider := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
			if initialPhase != providerInstallPhaseNotStarted {
				if err := writeProviderInstallPhase(name, initialPhase); err != nil {
					t.Fatal(err)
				}
			}
			originalWriter := writeAdoptedEntriesFn
			t.Cleanup(func() { writeAdoptedEntriesFn = originalWriter })
			interrupted := errors.New("injected close failure after durable row deletion")
			writeAdoptedEntriesFn = func(store *AdoptedEntries) error {
				if err := originalWriter(store); err != nil {
					return err
				}
				return interrupted
			}
			a := NewAPI()
			_, err := a.ExecuteDeAdoptWithOpts(name, io.Discard, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider}})
			writeAdoptedEntriesFn = originalWriter
			if !errors.Is(err, interrupted) {
				t.Fatalf("close error = %v, want interrupted cleanup", err)
			}
			if !provider.entry.Enabled || provider.calls != 1 {
				t.Fatalf("provider not restored before closure: enabled=%v calls=%d", provider.entry.Enabled, provider.calls)
			}
			if _, found, err := ReadAdoptProvenance(name); err != nil || found {
				t.Fatalf("row deletion did not persist: found=%v err=%v", found, err)
			}
			markerPath, err := providerInstallPhasePath(name)
			if err != nil {
				t.Fatal(err)
			}
			marker, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			source := cloneProviderSourceProvenance(rec.ProviderSource)
			source.DisablePhase = "disable_planned"
			source.DeAdoptPhase = ""
			next := &AdoptPlan{ManifestName: name, EntryName: rec.SourceEntryName, SourceClient: rec.SourceClient, Port: rec.Port, ManifestYAML: "name: " + name + "\n", providerSource: source}
			capture := func() error {
				lease, acquired, err := tryAcquireAdoptManifestLease(name)
				if err != nil || !acquired {
					t.Fatalf("capture lease: acquired=%v err=%v", acquired, err)
				}
				_, captureErr := a.captureAdoptProvenance(next)
				return errors.Join(captureErr, lease.Unlock())
			}
			err = capture()
			if err == nil {
				t.Fatal("new capture silently discarded retained provider history")
			}
			if !strings.Contains(err.Error(), "mcphub adopt-provenance forget "+name) || !strings.Contains(err.Error(), "dry-run") || strings.Contains(err.Error(), "--yes") {
				t.Errorf("capture refusal lacks safe inspection action: %v", err)
			}
			if got, readErr := os.ReadFile(markerPath); readErr != nil || !bytes.Equal(got, marker) {
				t.Fatalf("refusal changed recovery history: %v", readErr)
			}
			reviewed, err := a.BuildForgetAdoptProvenancePlan(name)
			if err != nil || reviewed.HasRow || !reviewed.HasSnapshotDir {
				t.Fatalf("rowless cleanup plan=%+v err=%v", reviewed, err)
			}
			if _, err := a.ForgetAdoptProvenance(name, ForgetAdoptProvenanceOpts{Yes: true, ConfirmIdentity: true, ExpectedHasRow: false}); err != nil {
				t.Fatalf("explicit reviewed cleanup: %v", err)
			}
			if err := capture(); err != nil {
				t.Fatalf("capture after explicit cleanup: %v", err)
			}
			if phase, err := readProviderInstallPhase(name); err != nil || phase != providerInstallPhaseNotStarted {
				t.Fatalf("new capture phase=%q err=%v", phase, err)
			}
			if provider.calls != 1 || !provider.entry.Enabled {
				t.Fatal("metadata inspection/cleanup changed provider activation")
			}
		})
	}
}
