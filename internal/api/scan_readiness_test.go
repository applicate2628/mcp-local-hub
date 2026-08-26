package api

import "testing"

// TestApplyReadinessToScanEntries_PreservesManifestEligibilityForDirectBinding
// guards the scan-to-reducer seam: ScanEntry.CanMigrate is the manifest owner's
// applicability result and must reach the sole readiness classifier unchanged.
func TestApplyReadinessToScanEntries_PreservesManifestEligibilityForDirectBinding(t *testing.T) {
	entries := []ScanEntry{{
		Name:       "fixture-server",
		CanMigrate: true,
		ClientPresence: map[string]ClientEntry{
			"codex-cli": {Transport: "stdio"},
		},
	}}

	if err := NewAPI().applyReadinessToScanEntries(entries, nil); err != nil {
		t.Fatalf("apply readiness: %v", err)
	}

	if got := entries[0].Classification; got != ReadinessClassificationConfiguredDirectCanMigrate {
		t.Fatalf("classification = %q, want %q", got, ReadinessClassificationConfiguredDirectCanMigrate)
	}
	if got := entries[0].MaterializationState; got != MaterializationAbsent {
		t.Fatalf("materialization state = %q, want %q", got, MaterializationAbsent)
	}
}
