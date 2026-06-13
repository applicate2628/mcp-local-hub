package daemon_env_overlay

import (
	"os"
	"testing"

	api "mcp-local-hub/internal/api"
)

// TestMain isolates the process-global api strict-mode-intent cache from the
// operator's REAL per-user state dir for the whole daemon_env_overlay test
// binary.
//
// Why this is required (pr301 r9): api.OperatorRequiresSingleUserHome() — which
// the read-side overlay gate (checkStateDirParentReadSafe) consults to decide
// strict-vs-relax — lazily resolves the supervisor-intent.json strict_mode bit
// ONCE per process and caches it. With no isolation, the first overlay test to
// resolve it with MCPHUB_ALLOW_UNHARDENED_STATE_READ unset (e.g.
// TestLoad_DefaultMode_BroadenedParent_Refuses) reads the operator's REAL
// %LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json THROUGH the read-side
// parent gate. On a host whose %LOCALAPPDATA% inherited an AD / Authenticated
// Users write ACE (the common corp-managed and even some solo-dev cases), that
// gated read FAILS, the intent is PRESENT-but-unreadable, and
// readStrictModeFromIntentBestEffort fails CLOSED to strict=TRUE (the r3
// present-unreadable behavior, intentionally retained). That strict verdict
// then pins the process-global cache, so the later
// TestLoad_RelaxOptOut_BroadenedParent_Succeeds (and the plain-temp-dir
// TestLoad_YAMLRoundTrip_ParsesDaemonRow) see "strict mode is active" and the
// relax-opt-out / default-relax paths they assert never run — a cross-test
// cache-pollution false failure that has nothing to do with the test's own
// contrived parent.
//
// Fix: point the api state-root override at a private temp dir (no
// supervisor-intent.json — an ABSENT intent, which under the pr301 r9
// canon-aligned posture relaxes regardless of the dir's broadening) and reset
// the cache so its first lazy resolution reads THIS isolated root, not the
// operator's real state dir. This is the same isolation r5 applied to the
// ~100 internal/api tests via hardenedTempDir; the overlay package (a separate
// test binary) was never given it.
//
// Tests that need their OWN state-root override (e.g. parent_path_expand_test's
// installStateRoot, which redirects DaemonStateDir to read an isolated
// hub-mcp.log) still set+restore it on top of this default; the strict-mode
// cache is already resolved to relax by then, so their override change does not
// re-trigger a real-dir read.
func TestMain(m *testing.M) {
	// A private temp dir with NO supervisor-intent.json. Under pr301 r9, an
	// absent intent relaxes regardless of the temp dir's broadening, so this
	// pins the strict-mode cache to RELAX deterministically — never reading the
	// operator's real (possibly broadened, present-but-unreadable) state dir.
	isolatedRoot, err := os.MkdirTemp("", "overlay-strictmode-isolated-")
	if err != nil {
		panic("daemon_env_overlay TestMain: mkdir isolated state root: " + err.Error())
	}
	restore := api.SetDaemonStateRootForTest(isolatedRoot)
	api.ResetStrictModeIntentCacheForTest()

	code := m.Run()

	restore()
	_ = os.RemoveAll(isolatedRoot)
	os.Exit(code)
}
