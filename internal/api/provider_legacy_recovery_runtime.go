package api

import "fmt"

func providerRecoveryOwnershipAbsent(rec *AdoptProvenanceRecord) (bool, error) {
	intent, err := loadSupervisorOwnedIntent()
	if err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	scope, err := providerAdoptOwnershipScope(rec)
	if err != nil {
		return false, err
	}
	if intent == nil {
		return true, nil
	}
	for _, daemon := range intent.Daemons {
		if supervisorIntentRowOwnedByScope(daemon, rec.ManifestName, scope) {
			return false, nil
		}
	}
	return true, nil
}

func providerRecoveryManifestExact(rec *AdoptProvenanceRecord) (bool, error) {
	root := adoptCommittedManifestDir()
	exists, err := manifestExistsIn(root, rec.ManifestName)
	if err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if !exists {
		return false, nil
	}
	if rec.ExpectedManifestHash == "" {
		return true, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	_, actualHash, err := NewAPI().ManifestGetInWithHash(root, rec.ManifestName)
	if err != nil || actualHash != rec.ExpectedManifestHash {
		return true, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	return true, nil
}

// claimProviderInstallNeverStarted takes recovery authority only from durable
// history. Before writing recovery_claimed it proves that no managed owner exists
// and that any already-created manifest is the exact adopt-owned content. After
// the claim it re-proves ownership, then hash-deletes that exact manifest. A
// markerless legacy receipt remains UNKNOWN and is never promoted from current
// absence.
func claimProviderInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	if rec == nil || rec.ProviderSource == nil {
		return false, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	absent, err := providerRecoveryOwnershipAbsent(rec)
	if err != nil || !absent {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	if _, err := providerRecoveryManifestExact(rec); err != nil {
		return false, err
	}
	claimed, err := providerInstallNeverStarted(rec)
	if err != nil || !claimed {
		return claimed, err
	}
	absent, err = providerRecoveryOwnershipAbsent(rec)
	if err != nil || !absent {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED")
	}
	exists, err := providerRecoveryManifestExact(rec)
	if err != nil {
		return false, err
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
