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
	"sync/atomic"
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
	// workerCommitDelay makes the teardown join below a DETERMINISTIC assertion
	// instead of a scheduling coin-flip. The underlying defect (review finding:
	// teardown restored the seam and returned while the abandoned worker was
	// still inside writeEventLine, creating the log file underneath t.TempDir's
	// concurrent RemoveAll — "TempDir RemoveAll cleanup: directory not empty")
	// only manifests when the worker happens to lose that race, which is
	// host-dependent: it reproduced 4/20 in review but 0/40 here. Holding the
	// worker for a bounded delay AFTER the release removes the luck: an
	// unjoined teardown returns microseconds after close(release) and therefore
	// CANNOT observe workerCommitted, while a joined one always can.
	const workerCommitDelay = 50 * time.Millisecond
	var workerCommitted atomic.Bool
	restore := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		<-release // simulates a filesystem/AV stall that outlives the caller's budget
		time.Sleep(workerCommitDelay)
		writeErr := l.writeEventLine(raw)
		workerCommitted.Store(true)
		return writeErr
	})
	defer func() {
		close(release) // let the abandoned worker goroutine finish so it releases l's locks and does not leak past this test
		// JOIN the abandoned worker before this test returns — closing
		// `release` only UNBLOCKS it. logger.mu is the precise join point: the
		// worker registers `defer l.mu.Unlock()` FIRST, so it runs LAST.
		// Acquiring it here therefore proves the worker finished the write AND
		// released the flock, i.e. it can no longer touch anything under dir
		// when t.TempDir's RemoveAll fires. If no worker was ever spawned (an
		// earlier assertion failed), the mutex is free and this is a no-op
		// rather than a deadlock.
		logger.mu.Lock()
		logger.mu.Unlock()
		if !workerCommitted.Load() {
			t.Errorf("teardown returned without joining the abandoned write worker: the worker can still create files under %s while t.TempDir removes it", dir)
		}
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

// TestSupervisorEventLog_EmitWithTimeoutTrackedPendingObservesLateWrite
// reproduces residual 2 (P1, review round 3): EmitWithTimeout's timeout does
// NOT mean the abandoned worker's write was lost — writeEventLine has no
// cancellable syscall surface, so the worker keeps running and (usually)
// finishes shortly after. EmitWithTimeoutTracked's returned
// *PendingSupervisorEventEmit must let a caller observe that SAME write's
// eventual completion instead of having no way to know the write actually
// landed.
//
// MUTATION: revert EmitWithTimeoutTracked to discard the pending handle
// (`_, err := l.emit(...); return nil, err`) — this test's `pending` is nil
// and Wait's nil-receiver contract returns ErrSupervisorEventEmitTimeout
// immediately instead of observing the release, so the "Wait returns nil
// once released" assertion fails.
func TestSupervisorEventLog_EmitWithTimeoutTrackedPendingObservesLateWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	release := make(chan struct{})
	var releaseOnce sync.Once
	safeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	restore := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		<-release // simulates a filesystem/AV stall that outlives the caller's budget
		return l.writeEventLine(raw)
	})
	defer func() {
		safeRelease() // no-op if the in-test goroutine below already released it
		// Same join as the sibling test above. Since review finding 2 the
		// worker sends on `done` only AFTER both releases, so by the time
		// pending.Wait has returned logger.mu is already free and this join is
		// a cheap backstop rather than the load-bearing wait it used to be. It
		// is kept because the join must still hold on the paths where no Wait
		// ran at all (an earlier assertion failed before it), where the worker
		// could otherwise still touch dir when t.TempDir's RemoveAll fires.
		logger.mu.Lock()
		logger.mu.Unlock()
		restore()
	}()

	const timeout = 150 * time.Millisecond
	start := time.Now()
	pending, err := logger.EmitWithTimeoutTracked(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-tracked-stalled-write",
	}, timeout)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeoutTracked with a stalled write error = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	if elapsed > time.Second {
		t.Errorf("EmitWithTimeoutTracked blocked %s despite its %s budget", elapsed, timeout)
	}
	if pending == nil {
		t.Fatalf("pending handle is nil after a genuine timeout; want a non-nil handle to await the abandoned worker")
	}

	// Release the stalled write from a separate goroutine WHILE Wait is
	// blocking, so this test genuinely exercises "late worker release"
	// rather than releasing before Wait is even called.
	go func() {
		time.Sleep(50 * time.Millisecond)
		safeRelease()
	}()
	waitErr := pending.Wait(2 * time.Second)
	if waitErr != nil {
		t.Fatalf("pending.Wait after the late release = %v, want nil (the original write succeeded)", waitErr)
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read log: %v", readErr)
	}
	if got := strings.Count(string(raw), "emit-timeout-tracked-stalled-write"); got != 1 {
		t.Fatalf("event log has %d rows for this event, want exactly 1", got)
	}
}

// TestSupervisorEventLog_GenuineFlockErrorReleasesMutexExactlyOnce reproduces
// the P1 review finding on this branch: emit's emitTimeout arm unlocked l.mu
// on the `!locked` path and then FELL THROUGH into the shared `lockErr != nil`
// arm, which unlocked it a SECOND time. gofrs/flock returns (false, err) —
// never (false, nil) — when its own setFh cannot open the lock file (v0.13.0
// flock_windows.go:140-146 / flock.go tryCtx), so any genuine non-timeout
// flock error (state directory gone, path inaccessible) took that path. The
// second unlock of an already-unlocked sync.Mutex is an UNRECOVERABLE runtime
// fatal, not a catchable panic — daemon recovery emits its audit row after
// terminating a daemon but before respawning it, so the fatal left the daemon
// stopped.
//
// A missing parent directory is the deterministic, portable way to force that
// genuine error: flock.New opens the lock path with O_CREATE, which cannot
// create a file inside a directory that does not exist.
//
// All three emit modes are exercised, not just the reported emitTimeout one:
// the defect is a lock-OWNERSHIP defect, and every mode reaches the same
// shared release discipline.
//
// MUTATION: restore the pre-fix shape in emit's emitTimeout arm — unlock l.mu
// inside `if !locked` and let a genuine error fall through to a second
// `l.mu.Unlock()`. The emitTimeout subtest then dies with
// `fatal error: sync: unlock of unlocked mutex` and takes the whole test
// binary with it.
func TestSupervisorEventLog_GenuineFlockErrorReleasesMutexExactlyOnce(t *testing.T) {
	cases := []struct {
		name string
		emit func(l *SupervisorEventLog, evt SupervisorEvent) error
	}{
		{"Emit", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.Emit(evt)
		}},
		{"TryEmit", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.TryEmit(evt)
		}},
		{"EmitWithTimeout", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.EmitWithTimeout(evt, 5*time.Second)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The parent directory deliberately does NOT exist, so the flock's
			// own O_CREATE open fails with a genuine, non-timeout error.
			path := filepath.Join(t.TempDir(), "no-such-dir", "supervisor-events.log")
			logger, err := OpenSupervisorEventLog(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = logger.Close() }()

			evt := SupervisorEvent{
				Severity: "info",
				Source:   "autostart",
				Event:    "emit-genuine-flock-error",
			}

			emitErr := tc.emit(logger, evt)
			if emitErr == nil {
				t.Fatalf("%s over an unopenable lock path returned nil; want a reported flock error", tc.name)
			}
			// A genuine flock error must be reported AS ITSELF, never laundered
			// into the bounded-skip sentinel — a caller that sees
			// ErrSupervisorEventEmitTimeout is told to retry/await a worker that
			// was never spawned.
			if errors.Is(emitErr, ErrSupervisorEventEmitTimeout) {
				t.Fatalf("%s reported a genuine flock error as the bounded-skip sentinel: %v", tc.name, emitErr)
			}
			if !strings.Contains(emitErr.Error(), "supervisor event log flock") {
				t.Fatalf("%s error = %v, want it wrapped as a supervisor event log flock error", tc.name, emitErr)
			}

			// The actual P1 assertion: l.mu was released EXACTLY once, so it is
			// neither still held (leak) nor double-unlocked (fatal). TryLock
			// succeeding proves it is unlocked and this goroutine can own it.
			if !logger.mu.TryLock() {
				t.Fatalf("%s left l.mu held after a genuine flock error", tc.name)
			}
			logger.mu.Unlock()

			// And the log handle stays usable: a second call takes the same
			// failing path and must behave identically rather than deadlocking
			// on a leaked mutex.
			if secondErr := tc.emit(logger, evt); secondErr == nil {
				t.Fatalf("%s second call over an unopenable lock path returned nil", tc.name)
			}
			if !logger.mu.TryLock() {
				t.Fatalf("%s left l.mu held after the second genuine flock error", tc.name)
			}
			logger.mu.Unlock()
		})
	}
}

// TestSupervisorEventLog_EmitWithTimeoutTrackedPendingNilOnImmediateSuccess
// pins the OTHER half of the contract: when the write completes before the
// caller's timeout, there is nothing to wait for and the returned handle
// must be nil (a caller checking `pending != nil` before calling Wait must
// not be tricked into waiting on a handle that was never populated).
func TestSupervisorEventLog_EmitWithTimeoutTrackedPendingNilOnImmediateSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	pending, err := logger.EmitWithTimeoutTracked(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-tracked-ok",
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("EmitWithTimeoutTracked uncontended: %v", err)
	}
	if pending != nil {
		t.Fatalf("pending handle is non-nil on immediate success; want nil (nothing to wait for)")
	}
}

// TestSupervisorEventLog_EmitReportsFlockReleaseFailure pins review finding 2:
// a SUCCESSFUL write followed by a FAILED cross-process flock release must be
// reported to the caller, not discarded.
//
// The pre-fix `_ = l.lock.Unlock()` returned a success verdict while the flock
// stayed held. That is not a cosmetic loss — the supervisor and the install CLI
// both emit into this same log across processes, so a silently retained flock
// blocks every OTHER process's event-log write until this one exits, and the
// caller that caused it is told nothing.
//
// All three emit modes are covered because they release through two DIFFERENT
// owners: Emit/TryEmit release in emit's deferred releaser, while
// EmitWithTimeout releases in the handed-off worker goroutine. A fix applied to
// only one of them would leave the other silently discarding.
//
// MUTATION: restore `_ = l.lock.Unlock()` in emit's deferred releaser (and/or
// `defer func() { _ = l.lock.Unlock() }()` in the worker). The release failure
// is discarded again and the affected subtests fail with
// "Emit returned nil; want the flock release failure reported".
func TestSupervisorEventLog_EmitReportsFlockReleaseFailure(t *testing.T) {
	cases := []struct {
		name string
		emit func(l *SupervisorEventLog, evt SupervisorEvent) error
	}{
		{"Emit", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.Emit(evt)
		}},
		{"TryEmit", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.TryEmit(evt)
		}},
		{"EmitWithTimeout", func(l *SupervisorEventLog, evt SupervisorEvent) error {
			return l.EmitWithTimeout(evt, 5*time.Second)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "supervisor-events.log")
			logger, err := OpenSupervisorEventLog(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = logger.Close() }()

			releaseFailure := errors.New("UnlockFileEx: simulated persistent failure")
			restore := SetSupervisorEventUnlockFnForTest(func(*SupervisorEventLog) error {
				return releaseFailure
			})
			defer func() {
				restore()
				// The seam suppressed the REAL Unlock, so the flock is still
				// held. Free it before t.TempDir's RemoveAll fires.
				_ = logger.lock.Unlock()
			}()

			evt := SupervisorEvent{
				Severity: "info",
				Source:   "autostart",
				Event:    "emit-flock-release-failure",
			}
			emitErr := tc.emit(logger, evt)
			if emitErr == nil {
				t.Fatalf("%s returned nil; want the flock release failure reported", tc.name)
			}
			if !errors.Is(emitErr, ErrSupervisorEventReleaseFailed) {
				t.Fatalf("%s error = %v, want it classified as ErrSupervisorEventReleaseFailed", tc.name, emitErr)
			}
			// The concrete cause survives the wrap, so an operator sees the real
			// syscall failure rather than only the classifier.
			if !errors.Is(emitErr, releaseFailure) {
				t.Fatalf("%s error = %v, want the underlying release cause preserved", tc.name, emitErr)
			}
			// A release failure must not be laundered into the bounded-skip
			// sentinel: that would tell the caller to await a worker rather
			// than that a cross-process lock is stuck.
			if errors.Is(emitErr, ErrSupervisorEventEmitTimeout) {
				t.Fatalf("%s reported a release failure as the bounded-skip sentinel: %v", tc.name, emitErr)
			}
			// This is a RELEASE failure, not a write failure: the row must
			// still be on disk. Otherwise the test could pass for the wrong
			// reason (an emit that simply failed earlier).
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("%s: read event log: %v", tc.name, readErr)
			}
			if !strings.Contains(string(raw), "emit-flock-release-failure") {
				t.Fatalf("%s: event row missing from %q; the write itself should have succeeded", tc.name, string(raw))
			}
			// The in-process mutex is released EXACTLY once on this path: not
			// still held (leak), not double-unlocked (runtime fatal). Its
			// unlock is unconditional and must never be skipped just because
			// the flock release failed.
			if !logger.mu.TryLock() {
				t.Fatalf("%s left l.mu held after a flock release failure", tc.name)
			}
			logger.mu.Unlock()
		})
	}
}

// TestSupervisorEventLog_TimeoutWorkerReleasesBeforeSignallingCompletion pins
// the ORDERING half of review finding 2. The handed-off emitTimeout worker used
// to send its result on `done` from the goroutine body, so its two deferred
// releases ran AFTER the send: a caller awaiting the pending handle could
// observe SUCCESS strictly before the release outcome even existed.
//
// The unlock seam deliberately sleeps so the window this assertion depends on
// is engineered LARGE rather than raced against the natural (sub-microsecond)
// one — a fast machine must not be able to turn this into a flake.
//
// MUTATION: restore the pre-fix worker body
//
//	defer l.mu.Unlock()
//	defer func() { _ = l.lock.Unlock() }()
//	done <- writeFn(l, raw)
//
// Wait then returns while the 250ms unlock seam is still running, and the test
// fails with "pending.Wait returned before the release outcome existed".
func TestSupervisorEventLog_TimeoutWorkerReleasesBeforeSignallingCompletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor-events.log")
	logger, err := OpenSupervisorEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = logger.Close() }()

	releaseFailure := errors.New("UnlockFileEx: simulated persistent failure")
	var unlockFinished atomic.Bool
	restoreUnlock := SetSupervisorEventUnlockFnForTest(func(*SupervisorEventLog) error {
		time.Sleep(250 * time.Millisecond)
		unlockFinished.Store(true)
		return releaseFailure
	})

	release := make(chan struct{})
	var releaseOnce sync.Once
	safeRelease := func() { releaseOnce.Do(func() { close(release) }) }
	restoreWrite := SetSupervisorEventWriteFnForTest(func(l *SupervisorEventLog, raw []byte) error {
		<-release // simulates a filesystem/AV stall that outlives the caller's budget
		return l.writeEventLine(raw)
	})
	defer func() {
		safeRelease()
		// Join on the worker before touching the seams or the temp dir. Under
		// the MUTATION this is load-bearing: there, Wait returns before the
		// releases run, so the worker is still live at this point.
		logger.mu.Lock()
		logger.mu.Unlock()
		restoreWrite()
		restoreUnlock()
		_ = logger.lock.Unlock()
	}()

	const timeout = 150 * time.Millisecond
	pending, err := logger.EmitWithTimeoutTracked(SupervisorEvent{
		Severity: "info",
		Source:   "autostart",
		Event:    "emit-timeout-worker-release-ordering",
	}, timeout)
	if !errors.Is(err, ErrSupervisorEventEmitTimeout) {
		t.Fatalf("EmitWithTimeoutTracked with a stalled write error = %v, want ErrSupervisorEventEmitTimeout", err)
	}
	if pending == nil {
		t.Fatal("pending handle is nil after a genuine timeout; want a handle to await the abandoned worker")
	}

	// Unblock the stalled write from a separate goroutine WHILE Wait blocks, so
	// this genuinely exercises the late-worker path.
	go func() {
		time.Sleep(50 * time.Millisecond)
		safeRelease()
	}()
	waitErr := pending.Wait(10 * time.Second)

	if !unlockFinished.Load() {
		t.Fatal("pending.Wait returned before the release outcome existed; the worker signalled completion while its flock release was still in flight")
	}
	if !errors.Is(waitErr, ErrSupervisorEventReleaseFailed) {
		t.Fatalf("pending.Wait error = %v, want the worker's flock release failure classified as ErrSupervisorEventReleaseFailed", waitErr)
	}
	if !errors.Is(waitErr, releaseFailure) {
		t.Fatalf("pending.Wait error = %v, want the underlying release cause preserved", waitErr)
	}
}
