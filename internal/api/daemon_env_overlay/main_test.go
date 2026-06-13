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
// Why this is kept (pr301 r10 — process-global cache isolation): the gate-free
// read introduced in pr301 r10 made the PRODUCTION path relax on a broadened
// state dir whose intent is absent OR strict_mode=false (the fleet-safety fix),
// so on THIS host the overlay tests pass even without this isolation. But the
// isolation is still required for cross-host DETERMINISM:
// api.OperatorRequiresSingleUserHome() — which the read-side overlay gate
// (checkStateDirParentReadSafe) consults to decide strict-vs-relax — lazily
// resolves the supervisor-intent.json strict_mode bit ONCE per process and
// caches it. With no isolation, the first overlay test to resolve it reads the
// operator's REAL %LOCALAPPDATA%\mcp-local-hub\supervisor-intent.json. On a host
// whose real intent carries strict_mode=TRUE, that resolves the process-global
// cache to STRICT, and the later TestLoad_RelaxOptOut_BroadenedParent_Succeeds /
// TestLoad_DefaultMode_SafeParent_Succeeds (which assert the relax / default
// paths) would then see "strict mode is active" and fail — a cross-test
// cache-pollution false failure that has nothing to do with the test's own
// contrived parent.
//
// Fix: point the api state-root override at a private temp dir (no
// supervisor-intent.json — an ABSENT intent, which under the pr301 r10 gate-free
// read relaxes deterministically) and reset the cache so its first lazy
// resolution reads THIS isolated root, not the operator's real state dir. This
// is the same isolation r5 applied to the ~100 internal/api tests via
// hardenedTempDir; the overlay package (a separate test binary) was never given
// it.
//
// Tests that need their OWN state-root override (e.g. parent_path_expand_test's
// installStateRoot, which redirects DaemonStateDir to read an isolated
// hub-mcp.log) still set+restore it on top of this default; the strict-mode
// cache is already resolved to relax by then, so their override change does not
// re-trigger a real-dir read.
func TestMain(m *testing.M) {
	// A private temp dir with NO supervisor-intent.json. Under the pr301 r10
	// gate-free read, an absent intent relaxes regardless of the temp dir's
	// broadening, so this pins the strict-mode cache to RELAX deterministically —
	// never reading the operator's real (possibly strict_mode=true) state dir.
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
