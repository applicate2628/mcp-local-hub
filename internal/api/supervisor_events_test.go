// Package api — tests for supervisor-events.log JSONL helper (v0.5.0
// Task 2.3). Mirrors the discipline of gui_event_log_test.go and
// watchdog_log_test.go but exercises the supervisor envelope shape:
// `event` discriminator + `task_name` identity field.
package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// TestSupervisorEvent_EnvelopeShape verifies the wire shape of one
// emitted event: schema_version "1", event discriminator, task_name
// identity field, body object. Mirrors gui_event_log.go:19-25 with
// the supervisor-specific additions (event + task_name).
func TestSupervisorEvent_EnvelopeShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "ipc-command",
		TaskName: `\mcp-local-hub-memory-default`,
		Body:     map[string]any{"cmd": "exit", "result": "ok"},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(string(raw), "\n")
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got["schema_version"] != "1" {
		t.Fatalf("schema_version: %v", got["schema_version"])
	}
	if got["event"] != "ipc-command" {
		t.Fatalf("event: %v", got["event"])
	}
	if got["source"] != "ipc" {
		t.Fatalf("source: %v", got["source"])
	}
	if got["severity"] != "info" {
		t.Fatalf("severity: %v", got["severity"])
	}
	if got["task_name"] != `\mcp-local-hub-memory-default` {
		t.Fatalf("task_name: %v", got["task_name"])
	}
	if _, ok := got["body"].(map[string]any); !ok {
		t.Fatalf("body not object: %T", got["body"])
	}
	if got["ts"] == "" || got["ts"] == nil {
		t.Fatalf("ts not auto-populated: %v", got["ts"])
	}
}

// TestSupervisorEvent_OversizeTruncation verifies the 16KB cap is
// enforced and that identity fields (event, source, task_name) are
// never truncated. Body fields take the hit per the intent_audit.go
// precedent. The _truncated marker must be present.
func TestSupervisorEvent_OversizeTruncation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	big := strings.Repeat("x", 32*1024) // 32KB single field, exceeds 16KB cap
	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "ipc-command",
		TaskName: `\mcp-local-hub-memory-default`,
		Body:     map[string]any{"large": big},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 17*1024 { // entry+newline ≤ 16KB+1024 buffer
		t.Fatalf("entry not truncated: %d bytes", len(raw))
	}
	s := string(raw)
	if !strings.Contains(s, `"_truncated":true`) {
		t.Fatalf("missing _truncated marker; got=%q", s)
	}
	// Identity fields MUST survive untouched per §35 of the precedent.
	if !strings.Contains(s, `"task_name":"\\mcp-local-hub-memory-default"`) {
		t.Fatalf("identity field task_name truncated; got=%q", s)
	}
	if !strings.Contains(s, `"event":"ipc-command"`) {
		t.Fatalf("identity field event truncated; got=%q", s)
	}
	if !strings.Contains(s, `"source":"ipc"`) {
		t.Fatalf("identity field source truncated; got=%q", s)
	}
}

// TestSupervisorEvent_Rotation verifies 10MB rotation: an oversize
// active log is renamed to .1 on next emit. Mirrors the precedent at
// gui_event_log.go:166-170 + intent_audit.go:545-560.
func TestSupervisorEvent_Rotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	// Pre-seed the active log over the rotation threshold (10MB).
	padding := make([]byte, supervisorEventLogRotateSize+1)
	for i := range padding {
		padding[i] = 'a'
	}
	if err := os.WriteFile(path, padding, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "lifecycle",
		Event:    "supervisor-start",
		Body:     map[string]any{"version": "0.5.0"},
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	// .1 must exist (rotated padding) and active log must be small
	// (just the one new entry).
	rotated := path + ".1"
	if _, err := os.Stat(rotated); err != nil {
		t.Fatalf(".1 not created: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("active stat: %v", err)
	}
	if st.Size() >= supervisorEventLogRotateSize {
		t.Fatalf("active log not rotated: size=%d", st.Size())
	}
}

// TestSupervisorEvent_SchemaVersionAutoFilled verifies that callers
// who pass zero-value SchemaVersion get "1" injected.
func TestSupervisorEvent_SchemaVersionAutoFilled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.Emit(SupervisorEvent{
		// Intentionally leave SchemaVersion / TS / Severity blank
		Source: "reconcile",
		Event:  "reconcile-tick",
	}); err != nil {
		t.Fatalf("emit: %v", err)
	}

	raw, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimRight(string(raw), "\n")), &got); err != nil {
		t.Fatal(err)
	}
	if got["schema_version"] != "1" {
		t.Fatalf("schema_version not auto-filled: %v", got["schema_version"])
	}
	if got["ts"] == "" || got["ts"] == nil {
		t.Fatalf("ts not auto-filled: %v", got["ts"])
	}
}

// TestSupervisorEvent_ConcurrentEmit fans 10 goroutines × 100 emits
// against a single logger to verify that the in-process mutex +
// gofrs/flock pairing serializes writes cleanly: exactly 1000 lines
// land, every line is valid JSON, and no line is interleaved with
// another (which would corrupt the JSONL stream). Mirrors the
// concurrency-safety discipline the in-process mutex is meant to
// guarantee (see SupervisorEventLog godoc).
func TestSupervisorEvent_ConcurrentEmit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	const goroutines = 10
	const perGoroutine = 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				_ = logger.Emit(SupervisorEvent{
					Severity: "info",
					Source:   "ipc",
					Event:    "concurrent-test",
					Body:     map[string]any{"goroutine": id, "iter": i},
				})
			}
		}(g)
	}
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("expected %d lines, got %d", goroutines*perGoroutine, len(lines))
	}
	for i, line := range lines {
		var evt SupervisorEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			t.Fatalf("line %d invalid JSON: %v: %q", i, err, line)
		}
	}
}

// TestSupervisorEvent_IdentityOversize verifies the identity-oversize
// gate fires when TaskName exceeds the 1024-byte cap. Identity fields
// (Event/Source/TaskName) are never truncated per §35; Emit must fail
// closed with ErrSupervisorEventIdentityOversize rather than letting
// the post-truncation re-marshal silently land a malformed entry.
// Mirrors the intent_audit.go discipline (plan §51).
func TestSupervisorEvent_IdentityOversize(t *testing.T) {
	dir := t.TempDir()
	logger, err := OpenSupervisorEventLog(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()
	err = logger.Emit(SupervisorEvent{
		Severity: "info",
		Source:   "ipc",
		Event:    "ipc-command",
		TaskName: strings.Repeat("A", 2048), // exceeds 1024 cap
		Body:     map[string]any{},
	})
	if !errors.Is(err, ErrSupervisorEventIdentityOversize) {
		t.Fatalf("expected ErrSupervisorEventIdentityOversize, got %v", err)
	}
}

func TestSupervisorEventLog_TryEmitSkipsContendedLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	lock := flock.New(path + supervisorEventLogLockSuffix)
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor event log: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.TryEmit(SupervisorEvent{
		Severity: "info",
		Source:   "reconcile",
		Event:    "try-emit-contended",
	}); err != nil {
		t.Fatalf("try emit: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("TryEmit under contention wrote log or returned unexpected stat error: %v", err)
	}
}

// TestSupervisorEventLog_EmitWithTimeoutSkipsWedgedLock pins the bounded
// contention behavior EmitWithTimeout adds for the strict-mode audit: with the
// flock held for the whole call, it must (1) report the bounded skip through
// ErrSupervisorEventEmitTimeout, (2) NOT write, and (3) return within a bound of its budget rather
// than blocking indefinitely. The held lock makes the timeout window
// deterministic (every TryLock fails until the deadline).
func TestSupervisorEventLog_EmitWithTimeoutSkipsWedgedLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	lock := flock.New(path + supervisorEventLogLockSuffix)
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor event log: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	const timeout = 150 * time.Millisecond
	start := time.Now()
	err = logger.EmitWithTimeout(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-wedged",
	}, timeout)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeout under a wedged lock error = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	elapsed := time.Since(start)
	// Waited ~the budget (proving it is the bounded-retry path, not an instant
	// TryEmit-style skip)...
	if elapsed < timeout/2 {
		t.Errorf("EmitWithTimeout returned in %s, well under the %s budget — did it actually wait for the flock?", elapsed, timeout)
	}
	// ...but returned bounded — never blocking indefinitely on the held flock.
	if elapsed > 10*time.Second {
		t.Errorf("EmitWithTimeout blocked %s, far past its %s budget — the bound is not enforced", elapsed, timeout)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("EmitWithTimeout under contention wrote the log or returned unexpected stat error: %v", err)
	}
}

// TestSupervisorEventLog_EmitWithTimeoutSkipsContendedMutex verifies the
// timeout covers in-process contention as well as the cross-process flock. A
// blocking Emit may hold mu indefinitely while it waits for a wedged flock,
// so acquiring this mutex must not use an unbounded Lock.
func TestSupervisorEventLog_EmitWithTimeoutSkipsContendedMutex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	logger.mu.Lock()
	defer logger.mu.Unlock()

	const timeout = 50 * time.Millisecond
	start := time.Now()
	err = logger.EmitWithTimeout(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-mutex-contended",
	}, timeout)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeout under in-process contention error = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("EmitWithTimeout blocked %s despite its %s mutex budget", elapsed, timeout)
	}
}

// TestSupervisorEventLog_EmitWithTimeoutWritesUncontended confirms the common
// case: with no contention, EmitWithTimeout durably appends the row (it is NOT
// lossy like TryEmit — the whole point vs the round-2 TryEmit attempt).
func TestSupervisorEventLog_EmitWithTimeoutWritesUncontended(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	if err := logger.EmitWithTimeout(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-ok",
	}, 5*time.Second); err != nil {
		t.Fatalf("EmitWithTimeout uncontended: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if !strings.Contains(string(raw), "emit-timeout-ok") {
		t.Fatalf("EmitWithTimeout uncontended did not write the event; log=%q", raw)
	}
}

// TestSupervisorEventLog_EmitWithTimeoutBoundsStalledWrite reproduces the
// P1-4 review finding: EmitWithTimeout previously bounded only lock
// ACQUISITION (the mutex + flock), never the rotation-check/open/write/close
// that follows once both locks are held. A filesystem or antivirus stall
// INSIDE that locked body — ordinary syscalls with no cancellable surface —
// could therefore block the caller indefinitely despite the documented
// "never hang forever" contract. This is exactly what blocks
// RequestSelfRestartExit (internal/gui/gui_self_restart.go), which emits
// synchronously immediately before os.Exit(0).
//
// Uses the injectable supervisorEventWriteFn seam to simulate a write that
// blocks past the caller's whole timeout budget, then asserts
// EmitWithTimeout still returns within a bound close to that budget instead
// of waiting for the injected write to finish.
//
// MUTATION: revert emit's emitTimeout branch to call
// supervisorEventWriteFn/writeEventLine synchronously (no goroutine/select) —
// this test then blocks until the injected write unblocks (well past its
// budget) and the elapsed-time assertion fails.
func TestSupervisorEventLog_EmitWithTimeoutBoundsStalledWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	release := make(chan struct{})
	restore := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		<-release // simulates a filesystem/AV stall that outlives the caller's budget
		return l.writeEventLine(raw)
	})
	defer func() {
		close(release) // let the abandoned worker goroutine finish so it releases l's locks and does not leak past this test
		restore()
	}()

	const timeout = 150 * time.Millisecond
	start := time.Now()
	err = logger.EmitWithTimeout(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-stalled-write",
	}, timeout)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeout with a stalled write error = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	if elapsed > time.Second {
		t.Errorf("EmitWithTimeout blocked %s despite its %s budget — the write phase is not bounded (P1-4 review finding)", elapsed, timeout)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("EmitWithTimeout returned before the stalled write committed, but the log already exists: %v", statErr)
	}
}
