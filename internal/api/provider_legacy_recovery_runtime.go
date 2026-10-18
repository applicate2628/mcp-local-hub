package api

import "fmt"

func providerRecoveryOwnershipAbsent(rec *AdoptProvenanceRecord) (bool, error) {
	return providerRecoveryOwnershipAbsentWithIntentLoader(rec, loadSupervisorOwnedIntent)
}

func providerRecoveryOwnershipAbsentWithIntentLoader(rec *AdoptProvenanceRecord, loadIntent func() (*SupervisorIntentFile, error)) (bool, error) {
	intent, err := loadIntent()
	if err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: load supervisor intent: %w", err)
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
	root, rootErr := adoptCommittedManifestDir()
	if rootErr != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: resolve manifest root: %w", rootErr)
	}
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

// claimProviderUnmanagedRecovery accepts never-started or positively settled
// unpublished rollback history. It takes authority only from durable
// history. Before writing recovery_claimed it proves that no managed owner exists
// and that any already-created manifest is the exact adopt-owned content. After
// the claim it re-proves ownership, then hash-deletes that exact manifest. A
// markerless legacy receipt remains UNKNOWN and is never promoted from current
// absence.
func claimProviderUnmanagedRecovery(rec *AdoptProvenanceRecord) (bool, error) {
	return claimProviderUnmanagedRecoveryWithIntentLoader(rec, loadSupervisorOwnedIntent)
}

func claimProviderUnmanagedRecoveryWithIntentLoader(rec *AdoptProvenanceRecord, loadIntent func() (*SupervisorIntentFile, error)) (bool, error) {
	if rec == nil || rec.ProviderSource == nil {
		return false, fmt.Errorf("E_PROVIDER_SOURCE_CHANGED")
	}
	absent, err := providerRecoveryOwnershipAbsentWithIntentLoader(rec, loadIntent)
	if err != nil {
		return false, err
	}
	if !absent {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: preinstall_owner_present")
	}
	if _, err := providerRecoveryManifestExact(rec); err != nil {
		return false, err
	}
	phase, phaseErr := readProviderInstallPhase(rec.ManifestName)
	if phaseErr != nil {
		if rec.ProviderSource.ManagedInstallState != ProviderManagedInstallPreInstall ||
			!providerInstallPhaseMissing(rec.ManifestName) {
			return false, phaseErr
		}
		phase = providerInstallPhaseNotStarted
	}
	if phase != providerInstallPhaseManagedSettled {
		claimed, err := providerInstallNeverStarted(rec)
		if err != nil || !claimed {
			return claimed, err
		}
	}
	absent, err = providerRecoveryOwnershipAbsentWithIntentLoader(rec, loadIntent)
	if err != nil {
		return false, err
	}
	if !absent {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: preinstall_owner_present")
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
	root, rootErr := adoptCommittedManifestDir()
	if rootErr != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: resolve manifest root: %w", rootErr)
	}
	if err := NewAPI().manifestDeleteInWithHashUnderLease(root, rec.ManifestName, rec.ExpectedManifestHash); err != nil {
		return false, fmt.Errorf("E_PROVIDER_LIFECYCLE_UNSUPPORTED: provider recovery manifest delete: %w", err)
	}
	if deAdoptAfterManifestDeleteHook != nil {
		deAdoptAfterManifestDeleteHook()
	}
	return true, nil
}
