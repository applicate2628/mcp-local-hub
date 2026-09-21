package api

import (
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// providerRestoreBindingsClear rechecks the physical client keys restored by E3
// before provider activation. A reinstall admitted before E2 may recreate one of
// those bindings after E3. The source name alone is insufficient after an alias
// relocation; use the same provenance lookup and binding matcher as E3 instead
// of the adopting-row garbage-collection classifier.
func providerRestoreBindingsClear(manifestName string) bool {
	current, found, err := ReadAdoptProvenance(manifestName)
	if err != nil || !found || current == nil || current.ProviderSource == nil {
		return false
	}
	if current.ProviderSource.DeAdoptPhase != "managed_removed" {
		return true
	}
	if exists, err := adoptManifestExistsFn(current.ManifestName); err != nil || exists {
		return false
	}
	// A claimed pre-Install recovery never mutated client bindings. Requiring
	// a client probe here would undo the recovery lane's independence from
	// unavailable adapters. Manifest absence is required for both paths.
	// Real managed settlements still recheck E3 below.
	phase, phaseErr := readProviderInstallPhase(manifestName)
	if phaseErr == nil && phase == providerInstallPhaseRecoveryClaimed {
		return true
	}
	all := clients.AllClients()
	for _, clientName := range current.AdoptClients {
		clientRec, ok := deAdoptClientRecord(current, clientName)
		adapter := all[clientName]
		if !ok || adapter == nil {
			return false
		}
		live, err := adapter.GetEntry(adoptClientTargetEntryName(*current, clientRec))
		if err != nil {
			return false
		}
		binding := config.ClientBinding{
			Client: clientName, Daemon: adoptDefaultDaemonName,
			URLPath: adoptDefaultURLPath, ToolTimeoutSec: clientRec.ToolTimeoutSec,
		}
		if live != nil && deAdoptLiveBindingMatcher(current, binding)(live) {
			return false
		}
	}
	return true
}
