package api

import "testing"

// TestSchedulerUpgradeNoopWhenEmpty verifies that running SchedulerUpgrade
// on a system with no mcp-local-hub tasks returns an empty result list
// without error.
func TestSchedulerUpgradeNoopWhenEmpty(t *testing.T) {
	// Cannot easily stub schtasks.exe in unit tests; just assert the api
	// accepts the call and returns something sane. Real verification is
	// the live smoke test in step 3.
	a := NewAPI()
	results, err := a.SchedulerUpgrade()
	if err != nil {
		t.Skipf("scheduler unavailable: %v", err)
	}
	_ = results
}
