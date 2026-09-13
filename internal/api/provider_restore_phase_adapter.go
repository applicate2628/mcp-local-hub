package api

func providerRestorePhaseAndBindingsAllowed(manifestName, phase string) bool {
	if phase != providerInstallPhaseRecoveryClaimed && phase != providerInstallPhaseManagedSettled {
		return false
	}
	return providerRestoreBindingsClear(manifestName)
}
