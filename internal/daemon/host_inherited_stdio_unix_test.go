//go:build linux || darwin

// Regression test for Codex security finding 63b417d2 (medium severity).
//
// PR #117 (commit 68791c9) fixed a child-exit response race by gating
// cmd.Wait() on stdout/stderr scanner EOF via a pipesDrained WaitGroup.
// That coupling was wrong: when an immediate stdio child exits but
// leaves a descendant that inherited the stdout/stderr pipes (the
// classic POSIX `( sleep 60 ) & exit 0` pattern), the scanners never
// see EOF, so:
//
//   - cmd.Wait() never returns
//   - childExited never closes
//   - the outer daemon supervisor never sees ChildExited()
//   - scheduler restart-on-failure is suppressed
//   - Stop() hangs on h.wg.Wait()
//
// The fix in this commit decouples childExited from pipe EOF (cmd.Wait
// runs immediately in the watcher goroutine) and bounds h.wg.Wait()
// inside Stop() so reparented descendants holding inherited pipes
// open cannot wedge shutdown.
//
// Test approach: spawn /bin/sh that backgrounds a long sleep into the
// inherited fds, then exits the immediate shell. Assert ChildExited()
// closes within 2s (proves the lifecycle decoupling) and Stop() returns
// within ~6s total (within the bounded h.wg.Wait timeout). Clean up
// the descendant via its written PID so the test does not leak
// processes.

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStdioHostInheritedStdioDescendantDoesNotWedgeChildExited(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "descendant.pid")

	// Background `sleep 60` MUST inherit /bin/sh's stdout/stderr (which
	// are our StdoutPipe/StderrPipe read-ends on the parent side) — that
	// is the deadlock condition Codex Cloud finding 63b417d2 reported.
	// No `>/dev/null 2>&1` redirect: that would hand sleep its own
	// /dev/null fds and the test would pass even with the bug present
	// (Codex bot P2 on f2512fe). Using `sleep 60 &` directly (no
	// subshell) so $! is sleep's PID and the cleanup SIGKILL is precise.
	script := `sleep 60 & echo $! > ` + pidFile + `; exit 0`

	h, err := NewStdioHost(HostConfig{
		Command: "/bin/sh",
		Args:    []string{"-c", script},
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		// Always clean up the inherited-stdio descendant so the test
		// does not leak processes across the run.
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	// ChildExited MUST close in BOUNDED time after the immediate child
	// exited. Before PR #117's pipesDrained.Wait gate this select blocked
	// indefinitely because the descendant kept fds open. The fix here
	// re-bounds the wait at pipeDrainTimeout (5s): the watcher fires the
	// timeout, calls cmd.Wait to close the parent's pipe read-ends, and
	// then closes childExited (Phase 4). Total budget includes the 5s
	// timeout + watcher reap; 8s gives comfortable headroom.
	//
	// Codex bot P1 on 34b1a30 was the reason childExited closes AFTER
	// pipe drain rather than immediately on Process.Wait return: closing
	// it on OS-exit raced with response delivery — the scanner could
	// dispatch a final reply to respCh AFTER childExited closed, and
	// handlePOST would non-deterministically pick the childExited branch
	// and return 502 even though a valid response existed.
	select {
	case <-h.ChildExited():
	case <-time.After(8 * time.Second):
		t.Fatalf("ChildExited() did not close within 8s — supervisor would never restart this daemon")
	}

	// Stop must also return within a bounded window. The fix added a
	// 5s cap on h.wg.Wait() inside Stop, so even with the descendant
	// holding pipes open, Stop returns by ~5-6s wall time.
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("Stop did not return within 8s — Stop would hang the outer daemon shutdown path")
	}
}
