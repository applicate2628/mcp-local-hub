// hub_mcp_scope_characterization_test.go — CHARACTERIZATION tests for
// the groups/namespaces "sess.ScopeKey → sess.ScopeKey" generalization.
//
// These tests pin the CURRENT, per-client behavior of the hub request-
// path functions the upcoming ScopeKey rename will touch. They are a
// regression fence: they assert what the code does TODAY (not what it
// should do), so the mechanical rename provably cannot change observable
// behavior. ZERO production code is changed by this file.
//
// Functions pinned (all reviewer-verified as untested at the function
// boundary — see work-items/decisions/2026-06-18-groups-namespaces-
// tool-visibility.md "Defect B"):
//
//   - daemonStillBound           (hub_mcp_aggregator.go) — snap.Bindings[client] lookup
//   - currentDaemonPort          (hub_mcp_aggregator.go) — snap.Bindings[s.ScopeKey] self-heal
//   - resolveToolsCallRoute      (hub_mcp_aggregator.go) — RouteMap + scope-key revalidation
//   - dispatchToolsCall          (hub_mcp_aggregator.go) — resolved-target path is scope-key-independent
//   - cross-client 401 gate      (hub_mcp_handler.go handlePost + handleDelete)
//   - per-client session-cap     (hub_mcp_session.go Create/deleteLocked perClient accounting)
//
// Spec: groups/namespaces decision §"Defect B" (tests-first mandate).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ----------------------------------------------------------------------
// daemonStillBound — snap.Bindings[client] membership predicate.
//
// The ScopeKey rename changes the `client` parameter name; this pins the
// exact lookup-then-tuple-match contract so the rename can't alter it.
// ----------------------------------------------------------------------

func TestCharacterizeDaemonStillBound(t *testing.T) {
	refA := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	bindingsWith := func(refs ...canonicalDaemonRef) map[string][]canonicalDaemonRef {
		return map[string][]canonicalDaemonRef{"claude-code": refs}
	}
	matchingRef := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: 9101}
	wrongPort := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: 9999}
	wrongServer := canonicalDaemonRef{Server: "srv2", Daemon: "claude-code", Port: 9101}

	cases := []struct {
		name   string
		snap   *ResolverSnapshot
		client string
		ref    canonicalToolRef
		want   bool
	}{
		{
			name:   "nil-snapshot-false",
			snap:   nil,
			client: "claude-code",
			ref:    refA,
			want:   false,
		},
		{
			name:   "missing-client-key-false",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: bindingsWith(matchingRef)},
			client: "codex-cli", // key absent
			ref:    refA,
			want:   false,
		},
		{
			name:   "empty-binding-list-false",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{"claude-code": nil}},
			client: "claude-code",
			ref:    refA,
			want:   false,
		},
		{
			name:   "ref-present-true",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: bindingsWith(matchingRef)},
			client: "claude-code",
			ref:    refA,
			want:   true,
		},
		{
			name:   "port-mismatch-false",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: bindingsWith(wrongPort)},
			client: "claude-code",
			ref:    refA,
			want:   false,
		},
		{
			name:   "server-mismatch-false",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: bindingsWith(wrongServer)},
			client: "claude-code",
			ref:    refA,
			want:   false,
		},
		{
			name:   "ref-present-among-many-true",
			snap:   &ResolverSnapshot{Gen: 1, Bindings: bindingsWith(wrongServer, wrongPort, matchingRef)},
			client: "claude-code",
			ref:    refA,
			want:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := daemonStillBound(tc.snap, tc.client, tc.ref); got != tc.want {
				t.Errorf("daemonStillBound = %v, want %v", got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------
// currentDaemonPort — self-heal port re-resolution from snap.Bindings[s.ScopeKey].
//
// Reviewer flagged this as an un-enumerated, untested touch site. Pins
// the (Server,Daemon)→Port re-resolution against the LIVE snapshot plus
// the route's-own-port fallback.
// ----------------------------------------------------------------------

func TestCharacterizeCurrentDaemonPort(t *testing.T) {
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	cases := []struct {
		name   string
		client string
		snap   *ResolverSnapshot
		want   int
	}{
		{
			name:   "nil-snapshot-falls-back-to-ref-port",
			client: "claude-code",
			snap:   nil,
			want:   9101, // ref.Port fallback
		},
		{
			name:   "missing-client-key-falls-back-to-ref-port",
			client: "claude-code",
			snap: &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
				"codex-cli": {{Server: "srv1", Daemon: "claude-code", Port: 9200}},
			}},
			want: 9101, // s.ScopeKey="claude-code" not in Bindings → ref.Port
		},
		{
			name:   "matching-server-daemon-returns-live-port",
			client: "claude-code",
			snap: &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
				"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: 9200}},
			}},
			want: 9200, // re-resolved to the NEW port (self-heal)
		},
		{
			name:   "no-server-match-falls-back-to-ref-port",
			client: "claude-code",
			snap: &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
				"claude-code": {{Server: "srvOTHER", Daemon: "claude-code", Port: 9200}},
			}},
			want: 9101, // server name does not match → ref.Port
		},
		{
			name:   "daemon-mismatch-falls-back-to-ref-port",
			client: "claude-code",
			snap: &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
				"claude-code": {{Server: "srv1", Daemon: "OTHER", Port: 9200}},
			}},
			want: 9101, // daemon name does not match → ref.Port
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetResolverForTest(t)
			if tc.snap != nil {
				PublishResolverSnapshot(tc.snap)
			}
			s := &hubSession{ScopeKey: tc.client}
			if got := s.currentDaemonPort(ref); got != tc.want {
				t.Errorf("currentDaemonPort = %d, want %d", got, tc.want)
			}
		})
	}
}

// ----------------------------------------------------------------------
// resolveToolsCallRoute — RouteMap lookup + scope-key revalidation.
//
// Pins the error-envelope contract for each rejection branch AND that
// the snapshot revalidation keys on sess.ScopeKey. No daemons are stood
// up: these branches return before any HTTP call.
// ----------------------------------------------------------------------

// decodeJSONRPCErr extracts {code,message} from a JSON-RPC error
// envelope body, failing the test if the body is not an error envelope.
func decodeJSONRPCErr(t *testing.T, body []byte) (int, string) {
	t.Helper()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v / body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected error envelope, got: %s", string(body))
	}
	return env.Error.Code, env.Error.Message
}

func TestCharacterizeResolveToolsCallRouteMissingName(t *testing.T) {
	sess := &hubSession{ScopeKey: "claude-code"}
	// params with no "name".
	target, err := resolveToolsCallRoute(sess, json.RawMessage(`1`), json.RawMessage(`{"arguments":{}}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target.errBody == nil {
		t.Fatal("expected errBody for missing name")
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32602 {
		t.Errorf("code=%d want -32602", code)
	}
	if !strings.Contains(msg, "missing name") {
		t.Errorf("message=%q want 'missing name' substring", msg)
	}
}

func TestCharacterizeResolveToolsCallRouteInvalidParams(t *testing.T) {
	sess := &hubSession{ScopeKey: "claude-code"}
	// malformed JSON params → -32602 Invalid params.
	target, err := resolveToolsCallRoute(sess, json.RawMessage(`1`), json.RawMessage(`{not json`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target.errBody == nil {
		t.Fatal("expected errBody for invalid params")
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32602 {
		t.Errorf("code=%d want -32602", code)
	}
	if !strings.Contains(msg, "Invalid params") {
		t.Errorf("message=%q want 'Invalid params' substring", msg)
	}
}

func TestCharacterizeResolveToolsCallRouteNilRouteMap(t *testing.T) {
	// RouteMap never Stored → nil pointer → -32601 Method not found.
	sess := &hubSession{ScopeKey: "claude-code"}
	target, err := resolveToolsCallRoute(sess, json.RawMessage(`1`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32601 {
		t.Errorf("code=%d want -32601", code)
	}
	if !strings.Contains(msg, "Method not found") {
		t.Errorf("message=%q want 'Method not found' substring", msg)
	}
}

func TestCharacterizeResolveToolsCallRouteUnknownName(t *testing.T) {
	sess := &hubSession{ScopeKey: "claude-code"}
	sess.RouteMap.Store(&map[string]canonicalToolRef{
		"srv1__read": {Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"},
	})
	target, err := resolveToolsCallRoute(sess, json.RawMessage(`1`), json.RawMessage(`{"name":"srv1__missing"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32601 {
		t.Errorf("code=%d want -32601", code)
	}
	if !strings.Contains(msg, "Method not found: srv1__missing") {
		t.Errorf("message=%q want 'Method not found: srv1__missing' substring", msg)
	}
}

// TestCharacterizeResolveToolsCallRouteOutOfScopeKeysOnSessClient pins
// THE behavior the ScopeKey rename must preserve byte-identical: when
// the snapshot pointer has MOVED since init, revalidation calls
// daemonStillBound(current, sess.ScopeKey, ref). If sess.ScopeKey is NOT a
// key in the new snapshot's Bindings, the route is refused with -32601
// "tool moved out of scope". The current snapshot here intentionally
// binds a DIFFERENT client only — proving the gate keys on sess.ScopeKey.
func TestCharacterizeResolveToolsCallRouteOutOfScopeKeysOnSessClient(t *testing.T) {
	resetResolverForTest(t)
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	sess := &hubSession{ScopeKey: "claude-code", InitSuccesses: map[canonicalDaemonRef]string{}}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})

	// SnapshotAtInit: the session's original snapshot (pointer P1).
	atInit := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: 9101}},
	}}
	sess.SnapshotAtInit = atInit

	// CURRENT snapshot (pointer P2 != P1) binds ONLY codex-cli. Because
	// sess.ScopeKey == "claude-code" is absent here, daemonStillBound
	// returns false → out-of-scope refusal.
	current := &ResolverSnapshot{Gen: 2, Bindings: map[string][]canonicalDaemonRef{
		"codex-cli": {{Server: "srv1", Daemon: "claude-code", Port: 9101}},
	}}
	PublishResolverSnapshot(current)

	target, err := resolveToolsCallRoute(sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32601 {
		t.Errorf("code=%d want -32601", code)
	}
	if !strings.Contains(msg, "tool moved out of scope") {
		t.Errorf("message=%q want 'tool moved out of scope' substring", msg)
	}
}

// TestCharacterizeResolveToolsCallRouteSameClientKeyStillBound pins the
// complement: when the moved snapshot STILL binds sess.ScopeKey to the
// ref, revalidation passes the daemonStillBound gate. It then fails at
// the next stage (-32603 no daemon session id) because InitSuccesses is
// empty — which is the expected contract for a session that never
// completed daemon initialize. This proves the scope-key gate admitted
// the route (did NOT return "tool moved out of scope").
func TestCharacterizeResolveToolsCallRouteSameClientKeyStillBound(t *testing.T) {
	resetResolverForTest(t)
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	sess := &hubSession{ScopeKey: "claude-code", InitSuccesses: map[canonicalDaemonRef]string{}}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
	sess.SnapshotAtInit = &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: 9101}},
	}}

	// Moved snapshot STILL binds claude-code to the same ref.
	current := &ResolverSnapshot{Gen: 2, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: 9101}},
	}}
	PublishResolverSnapshot(current)

	target, err := resolveToolsCallRoute(sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	// Passed the scope gate; failed at the no-daemon-SID stage instead.
	if code != -32603 {
		t.Errorf("code=%d want -32603 (passed scope gate, no daemon SID); message=%q", code, msg)
	}
	if strings.Contains(msg, "tool moved out of scope") {
		t.Errorf("scope gate wrongly refused a still-bound ref: %q", msg)
	}
}

// TestCharacterizeResolveToolsCallRouteSamePointerSkipsRevalidation pins
// the fast-path: when the live snapshot pointer EQUALS sess.SnapshotAtInit,
// the daemonStillBound revalidation is skipped entirely (current ==
// sess.SnapshotAtInit guard at the call site). With InitSuccesses empty
// the resolve still fails -32603, but never "tool moved out of scope" —
// proving the same-pointer fast path bypassed the scope check.
func TestCharacterizeResolveToolsCallRouteSamePointerSkipsRevalidation(t *testing.T) {
	resetResolverForTest(t)
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	// Snapshot binds NO client at all; if revalidation ran it would
	// refuse. The same-pointer guard must skip it.
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{}}
	PublishResolverSnapshot(snap)

	sess := &hubSession{ScopeKey: "claude-code", InitSuccesses: map[canonicalDaemonRef]string{}}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
	// SnapshotAtInit is the SAME pointer the live snapshot holds.
	sess.SnapshotAtInit = LoadResolverSnapshot()

	target, err := resolveToolsCallRoute(sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32603 {
		t.Errorf("code=%d want -32603 (revalidation skipped, no daemon SID); message=%q", code, msg)
	}
	if strings.Contains(msg, "tool moved out of scope") {
		t.Errorf("same-pointer fast path wrongly ran revalidation: %q", msg)
	}
}

// TestCharacterizeResolveToolsCallRouteResolvesTargetPort pins the
// success contract: with a populated RouteMap, a matching InitSuccesses
// row, and a non-moved snapshot, resolveToolsCallRoute returns a target
// carrying the ROUTE's port (ref.Port) and the daemon session id — no
// error body.
func TestCharacterizeResolveToolsCallRouteResolvesTargetPort(t *testing.T) {
	resetResolverForTest(t)
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}
	daemonKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: 9101}

	sess := &hubSession{
		ScopeKey:        "claude-code",
		ProtocolVersion: "2025-11-25",
		InitSuccesses:   map[canonicalDaemonRef]string{daemonKey: "daemon-sid-1"},
		DaemonProtoVer:  map[canonicalDaemonRef]string{daemonKey: "2025-11-25"},
	}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
	// Same-pointer fast path: skip revalidation.
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {daemonKey},
	}}
	PublishResolverSnapshot(snap)
	sess.SnapshotAtInit = LoadResolverSnapshot()

	target, err := resolveToolsCallRoute(sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if target.errBody != nil {
		code, msg := decodeJSONRPCErr(t, target.errBody)
		t.Fatalf("unexpected error body code=%d msg=%q", code, msg)
	}
	if target.ref.Port != 9101 {
		t.Errorf("target.ref.Port=%d want 9101", target.ref.Port)
	}
	if target.ref.RawName != "read" {
		t.Errorf("target.ref.RawName=%q want read", target.ref.RawName)
	}
	if target.daemonSID != "daemon-sid-1" {
		t.Errorf("target.daemonSID=%q want daemon-sid-1", target.daemonSID)
	}
	if target.daemonProto != "2025-11-25" {
		t.Errorf("target.daemonProto=%q want 2025-11-25", target.daemonProto)
	}
}

// TestCharacterizeResolveToolsCallRouteNoDaemonSID pins the -32603
// branch: a fully-resolved route whose daemonKey is ABSENT from
// InitSuccesses returns "no daemon session id for target".
func TestCharacterizeResolveToolsCallRouteNoDaemonSID(t *testing.T) {
	resetResolverForTest(t)
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: 9101, RawName: "read"}

	sess := &hubSession{
		ScopeKey:      "claude-code",
		InitSuccesses: map[canonicalDaemonRef]string{}, // no SID for the target
	}
	sess.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
	snap := &ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {{Server: "srv1", Daemon: "claude-code", Port: 9101}},
	}}
	PublishResolverSnapshot(snap)
	sess.SnapshotAtInit = LoadResolverSnapshot()

	target, err := resolveToolsCallRoute(sess, json.RawMessage(`42`), json.RawMessage(`{"name":"srv1__read"}`))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	code, msg := decodeJSONRPCErr(t, target.errBody)
	if code != -32603 {
		t.Errorf("code=%d want -32603", code)
	}
	if !strings.Contains(msg, "no daemon session id") {
		t.Errorf("message=%q want 'no daemon session id' substring", msg)
	}
}

// ----------------------------------------------------------------------
// dispatchToolsCall — resolved-target path is scope-key-independent.
//
// The design claims the dispatch path "does NOT depend on the scope
// key". This pins that: two sessions with DIFFERENT sess.ScopeKey values
// but the SAME resolved target (same daemon ref + SID) both reach the
// same stub daemon and get an identical rewritten-name response. If
// dispatch secretly re-keyed on sess.ScopeKey, one would diverge.
// ----------------------------------------------------------------------

func TestCharacterizeDispatchToolsCallIgnoresScopeKey(t *testing.T) {
	d1 := newStubDaemon(t, "d1-sid")
	ref := canonicalToolRef{Server: "srv1", Daemon: "claude-code", Port: d1.port, RawName: "read"}
	daemonKey := canonicalDaemonRef{Server: "srv1", Daemon: "claude-code", Port: d1.port}

	// Two sessions, DIFFERENT Client keys, identical resolved target.
	mk := func(client string) *hubSession {
		s := &hubSession{
			ScopeKey:         client,
			ProtocolVersion:  "2025-11-25",
			InitSuccesses:    map[canonicalDaemonRef]string{daemonKey: "d1-sid"},
			DaemonProtoVer:   map[canonicalDaemonRef]string{daemonKey: "2025-11-25"},
			InFlightRequests: map[requestIDKey]inflightEntry{},
		}
		s.RouteMap.Store(&map[string]canonicalToolRef{"srv1__read": ref})
		return s
	}

	// Pin a snapshot that binds BOTH client keys to the daemon so the
	// self-heal port re-resolution (currentDaemonPort) inside dispatch
	// finds the same live port for either scope key.
	resetResolverForTest(t)
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Bindings: map[string][]canonicalDaemonRef{
		"claude-code": {daemonKey},
		"codex-cli":   {daemonKey},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	params := json.RawMessage(`{"name":"srv1__read","arguments":{}}`)

	bodyA, errA := dispatchToolsCall(ctx, mk("claude-code"), json.RawMessage(`1`), params, ref, "d1-sid", "2025-11-25")
	if errA != nil {
		t.Fatalf("dispatch (claude-code): %v", errA)
	}
	bodyB, errB := dispatchToolsCall(ctx, mk("codex-cli"), json.RawMessage(`2`), params, ref, "d1-sid", "2025-11-25")
	if errB != nil {
		t.Fatalf("dispatch (codex-cli): %v", errB)
	}

	// Both reach the daemon with the same rewritten raw name.
	if !strings.Contains(string(bodyA), "called=read") {
		t.Errorf("claude-code dispatch body missing called=read: %s", bodyA)
	}
	if !strings.Contains(string(bodyB), "called=read") {
		t.Errorf("codex-cli dispatch body missing called=read: %s", bodyB)
	}
	if d1.callCount.Load() != 2 {
		t.Errorf("daemon callCount=%d want 2 (both scope keys dispatched)", d1.callCount.Load())
	}
}

// ----------------------------------------------------------------------
// Cross-client 401 gate (handlePost + handleDelete).
//
// hub_mcp_handler.go gates `sess.ScopeKey != clientID` → 401 empty body on
// BOTH the POST and DELETE paths. The ScopeKey rename rewrites this to
// `sess.ScopeKey != scopeKey` and the decision requires it stay
// byte-identical. Pin the exact contract: mismatch → 401 empty; match →
// NOT 401.
// ----------------------------------------------------------------------

func TestCharacterizeCrossClient401PostTable(t *testing.T) {
	cases := []struct {
		name         string
		sessionOwner string // client the session was minted under
		urlClient    string // client in the request URL path
		want401      bool
	}{
		{name: "owner-claude-url-codex-rejects", sessionOwner: "claude-code", urlClient: "codex-cli", want401: true},
		{name: "owner-codex-url-claude-rejects", sessionOwner: "codex-cli", urlClient: "claude-code", want401: true},
		{name: "owner-matches-url-not-401", sessionOwner: "claude-code", urlClient: "claude-code", want401: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t)
			sess, err := h.sessions.Create(tc.sessionOwner, "2025-11-25", nil)
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			req := authedRequest(t, http.MethodPost, "/clients/"+tc.urlClient+"/mcp", body)
			req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
			req.Header.Set("MCP-Protocol-Version", "2025-11-25")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if tc.want401 {
				if w.Code != http.StatusUnauthorized {
					t.Errorf("got %d, want 401 (cross-client reject)", w.Code)
				}
				// 401 empty body — no oracle.
				if w.Body.Len() != 0 {
					t.Errorf("401 body must be empty; got %q", w.Body.String())
				}
			} else if w.Code == http.StatusUnauthorized {
				t.Errorf("matching client wrongly rejected with 401")
			}
		})
	}
}

func TestCharacterizeCrossClient401DeleteTable(t *testing.T) {
	cases := []struct {
		name         string
		sessionOwner string
		urlClient    string
		want401      bool
	}{
		{name: "delete-owner-claude-url-codex-rejects", sessionOwner: "claude-code", urlClient: "codex-cli", want401: true},
		{name: "delete-owner-matches-url-not-401", sessionOwner: "claude-code", urlClient: "claude-code", want401: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandler(t)
			sess, err := h.sessions.Create(tc.sessionOwner, "2025-11-25", nil)
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			req := authedRequest(t, http.MethodDelete, "/clients/"+tc.urlClient+"/mcp", nil)
			req.Header.Set("Mcp-Session-Id", sess.ClientSessionID)
			req.Header.Set("MCP-Protocol-Version", "2025-11-25")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)

			if tc.want401 {
				if w.Code != http.StatusUnauthorized {
					t.Errorf("DELETE got %d, want 401 (cross-client reject)", w.Code)
				}
				if w.Body.Len() != 0 {
					t.Errorf("DELETE 401 body must be empty; got %q", w.Body.String())
				}
			} else if w.Code == http.StatusUnauthorized {
				t.Errorf("DELETE matching client wrongly rejected with 401")
			}
		})
	}
}

// ----------------------------------------------------------------------
// Per-client session-cap accounting (Create + deleteLocked perClient).
//
// hub_mcp_session.go keys the per-client cap on sess.ScopeKey:
//   - Create:        perClient[client] >= MaxPerClient → ErrSessionCapExceeded; else perClient[client]++
//   - deleteLocked:  perClient[sess.ScopeKey]--; if <=0 delete(perClient, sess.ScopeKey)
//
// The ScopeKey rename touches the struct field + these accounting sites.
// Pin the exact counter contract so the rename can't change which key
// the cap accrues to.
// ----------------------------------------------------------------------

// TestCharacterizeSessionCapKeyedPerClient pins that the cap accrues
// PER client key: filling claude-code to its cap does NOT block codex-cli.
func TestCharacterizeSessionCapKeyedPerClient(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 2, MaxGlobal: 100})
	defer store.Close()

	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Fatal(err)
	}
	// claude-code at cap → reject.
	if _, err := store.Create("claude-code", "2025-11-25", nil); !errors.Is(err, ErrSessionCapExceeded) {
		t.Errorf("claude-code 3rd create: got %v, want ErrSessionCapExceeded", err)
	}
	// codex-cli is a DIFFERENT key → its own fresh budget.
	if _, err := store.Create("codex-cli", "2025-11-25", nil); err != nil {
		t.Errorf("codex-cli create rejected despite claude-code being full: %v", err)
	}
}

// TestCharacterizeSessionCapDeleteFreesPerClientSlot pins the
// deleteLocked decrement contract: deleting a session under the per-
// client key decrements perClient[client], freeing a slot so a
// subsequent Create for the SAME client succeeds.
func TestCharacterizeSessionCapDeleteFreesPerClientSlot(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 1, MaxGlobal: 100})
	defer store.Close()

	s1, err := store.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	// At cap (MaxPerClient=1) → second create rejected.
	if _, err := store.Create("claude-code", "2025-11-25", nil); !errors.Is(err, ErrSessionCapExceeded) {
		t.Fatalf("2nd create at cap: got %v, want ErrSessionCapExceeded", err)
	}
	// Delete frees the per-client slot.
	if !store.Delete(s1.ClientSessionID) {
		t.Fatal("Delete returned false for a known session")
	}
	// Now a fresh create for the SAME client succeeds — slot freed.
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Errorf("create after delete: got %v, want success (per-client slot freed)", err)
	}
}

// TestCharacterizeSessionCapDeleteOfLastRemovesKey pins the
// delete-of-last contract: when perClient[client] drops to <=0 the key
// is removed from the map. Observable proxy: after deleting the only
// claude-code session, the full per-client budget is available again
// (MaxPerClient fresh creates succeed), which can only hold if the
// counter reset to zero (key dropped), not lingered negative/stale.
func TestCharacterizeSessionCapDeleteOfLastRemovesKey(t *testing.T) {
	store := NewHubSessionStore(SessionStoreOpts{MaxPerClient: 2, MaxGlobal: 100})
	defer store.Close()

	s1, err := store.Create("claude-code", "2025-11-25", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !store.Delete(s1.ClientSessionID) {
		t.Fatal("Delete returned false for a known session")
	}
	// Full budget restored: MaxPerClient (2) fresh creates must succeed.
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Errorf("1st create after delete-of-last: %v", err)
	}
	if _, err := store.Create("claude-code", "2025-11-25", nil); err != nil {
		t.Errorf("2nd create after delete-of-last: %v", err)
	}
	// And the 3rd is back to being capped.
	if _, err := store.Create("claude-code", "2025-11-25", nil); !errors.Is(err, ErrSessionCapExceeded) {
		t.Errorf("3rd create after delete-of-last: got %v, want ErrSessionCapExceeded", err)
	}
}
