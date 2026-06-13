package cli

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// repoRoot locates the module root by walking up from the test's CWD
// until it finds go.mod. Needed because `go run ./cmd/mcphub` resolves
// relative to the command's working directory, which for this test is
// internal/cli/, not the repo root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}

// TestGuiCmd_SecondInstanceActivates spawns two `mcphub gui` processes
// and asserts the second exits 0 without binding a new port (the first
// keeps running).
func TestGuiCmd_SecondInstanceActivates(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	if runtime.GOOS == "windows" && os.Getenv("CI") != "" {
		// Named-object isolation between CI containers is unreliable.
		t.Skip("flaky in Windows CI sandbox")
	}
	// PR #26 F4: on a headless Linux session (no $DISPLAY, no
	// $WAYLAND_DISPLAY — the standard ubuntu-latest CI shape) the
	// incumbent's OnActivateWindow callback returns ErrActivationNoTarget,
	// the handler maps to 503, and the second instance prints the
	// SSH-tunnel guidance instead of "activated existing mcphub gui".
	// The "activated" assertion below would spuriously fail on that
	// path, even though the headless contract is working as designed.
	// Sonnet review on PR #26 P1 (gui_integration_test.go:107).
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("headless Linux: second instance prints SSH-tunnel guidance, not 'activated' (PR #26 F4 contract)")
	}
	// Use a standalone temp dir we control manually: go-exec's child
	// process (the built mcphub.exe) can outlive Cmd.Wait on Windows
	// because `go run` spawns a grandchild, and t.TempDir's cleanup
	// would race with that grandchild's still-open flock handle.
	pidportDir, err := os.MkdirTemp("", "gui-integration-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCPHUB_GUI_TEST_PIDPORT_DIR", pidportDir)

	// FLEET-SAFETY (PR #300 r1 P1 + r2 P1): the cli TestMain installs only an
	// IN-MEMORY daemonStateRootOverride, which does NOT propagate to a
	// subprocess. Without redirecting the child's own state-dir resolution,
	// the `mcphub gui` child (and the `mcphub supervise` grandchild it spawns)
	// would reach api.DaemonStateDir() and touch the REAL per-user
	// %LOCALAPPDATA%\mcp-local-hub state + IPC pipe — exactly the live-fleet
	// hazard the TestMain redirect was meant to remove. We therefore (1) build
	// the child WITH the test_state_path_env tag so MCPHUB_STATE_DIR_OVERRIDE
	// is honored BEFORE the platform resolver even on a real Windows host
	// (where SHGetKnownFolderPath succeeds and the LOCALAPPDATA fallback would
	// be inert — PR #300 r2 P1), and (2) point childStateEnv at a per-test
	// temp dir. Same manual-MkdirTemp rationale as pidportDir: the grandchild
	// can outlive Cmd.Wait on Windows, so t.TempDir's cleanup would race its
	// open handles.
	childState, err := os.MkdirTemp("", "gui-integration-state-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(childState); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		_ = os.RemoveAll(childState)
	})

	exe, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	first := exec.Command(exe, goRunArgsWithTestTag("./cmd/mcphub", "gui", "--no-browser", "--no-tray", "--port", "0")...)
	first.Dir = root
	first.Env = append(os.Environ(), childStateEnv(childState)...)
	// Pipe child I/O into io.Discard and arm WaitDelay so Wait cannot
	// hang on grandchild pipe handles surviving the go-run wrapper's
	// death. `go run` builds a temp binary and execs it, so killing
	// the wrapper leaves the mcphub grandchild holding our pipe ends;
	// WaitDelay forces those pipes closed shortly after process exit.
	first.Stdout = io.Discard
	first.Stderr = io.Discard
	first.WaitDelay = 2 * time.Second
	if err := first.Start(); err != nil {
		t.Fatalf("start first: %v", err)
	}
	t.Cleanup(func() {
		// Kill the go-run wrapper and wait for it. On Windows the actual
		// mcphub.exe is a grandchild; its handle on the flock file is
		// what keeps pidportDir un-deletable. Retry the rmdir a few
		// times to let Windows release the handle after the grandchild
		// exits.
		_ = first.Process.Kill()
		_ = first.Wait()
		for i := 0; i < 20; i++ {
			if err := os.RemoveAll(pidportDir); err == nil {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		// Best-effort: final attempt, ignore error. Leaving junk in
		// the OS temp dir is strictly better than failing the test on
		// a platform-specific cleanup race.
		_ = os.RemoveAll(pidportDir)
	})

	// Give the first instance up to 5s to bind. Cold `go run` compile
	// of cmd/mcphub can exceed 2s on a first invocation; 5s stays well
	// under the test's 30s timeout and leaves margin for the second
	// instance's own compile + handshake.
	time.Sleep(5 * time.Second)

	second := exec.Command(exe, goRunArgsWithTestTag("./cmd/mcphub", "gui", "--no-browser", "--no-tray")...)
	second.Dir = root
	second.Env = append(os.Environ(), childStateEnv(childState)...)
	out, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second instance failed: %v\noutput: %s", err, out)
	}
	// Both instances spawn with --no-browser, so the SECURITY guard
	// (--no-browser refuses LaunchBrowser fallback in the callback)
	// makes the activate-window handler return 503 → handshake
	// returns ErrIncumbentNoActivationTarget → second instance prints
	// the headless-style guidance instead of "activated". Either
	// output proves the handshake reached the incumbent.
	out2 := string(out)
	ok := strings.Contains(out2, "activated") ||
		strings.Contains(out2, "already running headless")
	if !ok {
		t.Errorf("second instance output should confirm handshake (activated OR headless guidance); got: %s", out2)
	}

	// ISOLATION PROOF (PR #300 r1 P1 + r2 P2): assert the child's
	// api.DaemonStateDir() actually resolved the TEMP override and NEVER
	// touched the real %LOCALAPPDATA%\mcp-local-hub.
	//
	// The first `mcphub gui` child calls ensureSupervisorRunning, whose very
	// first step is api.DaemonStateDir() (gui_supervisor_owner.go:89, before
	// any probe), which calls ensureStateRoot -> os.MkdirAll(<stateDir>). With
	// the MCPHUB_STATE_DIR_OVERRIDE+tag redirect, <stateDir> is
	// childStateDirOverrideLeaf(childState) = <childState>/supervisor-state, so
	// the child MUST create exactly that dir.
	//
	// WHY THIS LEAF, NOT <childState>/mcp-local-hub (PR #300 r2 P2): the GUI
	// command resolves gui.PidportPath() (gui.go:184), whose gui.AppDataDir
	// (internal/gui/paths.go) MkdirAll's <LOCALAPPDATA>/mcp-local-hub =
	// <childState>/mcp-local-hub from the LOCALAPPDATA env — INDEPENDENTLY of
	// api.DaemonStateDir(). So checking <childState>/mcp-local-hub would pass
	// even if daemonStateDir() resolved the REAL per-user path (the prior r1
	// proof was therefore non-conclusive). The override leaf is a DISTINCT
	// subdir created ONLY through api.DaemonStateDir()'s ensureStateRoot, so
	// its existence is conclusive proof the SUPERVISOR STATE path (not the GUI
	// pidport) was redirected. If the redirect had failed, the child would
	// have MkdirAll'd the REAL %LOCALAPPDATA%\mcp-local-hub and this distinct
	// override subdir would never appear.
	//
	// We assert ONLY existence, not non-emptiness: the supervise grandchild is
	// killed during t.Cleanup (we Kill the go-run wrapper) and may not have
	// flushed supervisor-events.log / supervisor.lock yet, so requiring
	// artifacts would test grandchild write-timing, not isolation. Leaf
	// existence + the process-level byte-identity of the real
	// supervisor-intent.json (verified out-of-band in the PR) together prove
	// the subprocess stayed off the live fleet.
	stateLeaf := childStateDirOverrideLeaf(childState)
	if info, statErr := os.Stat(stateLeaf); statErr != nil || !info.IsDir() {
		t.Fatalf("isolation proof failed: child mcphub api.DaemonStateDir() did not create its redirected override dir %q (stat err: %v)\n"+
			"This means the subprocess resolved the REAL per-user state dir instead of the temp override — "+
			"the live-fleet hazard is NOT closed.\nsecond-instance output:\n%s", stateLeaf, statErr, out2)
	}
}
