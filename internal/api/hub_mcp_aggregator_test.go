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
			if sd.onList != nil {
				sd.onList(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"d1"},{"name":"write","description":"d2"}]}}`))
		case "tools/call":
			sd.callCount.Add(1)
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
		case "notifications/cancelled":
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

	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	_, _ = AggregateInitialize(ctx, sess)
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
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
	if _, err := AggregateInitialize(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if maxConcurrent.Load() > int32(FanOutConcurrency) {
		t.Errorf("concurrency bound violated: max=%d cap=%d", maxConcurrent.Load(), FanOutConcurrency)
	}
}

// ForwardCancellation: insert an in-flight entry, then invoke
// ForwardCancellation; the entry is removed AND the daemon receives a
// notifications/cancelled envelope carrying the daemon's request id
// in params.requestId. Cleanup #4 strengthens this from "entry
// removed" only to also asserting the daemon-visible body.
func TestForwardCancellationRemovesInFlight(t *testing.T) {
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
	sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       sess.IntendedParticipants[0],
		DaemonSessionID: "d1-sid",
		DaemonRequestID: json.RawMessage(`"hub-7"`),
		StartedAt:       time.Now(),
	})
	if sess.InFlightCount() != 1 {
		t.Fatalf("inFlight=%d want 1", sess.InFlightCount())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ForwardCancellation(ctx, sess, clientReqID)
	if sess.InFlightCount() != 0 {
		t.Errorf("inFlight=%d after cancel want 0", sess.InFlightCount())
	}

	// Daemon must have received exactly one notifications/cancelled
	// envelope whose params.requestId matches the daemon-side id we
	// stored at InsertInFlight time (NOT the client-side id 99).
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
}
