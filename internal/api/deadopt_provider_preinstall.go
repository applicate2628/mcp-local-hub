package api

import (
	"fmt"
	"time"
)

// settleProviderPreInstallManagedStop persists the fact that the managed-daemon
// stop obligation was already satisfied by a positive proof that Install never
// created a supervisor ownership row. The caller holds the per-manifest lease
// and must obtain that proof immediately before calling this helper.
//
// Persisting the phase while the record is still adopting closes the crash
// window between entering de_adopting and E4: a retry no longer has to infer a
// historical pre-Install fact from the current operation state.
func settleProviderPreInstallManagedStop(manifestName string, expected *AdoptProvenanceRecord) (*AdoptProvenanceRecord, error) {
	if expected == nil || expected.ProviderSource == nil {
		return nil, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	var updated *AdoptProvenanceRecord
	err := withAdoptedEntriesLock(func() error {
		store, err := readAdoptedEntries()
		if err != nil {
			return fmt.Errorf("provider pre-install settlement: read store: %w", err)
		}
		for i := range store.Records {
			record := &store.Records[i]
			if record.ManifestName != manifestName {
				continue
			}
			if !deAdoptProvenanceIdentityMatches(expected, record) ||
				record.OperationState != AdoptOperationStateAdopting ||
				record.ProviderSource == nil ||
				*record.ProviderSource != *expected.ProviderSource {
				return fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
			}
			if record.ProviderSource.DeAdoptPhase != "" {
				if record.ProviderSource.DeAdoptPhase != "managed_stop_settled" {
					return fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
				}
				copyRecord := *record
				copyProvider := *record.ProviderSource
				copyRecord.ProviderSource = &copyProvider
				updated = &copyRecord
				return nil
			}
			record.ProviderSource.DeAdoptPhase = "managed_stop_settled"
			record.UpdatedAt = time.Now().UTC()
			if err := writeAdoptedEntries(store); err != nil {
				return fmt.Errorf("provider pre-install settlement: write store: %w", err)
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
