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

	exe, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	root := repoRoot(t)
	first := exec.Command(exe, "run", "./cmd/mcphub", "gui", "--no-browser", "--no-tray", "--port", "0")
	first.Dir = root
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

	second := exec.Command(exe, "run", "./cmd/mcphub", "gui", "--no-browser", "--no-tray")
	second.Dir = root
	out, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second instance failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "activated") {
		t.Errorf("second instance output should confirm activation; got: %s", out)
	}
}
