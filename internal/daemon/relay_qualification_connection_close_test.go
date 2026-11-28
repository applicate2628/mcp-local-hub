package daemon

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Hold one Close call around an owned, real TCP connection. No protocol,
// endpoint, publisher, or native-child qualification evidence is supplied.
type qualificationGatedCloseConn struct {
	net.Conn
	entered, releaseCh  chan struct{}
	releaseOnce         sync.Once
	calls               atomic.Int32
	failure             error
	readGate, writeGate *qualificationIOGate
}

func (c *qualificationGatedCloseConn) release() {
	c.releaseOnce.Do(func() { close(c.releaseCh) })
}
func (c *qualificationGatedCloseConn) Close() error {
	if c.calls.Add(1) != 1 {
		return errors.New("underlying connection Close was replayed")
	}
	close(c.entered)
	<-c.releaseCh
	return errors.Join(c.Conn.Close(), c.failure)
}
func (c *qualificationGatedCloseConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.readGate != nil {
		c.readGate.hold()
	}
	return n, err
}
func (c *qualificationGatedCloseConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if c.writeGate != nil {
		c.writeGate.hold()
	}
	return n, err
}

type qualificationIOGate struct {
	entered, releaseCh chan struct{}
	releaseOnce        sync.Once
}

func (g *qualificationIOGate) hold() {
	close(g.entered)
	<-g.releaseCh
}
func (g *qualificationIOGate) release() {
	g.releaseOnce.Do(func() { close(g.releaseCh) })
}

type qualificationCloseFixture struct {
	conn       *mcpObservedConnection
	raw        *qualificationGatedCloseConn
	peer       net.Conn
	guard      context.Context
	firstDone  chan struct{}
	firstError error
	ioGate     *qualificationIOGate
	ioDone     chan struct{}
}

func waitQualificationClose(t *testing.T, ctx context.Context, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
		return
	default:
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("%s did not finish: %v", what, ctx.Err())
	}
}

func newQualificationCloseFixture(t *testing.T) *qualificationCloseFixture {
	t.Helper()
	guard, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	client, err := (&net.Dialer{}).DialContext(guard, "tcp4", listener.Addr().String())
	if err != nil {
		_ = listener.Close()
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	closeErr := listener.Close()
	if err != nil || closeErr != nil {
		_ = client.Close()
		if peer != nil {
			_ = peer.Close()
		}
		t.Fatal(errors.Join(err, closeErr))
	}
	raw := &qualificationGatedCloseConn{Conn: client, entered: make(chan struct{}), releaseCh: make(chan struct{})}
	conn := &mcpObservedConnection{Conn: raw, idle: make(chan struct{})}
	close(conn.idle)
	f := &qualificationCloseFixture{conn: conn, raw: raw, peer: peer, guard: guard}
	t.Cleanup(func() {
		defer func() {
			if err := peer.Close(); err != nil {
				t.Errorf("close owned peer: %v", err)
			}
		}()
		raw.release()
		if f.ioGate != nil {
			f.ioGate.release()
		}
		if f.firstDone == nil {
			_ = conn.Close()
		} else {
			waitQualificationClose(t, guard, f.firstDone, "original Close")
		}
		if f.ioDone != nil {
			waitQualificationClose(t, guard, f.ioDone, "original I/O")
		}
		if raw.calls.Load() != 1 {
			t.Errorf("underlying Close calls = %d, want 1", raw.calls.Load())
		}
	})
	return f
}
func (f *qualificationCloseFixture) startClose(t *testing.T) {
	t.Helper()
	f.firstDone = make(chan struct{})
	go func() {
		f.firstError = f.conn.Close()
		close(f.firstDone)
	}()
	waitQualificationClose(t, f.guard, f.raw.entered, "entry into original Close")
	f.conn.mu.Lock()
	pending := f.conn.attempted && !f.conn.closed && f.conn.closeErr == nil
	f.conn.mu.Unlock()
	if !pending {
		t.Fatal("fixture did not retain the in-flight original Close")
	}
}
func (f *qualificationCloseFixture) finishClose(t *testing.T) {
	t.Helper()
	f.raw.release()
	waitQualificationClose(t, f.guard, f.firstDone, "original Close")
}
func (f *qualificationCloseFixture) holdIO(t *testing.T, mode string) {
	t.Helper()
	f.ioGate = &qualificationIOGate{entered: make(chan struct{}), releaseCh: make(chan struct{})}
	deadline, _ := f.guard.Deadline()
	if err := f.raw.Conn.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	f.ioDone = make(chan struct{})
	if mode == "read" {
		f.raw.readGate = f.ioGate
		go func() {
			_, _ = f.conn.Read(make([]byte, 1))
			close(f.ioDone)
		}()
		if _, err := f.peer.Write([]byte("r")); err != nil {
			t.Fatal(err)
		}
	} else {
		f.raw.writeGate = f.ioGate
		go func() {
			_, _ = f.conn.Write([]byte("w"))
			close(f.ioDone)
		}()
	}
	waitQualificationClose(t, f.guard, f.ioGate.entered, "original "+mode)
}

// Channel-expression evaluation is the causal barrier: after Close returned
// its transient pending result, release and receive the first actual Close.
// The old receiver then retained that stale pending error even with Closed=true.
type qualificationCloseReceivingContext struct {
	context.Context
	once       sync.Once
	beforeDone func()
}

func (c *qualificationCloseReceivingContext) Done() <-chan struct{} {
	c.once.Do(c.beforeDone)
	return c.Context.Done()
}

func TestQualificationConnectionCloseReceivesOriginalResult(t *testing.T) {
	fault := errors.New("controlled original Close failure")
	for _, name := range []string{"concurrent-success", "concurrent-failure", "closed-canceled", "closed-expired", "inflight-canceled", "inflight-expired", "no-deadline-inflight"} {
		t.Run(name, func(t *testing.T) {
			f := newQualificationCloseFixture(t)
			if name == "concurrent-failure" {
				f.raw.failure = fault
			}
			f.startClose(t)
			var receive context.Context = f.guard
			wantCause := error(nil)
			pending := strings.HasPrefix(name, "inflight-") || name == "no-deadline-inflight"
			switch {
			case strings.HasPrefix(name, "concurrent-"):
				receive = &qualificationCloseReceivingContext{Context: f.guard, beforeDone: func() { f.finishClose(t) }}
			case name == "no-deadline-inflight":
				receive = context.Background()
			default:
				if !pending {
					f.finishClose(t)
				}
				if strings.HasSuffix(name, "-canceled") {
					ctx, cancel := context.WithCancel(f.guard)
					cancel()
					receive, wantCause = ctx, context.Canceled
				} else {
					ctx, cancel := context.WithDeadline(f.guard, time.Now().Add(-time.Second))
					defer cancel()
					receive, wantCause = ctx, context.DeadlineExceeded
				}
			}
			view, err := f.conn.closeObserved(receive)
			if !view.CloseAttempted || !view.IOComplete || f.raw.calls.Load() != 1 {
				t.Fatalf("original close ownership changed: view=%+v calls=%d err=%v", view, f.raw.calls.Load(), err)
			}
			switch {
			case name == "concurrent-success":
				if err != nil || !view.Closed || f.firstError != nil {
					t.Fatalf("settled original Close retained transient refusal: view=%+v err=%v first=%v", view, err, f.firstError)
				}
			case name == "concurrent-failure":
				if !errors.Is(err, fault) || view.Closed || strings.Contains(err.Error(), "MCP_CONNECTION_CLOSE_PENDING") || !errors.Is(f.conn.Close(), fault) {
					t.Fatalf("actual original Close failure was lost or retried: view=%+v err=%v", view, err)
				}
			case name == "no-deadline-inflight":
				if err == nil || !strings.Contains(err.Error(), "MCP_QUALIFICATION_CLEANUP_DEADLINE_REQUIRED") || view.Closed {
					t.Fatalf("unbounded receiving was admitted: view=%+v err=%v", view, err)
				}
			default:
				if !errors.Is(err, wantCause) || view.Closed == pending {
					t.Fatalf("canceled/expired receiving changed original outcome: view=%+v err=%v want=%v", view, err, wantCause)
				}
			}
		})
	}
}

func TestQualificationConnectionCloseReceivesOriginalIO(t *testing.T) {
	for _, name := range []string{"read-joined", "write-joined", "read-canceled", "write-canceled"} {
		t.Run(name, func(t *testing.T) {
			f := newQualificationCloseFixture(t)
			mode, outcome, _ := strings.Cut(name, "-")
			f.holdIO(t, mode)
			f.startClose(t)
			var receive context.Context
			if outcome == "joined" {
				receive = &qualificationCloseReceivingContext{Context: f.guard, beforeDone: func() {
					f.finishClose(t)
					f.ioGate.release()
					waitQualificationClose(t, f.guard, f.ioDone, "original I/O")
				}}
			} else {
				f.finishClose(t)
				ctx, cancel := context.WithCancel(f.guard)
				cancel()
				receive = ctx
			}
			view, err := f.conn.closeObserved(receive)
			if !view.CloseAttempted || !view.Closed || f.raw.calls.Load() != 1 {
				t.Fatalf("original Close was not retained: view=%+v calls=%d err=%v", view, f.raw.calls.Load(), err)
			}
			if outcome == "joined" {
				if err != nil || !view.IOComplete {
					t.Fatalf("settled original I/O refused: view=%+v err=%v", view, err)
				}
			} else if !errors.Is(err, context.Canceled) || view.IOComplete || !strings.Contains(err.Error(), "MCP_CONNECTION_CLOSURE_UNSETTLED") {
				t.Fatalf("pending original I/O claimed complete: view=%+v err=%v", view, err)
			}
		})
	}
}

func TestQualificationSessionCloseCachesUnsettledOriginalClose(t *testing.T) {
	f := newQualificationCloseFixture(t)
	f.startClose(t)
	relay := &HTTPToStdioRelay{URL: "http://127.0.0.1:1/mcp"}
	owner := &mcpHTTPQualificationOwner{relay: relay, finished: true, transport: &http.Transport{}, conns: []*mcpObservedConnection{f.conn}}
	relay.qualification = owner
	ctx, cancel := context.WithCancel(f.guard)
	cancel()
	first, err := relay.CloseSessionObserved(ctx)
	if first == nil || first.complete || err == nil || owner.snapshot.ResourcesComplete || owner.snapshot.Complete {
		t.Fatalf("in-flight original Close issued settled session proof: proof=%v err=%v", first, err)
	}
	f.finishClose(t)
	second, again := relay.CloseSessionObserved(f.guard)
	if first != second || again != first.err || second.complete || f.raw.calls.Load() != 1 {
		t.Fatalf("later completion replayed cleanup or replaced failed proof: same=%v err=%v calls=%d", first == second, again, f.raw.calls.Load())
	}
	if (&MCPHTTPQualificationObservation{owner: owner}).ReceiptSHA256() != "" {
		t.Fatal("partial transport fixture issued protocol qualification evidence")
	}
}
