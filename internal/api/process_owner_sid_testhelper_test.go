package api

import "testing"

// pinOwnerSIDMatch pins the SEC-F3 owner-SID arm
// (processOwnerSIDMatchesCurrentFn) to a same-user pass for the test scope,
// restoring the production default on cleanup. Stop-force tests that drive a
// SUCCESSFUL kill through requireMcphubPIDImage / requireMcphubPortOwnerPID use
// fake PIDs; without this pin the Windows production seam would try to open a
// real token for the fake PID, fail, and (correctly, fail-closed) refuse the
// kill — which is the SID arm's own behavior, exercised in the dedicated
// owner-SID tests, not what these image/port-path tests assert.
func pinOwnerSIDMatch(t *testing.T) {
	t.Helper()
	orig := processOwnerSIDMatchesCurrentFn
	t.Cleanup(func() { processOwnerSIDMatchesCurrentFn = orig })
	processOwnerSIDMatchesCurrentFn = func(int) (bool, error) { return true, nil }
}
