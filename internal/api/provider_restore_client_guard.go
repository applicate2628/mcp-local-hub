package api

// providerRestoreBindingsClear is the final client-side race gate. It is only
// load-bearing once E4 has reached managed_removed. A reinstall admitted before
// E2 may otherwise rewrite a hub binding after E3 restored clients; supervisor
// settlement alone would then remove its daemon while leaving that client entry
// pointing at the deleted manifest. classifyDeadAdoptingRow is reused because it
// already fails closed on unreadable adapters and recognizes the exact recorded
// hub binding shape.
func providerRestoreBindingsClear(manifestName string) bool {
	current, found, err := ReadAdoptProvenance(manifestName)
	if err != nil || !found || current == nil || current.ProviderSource == nil {
		return false
	}
	if current.ProviderSource.DeAdoptPhase != "managed_removed" {
		return true
	}
	return classifyDeadAdoptingRow(*current) != adoptRowCommittedKeep
}
