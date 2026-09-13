package api

import "fmt"

// claimProviderInstallNeverStarted is the production entry to destructive
// pre-Install recovery. Recovery authority comes only from the durable provider
// install marker. A markerless legacy receipt is historical state UNKNOWN and
// cannot be promoted from present-day runtime, listener, or intent absence.
//
// Once the claim wins, no future Install for this adoption can cross its mutation
// barrier. We can therefore prove supervisor ownership absent and, when
// ManifestCreate had already completed, remove only the exact recorded manifest
// through the normal hash gate. A crash after that delete is retry-safe because
// recovery_claimed is already durable.
func claimProviderInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	claimed, err := providerInstallNeverStarted(rec)
	if err != nil || !claimed {
		return claimed, err
	}

	intent, err := loadSupervisorOwnedIntent()
	if err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	scope, err := providerAdoptOwnershipScope(rec)
	if err != nil {
		return false, err
	}
	if intent != nil {
		for _, daemon := range intent.Daemons {
			if supervisorIntentRowOwnedByScope(daemon, rec.ManifestName, scope) {
				return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
			}
		}
	}

	exists, err := manifestExistsIn(adoptCommittedManifestDir(), rec.ManifestName)
	if err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if !exists {
		return true, nil
	}
	if deAdoptBeforeManifestDeleteHook != nil {
		deAdoptBeforeManifestDeleteHook()
	}
	if err := NewAPI().ManifestDeleteInWithHash(adoptCommittedManifestDir(), rec.ManifestName, rec.ExpectedManifestHash); err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: provider recovery manifest delete: %w", err)
	}
	if deAdoptAfterManifestDeleteHook != nil {
		deAdoptAfterManifestDeleteHook()
	}
	return true, nil
}
