package daemon

import (
	"context"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestExecStartClosesParentPipesOnFailure pins the load-bearing premise for
// why StdioHost.Start's cmd.Start()-failure branch is deliberately NOT given
// a manual pipe-close: Go's exec.Cmd.Start runs a deferred
// closeDescriptors(parentIOPipes) on ANY error return that leaves the process
// unstarted (verified against os/exec/exec.go's Start defer at go1.26). If a
// future Go release regressed that contract, adding manual closes to that
// branch would be required — so this test fails loudly if the auto-close
// stops happening, catching the premise-change before it becomes a leak.
//
// The check: after StdinPipe/StdoutPipe/StderrPipe succeed but Start fails
// (nonexistent binary), the returned parent pipe ends must already be closed.
// A closed *os.File returns an error containing "file already closed" from a
// second Close(); an OPEN one returns nil. So a nil second-Close would mean
// the pipe leaked.
func TestExecStartClosesParentPipesOnFailure(t *testing.T) {
	cmd := exec.Command("this-binary-does-not-exist-fdleak-9999")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StderrPipe: %v", err)
	}
	if err := cmd.Start(); err == nil {
		t.Fatal("expected Start to fail on nonexistent binary")
	}
	// Each of these was appended to cmd.parentIOPipes and must have been
	// closed by Start's deferred cleanup. Second Close should error.
	for name, c := range map[string]interface {
		Close() error
	}{"stdin": stdin, "stdout": stdout, "stderr": stderr} {
		if err := c.Close(); err == nil {
			t.Errorf("%s parent pipe was NOT auto-closed by cmd.Start() failure — "+
				"the manual-close-elision premise no longer holds; the "+
				"cmd.Start() branch in StdioHost.Start would now leak", name)
		}
	}
}

// TestStdioHostStartFailureNoGoroutineLeak drives a real StdioHost.Start
// failure and asserts it leaves no lingering reader/watcher goroutines. On the
// partial-init failure paths (StdinPipe/StdoutPipe/StderrPipe fail, or
// cmd.Start fails) Start must return before any goroutine, Job Object, or
// done-channel machinery is created — so a failed host has nothing to leak and
// nothing to Stop. This is the observable end-state guarantee behind the
// partial-init pipe-close fix.
//
// Note on the StdoutPipe/StderrPipe-fail branches specifically: those are only
// reachable when the underlying *exec.Cmd already has that stream set, which
// cannot happen through StdioHost.Start's public path (it builds a fresh
// exec.Cmd on every call and never pre-sets a stream). There is no interface
// seam wrapping the stdlib *exec.Cmd to inject a StdoutPipe/StderrPipe error,
// so those two branches are covered by inspection + the fd-close semantics
// asserted in TestExecStartClosesParentPipesOnFailure, not by a direct
// injection test. The realistic public-API partial-init failure is
// cmd.Start() (nonexistent binary), exercised here.
func TestStdioHostStartFailureNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	h, err := NewStdioHost(HostConfig{
		Command: "this-binary-does-not-exist-fdleak-9999",
	})
	if err != nil {
		t.Fatalf("NewStdioHost: %v", err)
	}
	if err := h.Start(ctx); err == nil {
		_ = h.Stop()
		t.Fatal("expected Start to fail on nonexistent binary")
	} else if !strings.Contains(err.Error(), "start subprocess") {
		t.Fatalf("unexpected Start error (want start-subprocess failure): %v", err)
	}

	// h.started stayed false; Stop() short-circuits and is a no-op. Confirm
	// it is safe to call and does not error.
	if err := h.Stop(); err != nil {
		t.Errorf("Stop on never-started host should be a nil no-op, got: %v", err)
	}

	// No reader/watcher goroutines were ever spawned on the failure path.
	// Allow a brief settle window, then assert we did not grow the goroutine
	// count. A small tolerance absorbs unrelated runtime/test-harness churn.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		after = runtime.NumGoroutine()
		if after <= before+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > before+2 {
		t.Errorf("goroutine leak after failed StdioHost.Start: before=%d after=%d", before, after)
	}
}

// TestClosePipeChildEndReclaimsLeakedChildDescriptor proves the fix for the
// child-end descriptor leak on the partial-init pipe-failure paths. When a
// *Pipe() helper succeeds it opens a pipe PAIR: the parent end is returned to
// the caller, while the CHILD end is stored on cmd.Std* (a *os.File) and is
// only reclaimed inside cmd.Start(). On a partial-init early return (a LATER
// *Pipe() failed, so Start never runs), closing only the returned parent end
// leaves that child end open — the exact leak the fix's closePipeChildEnd
// calls close.
//
// This reproduces the state right before a StderrPipe failure (StdinPipe +
// StdoutPipe already succeeded), then asserts closePipeChildEnd actually
// closes the child *os.File exec stashed on cmd.Stdin/cmd.Stdout. The
// StdoutPipe/StderrPipe-fail branches themselves are not injectable through
// StdioHost's public API (no seam wrapping the stdlib *exec.Cmd), so the
// branch WIRING is covered by inspection while this test pins the cleanup
// primitive's semantics — a no-op or wrong type-assertion here would resurface
// the leak and fail this test.
func TestClosePipeChildEndReclaimsLeakedChildDescriptor(t *testing.T) {
	cmd := exec.Command("this-binary-does-not-exist-fdleak-9999")
	stdinParent, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("StdinPipe: %v", err)
	}
	stdoutParent, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe: %v", err)
	}
	// The child ends exec stored on cmd.Std* — these are the fds that leak if
	// only the parent ends are closed.
	childStdin, ok := cmd.Stdin.(io.Closer)
	if !ok {
		t.Fatal("cmd.Stdin is not an io.Closer (*os.File expected)")
	}
	childStdout, ok := cmd.Stdout.(io.Closer)
	if !ok {
		t.Fatal("cmd.Stdout is not an io.Closer (*os.File expected)")
	}

	// The OLD (buggy) cleanup closed only the parent ends.
	_ = stdinParent.Close()
	_ = stdoutParent.Close()

	// The fix additionally reclaims the child ends.
	closePipeChildEnd(cmd.Stdin)
	closePipeChildEnd(cmd.Stdout)

	// A second close on each child end must now error (already closed),
	// proving closePipeChildEnd closed the fd. Without the fix these ends
	// would still be open and a first close here would return nil.
	if err := childStdin.Close(); err == nil {
		t.Error("child stdin end was left open (leaked) — closePipeChildEnd did not close it")
	}
	if err := childStdout.Close(); err == nil {
		t.Error("child stdout end was left open (leaked) — closePipeChildEnd did not close it")
	}
}

// TestClosePipeChildEndIsNilSafe guards the helper against a nil or
// non-Closer cmd.Std* value (e.g. a stream never set): it must be a silent
// no-op, never a panic, since it runs on error-cleanup paths.
func TestClosePipeChildEndIsNilSafe(t *testing.T) {
	closePipeChildEnd(nil)              // untyped nil
	closePipeChildEnd((io.Reader)(nil)) // typed-nil interface
	closePipeChildEnd("not-a-closer")   // non-Closer value
}
