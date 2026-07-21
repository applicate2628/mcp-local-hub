package cli

import (
	"bytes"
	"fmt"
	"sync"
	"testing"
)

// captureGUIDiagnosticEvents swaps the durable-emit seam for the duration of
// a test and returns a accessor for what was emitted. It also snapshots and
// restores the process-global sinks, which are sticky by design (durable is
// one-way in production).
func captureGUIDiagnosticEvents(t *testing.T) func() []string {
	t.Helper()

	var mu sync.Mutex
	var got []string

	origFn := logGUIDiagnosticEvent
	logGUIDiagnosticEvent = func(level, event string, fields map[string]any) error {
		mu.Lock()
		defer mu.Unlock()
		msg, _ := fields["message"].(string)
		got = append(got, msg)
		return nil
	}

	outFallback, outDurable := guiRuntimeStdout.fallback, guiRuntimeStdout.durable
	errFallback, errDurable := guiRuntimeStderr.fallback, guiRuntimeStderr.durable
	t.Cleanup(func() {
		logGUIDiagnosticEvent = origFn
		guiRuntimeStdout.mu.Lock()
		guiRuntimeStdout.fallback, guiRuntimeStdout.durable = outFallback, outDurable
		guiRuntimeStdout.mu.Unlock()
		guiRuntimeStderr.mu.Lock()
		guiRuntimeStderr.fallback, guiRuntimeStderr.durable = errFallback, errDurable
		guiRuntimeStderr.mu.Unlock()
	})

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// TestReleaseConsoleEngagesDurableSinkBeforeReleasing closes the silent-loss
// window between the console release and the sink switch.
//
// The release kills console-backed stdio. Any diagnostic written after that
// but before the sink is durable goes to a dead handle and is discarded —
// and consoleReleaseSink.Write returns len(p), nil unconditionally, so
// nothing anywhere reports the loss. The concurrent writer is real: the
// supervisor exit monitor writes "supervisor exited unexpectedly (PID %d)…"
// through guiRuntimeStderr from its own goroutine, and that line is the only
// record of a supervisor crash that predates the supervisor's audit log.
//
// The injected release stands in for that writer at the worst possible
// instant — the moment the console dies. If the sink is engaged first, the
// message is durable; if it is engaged after, it is gone.
func TestReleaseConsoleEngagesDurableSinkBeforeReleasing(t *testing.T) {
	events := captureGUIDiagnosticEvents(t)

	var console bytes.Buffer
	guiRuntimeStderr.setFallback(&console)
	guiRuntimeStdout.setFallback(&console)

	const crashLine = "warning: supervisor exited unexpectedly (PID 4242): exit status 2"
	releaseConsoleForBackgroundGUI(func() {
		// Written AT the release instant, exactly as the exit monitor's
		// goroutine may do.
		fmt.Fprintln(guiRuntimeStderr, crashLine)
	})

	found := false
	for _, e := range events() {
		if e == crashLine {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("a diagnostic written during the console release never reached the durable "+
			"sink; it went only to the console being torn down and was silently discarded. "+
			"Engage the sinks BEFORE release(). durable events seen: %q", events())
	}
}

// TestReleaseConsoleEngagesBothSinks pins that stdout and stderr are both
// durable once the release returns. They were engaged sequentially in an
// earlier revision, leaving an instant where one was durable and the other
// was not.
func TestReleaseConsoleEngagesBothSinks(t *testing.T) {
	captureGUIDiagnosticEvents(t)

	releaseConsoleForBackgroundGUI(func() {})

	guiRuntimeStdout.mu.Lock()
	outDurable := guiRuntimeStdout.durable
	guiRuntimeStdout.mu.Unlock()
	guiRuntimeStderr.mu.Lock()
	errDurable := guiRuntimeStderr.durable
	guiRuntimeStderr.mu.Unlock()

	if !outDurable || !errDurable {
		t.Fatalf("after the console release both sinks must be durable "+
			"(stdout=%v stderr=%v); a non-durable sink writes only to the dead console",
			outDurable, errDurable)
	}
}

// TestConsoleReleaseSinkStillWritesToALiveFallback guards the requirement
// that redirected runs are unaffected. FreeConsole closes console handles
// but not pipe or file handles, so `mcphub gui > log 2>&1` must keep
// receiving every line it received before — the durable event is an
// ADDITION, not a replacement.
func TestConsoleReleaseSinkStillWritesToALiveFallback(t *testing.T) {
	captureGUIDiagnosticEvents(t)

	var redirected bytes.Buffer
	guiRuntimeStderr.setFallback(&redirected)

	releaseConsoleForBackgroundGUI(func() {})
	fmt.Fprintln(guiRuntimeStderr, "tray: some runtime failure")

	if got := redirected.String(); got == "" {
		t.Fatal("post-release write did not reach the live (redirected) fallback; a run with " +
			"stdout/stderr redirected to a file or pipe must be byte-for-byte unaffected by " +
			"the console release")
	}
}
