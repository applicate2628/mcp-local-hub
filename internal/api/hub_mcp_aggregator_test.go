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
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	onDelete func(w http.ResponseWriter, r *http.Request)

	// counters
	initCount   atomic.Int32
	listCount   atomic.Int32
	callCount   atomic.Int32
	notifyCount atomic.Int32
	deleteCount atomic.Int32

	// bodyMu guards capture fields below. Updated by the dispatch
	// loop AFTER reading r.Body, BEFORE delegating to the per-method
	// hook (because r.Body is single-shot — hooks that re-call
	// readAllBody on r get empty bytes). Tests assert on the captured
	// bytes when they need to inspect the daemon-facing request
	// payload (e.g. quote/backslash round-trip checks).
	bodyMu       sync.Mutex
	lastInitBody []byte
	lastListBody []byte
	lastCallBody []byte
}

func newStubDaemon(t *testing.T, sessionID string) *stubDaemon {
	t.Helper()
	sd := &stubDaemon{sessionID: sessionID}
	sd.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			sd.deleteCount.Add(1)
			if sd.onDelete != nil {
				sd.onDelete(w, r)
				return
			}
			w.WriteHeader(http.StatusAccepted)
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
		ScopeKey:         "claude-code",
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
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

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
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
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
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

func TestAggregateToolsListFiltersRemovedLiveBinding(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}

	resetResolverForTest(t)
	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	current := &ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": nil},
	}
	PublishResolverSnapshot(current)

	listBefore := d1.listCount.Load()
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	names := decodeToolsListNames(t, body)
	for _, name := range names {
		if strings.HasPrefix(name, "srv1__") {
			t.Fatalf("removed daemon tool %q leaked into tools/list; names=%v body=%s", name, names, string(body))
		}
	}
	if got := d1.listCount.Load(); got != listBefore {
		t.Fatalf("tools/list fanned out to removed daemon; listCount %d -> %d", listBefore, got)
	}
	rm := sess.RouteMap.Load()
	if rm == nil {
		t.Fatalf("RouteMap not published after live binding removal")
	}
	if len(*rm) != 0 {
		t.Fatalf("RouteMap retained removed daemon routes: %+v", *rm)
	}

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	callBody, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	code, _ := decodeRPCError(t, callBody)
	if code != -32601 {
		t.Fatalf("tools/call code=%d want -32601 after matching list removal; body=%s", code, string(callBody))
	}
}

// TestAggregateToolsListDropsMovedBindingWithNoCachedSession pins the OPTION B'
// tools/list behavior: when a participant's live binding MOVED to a new port and
// has NO cached fresh session, the daemon is DROPPED from this tools/list response
// (its tools simply do not appear), the same outcome as a removed binding. A
// SECOND participant whose binding is unchanged still contributes its tools. The
// moved-to port is NEVER contacted (no synchronous reinit / list), and one
// `moved-binding-dropped-rediscover` info event is emitted to hub-mcp.log naming
// the old + new port so the drop is observable.
func TestAggregateToolsListDropsMovedBindingWithNoCachedSession(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })
	stateDir := hubMcpStateTestHelper(t)

	movingDaemon := newStubDaemon(t, "d1-sid")     // srv1: moves to a new port, no cached session → dropped
	stableDaemon := newStubDaemon(t, "d2-sid")     // srv2: unchanged binding → its tools survive
	newPortDaemon := newStubDaemon(t, "fresh-sid") // the moved-to port; must NEVER be contacted

	sess := sessionWithParticipants(movingDaemon, stableDaemon)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	// srv1 moves to newPortDaemon.port; srv2 stays on its original port.
	current := &ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			"claude-code": {
				{Server: sess.IntendedParticipants[0].Server, Daemon: sess.IntendedParticipants[0].Daemon, Port: newPortDaemon.port},
				sess.IntendedParticipants[1],
			},
		},
	}
	PublishResolverSnapshot(current)

	movingListBefore := movingDaemon.listCount.Load()
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	names := decodeToolsListNames(t, body)
	// srv2 (stable) contributes read + write; srv1 (moved, dropped) contributes nothing.
	for _, name := range names {
		if strings.HasPrefix(name, "srv1__") {
			t.Fatalf("dropped moved daemon's tool %q leaked into tools/list; names=%v body=%s", name, names, string(body))
		}
	}
	hasStable := false
	for _, name := range names {
		if strings.HasPrefix(name, "srv2__") {
			hasStable = true
		}
	}
	if !hasStable {
		t.Fatalf("stable daemon's tools missing from tools/list; names=%v body=%s", names, string(body))
	}
	// The moved-from port is not re-listed; the moved-to port is NEVER contacted.
	if got := movingDaemon.listCount.Load(); got != movingListBefore {
		t.Fatalf("moved-from daemon received tools/list; listCount %d -> %d", movingListBefore, got)
	}
	if got := newPortDaemon.listCount.Load(); got != 0 {
		t.Fatalf("moved-to port received tools/list (synchronous reinit?); listCount=%d want 0", got)
	}
	if got := newPortDaemon.initCount.Load(); got != 0 {
		t.Fatalf("moved-to port was initialized inside tools/list; initCount=%d want 0", got)
	}
	rm := sess.RouteMap.Load()
	if rm == nil {
		t.Fatalf("RouteMap not published")
	}
	for key := range *rm {
		if strings.HasPrefix(key, "srv1__") {
			t.Fatalf("RouteMap retained dropped moved-daemon route %q in %+v", key, *rm)
		}
	}

	// The drop event must be observable in hub-mcp.log, naming old + new port.
	logBytes, rerr := os.ReadFile(filepath.Join(stateDir, "hub-mcp.log"))
	if rerr != nil {
		t.Fatalf("read hub-mcp.log: %v", rerr)
	}
	if !bytes.Contains(logBytes, []byte(`"event":"moved-binding-dropped-rediscover"`)) {
		t.Fatalf("moved-binding-dropped-rediscover event not emitted; log=%s", logBytes)
	}
	if !bytes.Contains(logBytes, []byte(`"old_port":`)) || !bytes.Contains(logBytes, []byte(`"new_port":`)) {
		t.Fatalf("drop event missing old_port/new_port fields; log=%s", logBytes)
	}

	// A tools/call for the dropped daemon's tool returns -32601 (rediscover),
	// never dispatching to the dead old port or the moved-to port.
	callBody, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read","arguments":{}}`))
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	code, _ := decodeRPCError(t, callBody)
	if code != -32601 {
		t.Fatalf("tools/call for dropped moved daemon code=%d want -32601; body=%s", code, string(callBody))
	}
	if got := movingDaemon.callCount.Load(); got != 0 {
		t.Fatalf("moved-from daemon received tools/call; callCount=%d", got)
	}
	if got := newPortDaemon.callCount.Load(); got != 0 {
		t.Fatalf("moved-to port received tools/call; callCount=%d", got)
	}
}

// TestReinitDetachedCachesSessionWhenCallerCancels pins the detached-reinit
// session-lifecycle invariant (bot findings #1/#2/#3): the shared singleflight
// reinit runs on a DETACHED ctx, so it COMPLETES a full MCP handshake (initialize
// + notifications/initialized) and produces a live daemon session even when the
// triggering caller's ctx was cancelled and that caller walked away. With no live
// consumer the daemon session would ORPHAN. The fix drains the detached result and
// CACHES it (the natural consumer — the next request reuses it) so it never leaks.
//
// Drive: cancel the caller's ctx WHILE the work fn is blocked inside the daemon's
// initialize, then release the daemon. Assert (a) the caller saw ctx.Canceled,
// (b) the detached reinit still ran the FULL lifecycle — the daemon received both
// the initialize and the notifications/initialized (#2), and (c) the fresh session
// is CACHED in InitSuccesses (#1/#3 — cached, not orphaned). The daemon's idle-GC
// would otherwise be the only reaper; here a follow-up request reuses the session.
func TestReinitDetachedCachesSessionWhenCallerCancels(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const freshSID = "detached-fresh-sid"
	gate := make(chan struct{})
	d := newStubDaemon(t, freshSID)
	d.onInit = func(w http.ResponseWriter, r *http.Request) {
		<-gate // block the detached work fn inside initialize until released
		w.Header().Set("Mcp-Session-Id", freshSID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}

	sess := sessionWithParticipants(d)
	ref := sess.IntendedParticipants[0]

	// Caller ctx that we cancel while the detached work fn is gated.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct {
		state daemonInitState
		err   error
	}, 1)
	go func() {
		st, err := sess.reinitializeDaemonBinding(ctx, ref)
		done <- struct {
			state daemonInitState
			err   error
		}{st, err}
	}()

	// Wait until the work fn has reached the daemon's initialize (it is now
	// blocked on the gate), then cancel the caller's ctx so the select takes
	// the ctx.Done() branch.
	deadline := time.Now().Add(3 * time.Second)
	for d.initCount.Load() == 0 {
		if time.Now().After(deadline) {
			close(gate)
			t.Fatal("work fn never reached daemon initialize")
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancel()

	// The caller returns promptly with ctx.Canceled (it did NOT wait for the
	// detached work to finish).
	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("reinit returned nil err on caller-cancel; want ctx.Canceled (state=%+v)", res.state)
		}
		if res.state.SessionID != "" {
			t.Fatalf("reinit returned a session id on caller-cancel; want empty (state=%+v)", res.state)
		}
	case <-time.After(2 * time.Second):
		close(gate)
		t.Fatal("reinit did not return promptly after caller-cancel")
	}

	// Release the daemon so the DETACHED work fn completes the full handshake.
	close(gate)

	// The detached drain caches the fresh session. Poll until it lands.
	cacheDeadline := time.Now().Add(3 * time.Second)
	var cachedOK bool
	var cached daemonInitState
	for time.Now().Before(cacheDeadline) {
		if cached, cachedOK = sess.cachedDaemonInitState(ref); cachedOK {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cachedOK {
		t.Fatalf("detached reinit did not cache its session after caller-cancel (orphan leak); InitSuccesses=%v", sess.InitSuccesses)
	}
	if cached.SessionID != freshSID {
		t.Fatalf("cached session id=%q want %q", cached.SessionID, freshSID)
	}

	// #2: the detached reinit completed the FULL lifecycle — notifications/initialized
	// was sent under the detached ctx, not stopped at initialize.
	notifyDeadline := time.Now().Add(3 * time.Second)
	for d.notifyCount.Load() == 0 && time.Now().Before(notifyDeadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := d.notifyCount.Load(); got == 0 {
		t.Fatalf("detached reinit did not send notifications/initialized (half-initialized cached session); notifyCount=%d", got)
	}
	// The handshake produced exactly one live session; it was cached, never DELETEd.
	if got := d.deleteCount.Load(); got != 0 {
		t.Fatalf("detached reinit DELETEd a fully-initialized session it should have cached; deleteCount=%d", got)
	}
}

// TestEmptyKnownGroupWhenLastParticipantMovedAndDropped pins bot finding #4: a
// binding that MOVED to a new port with NO cached fresh session is DROPPED from
// tools/list (moved-no-cache → rediscover). When it is the ONLY participant, the
// empty-known decision must NOT count it as live — otherwise the group wrongly
// returns the -32000 all-failed envelope instead of the empty-known success the
// dropped-and-rediscover contract intends. countLiveParticipants must use the SAME
// keep/drop predicate as filterToolsListSuccessesByLiveSnapshot.
func TestEmptyKnownGroupWhenLastParticipantMovedAndDropped(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	scope := GroupScopeKey("moved-emptied")
	oldDaemon := newStubDaemon(t, "old-sid")
	newPortDaemon := newStubDaemon(t, "fresh-sid") // moved-to port; never contacted (no cached session)

	ref := canonicalDaemonRef{Server: "srv1", Daemon: "daemon-a", Port: oldDaemon.port}
	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{scope: {ref}},
		Groups:   map[string]bool{scope: true},
	}
	// The sole participant MOVED to newPortDaemon.port (still bound in the group,
	// just at a new port) — distinct from the removed-binding case where the scope
	// has zero bindings. No fresh session is cached for the new port → dropped.
	movedRef := canonicalDaemonRef{Server: ref.Server, Daemon: ref.Daemon, Port: newPortDaemon.port}
	PublishResolverSnapshot(&ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{scope: {movedRef}},
		Groups:   map[string]bool{scope: true},
	})

	sess := &hubSession{
		ClientSessionID:      "client-sid-moved-emptied",
		ScopeKey:             scope,
		ProtocolVersion:      "2025-11-25",
		SnapshotAtInit:       atInit,
		IntendedParticipants: []canonicalDaemonRef{ref},
		InitSuccesses:        map[canonicalDaemonRef]string{ref: "old-sid"},
		DaemonProtoVer:       map[canonicalDaemonRef]string{ref: "2025-11-25"},
		InFlightRequests:     map[requestIDKey]inflightEntry{},
		InitAt:               time.Now(),
		LastUsedAt:           time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	// The moved-and-dropped sole participant must yield an empty-KNOWN success,
	// not a -32000 all-failed envelope.
	assertEmptyKnownToolsList(t, body)
	// Neither port is contacted for tools/list: old binding is gone (moved),
	// new port has no cached session (dropped, rediscover later).
	if got := oldDaemon.listCount.Load(); got != 0 {
		t.Fatalf("moved-from daemon received tools/list; listCount=%d", got)
	}
	if got := newPortDaemon.listCount.Load(); got != 0 {
		t.Fatalf("moved-to port received tools/list (synchronous reinit?); listCount=%d", got)
	}
	if got := newPortDaemon.initCount.Load(); got != 0 {
		t.Fatalf("moved-to port was initialized inside tools/list; initCount=%d", got)
	}
}

func TestAggregateToolsListUsesOneResolverSnapshotForHiddenTools(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	scope := GroupScopeKey("frontend")
	d1 := newStubDaemon(t, "d1-sid")
	ref := canonicalDaemonRef{Server: "srv1", Daemon: "daemon-a", Port: d1.port}

	initial := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{scope: {ref}},
		ToolsHidden: map[string]map[string][]string{
			scope: {"srv1": {"read"}},
		},
		Groups: map[string]bool{scope: true},
	}
	republished := &ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{scope: {ref}},
		Groups:   map[string]bool{scope: true},
	}
	PublishResolverSnapshot(initial)

	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		PublishResolverSnapshot(republished)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read","description":"must stay hidden"}]}}`))
	}

	sess := &hubSession{
		ClientSessionID:      "client-sid-snapshot-once",
		ScopeKey:             scope,
		ProtocolVersion:      "2025-11-25",
		SnapshotAtInit:       initial,
		IntendedParticipants: []canonicalDaemonRef{ref},
		InitSuccesses:        map[canonicalDaemonRef]string{ref: "d1-sid"},
		DaemonProtoVer:       map[canonicalDaemonRef]string{ref: "2025-11-25"},
		InFlightRequests:     map[requestIDKey]inflightEntry{},
		InitAt:               time.Now(),
		LastUsedAt:           time.Now(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	if names := decodeToolsListNames(t, body); len(names) != 0 {
		t.Fatalf("tools/list used a later resolver snapshot for hidden tools; names=%v body=%s", names, string(body))
	}
	if rm := sess.RouteMap.Load(); rm == nil {
		t.Fatalf("RouteMap not published")
	} else if len(*rm) != 0 {
		t.Fatalf("hidden tool leaked into RouteMap after resolver republish: %+v", *rm)
	}
}

// TestAggregateToolsCallMovedNoCachedSessionReturnsMinus32601 pins the OPTION B'
// behavior: a tools/call whose live binding MOVED to a new port and has NO cached
// fresh session does NOT synchronously re-initialize inside tools/call. It returns
// the existing -32601 "tool moved out of scope; reinitialize session" (the same
// error a removed binding returns). The client re-handshakes / re-lists and
// rediscovers the tool at its new port. CRITICAL (r5 regression guard): NO
// outbound tools/call is dispatched to the dead old port (oldDaemon.callCount==0),
// AND nothing is dispatched to the new port either (newDaemon.callCount==0) — the
// hub does not reach for the moved daemon at all on this path.
func TestAggregateToolsCallMovedNoCachedSessionReturnsMinus32601(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "d1-old-sid"
	const freshSID = "d1-fresh-sid"

	oldDaemon := newStubDaemon(t, oldSID)
	newDaemon := newStubDaemon(t, freshSID)

	sess := sessionWithParticipants(oldDaemon)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	current := &ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			"claude-code": {{
				Server: sess.IntendedParticipants[0].Server,
				Daemon: sess.IntendedParticipants[0].Daemon,
				Port:   newDaemon.port,
			}},
		},
	}
	PublishResolverSnapshot(current)

	initBefore := newDaemon.initCount.Load()
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read","arguments":{}}`))
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	code, msg := decodeRPCError(t, body)
	if code != -32601 {
		t.Fatalf("tools/call code=%d msg=%q want -32601 for moved binding with no cached session; body=%s", code, msg, string(body))
	}
	if !strings.Contains(msg, "moved out of scope") {
		t.Fatalf("tools/call msg=%q want rediscover-prompt 'moved out of scope'; body=%s", msg, string(body))
	}
	// r5 regression guard: NO outbound dispatch to the dead old port.
	if got := oldDaemon.callCount.Load(); got != 0 {
		t.Fatalf("old moved-from daemon received tools/call (dispatch to dead old port); callCount=%d", got)
	}
	// And no synchronous re-initialize / dispatch to the new port either.
	if got := newDaemon.callCount.Load(); got != 0 {
		t.Fatalf("new moved-to daemon received tools/call; callCount=%d want 0 (no synchronous reinit)", got)
	}
	if got := newDaemon.initCount.Load(); got != initBefore {
		t.Fatalf("new moved-to daemon was re-initialized inside tools/call; initCount %d -> %d want unchanged", initBefore, got)
	}
}

// TestAggregateToolsCallMovedWithCachedFreshSessionDispatches pins the RETAINED
// fast path: when a same-port restart consumer already re-handshook the moved
// daemon (a fresh session for the new port is cached in InitSuccesses), a
// tools/call for the moved binding dispatches straight to the NEW port with the
// fresh session — no -32601, no extra initialize.
func TestAggregateToolsCallMovedWithCachedFreshSessionDispatches(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "d1-old-sid"
	const freshSID = "d1-fresh-sid"

	oldDaemon := newStubDaemon(t, oldSID)
	newDaemon := newStubDaemon(t, freshSID)
	newDaemon.onCall = func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Mcp-Session-Id"); got != freshSID {
			http.Error(w, "stale moved-binding session", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"daemon","result":{"content":[{"type":"text","text":"called=read"}]}}`))
	}

	sess := sessionWithParticipants(oldDaemon)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	movedRef := canonicalDaemonRef{
		Server: sess.IntendedParticipants[0].Server,
		Daemon: sess.IntendedParticipants[0].Daemon,
		Port:   newDaemon.port,
	}
	current := &ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {movedRef}},
	}
	PublishResolverSnapshot(current)

	// Simulate the same-port restart consumers having already re-handshook the
	// moved daemon: seed a fresh cached session for the NEW port.
	sess.mu.Lock()
	sess.InitSuccesses[movedRef] = freshSID
	sess.DaemonProtoVer[movedRef] = "2025-11-25"
	sess.mu.Unlock()

	initBefore := newDaemon.initCount.Load()
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read","arguments":{}}`))
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), `called=read`) {
		t.Fatalf("tools/call did not dispatch through moved route with cached fresh session; body=%s", string(body))
	}
	if got := oldDaemon.callCount.Load(); got != 0 {
		t.Fatalf("old moved-from daemon received tools/call; callCount=%d", got)
	}
	if got := newDaemon.callCount.Load(); got != 1 {
		t.Fatalf("new moved-to daemon callCount=%d want 1", got)
	}
	if got := newDaemon.initCount.Load(); got != initBefore {
		t.Fatalf("cached fast path re-initialized the moved daemon; initCount %d -> %d want unchanged", initBefore, got)
	}
}

// TestAggregateToolsCallMovedRaceWithDeleteNoResurrection pins the OPTION B'
// safety property: a session DELETE concurrent with a tools/call whose binding
// MOVED to a new port can never resurrect a daemon session. In the old model the
// moved tools/call synchronously re-initialized the daemon, which could race
// handleDelete and resurrect a session after the DELETE. In the new model the
// moved-with-no-cached-session path returns -32601 WITHOUT contacting the daemon
// at all, so there is nothing to resurrect: DELETE always wins. This test drives
// the store DELETE and the moved tools/call concurrently and asserts -32601, no
// outbound dispatch to the moved-to port, and no fresh InitSuccesses entry.
func TestAggregateToolsCallMovedRaceWithDeleteNoResurrection(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	oldDaemon := newStubDaemon(t, "d1-sid")
	newPortDaemon := newStubDaemon(t, "fresh-sid") // moved-to port; must NEVER be contacted

	sess := sessionWithParticipants(oldDaemon)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}

	// Register the session in a store so the concurrent DELETE is a real removal.
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 16, MaxGlobal: 256})
	t.Cleanup(store.Close)
	store.mu.Lock()
	store.sessions[sess.ClientSessionID] = sess
	store.perClient[sess.ScopeKey]++
	store.lruIndex[sess.ClientSessionID] = store.lru.PushFront(sess.ClientSessionID)
	store.mu.Unlock()

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	movedRef := canonicalDaemonRef{
		Server: sess.IntendedParticipants[0].Server,
		Daemon: sess.IntendedParticipants[0].Daemon,
		Port:   newPortDaemon.port,
	}
	current := &ResolverSnapshot{
		Gen:      2,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {movedRef}},
	}
	PublishResolverSnapshot(current)

	// Race the DELETE against the moved tools/call.
	var wg sync.WaitGroup
	var callBody []byte
	var callErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		store.Delete(sess.ClientSessionID)
	}()
	go func() {
		defer wg.Done()
		callBody, callErr = AggregateToolsCall(ctx, sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read","arguments":{}}`))
	}()
	wg.Wait()

	if callErr != nil {
		t.Fatalf("AggregateToolsCall: %v", callErr)
	}
	code, _ := decodeRPCError(t, callBody)
	if code != -32601 {
		t.Fatalf("moved tools/call racing DELETE code=%d want -32601; body=%s", code, string(callBody))
	}
	// No resurrection: the moved-to port is never contacted on any path.
	if got := newPortDaemon.initCount.Load(); got != 0 {
		t.Fatalf("moved-to port was initialized (session resurrection); initCount=%d want 0", got)
	}
	if got := newPortDaemon.callCount.Load(); got != 0 {
		t.Fatalf("moved-to port received tools/call; callCount=%d want 0", got)
	}
	// And no fresh InitSuccesses entry was written for the moved port.
	sess.mu.Lock()
	_, resurrected := sess.InitSuccesses[movedRef]
	sess.mu.Unlock()
	if resurrected {
		t.Fatalf("InitSuccesses gained a fresh entry for the moved port after DELETE — session resurrected")
	}
}

func TestAggregateToolsListRemovedSuccessWithRetainedFailureReturnsAllFailed(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

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
		t.Fatalf("InitSuccesses=%d want 1: %+v", len(sess.InitSuccesses), sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	current := &ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			"claude-code": {sess.IntendedParticipants[1]},
		},
	}
	PublishResolverSnapshot(current)

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	code, msg := decodeRPCError(t, body)
	if code != -32000 {
		t.Fatalf("code=%d want -32000 all-failed after live filter removed only success; msg=%q body=%s", code, msg, string(body))
	}
	failures := decodeRPCPartialFailures(t, body)
	if len(failures) != 1 || failures[0].Server != "srv2" {
		t.Fatalf("partialFailures=%+v want only retained failed daemon srv2", failures)
	}
	if got := d1.listCount.Load(); got != 0 {
		t.Fatalf("removed successful daemon received tools/list; listCount=%d", got)
	}
}

func TestAggregateToolsListLiveFilterDropsRemovedRoutesAndInitFailures(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid")
	d3 := newStubDaemon(t, "d3-sid")
	d2.onInit = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	sess := sessionWithParticipants(d1, d2, d3)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 2 {
		t.Fatalf("InitSuccesses=%d want 2: %+v", len(sess.InitSuccesses), sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}

	atInit := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = atInit
	current := &ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			"claude-code": {sess.IntendedParticipants[2]},
		},
	}
	PublishResolverSnapshot(current)

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}
	names := decodeToolsListNames(t, body)
	for _, name := range names {
		if strings.HasPrefix(name, "srv1__") || strings.HasPrefix(name, "srv2__") {
			t.Fatalf("removed daemon tool %q leaked into tools/list; names=%v body=%s", name, names, string(body))
		}
	}
	if got := d1.listCount.Load(); got != 0 {
		t.Fatalf("removed successful daemon received tools/list; listCount=%d", got)
	}
	if got := d3.listCount.Load(); got != 1 {
		t.Fatalf("retained successful daemon listCount=%d want 1", got)
	}
	failures := decodeToolsListPartialFailures(t, body)
	if len(failures) != 0 {
		t.Fatalf("removed init failure leaked into partialFailures: %+v", failures)
	}
	rm := sess.RouteMap.Load()
	if rm == nil {
		t.Fatalf("RouteMap not published after retained daemon tools/list")
	}
	for key := range *rm {
		if strings.HasPrefix(key, "srv1__") || strings.HasPrefix(key, "srv2__") {
			t.Fatalf("RouteMap retained removed daemon route %q in %+v", key, *rm)
		}
	}
	if _, ok := (*rm)["srv3__read"]; !ok {
		t.Fatalf("RouteMap missing retained daemon route srv3__read: %+v", *rm)
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
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
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
	if _, err := AggregateToolsList(context.Background(), sess, json.RawMessage(`2`), ""); err != nil {
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

func TestAggregateInitializeDeletesDaemonSessionWhenInitializedNotificationFails(t *testing.T) {
	const daemonSID = "d1-sid"
	d1 := newStubDaemon(t, daemonSID)
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	// DELETE is fire-and-forget (detached goroutine); guard captures for -race
	// and read them only after waitForCount confirms the DELETE landed.
	var deleteMu sync.Mutex
	var deleteSID string
	var deleteProto string
	d1.onDelete = func(w http.ResponseWriter, r *http.Request) {
		deleteMu.Lock()
		deleteSID = r.Header.Get("Mcp-Session-Id")
		deleteProto = r.Header.Get("MCP-Protocol-Version")
		deleteMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}

	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(context.Background(), sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 0 {
		t.Fatalf("notification-failed daemon must not be retained in InitSuccesses: %+v", sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	if got := waitForCount(&d1.deleteCount, 1); got != 1 {
		t.Fatalf("daemon DELETE count=%d want 1", got)
	}
	deleteMu.Lock()
	gotSID, gotProto := deleteSID, deleteProto
	deleteMu.Unlock()
	if gotSID != daemonSID {
		t.Fatalf("daemon DELETE Mcp-Session-Id=%q want %q", gotSID, daemonSID)
	}
	if gotProto != "2025-11-25" {
		t.Fatalf("daemon DELETE MCP-Protocol-Version=%q want %q", gotProto, "2025-11-25")
	}
}

func TestAggregateInitializeCleanupSurvivesCanceledClientContext(t *testing.T) {
	const daemonSID = "d1-sid"
	d1 := newStubDaemon(t, daemonSID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		cancel()
		w.WriteHeader(http.StatusInternalServerError)
	}
	deleteSeen := make(chan struct{}, 1)
	d1.onDelete = func(w http.ResponseWriter, r *http.Request) {
		deleteSeen <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}

	sess := sessionWithParticipants(d1)
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 0 {
		t.Fatalf("notification-failed daemon must not be retained in InitSuccesses: %+v", sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("InitFailures=%d want 1: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	select {
	case <-deleteSeen:
	case <-time.After(2 * time.Second):
		t.Fatalf("daemon DELETE was not attempted after client context cancellation")
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
	if _, err := AggregateToolsList(context.Background(), sess, json.RawMessage(`2`), ""); err != nil {
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
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`2`), ""); err != nil {
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

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`3`), "")
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
		body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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

// TestAggregateToolsListRejectsMalformedDaemonResponse pins codex
// bot r17 P1 closure on PR #157. postToolsList must surface a
// daemon response missing `result.tools` (or even missing `result`
// entirely) as a list-stage failure. The pre-r17 implementation
// parsed `env.Result.Tools` as an empty slice when the field was
// absent, so fanOutToolsList incremented listSuccessCount and
// assembleToolsListResponse published an empty route map — silently
// wiping any routes from a previous successful list. Later
// tools/call would 404 with -32601 instead of operators seeing
// the real malformed-response error.
//
// We feed the daemon a body without `result.tools` AND a body
// without `result` entirely; both must produce stage="tools/list"
// failure rows. RouteMap is NOT wiped because listSuccessCount
// drops to 0 (r6 P1 closure) when every daemon fails the parse.
func TestAggregateToolsListRejectsMalformedDaemonResponse(t *testing.T) {
	cases := map[string]string{
		"missing-result-tools": `{"jsonrpc":"2.0","id":2,"result":{}}`,
		"missing-result":       `{"jsonrpc":"2.0","id":2}`,
	}
	for name, daemonBody := range cases {
		t.Run(name, func(t *testing.T) {
			d1 := newStubDaemon(t, "d1-sid")
			d1.onList = func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(daemonBody))
			}

			sess := sessionWithParticipants(d1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
				t.Fatalf("init: %v", err)
			}

			body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
			if err != nil {
				t.Fatalf("AggregateToolsList: %v", err)
			}

			// All daemons (only d1) failed → -32000 envelope. The
			// partialFailure row must surface the malformed-response
			// error so operators can fix the daemon.
			var env struct {
				Error *struct {
					Code int `json:"code"`
					Data struct {
						Mcphub struct {
							PartialFailures []DaemonFailure `json:"partialFailures"`
						} `json:"mcphub"`
					} `json:"data"`
				} `json:"error"`
			}
			if uerr := json.Unmarshal(body, &env); uerr != nil {
				t.Fatalf("parse: %v body=%s", uerr, body)
			}
			if env.Error == nil || env.Error.Code != -32000 {
				t.Fatalf("want -32000 all-failed envelope; body=%s", body)
			}
			failures := env.Error.Data.Mcphub.PartialFailures
			if len(failures) != 1 {
				t.Fatalf("want 1 failure row, got %d: %+v", len(failures), failures)
			}
			f := failures[0]
			if f.Stage != "tools/list" {
				t.Errorf("Stage=%q want tools/list", f.Stage)
			}
			if !strings.Contains(f.Err, "missing") {
				t.Errorf("err message should mention `missing`; got %q", f.Err)
			}
		})
	}
}

// TestNegotiatedProtocolVersionPropagates pins codex bot r17 P1
// closure on PR #157. When a daemon's initialize response carries
// `result.protocolVersion: <X>` different from what the hub
// requested, every follow-up request (notifications/initialized,
// tools/list, tools/call, notifications/cancelled) must use <X>
// as the MCP-Protocol-Version header — not the hub-requested
// version.
//
// We rig a daemon to negotiate a downgraded version, then assert
// the daemon-observed MCP-Protocol-Version header on each follow-up
// request matches the daemon's negotiated value.
func TestNegotiatedProtocolVersionPropagates(t *testing.T) {
	const requestedProto = "2025-11-25"
	const negotiatedProto = "2024-11-05" // daemon downgrades

	var hdrMu sync.Mutex
	listProtoHeader, callProtoHeader, cancelProtoHeader := "", "", ""
	initNotifProtoHeader := ""

	d1 := newStubDaemon(t, "d1-sid")
	d1.onInit = func(w http.ResponseWriter, r *http.Request) {
		// Echo a DIFFERENT protocolVersion than the requested one.
		w.Header().Set("Mcp-Session-Id", "d1-sid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"` + negotiatedProto + `","capabilities":{},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	d1.onList = func(w http.ResponseWriter, r *http.Request) {
		hdrMu.Lock()
		listProtoHeader = r.Header.Get("MCP-Protocol-Version")
		hdrMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"read"}]}}`))
	}
	d1.onCall = func(w http.ResponseWriter, r *http.Request) {
		hdrMu.Lock()
		callProtoHeader = r.Header.Get("MCP-Protocol-Version")
		hdrMu.Unlock()
		// r.Body was drained by the dispatch loop before this hook
		// fires; read from lastCallBody instead.
		d1.bodyMu.Lock()
		body := d1.lastCallBody
		d1.bodyMu.Unlock()
		var env struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &env)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		out := `{"jsonrpc":"2.0","id":` + string(env.ID) + `,"result":{"content":[]}}`
		_, _ = w.Write([]byte(out))
	}
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		hdrMu.Lock()
		// Distinguish notifications/initialized (sent in init flow,
		// captured first) from notifications/cancelled (sent later).
		if initNotifProtoHeader == "" {
			initNotifProtoHeader = r.Header.Get("MCP-Protocol-Version")
		} else {
			cancelProtoHeader = r.Header.Get("MCP-Protocol-Version")
		}
		hdrMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}

	sess := sessionWithParticipants(d1)
	sess.ProtocolVersion = requestedProto

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Per-daemon negotiated version stored on the session.
	if got := sess.DaemonProtoVer[sess.IntendedParticipants[0]]; got != negotiatedProto {
		t.Errorf("DaemonProtoVer[d1]=%q want %q", got, negotiatedProto)
	}

	// notifications/initialized used the negotiated version.
	hdrMu.Lock()
	gotInitNotif := initNotifProtoHeader
	hdrMu.Unlock()
	if gotInitNotif != negotiatedProto {
		t.Errorf("notifications/initialized MCP-Protocol-Version=%q want %q",
			gotInitNotif, negotiatedProto)
	}

	// tools/list uses the per-daemon negotiated version.
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`2`), ""); err != nil {
		t.Fatalf("list: %v", err)
	}
	hdrMu.Lock()
	gotList := listProtoHeader
	hdrMu.Unlock()
	if gotList != negotiatedProto {
		t.Errorf("tools/list MCP-Protocol-Version=%q want %q", gotList, negotiatedProto)
	}

	// tools/call uses the per-daemon negotiated version (via
	// inflightEntry.DaemonProtocol → postToolsCall).
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	// Set up resolver so tools/call route-validates.
	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)
	if _, err := AggregateToolsCall(ctx, sess, json.RawMessage(`3`), params); err != nil {
		t.Fatalf("call: %v", err)
	}
	hdrMu.Lock()
	gotCall := callProtoHeader
	hdrMu.Unlock()
	if gotCall != negotiatedProto {
		t.Errorf("tools/call MCP-Protocol-Version=%q want %q", gotCall, negotiatedProto)
	}

	// notifications/cancelled uses the per-daemon negotiated version
	// stored on the inflightEntry. Insert an entry + cancel.
	key, _ := newRequestIDKey(json.RawMessage(`88`))
	sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       sess.IntendedParticipants[0],
		DaemonSessionID: "d1-sid",
		DaemonProtocol:  negotiatedProto,
		DaemonRequestID: json.RawMessage(`"hub-y"`),
		StartedAt:       time.Now(),
	})
	ForwardCancellation(ctx, sess, json.RawMessage(`88`))
	hdrMu.Lock()
	gotCancel := cancelProtoHeader
	hdrMu.Unlock()
	if gotCancel != negotiatedProto {
		t.Errorf("notifications/cancelled MCP-Protocol-Version=%q want %q",
			gotCancel, negotiatedProto)
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

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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
		ScopeKey:         "claude-code",
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
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

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
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

// TestNotificationsReturnImmediatelyWithoutDrainingBody pins codex
// bot r18 P1 closure on PR #157. The notification path
// (expectResponse=false) must NOT drain the response body before
// returning — for daemons that keep text/event-stream connections
// open after accepting a notification, draining blocks until EOF
// or client timeout, turning a successful notification into a
// ~timeout-length stall that can cascade through AggregateInitialize.
//
// We simulate "stream stays open" via a daemon that writes a 200
// status + flushes a keepalive line and then BLOCKS for a long
// time before EOF. AggregateInitialize must complete in well
// under the PerDaemonInitTimeout (5s).
func TestNotificationsReturnImmediatelyWithoutDrainingBody(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	notifyDone := make(chan struct{})
	// onNotify keeps the connection open until notifyDone is closed.
	// The hub's doDaemonPost (expectResponse=false) must return BEFORE
	// the daemon closes the body, otherwise the call would block here.
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
		// Block: don't return until notifyDone closes. If the hub
		// reads body-to-EOF it stalls here.
		<-notifyDone
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	done := make(chan struct{})
	var initErr error
	go func() {
		_, initErr = AggregateInitialize(ctx, sess, json.RawMessage(`1`))
		close(done)
	}()
	select {
	case <-done:
		// AggregateInitialize returned without waiting for the
		// daemon's body EOF. Release the daemon goroutine so the
		// test process can exit cleanly.
		close(notifyDone)
	case <-time.After(2 * time.Second):
		// Force release; test will fail below.
		close(notifyDone)
		<-done
		t.Fatalf("AggregateInitialize blocked past 2s; expected immediate return on notification 2xx (elapsed %v)", time.Since(start))
	}
	if initErr != nil {
		t.Fatalf("AggregateInitialize: %v", initErr)
	}
	if len(sess.InitSuccesses) != 1 {
		t.Errorf("want 1 init success, got %d: %+v", len(sess.InitSuccesses), sess.InitSuccesses)
	}
	if elapsed := time.Since(start); elapsed > 1*time.Second {
		t.Errorf("AggregateInitialize took %v; expected sub-second (the daemon's body blocked indefinitely, hub should not have waited)", elapsed)
	}
}

// TestPostInitializeRejectsMissingResultEnvelope pins codex bot r18
// P2 closure on PR #157. A daemon returning HTTP 200 +
// `{"jsonrpc":"2.0","id":1}` (no error, no result) + a valid
// Mcp-Session-Id header must NOT be treated as a successful
// initialize. The bot's concern: the previous decode used
// `Result struct {...}` which silently became the zero value when
// the field was absent, so postInitialize returned success despite
// the missing result. Now a pointer type distinguishes absent vs
// present, and missing result surfaces as an init-stage failure.
func TestPostInitializeRejectsMissingResultEnvelope(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d1.onInit = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "d1-sid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// No `result` field, no `error` field — protocol violation.
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1}`))
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("AggregateInitialize: %v", err)
	}
	if len(sess.InitSuccesses) != 0 {
		t.Errorf("daemon must NOT be recorded as success on missing `result`; got %+v", sess.InitSuccesses)
	}
	if len(sess.InitFailures) != 1 {
		t.Fatalf("want 1 init failure, got %d: %+v", len(sess.InitFailures), sess.InitFailures)
	}
	f := sess.InitFailures[0]
	if f.Stage != "initialize" {
		t.Errorf("Stage=%q want initialize", f.Stage)
	}
	if !strings.Contains(f.Err, "missing `result`") {
		t.Errorf("error should mention missing result; got %q", f.Err)
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

// decodeRPCError extracts the {code,message} from a JSON-RPC error envelope,
// failing the test if the body is not an error envelope.
func decodeRPCError(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse error envelope: %v body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error envelope, got: %s", string(body))
	}
	return env.Error.Code, env.Error.Message
}

func decodeRPCPartialFailures(t *testing.T, body []byte) []DaemonFailure {
	t.Helper()
	var env struct {
		Error *struct {
			Data struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse error envelope: %v body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error envelope, got: %s", string(body))
	}
	return env.Error.Data.Mcphub.PartialFailures
}

func decodeToolsListNames(t *testing.T, body []byte) []string {
	t.Helper()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse tools/list envelope: %v body=%s", err, string(body))
	}
	if env.Error != nil {
		t.Fatalf("expected tools/list result envelope, got error code=%d message=%q body=%s", env.Error.Code, env.Error.Message, string(body))
	}
	if env.Result == nil {
		t.Fatalf("expected tools/list result, got body=%s", string(body))
	}
	names := make([]string, 0, len(env.Result.Tools))
	for _, tool := range env.Result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func decodeToolsListPartialFailures(t *testing.T, body []byte) []DaemonFailure {
	t.Helper()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result *struct {
			Meta struct {
				Mcphub struct {
					PartialFailures []DaemonFailure `json:"partialFailures"`
				} `json:"mcphub"`
			} `json:"_meta"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("parse tools/list envelope: %v body=%s", err, string(body))
	}
	if env.Error != nil {
		t.Fatalf("expected tools/list result envelope, got error code=%d message=%q body=%s", env.Error.Code, env.Error.Message, string(body))
	}
	if env.Result == nil {
		t.Fatalf("expected tools/list result, got body=%s", string(body))
	}
	return env.Result.Meta.Mcphub.PartialFailures
}

// waitForCount polls an atomic counter until it reaches want or a bounded
// deadline elapses, returning the final observed value. The orphaned-session
// cleanup DELETE on a failed notifications/initialized is fire-and-forget (it
// runs on a detached goroutine so it does not block the request response), so a
// test that asserts the DELETE happened must wait for the detached goroutine
// instead of reading the counter synchronously right after the call returns.
func waitForCount(c *atomic.Int32, want int32) int32 {
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := c.Load()
		if got >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestAggregateToolsCall_TransportFailureReturnsMinus32000 pins CURRENT behavior
// when the upstream daemon is unreachable (connection refused): the hub returns a
// -32000 "tools/call failed" envelope. Regression baseline for the hot-swap (a)
// failure-driven self-heal (Phase 2) — that change will turn THIS transport-
// failure case into a transparent re-init + retry. The daemon is NEVER hit here
// (callCount stays 0), which is exactly why a transport failure is SAFE to retry
// (no side effect could have run).
func TestAggregateToolsCall_TransportFailureReturnsMinus32000(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}
	resetResolverForTest(t)
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants}}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	// Take the daemon down AFTER init/list — the tools/call now hits a closed
	// listener (connection refused = transport failure, request never lands).
	d1.server.Close()

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall returned a Go error (want a JSON-RPC envelope): %v", err)
	}
	code, msg := decodeRPCError(t, body)
	if code != -32000 {
		t.Errorf("error code=%d want -32000 (body=%s)", code, string(body))
	}
	if !strings.Contains(msg, "tools/call failed") {
		t.Errorf("error message=%q want 'tools/call failed' substring", msg)
	}
	if d1.callCount.Load() != 0 {
		t.Errorf("transport failure must never reach the daemon; callCount=%d want 0", d1.callCount.Load())
	}
	if got := sess.InFlightCount(); got != 0 {
		t.Errorf("inFlight=%d after call want 0", got)
	}
}

// TestAggregateToolsCall_DaemonHTTPErrorReturnsMinus32000 pins CURRENT behavior
// when the upstream daemon RECEIVES the call but rejects it with HTTP>=400 (e.g.
// a stale-session 4xx after a restart): the hub returns -32000. Baseline for the
// hot-swap (a) change (Phase 2): an HTTP-level rejection must NOT be retried,
// because the daemon already received the request and a non-idempotent tool's
// side effect may have run (MCP tools/call has no idempotency key). The
// callCount==1 assertion is load-bearing: after (a) lands it must STAY 1.
func TestAggregateToolsCall_DaemonHTTPErrorReturnsMinus32000(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}
	resetResolverForTest(t)
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants}}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	// Daemon receives the tools/call but rejects it with HTTP 400.
	d1.onCall = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"bad session"}}`))
	}

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall returned a Go error (want a JSON-RPC envelope): %v", err)
	}
	code, _ := decodeRPCError(t, body)
	if code != -32000 {
		t.Errorf("error code=%d want -32000 (body=%s)", code, string(body))
	}
	// The daemon WAS hit exactly once — an HTTP-level error means the request
	// landed, so a non-idempotent side effect may have run. This is why (a)
	// must NOT retry it; after (a) lands, callCount must STAY 1.
	if d1.callCount.Load() != 1 {
		t.Errorf("daemon should be hit exactly once on an HTTP error; callCount=%d want 1", d1.callCount.Load())
	}
}

// TestAggregateToolsCall_SelfHealsAfterRestart is the hot-swap (a) self-heal
// success path (Phase 2): the original daemon (d1) is taken down (connection
// refused = transport failure), the resolver snapshot is re-pointed at a live
// replacement (d2) — modelling a daemon that came back — and the hub
// transparently re-initializes + retries the call ONCE, returning the daemon's
// real response instead of -32000. d2 must see exactly one re-init and one call.
func TestAggregateToolsCall_SelfHealsAfterRestart(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid-fresh") // the "restarted" daemon on a new port
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}

	// Publish a snapshot binding srv1/claude-code to d2's port, and set it as
	// SnapshotAtInit so resolveToolsCallRoute's stale-revalidation is skipped
	// (current == SnapshotAtInit). The ROUTE map still points at d1's port, so
	// the first dispatch hits the (closed) d1 and the self-heal re-resolves to
	// d2 via the snapshot.
	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: d2.port}}},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	// Take d1 down: the first tools/call gets connection-refused (transport).
	d1.server.Close()

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), "called=read") {
		t.Errorf("self-heal retry did not reach the replacement daemon; body=%s", string(body))
	}
	if d2.initCount.Load() < 1 {
		t.Errorf("replacement daemon was not re-initialized; d2.initCount=%d want >=1", d2.initCount.Load())
	}
	if d2.callCount.Load() != 1 {
		t.Errorf("replacement daemon callCount=%d want exactly 1 (one retry)", d2.callCount.Load())
	}
	if got := sess.InFlightCount(); got != 0 {
		t.Errorf("inFlight=%d after self-heal want 0", got)
	}
}

func TestAggregateToolsCall_SelfHealDeletesReinitSessionWhenInitializedFails(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "d1-old-sid"
	const freshSID = "d2-fresh-sid"

	d1 := newStubDaemon(t, oldSID)
	d2 := newStubDaemon(t, freshSID)
	d2.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		w.WriteHeader(http.StatusInternalServerError)
	}
	// The orphaned-session DELETE runs on a detached goroutine (fire-and-forget),
	// so the test reads these captures only after waitForCount confirms the
	// DELETE landed. Guard them so the goroutine write + test read are race-safe.
	var deleteMu sync.Mutex
	var deleteSID string
	var deleteProto string
	d2.onDelete = func(w http.ResponseWriter, r *http.Request) {
		deleteMu.Lock()
		deleteSID = r.Header.Get("Mcp-Session-Id")
		deleteProto = r.Header.Get("MCP-Protocol-Version")
		deleteMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}

	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: d2.port}}},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	d1.server.Close()

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall returned Go error (want JSON-RPC error envelope): %v", err)
	}
	code, _ := decodeRPCError(t, body)
	if code != -32000 {
		t.Fatalf("tools/call code=%d want -32000 when self-heal reinit notification fails; body=%s", code, string(body))
	}
	// DELETE is fire-and-forget: wait for the detached cleanup goroutine.
	if got := waitForCount(&d2.deleteCount, 1); got != 1 {
		t.Fatalf("replacement daemon DELETE count=%d want 1", got)
	}
	deleteMu.Lock()
	gotSID, gotProto := deleteSID, deleteProto
	deleteMu.Unlock()
	if gotSID != freshSID {
		t.Fatalf("replacement daemon DELETE Mcp-Session-Id=%q want %q", gotSID, freshSID)
	}
	if gotProto != "2025-11-25" {
		t.Fatalf("replacement daemon DELETE MCP-Protocol-Version=%q want %q", gotProto, "2025-11-25")
	}
	if got := d2.callCount.Load(); got != 0 {
		t.Fatalf("replacement daemon callCount=%d want 0 after failed initialized notification", got)
	}
	daemonKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port}
	if got := sess.InitSuccesses[daemonKey]; got != oldSID {
		t.Fatalf("reinit failure changed cached daemon session for stale route to %q, want original %q", got, oldSID)
	}
	if got := sess.DaemonProtoVer[daemonKey]; got != "2025-11-25" {
		t.Fatalf("reinit failure changed cached daemon proto for stale route to %q, want %q", got, "2025-11-25")
	}
}

// TestSelfHealRetryInFlightCancellationUsesReplacementPort pins the
// self-heal cancellation race: when a retry re-resolves a daemon onto a new
// port, the in-flight row used by ForwardCancellation must point at that live
// retry port, not the stale route port that failed before the retry.
func TestSelfHealRetryInFlightCancellationUsesReplacementPort(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	d2 := newStubDaemon(t, "d2-sid-fresh")

	callEntered := make(chan struct{})
	releaseCall := make(chan struct{})
	var d2CancelCount atomic.Int32
	d2.onCall = func(w http.ResponseWriter, r *http.Request) {
		close(callEntered)
		<-releaseCall
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"retry","result":{"content":[{"type":"text","text":"ok"}]}}`))
	}
	d2.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		if peekMethod(body) == "notifications/cancelled" {
			d2CancelCount.Add(1)
		}
		w.WriteHeader(http.StatusAccepted)
	}

	sess := sessionWithParticipants(d1)
	resetResolverForTest(t)
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: d2.port}}},
	}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	clientReqID := json.RawMessage(`42`)
	key, err := newRequestIDKey(clientReqID)
	if err != nil {
		t.Fatalf("newRequestIDKey: %v", err)
	}
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: d1.port, RawName: "read"}
	if !sess.InsertInFlight(key, inflightEntry{
		DaemonRef:       canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port},
		DaemonSessionID: "d1-sid",
		DaemonProtocol:  sess.ProtocolVersion,
		DaemonRequestID: json.RawMessage(`"stale-attempt"`),
		StartedAt:       time.Now(),
	}) {
		t.Fatalf("setup: InsertInFlight returned false")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		_, ok := sess.selfHealRetry(ctx, ref, key, json.RawMessage(`{"name":"read","arguments":{}}`))
		done <- ok
	}()

	select {
	case <-callEntered:
	case <-ctx.Done():
		t.Fatalf("replacement daemon did not receive retry before timeout: %v", ctx.Err())
	}

	entry, ok := sess.LookupInFlight(key)
	if !ok {
		t.Fatalf("in-flight row missing while retry is running")
	}
	if entry.DaemonRef.Port != d2.port {
		t.Fatalf("retry in-flight DaemonRef.Port=%d want replacement port %d", entry.DaemonRef.Port, d2.port)
	}

	ForwardCancellation(ctx, sess, clientReqID)
	if d2CancelCount.Load() != 1 {
		t.Errorf("replacement daemon cancellation count=%d want 1", d2CancelCount.Load())
	}
	if d1.notifyCount.Load() != 0 {
		t.Errorf("stale daemon notifyCount=%d want 0", d1.notifyCount.Load())
	}

	close(releaseCall)
	select {
	case ok := <-done:
		if !ok {
			t.Fatalf("selfHealRetry returned ok=false")
		}
	case <-ctx.Done():
		t.Fatalf("selfHealRetry did not finish before timeout: %v", ctx.Err())
	}
}

// TestAggregateToolsCall_ProactiveReinitOnStalePort is the hot-swap (b)
// event-driven path: when the supervisor-state restart watcher marks a daemon's
// port stale (a per-port current_pid change was observed), the NEXT tools/call
// re-initializes the cached session BEFORE dispatching — so the client never
// sees the stale-session failure that (a) recovers only reactively. The daemon
// is UP the whole time; no failure is needed to trigger the proactive re-init.
// The mark is consume-once: a second call must NOT re-init again.
func TestAggregateToolsCall_ProactiveReinitOnStalePort(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}
	resetResolverForTest(t)
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants}}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	initBefore := d1.initCount.Load()
	// Simulate the supervisor-state watcher observing a restart of d1's port.
	sess.markStalePort(d1.port)

	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), "called=read") {
		t.Errorf("proactive re-init call did not succeed; body=%s", string(body))
	}
	if d1.initCount.Load() <= initBefore {
		t.Errorf("daemon was not proactively re-initialized; initCount %d -> %d", initBefore, d1.initCount.Load())
	}
	if d1.callCount.Load() != 1 {
		t.Errorf("callCount=%d want 1 (no failure, single call)", d1.callCount.Load())
	}
	// Consume-once: a second call must NOT re-init again (mark cleared).
	initAfterFirst := d1.initCount.Load()
	if _, err := AggregateToolsCall(ctx, sess, json.RawMessage(`43`), params); err != nil {
		t.Fatal(err)
	}
	if d1.initCount.Load() != initAfterFirst {
		t.Errorf("stale mark not consumed: second call re-initialized again (initCount %d -> %d)", initAfterFirst, d1.initCount.Load())
	}
}

func TestAggregateToolsCall_ProactiveReinitDeletesSessionWhenInitializedFails(t *testing.T) {
	resetResolverForTest(t)
	t.Cleanup(func() { resetResolverForTest(t) })

	const oldSID = "d1-old-sid"
	const freshSID = "d1-fresh-sid"

	d1 := newStubDaemon(t, oldSID)
	d1.onInit = func(w http.ResponseWriter, r *http.Request) {
		if d1.initCount.Load() >= 2 {
			w.Header().Set("Mcp-Session-Id", freshSID)
		} else {
			w.Header().Set("Mcp-Session-Id", oldSID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	d1.onNotify = func(w http.ResponseWriter, r *http.Request, body []byte) {
		if d1.notifyCount.Load() >= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
	// DELETE is fire-and-forget (detached goroutine), so these captures are
	// read only after waitForCount confirms it landed; guard for -race safety.
	var deleteMu sync.Mutex
	var deleteSID string
	var deleteProto string
	d1.onDelete = func(w http.ResponseWriter, r *http.Request) {
		deleteMu.Lock()
		deleteSID = r.Header.Get("Mcp-Session-Id")
		deleteProto = r.Header.Get("MCP-Protocol-Version")
		deleteMu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}
	var callMu sync.Mutex
	var callSID string
	d1.onCall = func(w http.ResponseWriter, r *http.Request) {
		callMu.Lock()
		callSID = r.Header.Get("Mcp-Session-Id")
		callMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"daemon","result":{"content":[{"type":"text","text":"called=read"}]}}`))
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}
	resetResolverForTest(t)
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants}}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	sess.markStalePort(d1.port)
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)
	body, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
	if err != nil {
		t.Fatalf("AggregateToolsCall: %v", err)
	}
	if !strings.Contains(string(body), "called=read") {
		t.Fatalf("proactive reinit fallback call did not succeed with original session; body=%s", string(body))
	}
	// DELETE is fire-and-forget: wait for the detached cleanup goroutine.
	if got := waitForCount(&d1.deleteCount, 1); got != 1 {
		t.Fatalf("daemon DELETE count=%d want 1", got)
	}
	deleteMu.Lock()
	gotDeleteSID, gotDeleteProto := deleteSID, deleteProto
	deleteMu.Unlock()
	if gotDeleteSID != freshSID {
		t.Fatalf("daemon DELETE Mcp-Session-Id=%q want %q", gotDeleteSID, freshSID)
	}
	if gotDeleteProto != "2025-11-25" {
		t.Fatalf("daemon DELETE MCP-Protocol-Version=%q want %q", gotDeleteProto, "2025-11-25")
	}
	callMu.Lock()
	gotCallSID := callSID
	callMu.Unlock()
	if gotCallSID != oldSID {
		t.Fatalf("tools/call Mcp-Session-Id=%q want original %q after failed proactive reinit", gotCallSID, oldSID)
	}
	daemonKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port}
	if got := sess.InitSuccesses[daemonKey]; got != oldSID {
		t.Fatalf("failed proactive reinit cached sid=%q want original %q", got, oldSID)
	}
	if got := sess.DaemonProtoVer[daemonKey]; got != "2025-11-25" {
		t.Fatalf("failed proactive reinit cached proto=%q want %q", got, "2025-11-25")
	}
}

// TestAggregateToolsCall_ConcurrentProactiveReinitRereadsFreshSID pins the
// restart-race fixed after Aardvark's report: multiple tools/call requests can
// resolve the old daemon session id before one caller consumes the stale-port
// mark. Followers must wait for the proactive re-init and re-read the refreshed
// daemon session id before dispatching, not send the old Mcp-Session-Id.
func TestAggregateToolsCall_ConcurrentProactiveReinitRereadsFreshSID(t *testing.T) {
	const oldSID = "d1-old-sid"
	const freshSID = "d1-fresh-sid"

	reinitStarted := make(chan struct{})
	releaseReinit := make(chan struct{})
	var reinitStartedOnce sync.Once
	var callHeadersMu sync.Mutex
	var callHeaders []string

	d1 := newStubDaemon(t, oldSID)
	d1.onInit = func(w http.ResponseWriter, r *http.Request) {
		if d1.initCount.Load() >= 2 {
			reinitStartedOnce.Do(func() { close(reinitStarted) })
			select {
			case <-releaseReinit:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Mcp-Session-Id", freshSID)
		} else {
			w.Header().Set("Mcp-Session-Id", oldSID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"1"}}}`))
	}
	d1.onCall = func(w http.ResponseWriter, r *http.Request) {
		callHeadersMu.Lock()
		callHeaders = append(callHeaders, r.Header.Get("Mcp-Session-Id"))
		callHeadersMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"daemon","result":{"content":[{"type":"text","text":"ok"}]}}`))
	}

	sess := sessionWithParticipants(d1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), ""); err != nil {
		t.Fatal(err)
	}
	resetResolverForTest(t)
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": sess.IntendedParticipants}}
	sess.SnapshotAtInit = snap
	PublishResolverSnapshot(snap)

	sess.markStalePort(d1.port)
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)

	firstDone := make(chan error, 1)
	go func() {
		_, err := AggregateToolsCall(ctx, sess, json.RawMessage(`42`), params)
		firstDone <- err
	}()

	select {
	case <-reinitStarted:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for proactive re-init to start: %v", ctx.Err())
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := AggregateToolsCall(ctx, sess, json.RawMessage(`43`), params)
		secondDone <- err
	}()

	// Give the follower a chance to reach the stale-port synchronization point
	// while the first caller is still re-initializing.
	time.Sleep(25 * time.Millisecond)
	close(releaseReinit)

	if err := <-firstDone; err != nil {
		t.Fatalf("first AggregateToolsCall: %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second AggregateToolsCall: %v", err)
	}

	callHeadersMu.Lock()
	gotHeaders := append([]string(nil), callHeaders...)
	callHeadersMu.Unlock()
	if len(gotHeaders) != 2 {
		t.Fatalf("call headers = %v, want 2 calls", gotHeaders)
	}
	for i, got := range gotHeaders {
		if got != freshSID {
			t.Fatalf("call %d used Mcp-Session-Id %q, want fresh %q; all headers=%v", i, got, freshSID, gotHeaders)
		}
	}
	if got := d1.initCount.Load(); got != 2 {
		t.Fatalf("initCount=%d want 2 (initial + one coalesced proactive re-init)", got)
	}
}
