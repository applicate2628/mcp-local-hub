package api

import "testing"

func TestProviderRestoreBindingsClearRequiresNoCommittedHubBinding(t *testing.T) {
	// Detailed reinstall-race setup lives in provider_restore_client_race_test.go.
	// This focused unit keeps the helper itself in the provider restore contract.
	if providerRestoreBindingsClear("") {
		t.Fatal("empty manifest identity unexpectedly passed provider restore binding guard")
	}
}
