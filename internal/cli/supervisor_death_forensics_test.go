package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// ---------------------------------------------------------------------------
// Change 2 — process-wide goroutine panic capture.
// ---------------------------------------------------------------------------

// TestGuardSupervisorGoroutine_EmitsEventAndReRaises is the core contract:
// a panic on a NON-dispatcher goroutine must produce a durable
// `supervisor-goroutine-panic` row AND still kill the goroutine.
//
// Both halves matter. Without the event the death is silent (the defect).
// Without the re-raise the supervisor would continue past a panic with
// half-applied state — strictly worse than crashing.
func TestGuardSupervisorGoroutine_EmitsEventAndReRaises(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, api.SupervisorEventLogFileLeaf)
	events, err := api.OpenSupervisorEventLog(logPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	reRaised := make(chan any, 1)
	done := make(chan struct{})

	go func() {
		defer close(done)
		// Outer recover observes the guard's RE-RAISE. If the guard
		// swallowed the panic this channel stays empty and the test fails.
		defer func() { reRaised <- recover() }()
		func() {
			defer guardSupervisorGoroutine(events, "unit-test-worker", "\\mcp-local-hub-probe")
			panicFromNestedFrame()
		}()
	}()

	<-done
	got := <-reRaised
	if got == nil {
		t.Fatal("guard SWALLOWED the panic; contract is capture-then-re-raise, never suppress")
	}
	if fmt.Sprint(got) != "forensics-probe-panic" {
		t.Fatalf("re-raised value = %v, want the original panic value", got)
	}

	entry := findSupervisorEvent(t, logPath, "supervisor-goroutine-panic")
	if entry.Severity != "error" {
		t.Fatalf("severity = %q, want error", entry.Severity)
	}
	if entry.TaskName != "\\mcp-local-hub-probe" {
		t.Fatalf("task_name = %q, want the guarded task", entry.TaskName)
	}
	if role, _ := entry.Body["role"].(string); role != "unit-test-worker" {
		t.Fatalf("body.role = %q, want unit-test-worker", role)
	}
	if rec, _ := entry.Body["recovered"].(string); rec != "forensics-probe-panic" {
		t.Fatalf("body.recovered = %q, want the panic value", rec)
	}
	stack, _ := entry.Body["stack"].(string)
	if !strings.Contains(stack, "panicFromNestedFrame") {
		t.Fatalf("body.stack lost the ORIGINAL panicking frame; a stack that only shows the recover site has no forensic value.\nstack:\n%s", stack)
	}
}

// TestGuardSupervisorGoroutine_NoPanicIsInert proves the guard costs
// nothing on the healthy path — it must not emit or interfere when the
// guarded goroutine returns normally.
func TestGuardSupervisorGoroutine_NoPanicIsInert(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, api.SupervisorEventLogFileLeaf)
	events, err := api.OpenSupervisorEventLog(logPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	func() {
		defer guardSupervisorGoroutine(events, "healthy-worker", "")
	}()

	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("guard emitted on the healthy path (log exists), stat err = %v", err)
	}
}

// TestGuardSupervisorGoroutine_NilEventLogStillReRaises proves the guard is
// never itself a source of failure: with no event log it still re-raises
// rather than panicking inside the guard or swallowing.
func TestGuardSupervisorGoroutine_NilEventLogStillReRaises(t *testing.T) {
	reRaised := make(chan any, 1)
	func() {
		defer func() { reRaised <- recover() }()
		func() {
			defer guardSupervisorGoroutine(nil, "no-log-worker", "")
			panic("nil-log-probe")
		}()
	}()
	if got := <-reRaised; got == nil {
		t.Fatal("guard with a nil event log swallowed the panic")
	}
}

func panicFromNestedFrame() { panic("forensics-probe-panic") }

// TestSuperviseLongLivedGoroutinesAreGuarded is a STRUCTURAL regression
// guard over the supervisor composition root. As of this change supervise.go
// contains 15 `go` statements and all 15 are guarded.
//
// The defect being prevented is not a single missing guard — it is a future
// goroutine added WITHOUT one, silently re-opening the forensic gap. Every
// `go` statement in supervise.go must defer guardSupervisorGoroutine, so a
// new unguarded long-lived goroutine fails this test at review time rather
// than during an unexplained production death.
//
// COVERAGE IS FILE-SCOPED, and that is a real limit worth stating: this test
// parses supervise.go ONLY. A long-lived supervisor goroutine launched from
// any other file in the package — or moved out of supervise.go by a future
// refactor — leaves this test's coverage silently, with no failure to warn
// anyone. The guard helper itself is package-wide; only the enforcement is
// file-scoped. Widening enforcement to the whole package would need an
// allowlist for the many short-lived test/helper goroutines, which is why it
// is scoped here rather than being scoped by accident.
func TestSuperviseLongLivedGoroutinesAreGuarded(t *testing.T) {
	const src = "supervise.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var unguarded []string
	ast.Inspect(file, func(n ast.Node) bool {
		goStmt, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		pos := fset.Position(goStmt.Pos())
		lit, ok := goStmt.Call.Fun.(*ast.FuncLit)
		if !ok {
			// `go f(...)` cannot carry a guard: the guard must be deferred
			// INSIDE the goroutine. Wrap it in a closure that defers one.
			unguarded = append(unguarded, fmt.Sprintf("%s:%d: bare `go f(...)` launch — wrap in a closure that defers guardSupervisorGoroutine", src, pos.Line))
			return true
		}
		if !funcLitDefersGuard(lit) {
			unguarded = append(unguarded, fmt.Sprintf("%s:%d: goroutine closure does not defer guardSupervisorGoroutine", src, pos.Line))
		}
		return true
	})

	if len(unguarded) > 0 {
		t.Fatalf("unguarded goroutine launch(es) in the supervisor composition root — a panic on any of these kills the supervisor with NO event and NO supervisor-exit row:\n  %s",
			strings.Join(unguarded, "\n  "))
	}
}

// funcLitDefersGuard reports whether the func literal's own body defers
// guardSupervisorGoroutine. Only the literal's TOP-LEVEL statements are
// examined on purpose: a guard nested inside an inner closure would not
// protect the goroutine itself.
func funcLitDefersGuard(lit *ast.FuncLit) bool {
	if lit.Body == nil {
		return false
	}
	for _, stmt := range lit.Body.List {
		deferStmt, ok := stmt.(*ast.DeferStmt)
		if !ok {
			continue
		}
		ident, ok := deferStmt.Call.Fun.(*ast.Ident)
		if ok && ident.Name == "guardSupervisorGoroutine" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Change 1 — stderr sink captures a real Go runtime panic.
// ---------------------------------------------------------------------------

// forensicsSinkChildEnv, when set, makes the test binary act as a child
// that installs the sink and then panics for real.
const forensicsSinkChildEnv = "MCPHUB_TEST_STDERR_SINK_CHILD_STATEDIR"

// forensicsSinkChildModeEnv selects WHICH goroutine the child panics on.
// The two modes cover structurally different capture paths and neither
// substitutes for the other:
//
//	"goroutine" (default) — panic on a background goroutine. No deferred
//	    function of the panicking goroutine touches the sink, so capture
//	    depends only on the std-handle redirect still being installed.
//	"main" — panic on the goroutine that owns the sink's deferred release.
//	    Capture depends on that defer being panic-AWARE. A plain
//	    `defer sink.release()` fails this mode and passes the other, which
//	    is exactly how the hole survived the first round of review.
const forensicsSinkChildModeEnv = "MCPHUB_TEST_STDERR_SINK_CHILD_MODE"

// runForensicsSinkCrashChild is the crash-child body invoked from the
// package's single TestMain (settings_registry_test.go) when
// forensicsSinkChildEnv is set. It installs the sink and then dies of a
// genuine unrecovered runtime panic. A real subprocess is the only honest
// way to prove runtime-panic capture: an in-process test cannot let the Go
// runtime actually terminate the process, and it is the runtime's own
// writer — not any code of ours — whose behavior is under test.
//
// Never returns.
func runForensicsSinkCrashChild(stateDir string) {
	if os.Getenv(forensicsSinkChildModeEnv) == "main" {
		_ = forensicsSinkCrashOnMainGoroutine(stateDir)
		os.Exit(4) // unreachable: the panic above kills the process
	}

	sink := openSupervisorStderrSink(stateDir)
	if !sink.redirected {
		// Report the refusal through the ORIGINAL stderr so the parent can
		// explain the failure instead of timing out mysteriously.
		fmt.Fprintf(os.Stderr, "CHILD-SINK-NOT-REDIRECTED reason=%s err=%v\n", sink.reason, sink.err)
		os.Exit(3)
	}
	// A genuine unrecovered runtime panic on a NON-main, NON-dispatcher
	// goroutine — the exact shape that left zero trace in the forensic
	// window.
	go func() { panic("forensics-child-runtime-panic") }()
	time.Sleep(30 * time.Second) // killed by the panic long before this
	os.Exit(4)
}

// forensicsSinkCrashOnMainGoroutine mirrors runSupervise's EXACT shape —
// named error return, `defer sink.releaseOnExit(&err)`, then a panic on the
// same goroutine that registered that defer. Any divergence here would make
// the test prove something other than the production path.
func forensicsSinkCrashOnMainGoroutine(stateDir string) (err error) {
	sink := openSupervisorStderrSink(stateDir)
	if !sink.redirected {
		fmt.Fprintf(os.Stderr, "CHILD-SINK-NOT-REDIRECTED reason=%s err=%v\n", sink.reason, sink.err)
		os.Exit(3)
	}
	defer sink.releaseOnExit(&err)
	panic("forensics-child-main-goroutine-panic")
}

// TestSupervisorStderrSink_CapturesRuntimePanic is the end-to-end proof of
// the sink's whole reason to exist: a Go runtime panic on an ordinary
// goroutine must land on disk.
//
// This is deliberately a real subprocess with a real unrecovered panic and
// a real process death. Anything less would prove only that we can write to
// a file, not that the RUNTIME's own writer follows the redirect.
func TestSupervisorStderrSink_CapturesRuntimePanic(t *testing.T) {
	stateDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorStderrSink_CapturesRuntimePanic")
	cmd.Env = append(os.Environ(), forensicsSinkChildEnv+"="+stateDir)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("child exited 0; it was expected to die of an unrecovered panic.\nchild output:\n%s", out)
	}
	if strings.Contains(string(out), "CHILD-SINK-NOT-REDIRECTED") {
		t.Fatalf("child refused to install the sink:\n%s", out)
	}

	sinkPath := filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf)
	raw, readErr := os.ReadFile(sinkPath)
	if readErr != nil {
		t.Fatalf("sink file absent after child death — the forensic gap is still open: %v\nchild output:\n%s", readErr, out)
	}
	content := string(raw)

	if !strings.Contains(content, "panic: forensics-child-runtime-panic") {
		t.Fatalf("sink did not capture the runtime panic MESSAGE.\nsink:\n%s", content)
	}
	if !strings.Contains(content, "goroutine") {
		t.Fatalf("sink did not capture the goroutine TRACEBACK.\nsink:\n%s", content)
	}
	if !strings.Contains(content, "session-start=") {
		t.Fatalf("sink missing the session banner that attributes the panic to a pid/session.\nsink:\n%s", content)
	}
	if !strings.Contains(content, fmt.Sprintf("pid=%d", cmd.Process.Pid)) {
		t.Fatalf("session banner does not name the child pid %d.\nsink:\n%s", cmd.Process.Pid, content)
	}
}

// TestSupervisorStderrSink_CapturesMainGoroutinePanic closes the hole the
// background-goroutine test structurally could not see.
//
// runSupervise's own goroutine runs the whole of startup, the final select
// loop, and every signal / IPC-exit handler — so it is the MOST likely place
// for a supervisor to die, and plausibly the shape of the 19 s and 59 s
// sessions in the forensic window. Go runs a panicking goroutine's defers
// BEFORE printing the traceback, so a non-panic-aware
// `defer sink.release()` restores the (detached ⇒ nowhere) stderr first and
// the traceback is lost.
//
// MUTATION PROOF: replace releaseOnExit's recover branch with a plain
// s.release() and this test FAILS while
// TestSupervisorStderrSink_CapturesRuntimePanic still PASSES.
func TestSupervisorStderrSink_CapturesMainGoroutinePanic(t *testing.T) {
	stateDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestSupervisorStderrSink_CapturesMainGoroutinePanic")
	cmd.Env = append(os.Environ(),
		forensicsSinkChildEnv+"="+stateDir,
		forensicsSinkChildModeEnv+"=main",
	)
	out, err := cmd.CombinedOutput()

	if err == nil {
		t.Fatalf("child exited 0; a re-raised panic must still kill the process (never swallowed).\nchild output:\n%s", out)
	}
	if strings.Contains(string(out), "CHILD-SINK-NOT-REDIRECTED") {
		t.Fatalf("child refused to install the sink:\n%s", out)
	}

	raw, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf))
	if readErr != nil {
		t.Fatalf("sink file absent after main-goroutine death: %v\nchild output:\n%s", readErr, out)
	}
	content := string(raw)

	// The load-bearing assertion: the RUNTIME traceback, not merely our own
	// marker, must be in the sink.
	if !strings.Contains(content, "panic: forensics-child-main-goroutine-panic") {
		t.Fatalf("sink did not capture the MAIN-goroutine runtime panic — the deferred release restored stderr before the runtime printed.\nsink:\n%s", content)
	}
	if !strings.Contains(content, "goroutine") {
		t.Fatalf("sink captured the panic line but not the traceback.\nsink:\n%s", content)
	}
	// Proves the value was preserved through recover/re-raise rather than
	// replaced or swallowed.
	if !strings.Contains(content, "recovered") {
		t.Fatalf("expected a re-raised panic marker (recovered/repanicked) proving capture-then-re-raise.\nsink:\n%s", content)
	}
	if !strings.Contains(content, "MAIN-GOROUTINE PANIC=") {
		t.Fatalf("sink missing the main-goroutine panic marker that distinguishes it from a background-goroutine death.\nsink:\n%s", content)
	}
	// The traceback must NOT have leaked to the original stderr instead.
	if strings.Contains(string(out), "panic: forensics-child-main-goroutine-panic") {
		t.Fatalf("traceback went to the child's ORIGINAL stderr, not the sink — under detached autostart that is the void.\nchild output:\n%s", out)
	}
}

// TestSupervisorStderrSink_RecordsErrorExit proves a non-panic error return
// is distinguishable from a hard death in the sink.
//
// Without this, an early startup failure left the sink holding a bare
// session banner with no marker — byte-identical to what a crash leaves,
// which would send an operator hunting a crash that never happened. cobra
// prints the returned error only AFTER runSupervise returns, by which point
// the restore has already redirected it to the detached void.
func TestSupervisorStderrSink_RecordsErrorExit(t *testing.T) {
	stateDir := t.TempDir()
	sink := openSupervisorStderrSink(stateDir)
	if !sink.redirected {
		sink.release()
		t.Skipf("stderr not redirectable in this environment (reason=%q)", sink.reason)
	}

	// Mirror runSupervise's shape: named return + deferred releaseOnExit.
	func() (err error) {
		defer sink.releaseOnExit(&err)
		return fmt.Errorf("probe-startup-failure")
	}()

	raw, readErr := os.ReadFile(filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf))
	if readErr != nil {
		t.Fatalf("read sink: %v", readErr)
	}
	content := string(raw)
	if !strings.Contains(content, "error-exit=probe-startup-failure") {
		t.Fatalf("sink does not record the error return; a startup failure is indistinguishable from a hard death.\nsink:\n%s", content)
	}
	if sink.redirected {
		t.Fatal("releaseOnExit must still release on a non-panic error return")
	}
}

// TestSupervisorStderrSink_ReleaseOnExitCleanPathReleases guards the
// ordinary success path: no panic, no error, so the sink must be released
// normally (the handle-leak fix must survive the panic-aware rework).
func TestSupervisorStderrSink_ReleaseOnExitCleanPathReleases(t *testing.T) {
	stateDir := t.TempDir()
	sink := openSupervisorStderrSink(stateDir)
	if !sink.redirected {
		sink.release()
		t.Skipf("stderr not redirectable in this environment (reason=%q)", sink.reason)
	}

	func() (err error) {
		defer sink.releaseOnExit(&err)
		return nil
	}()

	if sink.redirected {
		t.Fatal("clean return must release the sink; an unreleased handle makes the file undeletable on Windows")
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf))
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if strings.Contains(string(raw), "error-exit=") {
		t.Fatalf("clean return must not write an error marker.\nsink:\n%s", string(raw))
	}
}

// TestSupervisorStderrSink_RotatesAtOpenBoundary proves the sink honors the
// 10 MB -> .1 discipline when it is opened, so restarts cannot grow it
// without bound.
func TestSupervisorStderrSink_RotatesAtOpenBoundary(t *testing.T) {
	stateDir := t.TempDir()
	sinkPath := filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf)

	f, err := os.Create(sinkPath)
	if err != nil {
		t.Fatalf("seed sink: %v", err)
	}
	if err := f.Truncate(api.SupervisorStderrSinkRotateSizeBytes); err != nil {
		t.Fatalf("grow sink to threshold: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close seeded sink: %v", err)
	}

	sink := openSupervisorStderrSink(stateDir)
	t.Cleanup(sink.release)

	if !sink.rotated {
		t.Fatalf("sink at the 10MB boundary was not rotated (reason=%q)", sink.reason)
	}
	if _, err := os.Stat(sinkPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup: %v", err)
	}
	st, err := os.Stat(sinkPath)
	if err != nil {
		t.Fatalf("expected a fresh active sink: %v", err)
	}
	if st.Size() >= api.SupervisorStderrSinkRotateSizeBytes {
		t.Fatalf("active sink still oversize after rotation: %d bytes", st.Size())
	}
}

// TestSupervisorStderrSink_WritesSessionBanner proves every session is
// attributable, which is what makes "banner with no matching graceful-exit
// marker" usable as death evidence.
func TestSupervisorStderrSink_WritesSessionBanner(t *testing.T) {
	stateDir := t.TempDir()
	sink := openSupervisorStderrSink(stateDir)
	t.Cleanup(sink.release)

	if !sink.redirected {
		t.Skipf("stderr not redirectable in this environment (reason=%q); subprocess test covers capture", sink.reason)
	}
	sink.noteGracefulExit("unit-test")

	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf))
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, fmt.Sprintf("pid=%d", os.Getpid())) {
		t.Fatalf("banner missing this process pid:\n%s", content)
	}
	if !strings.Contains(content, "graceful-exit=unit-test") {
		t.Fatalf("graceful-exit marker missing — a clean shutdown would be indistinguishable from a death:\n%s", content)
	}
}

// TestSupervisorStderrSink_RestoreLeavesProcessStderrUsable guards the test
// harness itself. openSupervisorStderrSink mutates PROCESS-WIDE state; if
// the restore were wrong, every later test in this binary would lose its
// stderr silently. This asserts that after restore, a write to stderr no
// longer reaches the sink file.
func TestSupervisorStderrSink_RestoreLeavesProcessStderrUsable(t *testing.T) {
	stateDir := t.TempDir()
	sink := openSupervisorStderrSink(stateDir)
	if !sink.redirected {
		sink.release()
		t.Skipf("stderr not redirectable in this environment (reason=%q)", sink.reason)
	}

	sink.release()

	const marker = "POST-RESTORE-MUST-NOT-REACH-SINK"
	fmt.Fprintln(os.Stderr, marker)

	raw, err := os.ReadFile(filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf))
	if err != nil {
		t.Fatalf("read sink: %v", err)
	}
	if strings.Contains(string(raw), marker) {
		t.Fatal("process stderr still points at the sink after restore; later tests would lose their stderr")
	}
}

// ---------------------------------------------------------------------------
// Change 3 — heartbeat.
// ---------------------------------------------------------------------------

// TestRunSupervisorHeartbeat_BeatsImmediatelyAndOnCadence pins the two
// properties the death-detection argument depends on: a beat at t=0 (so a
// sub-interval session still proves it existed) and a beat per interval
// thereafter (so absence is bounded evidence of death).
func TestRunSupervisorHeartbeat_BeatsImmediatelyAndOnCadence(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, api.SupervisorEventLogFileLeaf)
	events, err := api.OpenSupervisorEventLog(logPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned("\\mcp-local-hub-alpha", 4242, time.Now().UTC())
	tracker.MarkSpawned("\\mcp-local-hub-beta", 4243, time.Now().UTC())
	tracker.MarkSpawnFailed("\\mcp-local-hub-gamma", fmt.Errorf("probe failure"))

	done := make(chan struct{})
	go runSupervisorHeartbeat(done, events, tracker, "", time.Now().UTC().Add(-90*time.Second), 20*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	var beats []supervisorEventEntry
	for time.Now().Before(deadline) {
		beats = readSupervisorEvents(t, logPath, "supervisor-heartbeat")
		if len(beats) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(done)

	if len(beats) < 3 {
		t.Fatalf("got %d heartbeat rows, want >= 3 (immediate beat + cadence beats)", len(beats))
	}

	first := beats[0]
	if first.Severity != "info" {
		t.Fatalf("heartbeat severity = %q, want info", first.Severity)
	}
	if first.Source != "lifecycle" {
		t.Fatalf("heartbeat source = %q, want lifecycle", first.Source)
	}
	if got := numberField(t, first, "daemon_count"); got != 3 {
		t.Fatalf("daemon_count = %v, want 3 (two running + one spawn-failed)", got)
	}
	if got := numberField(t, first, "running_daemon_count"); got != 2 {
		t.Fatalf("running_daemon_count = %v, want 2; a spawn-failed daemon must not count as running", got)
	}
	if got := numberField(t, first, "uptime_seconds"); got < 89 {
		t.Fatalf("uptime_seconds = %v, want >= 89 for a 90s-old session", got)
	}
	if got := numberField(t, first, "pid"); int(got) != os.Getpid() {
		t.Fatalf("pid = %v, want %d", got, os.Getpid())
	}
}

// TestRunSupervisorHeartbeat_StopsOnDone proves the heartbeat does not
// outlive the supervisor loop — a heartbeat emitted after shutdown would be
// a false liveness signal, the exact failure mode this work removes.
func TestRunSupervisorHeartbeat_StopsOnDone(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, api.SupervisorEventLogFileLeaf)
	events, err := api.OpenSupervisorEventLog(logPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		runSupervisorHeartbeat(done, events, NewDaemonRuntimeTracker(), "", time.Now().UTC(), 15*time.Millisecond)
	}()

	time.Sleep(60 * time.Millisecond)
	close(done)

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat goroutine did not stop after done closed")
	}

	before := len(readSupervisorEvents(t, logPath, "supervisor-heartbeat"))
	time.Sleep(100 * time.Millisecond)
	if after := len(readSupervisorEvents(t, logPath, "supervisor-heartbeat")); after != before {
		t.Fatalf("heartbeat kept emitting after shutdown: %d -> %d rows", before, after)
	}
}

// TestSupervisorHeartbeatInterval_FitsRotationBudget locks the interval
// justification into a test so a future retune must re-argue it. At the
// documented ~260 bytes/row the active 10 MB log must hold well over the
// 42-hour forensic window that motivated this work.
func TestSupervisorHeartbeatInterval_FitsRotationBudget(t *testing.T) {
	const approxRowBytes = 260
	rowsPerDay := int64(24*time.Hour) / int64(supervisorHeartbeatInterval)
	bytesPerDay := rowsPerDay * approxRowBytes
	daysOfHistory := api.SupervisorStderrSinkRotateSizeBytes / bytesPerDay

	if daysOfHistory < 7 {
		t.Fatalf("heartbeat interval %v yields only ~%d days of history in a 10MB log (%d rows/day); the forensic window needs far more",
			supervisorHeartbeatInterval, daysOfHistory, rowsPerDay)
	}
	if supervisorHeartbeatInterval > 2*time.Minute {
		t.Fatalf("heartbeat interval %v exceeds 2m; death could not be localized within the ~60s liveness-task relaunch cadence",
			supervisorHeartbeatInterval)
	}
}

// ---------------------------------------------------------------------------
// Shared test helpers.
// ---------------------------------------------------------------------------

type supervisorEventEntry struct {
	Severity string         `json:"severity"`
	Source   string         `json:"source"`
	Event    string         `json:"event"`
	TaskName string         `json:"task_name"`
	Body     map[string]any `json:"body"`
}

func readSupervisorEvents(t *testing.T, logPath, event string) []supervisorEventEntry {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("open event log: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []supervisorEventEntry
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry supervisorEventEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue // a partially-flushed line during a live read is not a failure
		}
		if entry.Event == event {
			out = append(out, entry)
		}
	}
	return out
}

func findSupervisorEvent(t *testing.T, logPath, event string) supervisorEventEntry {
	t.Helper()
	entries := readSupervisorEvents(t, logPath, event)
	if len(entries) == 0 {
		t.Fatalf("no %q event found in %s", event, logPath)
	}
	return entries[0]
}

func numberField(t *testing.T, entry supervisorEventEntry, key string) float64 {
	t.Helper()
	raw, ok := entry.Body[key]
	if !ok {
		t.Fatalf("body missing %q; body = %v", key, entry.Body)
	}
	num, ok := raw.(float64)
	if !ok {
		t.Fatalf("body[%q] = %v (%T), want a number", key, raw, raw)
	}
	return num
}
