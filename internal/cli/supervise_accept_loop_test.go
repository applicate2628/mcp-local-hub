// Package cli — unit tests for the supervisor IPC accept loop.
//
// Post-mortem context (2026-05-19 22:52:17): the pre-fix accept loop
// returned permanently on ANY Accept() error, including transient
// per-connection failures (the Accept-time hello-write race with a
// client that disconnects mid-handshake). The supervisor process
// stayed alive but its named pipe stopped accepting new connections,
// turning every `mcphub status` into an i/o timeout and breaking the
// GUI's Dashboard polling.
//
// These tests exercise the post-fix contract:
//
//  1. `net.ErrClosed` (the canonical signal that Stop() called
//     listener.Close()) DOES exit the loop cleanly.
//
//  2. ANY other error is treated as transient — the loop emits a
//     warn-level event and continues.
//
//  3. After `maxConsecutiveAcceptErrs` (100) back-to-back transient
//     errors, the loop exits anyway with a different reason so a
//     genuinely-broken listener cannot hot-loop the supervisor
//     forever.
//
//  4. A successful Accept() between transient errors resets the
//     consecutive-error counter (so flaky transports don't accrue
//     toward the budget across long lifetimes).

package cli

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// readSupervisorEventsLog reads the JSONL events file at path; tests
// then grep for substrings in the returned text. The events file is
// written line-buffered, so a small `time.Sleep` is included before
// the read to let the OS flush. We avoid `runtime.Gosched` because
// SupervisorEventLog.Emit is fully synchronous — the wait covers the
// kernel-side file flush rather than goroutine scheduling.
func readSupervisorEventsLog(path string) (string, error) {
	time.Sleep(20 * time.Millisecond)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// fakeAcceptor returns the queued (conn, err) sequence in order. If
// the caller runs past the queue length, every subsequent Accept()
// returns `net.ErrClosed` (mimicking listener.Close() taking effect).
type fakeAcceptor struct {
	mu       sync.Mutex
	queue    []fakeAcceptResult
	idx      int
	accepted atomic.Int32
}

type fakeAcceptResult struct {
	conn net.Conn
	err  error
}

func (f *fakeAcceptor) Accept() (net.Conn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.queue) {
		return nil, net.ErrClosed
	}
	r := f.queue[f.idx]
	f.idx++
	if r.err == nil && r.conn != nil {
		f.accepted.Add(1)
	}
	return r.conn, r.err
}

// fakeConn is the minimal net.Conn we hand back to acceptIPCConnections
// when we want the loop to proceed past the "ok" branch. It immediately
// closes — handleIPCConn's read loop sees EOF and exits, which keeps
// the test's accumulated goroutine count bounded.
type fakeConn struct{ closed atomic.Bool }

func (c *fakeConn) Read(p []byte) (int, error)         { return 0, net.ErrClosed }
func (c *fakeConn) Write(p []byte) (int, error)        { return 0, net.ErrClosed }
func (c *fakeConn) Close() error                       { c.closed.Store(true); return nil }
func (c *fakeConn) LocalAddr() net.Addr                { return &net.IPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr               { return &net.IPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error        { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error    { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error   { return nil }

// makeTestDeps returns an ipcDispatchDeps wired with a real
// SupervisorEventLog under t.TempDir() so emitted events can be
// inspected by reading the log file directly.
func makeTestDeps(t *testing.T) (ipcDispatchDeps, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(logPath)
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	return ipcDispatchDeps{
		stateDir: dir,
		events:   events,
	}, logPath
}

// runAcceptLoop runs the accept loop in a goroutine and blocks
// until it returns OR `timeout` elapses (test failure). Returns
// after the loop is fully done so the caller can inspect side effects.
func runAcceptLoop(t *testing.T, listener ipcAcceptor, deps ipcDispatchDeps, timeout time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		acceptIPCConnections(listener, deps)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("accept loop did not exit within %s", timeout)
	}
}

// TestAcceptLoop_ExitOnErrClosed verifies the canonical shutdown
// path: a wrapped net.ErrClosed from Accept() exits the loop
// cleanly and emits ONE info-severity ipc-accept-exit event.
func TestAcceptLoop_ExitOnErrClosed(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	listener := &fakeAcceptor{queue: []fakeAcceptResult{
		{err: net.ErrClosed},
	}}
	runAcceptLoop(t, listener, deps, 2*time.Second)

	body, err := readSupervisorEventsLog(logPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	if !strings.Contains(body, `"event":"ipc-accept-exit"`) {
		t.Errorf("expected ipc-accept-exit event, got log body:\n%s", body)
	}
	if !strings.Contains(body, `"severity":"info"`) {
		t.Errorf("expected info severity on graceful exit, got:\n%s", body)
	}
	if strings.Contains(body, `"event":"ipc-accept-transient-error"`) {
		t.Errorf("ErrClosed must NOT emit transient-error event, got:\n%s", body)
	}
}

// TestAcceptLoop_ContinuesOnTransientError exercises the 2026-05-19
// regression: a non-ErrClosed Accept() error must NOT terminate the
// loop. Pre-fix this test would have hung (the loop would have exited
// on call #1 and never reached the ErrClosed sentinel on call #2),
// then time out.
func TestAcceptLoop_ContinuesOnTransientError(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	transient := errors.New("write hello: The pipe is being closed.")
	listener := &fakeAcceptor{queue: []fakeAcceptResult{
		{err: transient},
		{err: transient},
		{err: transient},
		{err: net.ErrClosed},
	}}
	runAcceptLoop(t, listener, deps, 2*time.Second)

	body, err := readSupervisorEventsLog(logPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	// Three transient errors logged.
	transientCount := strings.Count(body, `"event":"ipc-accept-transient-error"`)
	if transientCount != 3 {
		t.Errorf("expected 3 transient-error events, got %d in:\n%s", transientCount, body)
	}
	// Exactly one ErrClosed exit event.
	exitCount := strings.Count(body, `"event":"ipc-accept-exit"`)
	if exitCount != 1 {
		t.Errorf("expected 1 ipc-accept-exit event, got %d in:\n%s", exitCount, body)
	}
	// The transient-error events must include the consecutive_err counter.
	if !strings.Contains(body, `"consecutive_err":1`) || !strings.Contains(body, `"consecutive_err":3`) {
		t.Errorf("expected consecutive_err counter to climb 1→3, got:\n%s", body)
	}
}

// TestAcceptLoop_ResetCounterOnSuccess verifies that a successful
// Accept() between transient failures resets the consecutive-error
// counter. Without this, a flaky transport that recovers
// periodically would eventually trip the maxConsecutiveAcceptErrs
// breaker after a long enough uptime even though it is making
// forward progress.
func TestAcceptLoop_ResetCounterOnSuccess(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	transient := errors.New("write hello: pipe race")
	listener := &fakeAcceptor{queue: []fakeAcceptResult{
		{err: transient}, // 1
		{err: transient}, // 2
		{conn: &fakeConn{}},
		{err: transient}, // back to 1 (was 2)
		{err: net.ErrClosed},
	}}
	runAcceptLoop(t, listener, deps, 2*time.Second)

	body, err := readSupervisorEventsLog(logPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	// After the success, consecutive_err MUST restart from 1. Count
	// of `"consecutive_err":1` should be 2 (once for the very first
	// failure, once for the post-success retry).
	c1 := strings.Count(body, `"consecutive_err":1`)
	if c1 != 2 {
		t.Errorf(`expected consecutive_err=1 twice (before+after success), got %d in:\n%s`, c1, body)
	}
	// And there should never be consecutive_err >= 3 in this scenario.
	if strings.Contains(body, `"consecutive_err":3`) {
		t.Errorf(`unexpected consecutive_err=3 in:\n%s`, body)
	}
}

// TestAcceptLoop_BudgetBreakerExitsOnFlood asserts the defense-in-
// depth breaker: after maxConsecutiveAcceptErrs (100) back-to-back
// transients, the loop exits with severity error and a distinct
// reason. Tests use the production constant directly so a future
// retune of the budget keeps the test honest.
func TestAcceptLoop_BudgetBreakerExitsOnFlood(t *testing.T) {
	deps, logPath := makeTestDeps(t)
	transient := errors.New("kernel pool exhaustion")
	q := make([]fakeAcceptResult, 0, maxConsecutiveAcceptErrs+5)
	for i := 0; i < maxConsecutiveAcceptErrs+5; i++ {
		q = append(q, fakeAcceptResult{err: transient})
	}
	listener := &fakeAcceptor{queue: q}
	// Allow more wall-time because the 50ms backoff x 100 transient
	// retries adds ~5s before the breaker fires.
	runAcceptLoop(t, listener, deps, 10*time.Second)

	body, err := readSupervisorEventsLog(logPath)
	if err != nil {
		t.Fatalf("read events log: %v", err)
	}
	if !strings.Contains(body, `"reason":"consecutive-transient-errors-exceeded-budget"`) {
		t.Errorf("expected breaker reason in events log:\n%s", body)
	}
	if !strings.Contains(body, `"severity":"error"`) {
		t.Errorf("expected error severity on breaker fire:\n%s", body)
	}
}
