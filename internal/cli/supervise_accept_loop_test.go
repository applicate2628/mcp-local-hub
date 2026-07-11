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
	"bufio"
	"encoding/json"
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

	"github.com/gofrs/flock"
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
//
// helloFn, when set, is the per-connection WriteHello behavior the
// accept loop (via serveIPCConn) drives on each accepted connection.
// A nil helloFn means "hello write always succeeds" (WriteHello
// returns nil without touching the conn), which is what the existing
// success-path tests want.
type fakeAcceptor struct {
	mu       sync.Mutex
	queue    []fakeAcceptResult
	idx      int
	accepted atomic.Int32
	helloFn  func(net.Conn) error
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

func (f *fakeAcceptor) WriteHello(conn net.Conn) error {
	if f.helloFn != nil {
		return f.helloFn(conn)
	}
	return nil
}

// waitForSupervisorEvent polls the JSONL events file until it contains
// substr, or fails the test after timeout. Needed because the
// per-connection serveIPCConn goroutine emits its event asynchronously
// relative to the accept loop returning, so a single post-loop read can
// race the emit.
func waitForSupervisorEvent(t *testing.T, logPath, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		b, err := os.ReadFile(logPath)
		if err == nil {
			last = string(b)
			if strings.Contains(last, substr) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("event %q not found in log within %s; got:\n%s", substr, timeout, last)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeConn is the minimal net.Conn we hand back to acceptIPCConnections
// when we want the loop to proceed past the "ok" branch. It immediately
// closes — handleIPCConn's read loop sees EOF and exits, which keeps
// the test's accumulated goroutine count bounded.
type fakeConn struct{ closed atomic.Bool }

func (c *fakeConn) Read(p []byte) (int, error)       { return 0, net.ErrClosed }
func (c *fakeConn) Write(p []byte) (int, error)      { return 0, net.ErrClosed }
func (c *fakeConn) Close() error                     { c.closed.Store(true); return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type scriptedDeadlineConn struct {
	mu        sync.Mutex
	reads     []scriptedRead
	deadlines []time.Time
	closed    bool
}

type scriptedRead struct {
	data string
	err  error
}

func (c *scriptedDeadlineConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reads) == 0 {
		return 0, net.ErrClosed
	}
	next := c.reads[0]
	c.reads = c.reads[1:]
	if next.data != "" {
		return copy(p, next.data), next.err
	}
	return 0, next.err
}

func (c *scriptedDeadlineConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *scriptedDeadlineConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *scriptedDeadlineConn) LocalAddr() net.Addr              { return &net.IPAddr{} }
func (c *scriptedDeadlineConn) RemoteAddr() net.Addr             { return &net.IPAddr{} }
func (c *scriptedDeadlineConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedDeadlineConn) SetWriteDeadline(time.Time) error { return nil }
func (c *scriptedDeadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadlines = append(c.deadlines, t)
	return nil
}

func TestHandleIPCConnRefreshesIdleReadDeadline(t *testing.T) {
	conn := &scriptedDeadlineConn{
		reads: []scriptedRead{
			{data: "\n"},
			{err: os.ErrDeadlineExceeded},
		},
	}
	done := make(chan struct{})
	go func() {
		handleIPCConn(conn, ipcDispatchDeps{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleIPCConn did not exit after read deadline error")
	}

	conn.mu.Lock()
	defer conn.mu.Unlock()
	if !conn.closed {
		t.Fatal("handleIPCConn did not close the connection after read error")
	}
	if len(conn.deadlines) != 2 {
		t.Fatalf("SetReadDeadline calls = %d, want 2", len(conn.deadlines))
	}
	for i, deadline := range conn.deadlines {
		remaining := time.Until(deadline)
		if remaining < 55*time.Second || remaining > ipcConnIdleTimeout+time.Second {
			t.Fatalf("deadline[%d] remaining = %v, want near %v", i, remaining, ipcConnIdleTimeout)
		}
	}
}

func TestHandleIPCConnStatusDoesNotWaitForContendedAuditLog(t *testing.T) {
	deps, _ := makeTestDeps(t)

	lock := flock.New(filepath.Join(deps.stateDir, api.SupervisorEventLogFileLeaf) + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock supervisor event log: %v", err)
	}
	defer func() { _ = lock.Unlock() }()

	if err := api.WriteSupervisorIntent(filepath.Join(deps.stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	deps.runtimeTracker = NewDaemonRuntimeTracker()
	var ready atomic.Bool
	var loaded atomic.Bool
	ready.Store(true)
	loaded.Store(true)
	deps.reconcileReady = &ready
	deps.intentFilesLoaded = &loaded
	var graceful gracefulCounter
	deps.gracefulInProgress = &graceful

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		handleIPCConn(serverConn, deps)
		close(done)
	}()

	if _, err := clientConn.Write([]byte(`{"version":1,"id":99,"cmd":"status"}` + "\n")); err != nil {
		t.Fatalf("write status request: %v", err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	line, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status response while audit log is contended: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if !resp.OK || resp.Error != nil || resp.ID != 99 {
		t.Fatalf("status response = %+v, want OK id=99", resp)
	}

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleIPCConn did not exit after client close")
	}
}

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

// TestServeIPCConn_SlowHelloDoesNotBlockNextAccept is the core
// decoupling property of the 2026-07 accept-flap fix: the hello write
// runs in the per-connection serveIPCConn goroutine, so a first
// connection whose hello write blocks (a client that dialed then
// vanished under host saturation) must NOT delay Accept() + service of
// the SECOND connection. Pre-fix, the hello write lived inside Accept()
// on the single accept loop, so the first slow hello stalled every
// subsequent dial.
func TestServeIPCConn_SlowHelloDoesNotBlockNextAccept(t *testing.T) {
	deps, _ := makeTestDeps(t)

	conn1 := &fakeConn{}
	conn2 := &fakeConn{}

	// conn1's hello write blocks until cleanup — it never returns
	// while the test body runs, standing in for a wedged/abandoned
	// client. Released in a t.Cleanup that (LIFO) runs before the
	// events log is closed; conn1's released path emits no event.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })

	served2 := make(chan struct{})
	listener := &fakeAcceptor{
		queue: []fakeAcceptResult{
			{conn: conn1},
			{conn: conn2},
			{err: net.ErrClosed},
		},
		helloFn: func(c net.Conn) error {
			switch c {
			case conn1:
				<-blockCh // hold the first connection's hello indefinitely
				return nil
			case conn2:
				close(served2)
				return nil
			}
			return nil
		},
	}

	go acceptIPCConnections(listener, deps)

	select {
	case <-served2:
		// conn2 was served while conn1's hello write is still blocked —
		// the accept loop is decoupled from per-connection hello I/O.
	case <-time.After(2 * time.Second):
		t.Fatal("second connection was not served while the first connection's hello write was blocked")
	}
}

// TestServeIPCConn_HelloWriteFailureIsIsolated proves a hello-write
// failure is confined to its own connection: it closes ONLY that
// connection, emits the DISTINCT ipc-hello-write-error event, and does
// NOT enter the accept loop's transient-error branch or touch the
// consecutive-error budget.
func TestServeIPCConn_HelloWriteFailureIsIsolated(t *testing.T) {
	deps, logPath := makeTestDeps(t)

	conn := &fakeConn{}
	helloErr := errors.New("write hello: The pipe is being closed.")
	listener := &fakeAcceptor{
		queue: []fakeAcceptResult{
			{conn: conn},
			{err: net.ErrClosed},
		},
		helloFn: func(net.Conn) error { return helloErr },
	}
	runAcceptLoop(t, listener, deps, 2*time.Second)

	body := waitForSupervisorEvent(t, logPath, `"event":"ipc-hello-write-error"`, 2*time.Second)

	if strings.Contains(body, `"event":"ipc-accept-transient-error"`) {
		t.Errorf("hello-write failure must NOT emit ipc-accept-transient-error, got:\n%s", body)
	}
	if strings.Contains(body, `"consecutive_err"`) {
		t.Errorf("hello-write failure must NOT touch the accept-error budget, got:\n%s", body)
	}
	if !strings.Contains(body, `"severity":"warn"`) {
		t.Errorf("expected warn severity on ipc-hello-write-error, got:\n%s", body)
	}
	// The subsequent ErrClosed still exits the loop cleanly.
	if !strings.Contains(body, `"event":"ipc-accept-exit"`) {
		t.Errorf("expected ipc-accept-exit on ErrClosed, got:\n%s", body)
	}
	if !conn.closed.Load() {
		t.Error("hello-write failure must close the connection")
	}
}

// TestServeIPCConn_HelloIsFirstFrame preserves the wire contract: the
// hello frame is still the FIRST server frame the client reads, before
// any command response — only its timing moved off the accept loop into
// serveIPCConn. Drives the real production WriteHello + handleIPCConn
// over an in-memory net.Pipe.
func TestServeIPCConn_HelloIsFirstFrame(t *testing.T) {
	deps, _ := makeTestDeps(t)
	if err := api.WriteSupervisorIntent(filepath.Join(deps.stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1}); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	deps.runtimeTracker = NewDaemonRuntimeTracker()
	var ready atomic.Bool
	var loaded atomic.Bool
	ready.Store(true)
	loaded.Store(true)
	deps.reconcileReady = &ready
	deps.intentFilesLoaded = &loaded
	var graceful gracefulCounter
	deps.gracefulInProgress = &graceful

	// Fields-only listener: WriteHello reads pid/startedAt only; it
	// never touches the (nil) bound listener, so no real pipe/socket is
	// needed to exercise the production hello frame.
	listener := &SupervisorIPCListener{pid: 4242, startedAt: "2026-07-11T00:00:00Z"}

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	done := make(chan struct{})
	go func() {
		serveIPCConn(serverConn, listener, deps)
		close(done)
	}()

	reader := bufio.NewReader(clientConn)

	// FIRST server frame MUST be the hello.
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	helloLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read hello frame: %v", err)
	}
	var helloFrame struct {
		Hello api.IPCHello `json:"hello"`
	}
	if err := json.Unmarshal([]byte(helloLine), &helloFrame); err != nil {
		t.Fatalf("decode hello frame %q: %v", helloLine, err)
	}
	if helloFrame.Hello.Version != 1 || helloFrame.Hello.PID != 4242 || helloFrame.Hello.StartedAt != "2026-07-11T00:00:00Z" {
		t.Fatalf("hello frame = %+v, want version=1 pid=4242 startedAt=2026-07-11T00:00:00Z", helloFrame.Hello)
	}

	// THEN a command response, AFTER the hello.
	if _, err := clientConn.Write([]byte(`{"version":1,"id":7,"cmd":"status"}` + "\n")); err != nil {
		t.Fatalf("write status request: %v", err)
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	respLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status response after hello: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(respLine), &resp); err != nil {
		t.Fatalf("decode status response %q: %v", respLine, err)
	}
	if !resp.OK || resp.Error != nil || resp.ID != 7 {
		t.Fatalf("status response = %+v, want OK id=7", resp)
	}

	_ = clientConn.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveIPCConn did not exit after client close")
	}
}
