package api

// providerPreManifestRecovery routes never-entered Install and positively
// settled unpublished rollback to provider-only recovery. These are separate
// durable proofs; neither current absence nor missing legacy history is proof.
func providerPreManifestRecovery(rec *AdoptProvenanceRecord) bool {
	if rec == nil || rec.ProviderSource == nil || rec.ProviderSource.DeAdoptPhase != "" {
		return false
	}
	if rec.OperationState != AdoptOperationStateAdopting && rec.OperationState != AdoptOperationStateDeAdopting {
		return false
	}
	if rec.ProviderSource.DisablePhase != "disable_planned" && rec.ProviderSource.DisablePhase != "disable_applied" {
		return false
	}
	phase, err := readProviderInstallPhase(rec.ManifestName)
	if err != nil {
		return false
	}
	if phase == providerInstallPhaseManagedSettled {
		// Normal stop also writes this marker before its separate phase CAS.
		// A retained descriptor must stay on the managed teardown path.
		absent, absenceErr := providerRecoveryOwnershipAbsent(rec)
		return absenceErr == nil && absent
	}
	return phase == providerInstallPhaseNotStarted || phase == providerInstallPhaseRecoveryClaimed
}
