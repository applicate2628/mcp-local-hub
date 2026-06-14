package cli

import (
	"path/filepath"
	"testing"
)

// TestProductionStateDir_IgnoresEnvOverride guards the fix for bug
// 2026-06-03-cli-supervise-statedir-override-ungated: the PRODUCTION state-dir
// resolver (productionStateDir — what stateDirFunc ships as in a release binary)
// must NOT honor MCPHUB_STATE_DIR_OVERRIDE. That env is a test-only seam, so a
// shipped `mcphub supervise` / `migrate serena` / `overlay *` cannot be
// redirected by a stray env left in a shell/profile.
//
// The package TestMain reassigns stateDirFunc to an env-reading variant (so the
// rest of the suite keeps redirecting via the env); this test calls
// productionStateDir directly to assert the SHIPPED behavior ignores it. It
// asserts on the returned path string only and never writes, so it cannot touch
// real state.
func TestProductionStateDir_IgnoresEnvOverride(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "should-be-ignored-by-production")
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", sentinel)

	got, err := productionStateDir()
	if err != nil {
		t.Fatalf("productionStateDir: %v", err)
	}
	if got == sentinel {
		t.Fatalf("productionStateDir honored MCPHUB_STATE_DIR_OVERRIDE (%q) — a release binary must ignore it", sentinel)
	}
}

// TestStateDirFunc_TestSeamHonorsEnvOverride confirms the TestMain reassignment
// is active package-wide: under the test seam, stateDirFunc DOES honor a
// per-test MCPHUB_STATE_DIR_OVERRIDE, so existing cli tests that redirect
// supervisor state via the env keep working after the production env-read was
// removed. Asserts on the path string only; never writes.
func TestStateDirFunc_TestSeamHonorsEnvOverride(t *testing.T) {
	want := filepath.Join(t.TempDir(), "per-test-override")
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", want)

	got, err := stateDirFunc()
	if err != nil {
		t.Fatalf("stateDirFunc: %v", err)
	}
	if got != want {
		t.Fatalf("test-seam stateDirFunc = %q, want %q (the TestMain env-reading reassignment must be active)", got, want)
	}
}
