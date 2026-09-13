package api

// providerPreManifestRecovery is the read-only routing predicate shared by the
// planner and lease-held executor. Despite the historical name, this is a
// pre-Install predicate, not a pre-ManifestCreate predicate: a crash may occur
// after the exact adopt manifest was created but before Install crossed its
// mutation barrier. The durable provider-install marker is the historical proof;
// current manifest presence or absence is not. Markerless legacy receipts remain
// excluded because present-day absence cannot prove that Install was never entered.
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
	return providerInstallPhaseIsNotStarted(rec.ManifestName)
}
