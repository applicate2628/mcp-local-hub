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
	// re-bounds the wait at pipeDrainTimeout: the watcher fires the
	// timeout, calls cmd.Wait to close the parent's pipe read-ends, and
	// then closes childExited (Phase 4). Budget below derives from
	// pipeDrainTimeout (the only meaningful constant) plus a 3s watcher
	// reap headroom — Codex CLI xhigh P3 on e26209a flagged the prior
	// hardcoded 8s as brittle.
	//
	// Codex bot P1 on 34b1a30 was the reason childExited closes AFTER
	// pipe drain rather than immediately on Process.Wait return: closing
	// it on OS-exit raced with response delivery — the scanner could
	// dispatch a final reply to respCh AFTER childExited closed, and
	// handlePOST would non-deterministically pick the childExited branch
	// and return 502 even though a valid response existed.
	childExitedBudget := pipeDrainTimeout + 3*time.Second
	select {
	case <-h.ChildExited():
	case <-time.After(childExitedBudget):
		t.Fatalf("ChildExited() did not close within %s — supervisor would never restart this daemon", childExitedBudget)
	}

	// Stop must also return within a bounded window. The fix added a
	// 1s cap on h.wg.Wait() inside Stop, so even with the descendant
	// holding pipes open, Stop returns within seconds of childExited.
	stopBudget := pipeDrainTimeout + 3*time.Second
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned error: %v", err)
		}
	case <-time.After(stopBudget):
		t.Fatalf("Stop did not return within %s — Stop would hang the outer daemon shutdown path", stopBudget)
	}
}

// TestStdioHostProcExitedGateSkipsKillAfterReap is the regression test
// for the procExited PID-reuse gate (Codex CLI xhigh P2 on e26209a:
// "procExited gate is not directly covered"). After cmd.Process.Wait
// reaps the OS child in Phase 1, the original PID is eligible for
// reuse on POSIX. Stop's kill-skip select MUST consult procExited (not
// childExited, which closes only after the up-to-pipeDrainTimeout
// drain wait) so it doesn't issue a fresh PID-based pkill/kill against
// a possibly-reused PID.
//
// Two assertions, both load-bearing:
//   1. ChildExited() is STILL OPEN at Stop time — proves we are inside
//      the procExited-closed / childExited-open window the gate
//      protects.
//   2. killProcessTreeCalls counter does NOT increment during Stop —
//      proves the kill was actually skipped, not "happened to do
//      nothing against a recycled PID".
//
// Codex Cloud bot P1 on cd2c118 flagged that a fast-exit child
// (`exit 0` with no descendant) collapses the window on fast machines,
// making assertion #1 nondeterministic. This redesign uses the same
// inherited-stdio descendant pattern as the deadlock test directly
// above: `sleep 60 &` keeps stdout/stderr open after /bin/sh exits, so
// Phase 2 hits pipeDrainTimeout (5s). The procExited-closed /
// childExited-open window is then GUARANTEED open for ~5 seconds
// regardless of machine speed, far larger than any scheduler jitter.
// kosyak: 2026-05-06-claude-brittle-test-assertion-non-deterministic-window.md
func TestStdioHostProcExitedGateSkipsKillAfterReap(t *testing.T) {
	tmp := t.TempDir()
	pidFile := filepath.Join(tmp, "descendant.pid")

	// sleep 60 inherits /bin/sh's stdout/stderr, holding our pipes open
	// after /bin/sh exits — Phase 2 then hits pipeDrainTimeout, opening
	// the procExited→childExited window for the full pipeDrainTimeout.
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
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	// Wait for procExited (Phase 1 done — /bin/sh OS-exited).
	select {
	case <-h.procExited:
	case <-time.After(2 * time.Second):
		t.Fatal("procExited did not close within 2s of /bin/sh exit-0")
	}

	// ChildExited MUST still be open: Phase 2 is now blocked on
	// pipeDrainTimeout because the descendant `sleep 60` holds our
	// stdout/stderr pipes open. This is GUARANTEED by the
	// inherited-stdio pattern — unlike a fast-exit child where the
	// window collapses on fast machines (Cloud bot P1 on cd2c118).
	select {
	case <-h.ChildExited():
		t.Fatal("ChildExited closed unexpectedly while sleep descendant should be holding pipes — Phase 2 escape route under test")
	default:
	}

	killsBefore := killProcessTreeCalls.Load()

	// Stop now. The kill-skip select inside Stop reads procExited
	// (closed) and skips killProcessTree, avoiding the PID-reuse hazard.
	stopDone := make(chan error, 1)
	go func() { stopDone <- h.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop returned unexpected error after procExited gate: %v", err)
		}
	case <-time.After(pipeDrainTimeout + 3*time.Second):
		t.Fatalf("Stop did not return within bounded window after procExited gate")
	}

	// Prove kill was actually skipped, not just "succeeded against
	// nothing". Load-bearing assertion for P2 #2 — without it, a
	// regression that issues a kill against a recycled PID would still
	// pass the test.
	if killsAfter := killProcessTreeCalls.Load(); killsAfter != killsBefore {
		t.Errorf("killProcessTree was invoked %d times during Stop after procExited closed (want 0); kill-skip gate is broken", killsAfter-killsBefore)
	}
}
