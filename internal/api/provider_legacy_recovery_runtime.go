package api

// claimProviderInstallNeverStarted is the production entry to the destructive
// never-started claim. Recovery authority comes only from the durable provider
// install marker. A markerless legacy receipt is historical state UNKNOWN and
// cannot be promoted from present-day runtime, listener, or intent absence.
func claimProviderInstallNeverStarted(rec *AdoptProvenanceRecord) (bool, error) {
	return providerInstallNeverStarted(rec)
}
