package api

// ProviderRecoveryExecutable reports whether this plan is an explicit durable
// pre-Install provider recovery which may be applied without ordinary client
// restoration. An absent manifest is valid for the pre-ManifestCreate crash
// lane; when a manifest is present it must still pass the exact hash gate.
func (p *DeAdoptPlan) ProviderRecoveryExecutable() bool {
	if p == nil || !p.providerRecovery || p.Routing == DeAdoptRoutingRefuse || p.RefusalReason != "" {
		return false
	}
	return !p.Manifest.Present || p.Manifest.HashReady
}
