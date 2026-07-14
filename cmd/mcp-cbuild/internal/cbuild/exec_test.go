package cbuild

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestBoundedBufferCaps proves the capture sink retains only the last max bytes
// (ring semantics) so a runaway build cannot OOM the server.
func TestBoundedBufferCaps(t *testing.T) {
	const max = 1024
	b := newBoundedBuffer(max)
	for i := 0; i < 100; i++ { // 6400 bytes total, well over the cap
		if _, err := b.Write([]byte(strings.Repeat("x", 64))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := b.String(); len(got) > max {
		t.Errorf("retained %d bytes, want <= %d", len(got), max)
	}
	if !b.truncated {
		t.Error("expected truncated=true after exceeding the cap")
	}

	// Under-cap writes are retained verbatim, untruncated.
	small := newBoundedBuffer(max)
	small.Write([]byte("hello"))
	if small.String() != "hello" || small.truncated {
		t.Errorf("small write: got %q truncated=%v", small.String(), small.truncated)
	}

	// The retained slice is the TAIL (most recent bytes), not the head.
	tail := newBoundedBuffer(8)
	tail.Write([]byte("0123456789ABCDEF"))
	if tail.String() != "89ABCDEF" {
		t.Errorf("tail = %q, want 89ABCDEF", tail.String())
	}
}

// TestResolveBuildDirWithinSourceSymlinkEscape proves the purge guard resolves
// symlinks and refuses a build dir whose real target escapes the source tree,
// while still allowing an in-tree symlink. Skips on hosts without symlink
// support (e.g. Windows without the create-symlink privilege).
func TestResolveBuildDirWithinSourceSymlinkEscape(t *testing.T) {
	src := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, guaranteed outside src

	escapeLink := filepath.Join(src, "build")
	if err := os.Symlink(outside, escapeLink); err != nil {
		t.Skipf("symlinks unsupported on this host (%v); skipping", err)
	}
	// Lexically "${sourceDir}/build" is inside src, but the symlink target
	// escapes it → must be refused.
	if _, err := resolveBuildDirWithinSource("${sourceDir}/build", src, "dev"); err == nil {
		t.Error("expected refusal: build dir symlink escapes the source tree")
	}

	// A symlink that stays inside the tree is allowed.
	inside := filepath.Join(src, "actual")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	if err := os.Symlink(inside, filepath.Join(src, "innerbuild")); err != nil {
		t.Skipf("second symlink unsupported (%v); skipping", err)
	}
	if _, err := resolveBuildDirWithinSource("${sourceDir}/innerbuild", src, "dev"); err != nil {
		t.Errorf("in-tree symlink build dir wrongly refused: %v", err)
	}
}

// TestRunCommandTimeoutReturnsPromptly is a best-effort proof that a cancelled
// long-running command returns well before the command's own duration — i.e.
// the process-tree kill fires and cmd.WaitDelay guarantees Wait does not wedge.
func TestRunCommandTimeoutReturnsPromptly(t *testing.T) {
	bin, args, ok := longSleepCommand()
	if !ok {
		t.Skip("no sleep-capable command available on this host")
	}
	start := time.Now()
	res := runCommand(context.Background(), 500*time.Millisecond, "", bin, args)
	elapsed := time.Since(start)

	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got %+v", res)
	}
	// The command sleeps ~30s; runCommand must return far sooner (500ms timeout
	// + kill + the 8s WaitDelay backstop). A block near 30s means tree-kill /
	// WaitDelay is ineffective.
	if elapsed > 20*time.Second {
		t.Errorf("runCommand blocked %v after a 500ms timeout — tree-kill/WaitDelay ineffective", elapsed)
	}
}

// longSleepCommand returns a ~30s-sleeping command available on the host, or
// ok=false to skip.
func longSleepCommand() (string, []string, bool) {
	if runtime.GOOS == "windows" {
		// `ping -n N 127.0.0.1` waits ~1s between echoes; -n 31 ≈ 30s.
		if p, err := exec.LookPath("ping"); err == nil {
			return p, []string{"-n", "31", "127.0.0.1"}, true
		}
		return "", nil, false
	}
	if p, err := exec.LookPath("sleep"); err == nil {
		return p, []string{"30"}, true
	}
	return "", nil, false
}
