package api

import (
	"fmt"
	"time"
)

// advanceProviderPreInstallManagedRemoved closes the provider managed-side
// obligation only after the exact adopt manifest is already absent and the
// caller has positively re-proved that no supervisor-owned daemon exists.
//
// Unlike an ordinary provider de-adopt, the pre-Install crash lane never had a
// managed daemon to stop or a supervisor descriptor to remove. Persisting
// managed_stop_settled before E4 loses that distinction and creates a race where
// a late regular Install row can be deleted without ever being stopped. This
// helper therefore performs the single durable empty -> managed_removed jump
// only after E4's manifest deletion/absence proof. A crash before this write
// leaves an empty phase; retry re-proves the same facts instead of trusting
// historical absence.
func advanceProviderPreInstallManagedRemoved(manifestName string, expected *AdoptProvenanceRecord) (*AdoptProvenanceRecord, error) {
	if expected == nil || expected.ProviderSource == nil || expected.ProviderSource.DeAdoptPhase != "" {
		return nil, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	if expected.OperationState != AdoptOperationStateAdopting &&
		expected.OperationState != AdoptOperationStateDeAdopting {
		return nil, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	var updated *AdoptProvenanceRecord
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("provider pre-install removal: read store: %w", err)
		}
		for i := range store.Records {
			record := &store.Records[i]
			if record.ManifestName != manifestName {
				continue
			}
			if !deAdoptProvenanceIdentityMatches(expected, record) ||
				record.OperationState != AdoptOperationStateDeAdopting ||
				record.ProviderSource == nil ||
				*record.ProviderSource != *expected.ProviderSource ||
				record.ProviderSource.DeAdoptPhase != "" {
				return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
			}
			record.ProviderSource.DeAdoptPhase = "managed_removed"
			record.UpdatedAt = time.Now().UTC()
			if err := writeAdoptedEntries(store); err != nil {
				return fmt.Errorf("provider pre-install removal: write store: %w", err)
			}
			copyRecord := *record
			copyProvider := *record.ProviderSource
			copyRecord.ProviderSource = &copyProvider
			updated = &copyRecord
			return nil
		}
		return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}
