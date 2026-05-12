// hub_mcp_aggregator_test.go — Phase 3 Task 3.4 (G4 unified hub MCP).
//
// Aggregator tests. Stand up fake daemon endpoints via net/http/httptest,
// drive the aggregator at the method-call level (not yet wired through
// the HTTP handler — that's Phase 4). Covers:
//
//   - AggregateInitialize: 2-daemon fan-out with one success + one 503.
//     InitSuccesses gets the success row; InitFailures gets the 503 row
//     with stage="initialize".
//   - AggregateToolsList: 2-daemon success — merged + namespaced
//     ("<server>__<rawname>") + RouteMap populated + _meta empty.
//   - AggregateToolsList all-failed (no init successes OR all list-time
//     failures) → JSON-RPC -32000 error envelope with
//     data.mcphub.partialFailures.
//   - AggregateToolsCall: canonical rewrite (params.name from exposed
//     to raw) + InsertInFlight + RemoveInFlight + daemon response
//     pass-through.
//   - AggregateToolsCall stale resolver: session's SnapshotAtInit is
//     stale (current Bindings dropped this (Server, Daemon)) → -32601
//     "tool moved out of scope".
//
// Spec: §"Per-hub session model" + §"Partial-failure visibility" +
// §"Tool-name namespacing". Plan: Task 3.4.

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubDaemon is an httptest.Server hosting a fake MCP daemon at /mcp.
// init / list / call / notify paths configurable via the handler
// hooks. onNotify covers notifications/cancelled and any other future
// notification method the aggregator forwards.
type stubDaemon struct {
	server    *httptest.Server
	port      int
	sessionID string

	// hooks: nil means "default behavior". onNotify receives the
	// already-drained request body bytes (the dispatch loop reads
	// r.Body once at top to peek method, so hooks that re-call
	// readAllBody on r get empty bytes; capturing the body in the
	// dispatch closure keeps notification-payload assertions
	// straightforward).
	onInit   func(w http.ResponseWriter, r *http.Request)
	onList   func(w http.ResponseWriter, r *http.Request)
	onCall   func(w http.ResponseWriter, r *http.Request)
	onNotify func(w http.ResponseWriter, r *http.Request, body []byte)

	// counters
	initCount   atomic.Int32
	listCount   atomic.Int32
	callCount   atomic.Int32
	notifyCount atomic.Int32

	// bodyMu guards capture fields below. Updated by the dispatch
	// loop AFTER reading r.Body, BEFORE delegating to the per-method
	// hook (because r.Body is single-shot — hooks that re-call
	// readAllBody on r get empty bytes). Tests assert on the captured
	// bytes when they need to inspect the daemon-facing request
	// payload (e.g. quote/backslash round-trip checks).
	bodyMu        sync.Mutex
	lastInitBody  []byte
	lastListBody  []byte
	lastCallBody  []byte
}

func newStubDaemon(t *testing.T, sessionID string) *stubDaemon {
	t.Helper()
	sd := &stubDaemon{sessionID: sessionID}
	sd.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := readAllBody(r)
		method := peekMethod(body)
		switch method {
		case "initialize":
			sd.initCount.Add(1)
			sd.bodyMu.Lock()
			sd.lastInitBody = append([]byte(nil), body...)
			sd.bodyMu.Unlock()
			if sd.onInit != nil {
				sd.onInit(w, r)
				return
			}
			w.Header().Set("Mcp-Session-Id", sd.sessionID)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
		case "tools/list":
			sd.listCount.Add(1)
			sd.bodyMu.Lock()
			sd.lastListBody = append([]byte(nil), body...)
			sd.bodyMu.Unlock()
			if sd.onList != nil {
				sd.onList(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"d1"},{"name":"write","description":"d2"}]}}`))
		case "tools/call":
			sd.callCount.Add(1)
			sd.bodyMu.Lock()
			sd.lastCallBody = append([]byte(nil), body...)
			sd.bodyMu.Unlock()
			if sd.onCall != nil {
				sd.onCall(w, r)
				return
			}
			// Echo back: include the params.name we received so tests
			// can assert canonical rewrite.
			var env struct {
				ID     json.RawMessage `json:"id"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			_ = json.Unmarshal(body, &env)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      env.ID,
				"result":  map[string]any{"content": []any{map[string]any{"type": "text", "text": "called=" + env.Params.Name}}},
			}
			out, _ := json.Marshal(resp)
			_, _ = w.Write(out)
		case "notifications/cancelled", "notifications/initialized":
			sd.notifyCount.Add(1)
			if sd.onNotify != nil {
				sd.onNotify(w, r, body)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(sd.server.Close)
	// Extract host:port → port. The aggregator uses the URL from the
	// canonicalDaemonRef.Port + 127.0.0.1, so we wire the daemon's
	// dynamically-assigned port back into the ref.
	u := sd.server.URL // "http://127.0.0.1:NNNNN"
	sd.port = portFromURL(u)
	return sd
}

func readAllBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	var buf strings.Builder
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			break
		}
	}
	return []byte(buf.String()), nil
}

func peekMethod(b []byte) string {
	var env struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(b, &env)
	return env.Method
}

func portFromURL(u string) int {
	// "http://127.0.0.1:53247" → 53247
	const prefix = "http://127.0.0.1:"
	if !strings.HasPrefix(u, prefix) {
		return 0
	}
	rest := u[len(prefix):]
	port := 0
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c < '0' || c > '9' {
			break
		}
		port = port*10 + int(c-'0')
	}
	return port
}

// helper: synthesize a hubSession with two participating daemons
// pointing at the stubs' ports.
func sessionWithParticipants(daemons ...*stubDaemon) *hubSession {
	s := &hubSession{
		ClientSessionID:  "client-sid-1",
		Client:           "claude-code",
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           time.Now(),
		LastUsedAt:       time.Now(),
	}
	for i, d := range daemons {
		ref := canonicalDaemonRef{
			Server: "srv" + string(rune('1'+i)),
			Daemon: "claude-code",
			Port:   d.port,
		}
		s.IntendedParticipants = append(s.IntendedParticipants, ref)
	}
	return s
}

func TestAggregateInitializePopulatesSuccessesAndFailures(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid")
	d2.onInit = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	sess := sessionWithParticipants(d1, d2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 1 {
		t.Errorf("InitSuccesses=%d want 1: %+v", len(sess.InitSuccesses), sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Errorf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	if len(sess.InitFailures) == 1 && sess.InitFailures[0].Stage != "initialize" {
		t.Errorf("InitFailures[0].Stage=%q want initialize", sess.InitFailures[0].Stage)
	}
	// Verify Mcp-Session-Id captured from the successful daemon.
	var found bool
	for _, sid := range sess.InitSuccesses {
		if sid == "d1-sid" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected daemon Mcp-Session-Id d1-sid, got %+v", sess.InitSuccesses)
	}
}

func TestAggregateToolsListMergesAndNamespaces(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid")
	sess := sessionWithParticipants(d1, d2)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 2 {
		t.Fatalf("want 2 init successes, got %d", len(sess.InitSuccesses))
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	// Parse response envelope.
	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Meta map[string]any `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("jsonrpc=%q want 2.0", env.JSONRPC)
	}
	// Namespaced names: "srv1__read", "srv1__write", "srv2__read", "srv2__write".
	want := map[string]bool{
		"srv1__read": true, "srv1__write": true,
		"srv2__read": true, "srv2__write": true,
	}
	got := map[string]bool{}
	for _, tool := range env.Result.Tools {
		got[tool.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing namespaced tool %q in %v", name, got)
		}
	}
	// RouteMap populated.
	rm := sess.RouteMap.Load()
	if rm == nil {
		t.Fatal("RouteMap not published")
	}
	if ref, ok := (*rm)["srv1__read"]; !ok || ref.RawName != "read" {
		t.Errorf("RouteMap missing srv1__read or RawName wrong: %+v ok=%v", ref, ok)
	}
}

func TestAggregateToolsListReportsAllFailedAsErrorMinus32000(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid")
	// Both return 503 on initialize → no init successes.
	bad := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	d1.onInit = bad
	d2.onInit = bad

	sess := sessionWithParticipants(d1, d2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = AggregateInitialize(ctx, sess, json.RawMessage(`1`))
	if len(sess.InitSuccesses) != 0 {
		t.Fatalf("expected 0 init successes, got %d", len(sess.InitSuccesses))
	}
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"data"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected error envelope, got result: %s", string(env.Result))
	}
	if env.Error.Code != -32000 {
		t.Errorf("Error.Code=%d want -32000", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "all participating daemons failed") {
		t.Errorf("Error.Message=%q want 'all participating daemons failed' substring", env.Error.Message)
	}
	// Cleanup #5 — partialFailures MUST include both init failures with
	// stage="initialize" and err containing "HTTP 503". The aggregator
	// emits one row per fan-out failure; here both daemons failed init
	// so no list-stage rows are expected.
	failures := env.Error.Data.Mcphub.PartialFailures
	if len(failures) != 2 {
		t.Fatalf("partialFailures=%d want 2: %+v", len(failures), failures)
	}
	servers := map[string]DaemonFailure{}
	for _, f := range failures {
		servers[f.Server] = f
	}
	for _, srv := range []string{"srv1", "srv2"} {
		f, ok := servers[srv]
		if !ok {
			t.Errorf("partialFailures missing server %q: %+v", srv, failures)
			continue
		}
		if f.Daemon != "claude-code" {
			t.Errorf("partialFailures[%q].Daemon=%q want claude-code", srv, f.Daemon)
		}
		if f.Stage != "initialize" {
			t.Errorf("partialFailures[%q].Stage=%q want initialize", srv, f.Stage)
		}
		if !strings.Contains(f.Err, "HTTP 503") {
			t.Errorf("partialFailures[%q].Err=%q want substring 'HTTP 503'", srv, f.Err)
		}
	}
}

// Cleanup #1b — init succeeded for every daemon, but EVERY tools/list
// call failed. listSuccessCount == 0 → -32000 all-failed envelope.
// The partialFailures rows must be stage="tools/list" (NOT
// "initialize"), since the initialize fan-out itself succeeded.
func TestAggregateToolsListAllListFailedReturnsMinus32000(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid")
	// initialize OK, tools/list returns 503.
	listBad := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	d1.onList = listBad
	d2.onList = listBad

	sess := sessionWithParticipants(d1, d2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if len(sess.InitSuccesses) != 2 {
		t.Fatalf("want 2 init successes, got %d", len(sess.InitSuccesses))
	}
	if len(sess.InitFailures) != 0 {
		t.Fatalf("want 0 init failures, got %d: %+v", len(sess.InitFailures), sess.InitFailures)
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"data"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected -32000 error envelope, got result: %s", string(env.Result))
	}
	if env.Error.Code != -32000 {
		t.Errorf("Error.Code=%d want -32000", env.Error.Code)
	}
	failures := env.Error.Data.Mcphub.PartialFailures
	if len(failures) != 2 {
		t.Fatalf("partialFailures=%d want 2: %+v", len(failures), failures)
	}
	for _, f := range failures {
		if f.Stage != "tools/list" {
			t.Errorf("partialFailures[%s].Stage=%q want tools/list (init succeeded, list failed)", f.Server, f.Stage)
		}
		if !strings.Contains(f.Err, "HTTP 503") {
			t.Errorf("partialFailures[%s].Err=%q want substring 'HTTP 503'", f.Server, f.Err)
		}
	}
}

func TestAggregateToolsCallCanonicalRewrite(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`)); err != nil {
		t.Fatal(err)
	}

	// Set up a non-stale resolver snapshot that includes srv1+claude-code
	// in the calling client's bindings (so the stale-revalidation check
	// passes).
	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	// Call srv1__read; the daemon should see params.name="read".
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), `called=read`) {
		t.Errorf("daemon did not receive rewritten name 'read': %s", string(body))
	}
	if d1.callCount.Load() != 1 {
		t.Errorf("d1 callCount=%d want 1", d1.callCount.Load())
	}
	// In-flight count returns to zero after the call completes.
	if got := sess.InFlightCount(); got != 0 {
		t.Errorf("inFlight=%d after call want 0", got)
	}
}

func TestAggregateToolsCallStaleResolverRefuses(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`)); err != nil {
		t.Fatal(err)
	}

	// session captured snap@gen1; resolver republished at gen2 WITHOUT
	// the (Server, Daemon) tuple in this client's bindings.
	resetResolverForTest(t)
	old := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = old
	// Current snapshot: no daemons.
	current := &ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": nil},
	}
	PublishResolverSnapshot(current)

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected error envelope, got body: %s", string(body))
	}
	if env.Error.Code != -32601 {
		t.Errorf("Error.Code=%d want -32601", env.Error.Code)
	}
	if !strings.Contains(env.Error.Message, "tool moved out of scope") {
		t.Errorf("Error.Message=%q want 'tool moved out of scope' substring", env.Error.Message)
	}
}

func TestAggregateToolsCallUnknownNameReturnsMinus32601(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`)); err != nil {
		t.Fatal(err)
	}
	// publish a non-stale resolver.
	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	params := json.RawMessage(`{"name":"bogus__missing","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), `-32601`) {
		t.Errorf("expected -32601, got %s", string(body))
	}
}

// Fan-out concurrency: AggregateInitialize fans 8 daemons out under
// the FanOutConcurrency=8 semaphore. Each daemon records max-concurrent
// init calls; the aggregated max stays <= FanOutConcurrency.
func TestAggregateInitializeConcurrencyBound(t *testing.T) {
	// 12 daemons, each delays 50ms in initialize to force overlap.
	var maxConcurrent atomic.Int32
	var currentConcurrent atomic.Int32
	delay := func(w http.ResponseWriter, r *http.Request) {
		now := currentConcurrent.Add(1)
		for {
			peak := maxConcurrent.Load()
			if now <= peak {
				break
			}
			if maxConcurrent.CompareAndSwap(peak, now) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		currentConcurrent.Add(-1)
		w.Header().Set("Mcp-Session-Id", "sid")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}
	const N = 12
	daemons := make([]*stubDaemon, 0, N)
	for i := 0; i < N; i++ {
		d := newStubDaemon(t, "sid")
		d.onInit = delay
		daemons = append(daemons, d)
	}
	sess := sessionWithParticipants(daemons...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if maxConcurrent.Load() > int32(FanOutConcurrency) {
		t.Errorf("concurrency bound violated: max=%d cap=%d", maxConcurrent.Load(), FanOutConcurrency)
	}
}

// TestForwardCancellationKeepsInFlightUntilCompletion pins codex
// bot r15 P2 closure on PR #157.
//
// MCP cancellation is best-effort. The daemon may still finish the
// original call later, so the original tools/call goroutine in
// dispatchToolsCall owns the in-flight row's lifecycle and removes
// it via its own `defer sess.RemoveInFlight` when the call
// completes. ForwardCancellation MUST NOT delete the row early —
// doing so would let a second tools/call with the same client
// request id slip past InsertInFlight's duplicate detection and
// cause ambiguous response correlation.
//
// Expected behavior:
//  1. ForwardCancellation sends notifications/cancelled to the
//     daemon with the daemon-generated request id.
//  2. The in-flight row stays until the original dispatch goroutine
//     runs its defer.
//  3. A second InsertInFlight with the same key returns false
//     (duplicate-detection still works during the cancellation race).
//  4. Daemon-visible body still carries the daemon-side request id
//     (`"hub-7"`), not the client id (`99`).
func TestForwardCancellationKeepsInFlightUntilCompletion(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	var notifyMu sync.Mutex
	var notifyBodies [][]byte
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		notifyMu.Lock()
		notifyBodies = append(notifyBodies, append([]byte(nil), body...))
		notifyMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}
	sess := sessionWithParticipants(d1)
	sess.InitSuccesses[sess.IntendedParticipants[0]] = "d1-sid"

	clientReqID := json.RawMessage(`99`)
	key, _ := newRequestIDKey(clientReqID)
	originalEntry := inflightEntry{
		DaemonRef:       sess.IntendedParticipants[0],
		DaemonSessionID: "d1-sid",
		DaemonRequestID: json.RawMessage(`"hub-7"`),
		StartedAt:       time.Now(),
	}
	if !sess.InsertInFlight(key, originalEntry) {
		t.Fatalf("setup: InsertInFlight returned false on empty session")
	}
	if sess.InFlightCount() != 1 {
		t.Fatalf("setup inFlight=%d want 1", sess.InFlightCount())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ForwardCancellation(ctx, sess, clientReqID)

	// 1) In-flight row MUST still be present. The original
	//    dispatchToolsCall goroutine owns removal.
	if sess.InFlightCount() != 1 {
		t.Errorf("inFlight=%d after cancel want 1 (lifecycle owned by dispatch goroutine)", sess.InFlightCount())
	}

	// 2) Duplicate-detection invariant: a second InsertInFlight with
	//    the same key MUST return false. The cancellation race must
	//    not open a window for duplicate slots.
	if sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       sess.IntendedParticipants[0],
		DaemonSessionID: "d1-sid",
		DaemonRequestID: json.RawMessage(`"hub-8"`),
		StartedAt:       time.Now(),
	}) {
		t.Errorf("InsertInFlight accepted duplicate during cancellation race — duplicate detection broken")
	}

	// 3) Daemon must have received exactly one
	//    notifications/cancelled envelope carrying the daemon-side id.
	if d1.notifyCount.Load() != 1 {
		t.Fatalf("daemon notifyCount=%d want 1", d1.notifyCount.Load())
	}
	notifyMu.Lock()
	defer notifyMu.Unlock()
	if len(notifyBodies) != 1 {
		t.Fatalf("notifyBodies=%d want 1", len(notifyBodies))
	}
	var env struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(notifyBodies[0], &env); err != nil {
		t.Fatalf("parse notify body: %v raw=%s", err, string(notifyBodies[0]))
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("notify jsonrpc=%q want 2.0", env.JSONRPC)
	}
	if env.Method != "notifications/cancelled" {
		t.Errorf("notify method=%q want notifications/cancelled", env.Method)
	}
	// The daemon receives the hub-generated daemon request id, not
	// the client's id (99). The test inserted "hub-7" as the daemon
	// request id.
	if string(env.Params.RequestID) != `"hub-7"` {
		t.Errorf("notify params.requestId=%s want \"hub-7\"", string(env.Params.RequestID))
	}

	// 4) Simulate dispatch-goroutine completion: explicit RemoveInFlight.
	//    Confirms the row IS removable after ForwardCancellation, so
	//    the change doesn't leak entries forever.
	sess.RemoveInFlight(key)
	if sess.InFlightCount() != 0 {
		t.Errorf("inFlight=%d after dispatch completion want 0", sess.InFlightCount())
	}
}

// TestAggregateInitializeEchoesClientRequestID pins the codex bot r1
// P1 closure: synthetic initialize response must use the CLIENT's
// JSON-RPC id, not a hardcoded id:1. Strict JSON-RPC clients validate
// id correlation between request and response; a mismatch breaks the
// initialize handshake even when the daemon fan-out succeeds.
func TestAggregateInitializeEchoesClientRequestID(t *testing.T) {
	cases := []struct {
		name  string
		reqID json.RawMessage
		want  string
	}{
		{name: "string-id", reqID: json.RawMessage(`"hub-init-42"`), want: `"hub-init-42"`},
		{name: "large-number-id", reqID: json.RawMessage(`9007199254740993`), want: `9007199254740993`},
		{name: "small-number-id", reqID: json.RawMessage(`7`), want: `7`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d1 := newStubDaemon(t, "d1-sid")
			sess := sessionWithParticipants(d1)
			body, err := AggregateInitialize(context.Background(), sess, tc.reqID)
			if err != nil {
				t.Fatalf("AggregateInitialize: %v", err)
			}
			var env struct {
				ID json.RawMessage `json:"id"`
			}
			if uerr := json.Unmarshal(body, &env); uerr != nil {
				t.Fatalf("unmarshal: %v / body=%s", uerr, body)
			}
			if string(env.ID) != tc.want {
				t.Errorf("response id = %s, want %s; body=%s", env.ID, tc.want, body)
			}
		})
	}
}

// TestAggregateToolsCallRejectsDuplicateInFlightID pins codex bot r1
// P2 closure: a second tools/call with the SAME client request id
// (while the first is still in flight) must be refused with -32600
// rather than overwrite the existing in-flight row.
func TestAggregateToolsCallRejectsDuplicateInFlightID(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	// Capture the current resolver snapshot into the session so
	// AggregateToolsCall's per-call revalidation passes (snapshot
	// pointer equality keeps the daemonStillBound check from firing
	// on the leftover snapshot from prior tests).
	sess.SnapshotAtInit = LoadResolverSnapshot()
	// Pre-populate route + in-flight so a second InsertInFlight for
	// the same client id collides.
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: d1.port, RawName: "read"}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
	key, err := newRequestIDKey(json.RawMessage(`"dup-id-7"`))
	if err != nil {
		t.Fatalf("newRequestIDKey: %v", err)
	}
	sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port},
		DaemonRequestID: json.RawMessage(`"hub-orig"`),
		StartedAt:       time.Now(),
	})
	// Now call AggregateToolsCall with the same client request id —
	// must refuse.
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(context.Background(), sess, json.RawMessage(`"dup-id-7"`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall returned error: %v", err)
	}
	var env struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("unmarshal: %v / body=%s", uerr, body)
	}
	if env.Error.Code != -32600 {
		t.Errorf("error.code = %d, want -32600; body=%s", env.Error.Code, body)
	}
	if !strings.Contains(env.Error.Message, "duplicate") {
		t.Errorf("error.message must mention 'duplicate'; got %q", env.Error.Message)
	}
}

// TestBuildRewrittenParamsPreservesBigIntegerPrecision pins the codex
// bot r3 P1 closure: rewriting the `name` field must NOT round numeric
// argument values through float64. Earlier `map[string]any` round-trip
// silently dropped precision for integers > 2^53, so tools could
// execute against the wrong resource id.
func TestBuildRewrittenParamsPreservesBigIntegerPrecision(t *testing.T) {
	// 2^53 + 1, unrepresentable exactly as float64.
	in := json.RawMessage(`{"name":"srv1__read","arguments":{"resource_id":9007199254740993,"nested":{"big":18446744073709551615}}}`)
	out, err := buildRewrittenParams("read", in)
	if err != nil {
		t.Fatalf("buildRewrittenParams: %v", err)
	}
	if !strings.Contains(string(out), `9007199254740993`) {
		t.Errorf("resource_id 9007199254740993 lost in rewrite: %s", out)
	}
	if !strings.Contains(string(out), `18446744073709551615`) {
		t.Errorf("nested big int 18446744073709551615 lost in rewrite: %s", out)
	}
	// And `name` was rewritten to the raw form.
	if !strings.Contains(string(out), `"name":"read"`) {
		t.Errorf("name not rewritten: %s", out)
	}
	if strings.Contains(string(out), `"name":"srv1__read"`) {
		t.Errorf("old namespaced name still present: %s", out)
	}
}

// TestPostInitializeSendsInitializedNotification pins the codex bot
// r4 P1 closure: after a successful initialize, the hub must send
// notifications/initialized to the daemon. Strict daemons can reject
// or ignore subsequent method calls until they observe this.
func TestPostInitializeSendsInitializedNotification(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	var sawInitialized atomic.Int32
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		if strings.Contains(string(body), `"notifications/initialized"`) {
			sawInitialized.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}
	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	// Give the best-effort notification a moment to flush.
	time.Sleep(100 * time.Millisecond)
	if got := sawInitialized.Load(); got != 1 {
		t.Errorf("daemon must receive notifications/initialized; got %d", got)
	}
}

// TestPostToolsListIncludesProtocolVersionHeader pins the codex bot
// r4 P1 closure: post-initialize HTTP calls must carry the
// MCP-Protocol-Version header.
func TestPostToolsListIncludesProtocolVersionHeader(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	var protoHeader string
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		protoHeader = r.Header.Get("MCP-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
	}
	sess := sessionWithParticipants(d1)
	sess.ProtocolVersion = "2025-11-25"
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AggregateToolsList(context.Background(), sess, json.RawMessage(`2`)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if protoHeader != "2025-11-25" {
		t.Errorf("tools/list MCP-Protocol-Version header = %q, want %q", protoHeader, "2025-11-25")
	}
}

// TestNameSpaceToolsPreservesNumericPrecision pins the codex bot r4
// P2 closure: rewriting `name` must not round numeric fields in tool
// metadata (e.g. inputSchema defaults, enum values).
func TestNameSpaceToolsPreservesNumericPrecision(t *testing.T) {
	tools := []json.RawMessage{
		json.RawMessage(`{"name":"read","inputSchema":{"properties":{"limit":{"default":9007199254740993}}}}`),
	}
	ref := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: 9101}
	out := nameSpaceTools(ref, tools)
	if len(out) != 1 {
		t.Fatalf("want 1 tool, got %d", len(out))
	}
	if !strings.Contains(string(out[0].Body), `9007199254740993`) {
		t.Errorf("default value 9007199254740993 lost in namespace rewrite: %s", out[0].Body)
	}
	if !strings.Contains(string(out[0].Body), `"name":"srv1__read"`) {
		t.Errorf("name not namespaced: %s", out[0].Body)
	}
	if out[0].Exposed != "srv1__read" {
		t.Errorf("Exposed=%q want srv1__read", out[0].Exposed)
	}
}

// TestAggregateInitializeFailsOnInitializedNotificationError pins the
// codex bot r5 P2 closure: if notifications/initialized fails after
// initialize succeeds, the daemon must be recorded as an init failure
// (stage="initialize") so subsequent calls report at the right stage.
func TestAggregateInitializeFailsOnInitializedNotificationError(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		// Simulate strict daemon that rejects the lifecycle notification.
		w.WriteHeader(http.StatusInternalServerError)
	}
	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 0 {
		t.Errorf("daemon must NOT be in InitSuccesses when notification failed; got %+v", sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	if sess.InitFailures[0].Stage != "initialize" {
		t.Errorf("Stage=%q want initialize", sess.InitFailures[0].Stage)
	}
	if !strings.Contains(sess.InitFailures[0].Err, "notifications/initialized") {
		t.Errorf("error must mention notifications/initialized; got %q", sess.InitFailures[0].Err)
	}
}

// TestRewriteResponseIDRejectsNullBody pins the codex bot r5 P1
// closure: a daemon returning body=`null` (HTTP 200) must NOT panic
// the request path. Earlier code would crash with "assignment to
// entry in nil map".
func TestRewriteResponseIDRejectsNullBody(t *testing.T) {
	_, err := rewriteResponseID([]byte("null"), json.RawMessage(`"client-id"`))
	if err == nil {
		t.Fatalf("expected error on null body; got nil")
	}
	if !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("error must explain why null is rejected; got %v", err)
	}
}

// TestAggregateInitializePersistsFallbackProtocolVersion pins the codex
// bot r6 P2 closure on PR #157. If a session is allocated without a
// negotiated MCP protocol version (sess.ProtocolVersion==""),
// AggregateInitialize must persist the fallback ONTO sess.ProtocolVersion
// so subsequent tools/list + tools/call calls (which re-read
// sess.ProtocolVersion at hub_mcp_aggregator.go:210 + :429) emit the
// MCP-Protocol-Version header consistently. Without persistence,
// initialize would carry the fallback but tools/list would send the
// header empty, breaking strict daemons that require it post-initialize.
func TestAggregateInitializePersistsFallbackProtocolVersion(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	var listProtoHeader string
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		listProtoHeader = r.Header.Get("MCP-Protocol-Version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
	}

	// Allocate a session with EMPTY ProtocolVersion to trip the fallback.
	sess := sessionWithParticipants(d1)
	sess.ProtocolVersion = ""

	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	// Fallback MUST be persisted onto the session, not just held locally.
	if sess.ProtocolVersion != hubProtocolVersionFallback {
		t.Errorf("sess.ProtocolVersion = %q after init; want %q (fallback must persist)",
			sess.ProtocolVersion, hubProtocolVersionFallback)
	}
	// And the subsequent tools/list call MUST carry the same fallback in
	// the MCP-Protocol-Version header — proves the persistence is wired
	// into the read paths that fanOutToolsList + postToolsList exercise.
	if _, err := AggregateToolsList(context.Background(), sess, json.RawMessage(`2`)); err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	if listProtoHeader != hubProtocolVersionFallback {
		t.Errorf("tools/list MCP-Protocol-Version header = %q, want %q (fallback must reach the daemon)",
			listProtoHeader, hubProtocolVersionFallback)
	}
}

// TestAggregateToolsListPreservesRouteMapOnAllFail pins the codex bot
// r6 P1 closure on PR #157. When every fan-out tools/list call fails
// (transient outage), the route map from the previously-successful
// tools/list must NOT be wiped. Otherwise a single bad list response
// strands every follow-up tools/call with -32601 "tool moved out of
// scope" even though the resolver state hasn't actually changed.
func TestAggregateToolsListPreservesRouteMapOnAllFail(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 1) First round: initialize + tools/list both succeed.
	//    RouteMap is populated with srv1__read + srv1__write.
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`2`)); err != nil {
		t.Fatalf("list#1: %v", err)
	}
	before := sess.RouteMap.Load()
	if before == nil {
		t.Fatalf("RouteMap unset after successful tools/list")
	}
	if _, ok := (*before)["srv1__read"]; !ok {
		t.Fatalf("RouteMap missing srv1__read after successful tools/list: %+v", *before)
	}
	keysBefore := len(*before)

	// 2) Daemon's list endpoint now fails — every fan-out returns HTTP 503.
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`3`))
	if err != nil {
		t.Fatalf("list#2: %v", err)
	}
	// The response is the all-failed envelope (-32000), confirmed by parse.
	var env struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("parse list#2 body: %v", uerr)
	}
	if env.Error == nil || env.Error.Code != -32000 {
		t.Fatalf("list#2 want -32000 error envelope; body=%s", string(body))
	}

	// 3) RouteMap MUST STILL hold the original routes — same pointer
	//    (no Store happened) and the same key set. Pointer-identity is
	//    the strongest assertion: it proves the route map was NEVER
	//    re-published, not even with identical contents.
	after := sess.RouteMap.Load()
	if after != before {
		t.Errorf("RouteMap pointer changed after all-failed tools/list; want preservation")
	}
	if got := len(*after); got != keysBefore {
		t.Errorf("RouteMap size = %d after all-failed list, want %d (preserved)", got, keysBefore)
	}
	if _, ok := (*after)["srv1__read"]; !ok {
		t.Errorf("RouteMap lost srv1__read after all-failed list: %+v", *after)
	}
}

// TestReadSSEResponseHandlesCompliantFrames pins the codex bot r10
// (P1+P2) + r12 (P1×3) closures on PR #157. readSSEResponse must:
//   - Accept `data:` with or without a single leading space.
//   - Join multiple `data:` lines within an event with `\n`.
//   - Respect event boundaries (empty line terminates an event).
//   - Skip events whose body is not a JSON-RPC response (pre-response
//     notifications like progress events).
//   - Handle CRLF line endings.
//   - Skip non-data fields (`event:`, `id:`, `retry:`, `:` comments).
//
// `id` discriminates JSON-RPC responses from notifications: responses
// have a non-empty `id` and no `method`; notifications have `method`
// and no `id`.
func TestReadSSEResponseHandlesCompliantFrames(t *testing.T) {
	const maxBytes = 16 * 1024
	cases := map[string]struct {
		raw  string
		want string
	}{
		"data-with-space":    {"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		"data-without-space": {"data:{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		"data-multi-line": {
			"data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n",
			"{\"jsonrpc\":\"2.0\",\n\"id\":1,\"result\":{}}",
		},
		"event-prefix-stripped": {
			"event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		"comment-line-ignored": {
			": keepalive\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n\n",
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		"crlf-terminated": {
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\r\n\r\n",
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
		"final-event-no-trailing-blank-line": {
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}",
			`{"jsonrpc":"2.0","id":1,"result":{}}`,
		},
	}
	for name, tc := range cases {
		got, err := readSSEResponse(strings.NewReader(tc.raw), maxBytes)
		if err != nil {
			t.Errorf("%s: unexpected error %v", name, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

// TestReadSSEResponseSkipsNotifications pins codex bot r12 P1 #3:
// when the daemon emits one or more progress notifications BEFORE
// the JSON-RPC response, the parser must skip those events and
// return ONLY the response event. The prior implementation would
// concatenate all `data:` payloads into one invalid JSON blob.
func TestReadSSEResponseSkipsNotifications(t *testing.T) {
	const maxBytes = 16 * 1024
	// Two pre-response notifications, then the response.
	raw := "" +
		"event: progress\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{\"step\":1}}\n" +
		"\n" +
		"event: progress\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"progress\",\"params\":{\"step\":2}}\n" +
		"\n" +
		"event: response\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":42,\"result\":{\"tools\":[]}}\n" +
		"\n"
	got, err := readSSEResponse(strings.NewReader(raw), maxBytes)
	if err != nil {
		t.Fatalf("readSSEResponse: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":42,"result":{"tools":[]}}`
	if string(got) != want {
		t.Errorf("got %q want %q (must skip pre-response notifications)", got, want)
	}
}

// TestReadSSEResponseRejectsStreamWithoutResponse pins codex bot
// r12 P1: a daemon emitting ONLY notifications (no JSON-RPC response
// envelope before EOF) must surface as a parse failure rather than
// silently succeed with an empty payload.
func TestReadSSEResponseRejectsStreamWithoutResponse(t *testing.T) {
	const maxBytes = 16 * 1024
	raw := "" +
		"event: progress\n" +
		"data: {\"jsonrpc\":\"2.0\",\"method\":\"progress\"}\n" +
		"\n"
	_, err := readSSEResponse(strings.NewReader(raw), maxBytes)
	if err == nil {
		t.Fatalf("must reject a stream with no JSON-RPC response")
	}
}

// TestReadSSEResponseEarlyExit pins codex bot r12 P1 #1: once the
// response event arrives the parser MUST return, even if the underlying
// stream remains open (compliant Streamable HTTP daemons may keep the
// connection open to send post-response notifications). We simulate
// the "open stream" via an io.Reader that blocks forever after
// emitting the response — readSSEResponse must return before reading
// past the response event.
func TestReadSSEResponseEarlyExit(t *testing.T) {
	const maxBytes = 16 * 1024
	prefix := "" +
		"event: response\n" +
		"data: {\"jsonrpc\":\"2.0\",\"id\":7,\"result\":{}}\n" +
		"\n"
	// Compose: response event + a Reader that blocks indefinitely. If
	// readSSEResponse incorrectly waits for EOF, the test hangs and
	// the test framework times out (no specific deadline here — the
	// surrounding `go test -timeout` catches it).
	blocker := &blockingReader{ch: make(chan struct{})}
	mr := io.MultiReader(strings.NewReader(prefix), blocker)

	done := make(chan struct{})
	var got []byte
	var gotErr error
	go func() {
		got, gotErr = readSSEResponse(mr, maxBytes)
		close(done)
	}()

	select {
	case <-done:
		// Good — returned without waiting for EOF.
	case <-time.After(2 * time.Second):
		// Force-unblock the goroutine so the test process can exit.
		close(blocker.ch)
		t.Fatalf("readSSEResponse blocked past the response event; expected early exit")
	}
	close(blocker.ch)
	if gotErr != nil {
		t.Fatalf("unexpected err: %v", gotErr)
	}
	want := `{"jsonrpc":"2.0","id":7,"result":{}}`
	if string(got) != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// blockingReader is an io.Reader that returns nothing until ch is
// closed, at which point it returns io.EOF.
type blockingReader struct {
	ch chan struct{}
}

func (b *blockingReader) Read(p []byte) (int, error) {
	<-b.ch
	return 0, io.EOF
}

// TestAggregateToolsListDeterministicOrdering pins codex bot r14 P2
// closure on PR #157. Two daemons returning disjoint tools must
// produce a result.tools array in alphabetical exposed-name order
// regardless of which goroutine completed its tools/list call
// first. The pre-r14 implementation appended in goroutine-completion
// order, so concurrent runs could yield different byte-identical
// responses even with identical inputs → unnecessary client cache
// churn.
//
// We run AggregateToolsList REPEATEDLY (5 times) with a deliberate
// per-daemon stagger that randomizes completion order, and assert
// every run produces the same sorted-by-exposed-name tools array.
func TestAggregateToolsListDeterministicOrdering(t *testing.T) {
	// d1 exposes one tool that sorts AFTER d2's; if completion order
	// leaked into output, d1's tool would appear first when d1
	// finished first, and second otherwise.
	d1 := newStubDaemon(t, "d1-sid")
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		// Simulate variable latency so completion order varies
		// across runs. Random sleep up to 5ms.
		time.Sleep(time.Duration(time.Now().UnixNano()%5) * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"zeta","description":"d1-zeta"}]}}`))
	}
	d2 := newStubDaemon(t, "d2-sid")
	d2.onList = func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(time.Duration(time.Now().UnixNano()%5) * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"alpha","description":"d2-alpha"}]}}`))
	}

	// Sessions: srv1 → d1, srv2 → d2. The exposed names will be
	// "srv1__zeta" and "srv2__alpha". Sorted alphabetically:
	// srv1__zeta < srv2__alpha (s-r-v-1 < s-r-v-2).
	makeSess := func() *hubSession {
		return sessionWithParticipants(d1, d2)
	}

	wantNames := []string{"srv1__zeta", "srv2__alpha"}
	for i := 0; i < 5; i++ {
		sess := makeSess()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
			cancel()
			t.Fatalf("iter %d: init: %v", i, err)
		}
		body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
		cancel()
		if err != nil {
			t.Fatalf("iter %d: list: %v", i, err)
		}
		var env struct {
			Result struct {
				Tools []json.RawMessage `json:"tools"`
			} `json:"result"`
		}
		if uerr := json.Unmarshal(body, &env); uerr != nil {
			t.Fatalf("iter %d: parse: %v body=%s", i, uerr, body)
		}
		gotNames := make([]string, 0, len(env.Result.Tools))
		for _, raw := range env.Result.Tools {
			var m struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &m)
			gotNames = append(gotNames, m.Name)
		}
		if len(gotNames) != len(wantNames) {
			t.Errorf("iter %d: got %d tools, want %d: %v", i, len(gotNames), len(wantNames), gotNames)
			continue
		}
		for j := range wantNames {
			if gotNames[j] != wantNames[j] {
				t.Errorf("iter %d: tools[%d] = %q, want %q (deterministic alpha order)", i, j, gotNames[j], wantNames[j])
			}
		}
	}
}

// TestAggregateToolsListIntraDaemonDuplicateNotCollision pins codex
// bot r13 P2 closure on PR #157. A single daemon returning the SAME
// tool name twice in its tools/list response is non-conformant but
// observed in the wild. The previous r9 implementation appended the
// daemon's ref per tool occurrence, so a single daemon's intra-list
// duplicate looked like a TWO-DAEMON namespace collision and dropped
// the tool from result.tools + RouteMap. Worse, two duplicate
// partialFailure rows were emitted for the same daemon.
//
// Expected behavior after r13:
//   - Tool stays in result.tools (deduplicated to ONE entry).
//   - RouteMap contains the routing entry.
//   - No partialFailure rows are emitted (routing is unambiguous).
//   - listSuccessCount = 1.
func TestAggregateToolsListIntraDaemonDuplicateNotCollision(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"first"},{"name":"read","description":"second"}]}}`))
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	var env struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
			Meta  struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("parse response: %v body=%s", uerr, body)
	}

	// 1) Exactly one srv1__read entry in result.tools (deduplicated).
	count := 0
	for _, raw := range env.Result.Tools {
		var m struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &m)
		if m.Name == "srv1__read" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("want 1 srv1__read entry after intra-daemon dedupe, got %d (tools=%+v)", count, env.Result.Tools)
	}

	// 2) No collision failure rows — intra-daemon duplicates are not
	// cross-daemon collisions.
	for _, f := range env.Result.Meta.Mcphub.PartialFailures {
		if strings.Contains(f.Err, "namespace collision") {
			t.Errorf("unexpected collision row for intra-daemon duplicate: %+v", f)
		}
	}

	// 3) RouteMap contains srv1__read — routing is unambiguous.
	rmPtr := sess.RouteMap.Load()
	if rmPtr == nil {
		t.Fatalf("RouteMap unset after intra-daemon-dedup tools/list")
	}
	if _, ok := (*rmPtr)["srv1__read"]; !ok {
		t.Errorf("RouteMap missing srv1__read after intra-daemon dedupe")
	}
}

// TestAggregateToolsListNamespaceCollision pins the codex bot r9 P2
// closure on PR #157. When two daemons under the SAME server expose
// the same raw tool name, the resulting exposed name
// "<server>__<rawname>" collides in the route map. The pre-r9
// behavior was a silent last-writer-wins overwrite — fan-out
// completion order is non-deterministic, so tools/call would route
// to different daemons on different runs.
//
// Expected post-r9 behavior:
//   - Colliding tool is dropped from result.tools (clients don't see
//     a tool they couldn't reliably call).
//   - One stage="tools/list" partialFailure row per colliding daemon,
//     err message naming every daemon that claimed the key. The
//     daemon-list ordering is deterministic (alphabetic by
//     server/daemon) so operator-facing diagnostics are stable.
//   - Non-colliding tools from the SAME daemons still appear in the
//     response (collision is per-tool, not per-daemon).
//   - listSuccessCount still counts collided daemons as successes;
//     the call returned cleanly even though the merge dropped some
//     tools.
func TestAggregateToolsListNamespaceCollision(t *testing.T) {
	// Two daemons both claiming srv1: one exposes {read, write},
	// the other exposes {read, format}. `srv1__read` collides;
	// `srv1__write` and `srv1__format` are unique.
	d1 := newStubDaemon(t, "d1-sid")
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"d1-read"},{"name":"write","description":"d1-write"}]}}`))
	}
	d2 := newStubDaemon(t, "d2-sid")
	d2.onList = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"d2-read"},{"name":"format","description":"d2-format"}]}}`))
	}

	// Both daemons must report Server="srv1". sessionWithParticipants
	// auto-assigns srv1, srv2, … by index, so we override d2's ref
	// after construction by swapping in a session built manually.
	sess := &hubSession{
		ClientSessionID:  "client-sid-1",
		Client:           "claude-code",
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           time.Now(),
		LastUsedAt:       time.Now(),
		IntendedParticipants: []canonicalDaemonRef{
			{Server: "srv1", Daemon: "daemon-a", Port: d1.port},
			{Server: "srv1", Daemon: "daemon-b", Port: d2.port},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}
	if len(sess.InitSuccesses) != 2 {
		t.Fatalf("want 2 init successes, got %d", len(sess.InitSuccesses))
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`))
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	var env struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
			Meta  struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("parse response: %v body=%s", uerr, body)
	}

	// 1) result.tools must contain ONLY srv1__write + srv1__format.
	//    srv1__read is dropped because it collided.
	names := make(map[string]int)
	for _, t := range env.Result.Tools {
		var m struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(t, &m)
		names[m.Name]++
	}
	if names["srv1__read"] != 0 {
		t.Errorf("colliding tool srv1__read leaked into result.tools (count=%d)", names["srv1__read"])
	}
	if names["srv1__write"] != 1 || names["srv1__format"] != 1 {
		t.Errorf("non-colliding tools missing: write=%d format=%d (want 1 each)",
			names["srv1__write"], names["srv1__format"])
	}

	// 2) partialFailures must contain one row per colliding daemon.
	//    Stage="tools/list"; Err names BOTH daemons.
	collisionRows := 0
	for _, f := range env.Result.Meta.Mcphub.PartialFailures {
		if f.Stage != "tools/list" {
			continue
		}
		if !strings.Contains(f.Err, "namespace collision") {
			continue
		}
		collisionRows++
		// The err message must reference both daemons regardless of
		// which one this row is for — operator needs the full list
		// to disambiguate.
		if !strings.Contains(f.Err, "srv1/daemon-a") {
			t.Errorf("collision err omits srv1/daemon-a: %q", f.Err)
		}
		if !strings.Contains(f.Err, "srv1/daemon-b") {
			t.Errorf("collision err omits srv1/daemon-b: %q", f.Err)
		}
		if !strings.Contains(f.Err, `"srv1__read"`) {
			t.Errorf("collision err omits the colliding key: %q", f.Err)
		}
	}
	if collisionRows != 2 {
		t.Errorf("want 2 collision rows (one per daemon), got %d in partialFailures: %+v",
			collisionRows, env.Result.Meta.Mcphub.PartialFailures)
	}

	// 3) RouteMap must NOT contain the colliding key — a subsequent
	//    tools/call against srv1__read would otherwise resolve to
	//    whichever daemon happened to land in the map last.
	rmPtr := sess.RouteMap.Load()
	if rmPtr == nil {
		t.Fatalf("RouteMap unset after successful (non-empty) tools/list")
	}
	if _, ok := (*rmPtr)["srv1__read"]; ok {
		t.Errorf("RouteMap retains colliding key srv1__read")
	}
	if _, ok := (*rmPtr)["srv1__write"]; !ok {
		t.Errorf("RouteMap missing non-colliding key srv1__write")
	}
	if _, ok := (*rmPtr)["srv1__format"]; !ok {
		t.Errorf("RouteMap missing non-colliding key srv1__format")
	}
}

// TestNotificationsAcceptSSEResponseWithoutPayload pins codex bot
// r16 P1 closure on PR #157. JSON-RPC notifications
// (notifications/initialized, notifications/cancelled) have no
// response by spec. Daemons running streamable HTTP endpoints
// commonly return Content-Type: text/event-stream for every
// 2xx response, but for a notification the SSE stream legitimately
// contains no JSON-RPC response event — only keepalive comments or
// an empty body.
//
// The pre-r16 doDaemonPost routed every text/event-stream body
// through readSSEResponse, which errored with "stream ended without
// a JSON-RPC response event". For notifications/initialized this
// propagated through AggregateInitialize and recorded the daemon
// as a stage="initialize" InitFailure, even though the notification
// was correctly accepted by a healthy daemon.
//
// Expected behavior post-r16: doDaemonPost drains the SSE body for
// notification calls (expectResponse=false) without parsing,
// returns success on 2xx, and AggregateInitialize records the
// daemon in InitSuccesses.
func TestNotificationsAcceptSSEResponseWithoutPayload(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	// onNotify is called for both notifications/initialized AND
	// notifications/cancelled. Make it emit an SSE keepalive-only
	// body — no `data:` lines, just a comment. This mirrors what a
	// real streamable-HTTP daemon returns when the request is a
	// notification.
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(": keepalive\n\n"))
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}

	// Daemon MUST land in InitSuccesses despite the SSE-shaped
	// notification response. The pre-r16 code would have recorded
	// it in InitFailures with stage="initialize" and an error
	// mentioning "notifications/initialized".
	if len(sess.InitSuccesses) != 1 {
		t.Errorf("want 1 init success, got %d: %+v", len(sess.InitSuccesses), sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 0 {
		t.Errorf("want 0 init failures, got %d: %+v", len(sess.InitFailures), sess.InitFailures)
	}

	// Stretch test: ForwardCancellation also sends a notification
	// (notifications/cancelled). Insert an in-flight row + run a
	// cancellation. The daemon's notify hook still returns SSE-
	// without-payload. Cancellation MUST NOT propagate an error
	// out (and ForwardCancellation has no return type anyway — the
	// internal err is swallowed). The assertion here is that the
	// daemon observed the notification arrive.
	key, _ := newRequestIDKey(json.RawMessage(`42`))
	sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       sess.IntendedParticipants[0],
		DaemonSessionID: "d1-sid",
		DaemonRequestID: json.RawMessage(`"hub-x"`),
		StartedAt:       time.Now(),
	})
	ForwardCancellation(ctx, sess, json.RawMessage(`42`))
	// d1.notifyCount counts BOTH notifications/initialized AND
	// notifications/cancelled. AggregateInitialize sent one, then
	// ForwardCancellation sent another → count == 2.
	if got := d1.notifyCount.Load(); got != 2 {
		t.Errorf("daemon notifyCount=%d want 2 (initialized + cancelled)", got)
	}
}

// TestPostInitializeEscapesProtocolVersion pins the codex bot r8 P2
// closure on PR #157. The initialize envelope MUST be built with
// json.Marshal (struct) — never `fmt.Sprintf` into a JSON template
// literal — so a protocolVersion containing `"` or `\` cannot
// corrupt the outbound JSON. Phase 4 receives this value from the
// client handshake, so the input is attacker-influenced.
//
// We feed a deliberately hostile string ("};DROP TABLE--\) through
// sess.ProtocolVersion, drive AggregateInitialize, and confirm the
// daemon receives a well-formed JSON document with the exact byte
// content preserved by JSON encoding.
func TestPostInitializeEscapesProtocolVersion(t *testing.T) {
	const hostile = "evil-version-\"};DROP TABLE--\\"

	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	sess.ProtocolVersion = hostile

	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}

	d1.bodyMu.Lock()
	body := d1.lastInitBody
	d1.bodyMu.Unlock()
	if len(body) == 0 {
		t.Fatalf("dispatch loop did not capture the initialize request body")
	}

	// Parse the body the daemon received — it MUST be well-formed JSON.
	// The bug the bot caught would have produced a body like
	//   ...protocolVersion":"evil-version-"};DROP TABLE--\","capabilities":{}...
	// which is NOT valid JSON (unescaped `"` inside the string).
	var env struct {
		Params struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("daemon received malformed JSON: %v\nbody=%s", err, body)
	}

	// And the protocol version must round-trip byte-exactly through
	// JSON encoding — proving the encoder did its job rather than
	// dropping or mangling the hostile characters.
	if env.Params.ProtocolVersion != hostile {
		t.Errorf("protocolVersion round-trip lost data:\n  got  %q\n  want %q", env.Params.ProtocolVersion, hostile)
	}
}

// TestPostInitializeRejectsEmptySessionIDHeader pins the codex bot r6
// P1 closure on PR #157. A daemon that returns HTTP 200 + no JSON-RPC
// error but omits the Mcp-Session-Id header is not a usable session:
// the hub cannot route tools/list / tools/call / cancellation back
// because the header is mandatory per the MCP Streamable HTTP spec.
// AggregateInitialize must record such a daemon in InitFailures
// (stage="initialize") and surface the missing-header reason.
func TestPostInitializeRejectsEmptySessionIDHeader(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d1.onInit = func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200, no JSON-RPC error, NO Mcp-Session-Id header.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}

	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 0 {
		t.Errorf("daemon must NOT be recorded as success without Mcp-Session-Id; got %+v", sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	f := sess.InitFailures[0]
	if f.Stage != "initialize" {
		t.Errorf("Stage=%q want initialize", f.Stage)
	}
	if !strings.Contains(f.Err, "Mcp-Session-Id") {
		t.Errorf("error must mention Mcp-Session-Id; got %q", f.Err)
	}
}
