//go:build !windows || !serena_qualification || !test_state_path_env

package cli

import "testing"

// These positive tests require the real prepared-package and intent pipeline.
// A normal test binary must report the missing prerequisite instead of skipping
// the integration gate or substituting synthetic publication authority.
func migrateSerenaPreparedTestEnv(t *testing.T) (stateDir, manifestDir string) {
	t.Helper()
	t.Fatal("SERENA_QUALIFICATION_REQUIRED: positive Serena installation requires the source-verified Windows serena_qualification,test_state_path_env test binary, a private qualification run root, and an admitted runtime Product")
	return "", ""
}
