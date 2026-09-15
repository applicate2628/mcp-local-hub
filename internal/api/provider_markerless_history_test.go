package api

import (
	"os"
	"testing"
)

func TestProviderInstallNeverStartedRejectsMarkerlessHistory(t *testing.T) {
	name := "provider-markerless-history-unknown"
	_, _, rec, _ := setupProviderPreInstallRecoveryFixture(t, name, AdoptOperationStateAdopting)
	marker, err := providerInstallPhasePath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatalf("remove provider install phase marker: %v", err)
	}

	claimed, err := providerInstallNeverStarted(rec)
	if err == nil || claimed {
		t.Fatalf("markerless history was converted into recovery authority: claimed=%t err=%v", claimed, err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("failed markerless claim materialized recovery state: %v", statErr)
	}
}
