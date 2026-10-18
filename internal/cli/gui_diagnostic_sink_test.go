package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"

	"mcp-local-hub/internal/api"
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

// This pins the legacy sink projection with a real closed io.Pipe reader.
// It is a fallback-delivery counterexample, not lower API append evidence.
func TestQuit041LegacySinkDiscardsActualFallbackFailure(t *testing.T) {
	reader, writer := io.Pipe()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	payload := []byte("fixture required diagnostic\n")
	if _, err := writer.Write(payload); err != io.ErrClosedPipe {
		t.Fatalf("fixture did not produce a real closed-pipe failure: %v", err)
	}
	sink := newConsoleReleaseSink(writer)
	n, err := sink.Write(payload)
	if n != len(payload) || err != nil {
		t.Fatalf("legacy outward projection changed: n=%d err=%v", n, err)
	}
	t.Log("actual fallback failure is absent from legacy Write result; required delivery needs an explicit receiving projection")
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

type quit041FallbackWriter struct {
	calls  *[]string
	writer io.Writer
	short  bool
}

func (w *quit041FallbackWriter) Write(p []byte) (int, error) {
	*w.calls = append(*w.calls, "fallback")
	if w.short {
		return len(p) - 1, nil
	}
	return w.writer.Write(p)
}

// This invokes the ACTUAL public required API and production marshal failure;
// it never substitutes a bare sentinel for the API producer. Shared append
// Sync/Close failures are received separately in API-package effects tests.
func TestQuit041RequiredSinkReceivesAPIAndFallbackCauses(t *testing.T) {
	original := logGUIDiagnosticEventRequired
	t.Cleanup(func() { logGUIDiagnosticEventRequired = original })
	reader, writer := io.Pipe()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var calls []string
	var producerErr error
	logGUIDiagnosticEventRequired = func(level, event string, fields map[string]any) error {
		calls = append(calls, "durable")
		producerErr = api.LogHubMcpEventRequired(level, event, map[string]any{"invalid": make(chan struct{})})
		return producerErr
	}
	sink := newConsoleReleaseSink(&quit041FallbackWriter{calls: &calls, writer: writer})
	sink.engageDurableSink()
	payload := []byte("fixture required diagnostic\n")
	n, err := sink.WriteRequired(payload)
	var unsupported *json.UnsupportedTypeError
	if n != len(payload) || producerErr == nil || !errors.Is(err, producerErr) || !errors.As(err, &unsupported) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("actual producer/fallback outcomes lost: n=%d err=%v", n, err)
	}
	if fmt.Sprint(calls) != "[durable fallback]" {
		t.Fatalf("destinations reordered/skipped: %v", calls)
	}
}

func TestQuit041RequiredSinkPreReleaseBlankAndShortFallback(t *testing.T) {
	original := logGUIDiagnosticEventRequired
	t.Cleanup(func() { logGUIDiagnosticEventRequired = original })
	durableCalls := 0
	logGUIDiagnosticEventRequired = func(string, string, map[string]any) error { durableCalls++; return nil }
	var calls []string
	sink := newConsoleReleaseSink(&quit041FallbackWriter{calls: &calls, short: true})
	if _, err := sink.WriteRequired([]byte("before release")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("pre-release fallback short write lost")
	}
	sink.engageDurableSink()
	if _, err := sink.WriteRequired([]byte("\r\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("blank fallback short write lost")
	}
	if durableCalls != 0 || len(calls) != 2 {
		t.Fatal("pre-release/blank routing changed")
	}
}
