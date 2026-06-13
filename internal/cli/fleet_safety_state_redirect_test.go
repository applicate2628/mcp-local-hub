package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
)

// TestDaemonStateDirRedirect_GlobalTestMainRoutesToTempDir is the POSITIVE
// proof that the process-global daemon-state-dir redirect installed by this
// package's TestMain (settings_registry_test.go) is active for EVERY test in
// internal/cli — including any test that forgets its own
// api.SetDaemonStateRootForTest isolation.
//
// FLEET-SAFETY (2026-06-13 incident): a `go test ./internal/cli/` run reached
// the real api.(*API).StopAll() with no per-test state-dir isolation and
// stopped all 22 live daemons under the developer's real
// %LOCALAPPDATA%\mcp-local-hub. The TestMain global redirect makes it
// STRUCTURALLY IMPOSSIBLE for any internal/cli test to resolve the real state
// dir: the default (no per-test override) now points at a throwaway temp dir.
//
// This test deliberately does NOT call SetDaemonStateRootForTest — it relies
// SOLELY on the TestMain-installed global redirect, so it is the canonical
// regression guard. If a future change drops the global redirect from TestMain,
// this test fails (DaemonStateDir would resolve the real platform path, which is
// neither under the OS temp dir nor carries the test-state prefix).
func TestDaemonStateDirRedirect_GlobalTestMainRoutesToTempDir(t *testing.T) {
	// No SetDaemonStateRootForTest here — that is the whole point. We assert the
	// GLOBAL default is already a safe temp dir.
	got, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir() under the global TestMain redirect must succeed; got error %v "+
			"(this means the global redirect is NOT installed — production resolution ran instead)", err)
	}

	// 1. POSITIVE: the resolved dir must carry the TestMain temp-dir prefix.
	//    os.MkdirTemp("", "mcphub-cli-test-state-*") creates the dir under
	//    os.TempDir() with the "mcphub-cli-test-state-" basename prefix. Under
	//    the override, DaemonStateDir() returns that root verbatim (no
	//    mcp-local-hub leaf appended — see ensureStateRoot).
	base := filepath.Base(got)
	if !strings.HasPrefix(base, "mcphub-cli-test-state-") {
		t.Fatalf("DaemonStateDir() = %q; want a directory whose basename carries the "+
			"TestMain temp prefix %q. The global state-dir redirect is not active — a real "+
			"test could reach the live fleet.", got, "mcphub-cli-test-state-")
	}

	// 2. POSITIVE: the resolved dir must live under the OS temp root, where
	//    os.MkdirTemp("") places it. Compare on cleaned absolute paths so a
	//    short-name / trailing-separator mismatch does not produce a false
	//    negative.
	tempRoot, err := filepath.Abs(os.TempDir())
	if err != nil {
		t.Fatalf("resolve abs OS temp root: %v", err)
	}
	gotAbs, err := filepath.Abs(got)
	if err != nil {
		t.Fatalf("resolve abs DaemonStateDir: %v", err)
	}
	if !strings.HasPrefix(gotAbs, filepath.Clean(tempRoot)) {
		t.Fatalf("DaemonStateDir() = %q is not under the OS temp root %q; the global "+
			"redirect did not route through os.MkdirTemp as expected", gotAbs, tempRoot)
	}

	// 3. NEGATIVE: the resolved dir must NOT be the real per-user fleet
	//    directory (<LocalAppData>\mcp-local-hub on Windows). The real fleet
	//    dir ends in the canonical "mcp-local-hub" leaf; the redirect target
	//    does not. This is the leak-detector: if the redirect ever fails, the
	//    basename would be the real "mcp-local-hub" leaf, not the temp prefix.
	if base == "mcp-local-hub" {
		t.Fatalf("DaemonStateDir() = %q resolves to the real per-user fleet leaf "+
			"%q — the global redirect FAILED and a test would touch the LIVE fleet", got, "mcp-local-hub")
	}
}

// TestDaemonStateDirRedirect_PerTestOverrideComposesAndRestoresToGlobal proves
// the composition contract the TestMain fleet-safety net depends on: a per-test
// SetDaemonStateRootForTest override followed by its restore returns to the
// GLOBAL temp dir, never to "" (which would re-expose the real platform path).
//
// This is the load-bearing invariant that lets per-test isolation and the
// global safety net coexist: SetDaemonStateRootForTest captures the LIVE
// override value on entry (under TestMain that is the global temp dir) and
// resets to it on restore.
func TestDaemonStateDirRedirect_PerTestOverrideComposesAndRestoresToGlobal(t *testing.T) {
	before, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("baseline DaemonStateDir(): %v", err)
	}

	perTest := t.TempDir()
	restore := api.SetDaemonStateRootForTest(perTest)

	during, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir() under per-test override: %v", err)
	}
	if filepath.Clean(during) != filepath.Clean(perTest) {
		t.Fatalf("per-test override active: DaemonStateDir() = %q, want %q", during, perTest)
	}

	restore()

	after, err := api.DaemonStateDir()
	if err != nil {
		t.Fatalf("DaemonStateDir() after restore: %v", err)
	}
	// After restore we must be back at the GLOBAL temp dir (== before), NOT at
	// the real platform path. A restore-to-"" regression would make `after`
	// the real fleet dir and this assertion fails.
	if filepath.Clean(after) != filepath.Clean(before) {
		t.Fatalf("restore() did not return to the global temp dir: after=%q, want %q "+
			"(a restore-to-empty regression would re-expose the real fleet dir)", after, before)
	}
	if filepath.Base(after) == "mcp-local-hub" {
		t.Fatalf("after restore DaemonStateDir() = %q resolves to the real fleet leaf; "+
			"the per-test restore leaked back to the live state dir", after)
	}
}
