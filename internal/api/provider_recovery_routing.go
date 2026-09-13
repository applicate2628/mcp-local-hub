package api

// providerPreManifestRecovery is the read-only routing predicate shared by the
// planner and lease-held executor. An E2 crash changes the operation state but
// not the durable never-started proof, so adopting and de_adopting recover the
// same lane. Markerless legacy receipts are deliberately excluded: present-day
// absence cannot prove that Install was never entered.
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
	exists, err := adoptManifestExistsFn(rec.ManifestName)
	if err != nil || exists {
		return false
	}
	return providerInstallPhaseIsNotStarted(rec.ManifestName)
}
