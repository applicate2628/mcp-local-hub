//go:build windows && test_state_path_env && w18_inline_baseline

package main

// This test-only build seam reconstructs the pre-W18 composition: an empty
// invocation falls through prepareInvocation and is routed to the inline Cobra
// GUI command. It lets the Windows process E2E run one identical acceptance
// oracle against the old blocking behavior before running the current binary.
func init() {
	launchBareGUIFn = func() (bool, error) { return false, nil }
}
