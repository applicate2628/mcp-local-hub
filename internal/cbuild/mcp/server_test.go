package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// syncBuffer is a concurrency-safe io.Writer so a test can drive Serve without
// the synchronous-blocking semantics of io.Pipe on the server's response writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// --- test harness ------------------------------------------------------------

type respFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type harness struct {
	t       *testing.T
	clientW *io.PipeWriter
	dec     *json.Decoder
	cancel  context.CancelFunc
	done    chan error
}

func newHarness(t *testing.T, tools ...Tool) *harness {
	t.Helper()
	srvIn, clientW := io.Pipe()
	clientR, srvOut := io.Pipe()
	srv := NewServer("test", "0", tools)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, srvIn, srvOut) }()
	return &harness{t: t, clientW: clientW, dec: json.NewDecoder(clientR), cancel: cancel, done: done}
}

func (h *harness) send(raw string) {
	h.t.Helper()
	if _, err := io.WriteString(h.clientW, raw+"\n"); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

func (h *harness) recv() respFrame {
	h.t.Helper()
	type out struct {
		r   respFrame
		err error
	}
	ch := make(chan out, 1)
	go func() {
		var r respFrame
		err := h.dec.Decode(&r)
		ch <- out{r, err}
	}()
	select {
	case o := <-ch:
		if o.err != nil {
			h.t.Fatalf("recv decode: %v", o.err)
		}
		if o.r.JSONRPC != "2.0" {
			h.t.Fatalf("response jsonrpc = %q, want 2.0", o.r.JSONRPC)
		}
		return o.r
	case <-time.After(10 * time.Second):
		h.t.Fatal("timed out waiting for a response")
		return respFrame{}
	}
}

func (h *harness) close() {
	_ = h.clientW.Close()
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
	}
}

// stubTool is a minimal Tool for driving the server in tests.
type stubTool struct {
	name string
	fn   func(ctx context.Context, args json.RawMessage) (any, error)
}

func (s stubTool) Name() string                { return s.name }
func (s stubTool) Title() string               { return "" }
func (s stubTool) Description() string         { return "stub" }
func (s stubTool) InputSchema() map[string]any { return map[string]any{"type": "object"} }
func (s stubTool) Call(ctx context.Context, a json.RawMessage) (any, error) {
	return s.fn(ctx, a)
}

// --- tests -------------------------------------------------------------------

// TestParseErrorVsInvalidRequest proves malformed JSON is -32700 while
// well-formed-but-not-a-request JSON (array, scalar, missing jsonrpc) is -32600.
func TestParseErrorVsInvalidRequest(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	cases := []struct {
		name string
		raw  string
		code int
	}{
		{"malformed json", `{"jsonrpc":"2.0",`, codeParseError},
		{"top-level array", `[1,2,3]`, codeInvalidRequest},
		{"scalar", `42`, codeInvalidRequest},
		{"missing jsonrpc", `{"id":1,"method":"ping"}`, codeInvalidRequest},
		{"wrong jsonrpc", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, codeInvalidRequest},
	}
	for _, tc := range cases {
		h.send(tc.raw)
		r := h.recv()
		if r.Error == nil {
			t.Errorf("%s: expected an error response, got result %s", tc.name, r.Result)
			continue
		}
		if r.Error.Code != tc.code {
			t.Errorf("%s: code = %d, want %d (%s)", tc.name, r.Error.Code, tc.code, r.Error.Message)
		}
	}
}

// TestPresentButInvalidIdIsInvalidRequest proves a present-and-null id (and
// bool/object ids) are invalid requests answered with a null id, NOT treated as
// notifications.
func TestPresentButInvalidIdIsInvalidRequest(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":{"x":1},"method":"ping"}`,
		`{"jsonrpc":"2.0","id":[1],"method":"ping"}`,
	} {
		h.send(raw)
		r := h.recv()
		if r.Error == nil || r.Error.Code != codeInvalidRequest {
			t.Errorf("%s: want -32600 invalid request, got %+v (result %s)", raw, r.Error, r.Result)
		}
		if string(r.ID) != "null" {
			t.Errorf("%s: response id = %s, want null", raw, r.ID)
		}
	}
}

// TestNotificationVsRequestForLifecycleNames proves that a lifecycle name with
// NO id is a notification (no response), while the same name WITH an id is a
// request that is answered (method-not-found is acceptable).
func TestNotificationVsRequestForLifecycleNames(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	// Notification (no id) → no response. Prove it by sending a ping right after
	// and asserting the FIRST frame we get back is the ping's (id 99), which can
	// only happen if the notification produced no frame.
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","id":99,"method":"ping"}`)
	r := h.recv()
	if string(r.ID) != "99" {
		t.Fatalf("first frame id = %s, want 99 (the notification must produce no response)", r.ID)
	}
	if r.Error != nil {
		t.Errorf("ping errored: %+v", r.Error)
	}

	// Same name WITH an id → a request, answered with method-not-found.
	h.send(`{"jsonrpc":"2.0","id":7,"method":"notifications/initialized"}`)
	r = h.recv()
	if r.Error == nil || r.Error.Code != codeMethodNotFound {
		t.Errorf("notifications/initialized with id: want -32601, got %+v", r.Error)
	}
	if string(r.ID) != "7" {
		t.Errorf("response id = %s, want 7", r.ID)
	}
}

// TestUnknownToolIsInvalidParams proves an unknown tool name in tools/call is
// -32602 invalid params (the method exists; the tool arg is bad), not -32601.
func TestUnknownToolIsInvalidParams(t *testing.T) {
	h := newHarness(t, stubTool{name: "known", fn: func(context.Context, json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	}})
	defer h.close()

	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"does-not-exist"}}`)
	r := h.recv()
	if r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool: want -32602 invalid params, got %+v (result %s)", r.Error, r.Result)
	}
}

// TestToolPanicRecovered proves a panicking tool handler is contained: the call
// returns an isError result rather than killing the daemon, and the server
// keeps serving subsequent requests.
func TestToolPanicRecovered(t *testing.T) {
	h := newHarness(t, stubTool{name: "boom", fn: func(context.Context, json.RawMessage) (any, error) {
		panic("kaboom")
	}})
	defer h.close()

	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom"}}`)
	r := h.recv()
	if r.Error != nil {
		t.Fatalf("panic surfaced as a protocol error, want an isError result: %+v", r.Error)
	}
	var res struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(r.Result, &res); err != nil {
		t.Fatalf("decode tools/call result: %v", err)
	}
	if !res.IsError {
		t.Errorf("panicking tool result isError = false, want true; result = %s", r.Result)
	}

	// The server is still alive after the recovered panic.
	h.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	r = h.recv()
	if r.Error != nil || string(r.ID) != "2" {
		t.Errorf("server did not survive the panic: id=%s err=%+v", r.ID, r.Error)
	}
}

// TestServeDrainsInFlightOnEOF proves that when stdin closes (EOF) while a
// tools/call is still running, Serve cancels the call AND waits for its
// cancellation cleanup (the domain layer's deferred process-tree kill) to run
// BEFORE returning. Without the drain, Serve returns immediately on EOF, main
// exits, and the in-flight build's cleanup never runs — orphaning its process
// tree during normal shutdown.
func TestServeDrainsInFlightOnEOF(t *testing.T) {
	started := make(chan struct{})
	var cleanedUp atomic.Bool
	tool := stubTool{name: "slow", fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done() // block until the server cancels us during EOF shutdown
		// Simulate the deferred tree-kill running only AFTER cancellation is seen.
		time.Sleep(50 * time.Millisecond)
		cleanedUp.Store(true)
		return nil, ctx.Err()
	}}

	srv := NewServer("test", "0", []Tool{tool})
	in, inW := io.Pipe()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), in, out) }()

	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	<-started       // the tool is now in flight
	_ = inW.Close() // stdin EOF

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return within 5s after EOF")
	}
	if !cleanedUp.Load() {
		t.Error("Serve returned before the in-flight tool ran its cancellation cleanup — EOF drain missing")
	}
}

// TestServeCancellationUnblocksReadAndDrains proves signal-driven context
// cancellation returns even while stdin remains open, and still waits for the
// in-flight tool's cancellation cleanup before returning.
func TestServeCancellationUnblocksReadAndDrains(t *testing.T) {
	started := make(chan struct{})
	var cleanedUp atomic.Bool
	tool := stubTool{name: "slow", fn: func(ctx context.Context, _ json.RawMessage) (any, error) {
		close(started)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		cleanedUp.Store(true)
		return nil, ctx.Err()
	}}

	srv := NewServer("test", "0", []Tool{tool})
	in, inW := io.Pipe()
	out := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, in, out) }()

	if _, err := io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	<-started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned an error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		_ = inW.Close()
		t.Fatal("Serve did not return after context cancellation while stdin remained open")
	}
	if !cleanedUp.Load() {
		t.Error("Serve returned before the in-flight tool ran its cancellation cleanup")
	}
	_ = inW.Close()
}
