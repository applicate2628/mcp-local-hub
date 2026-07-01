// hub_mcp_groups_phase4b_test.go — groups/namespaces Phase 4b
// (the /g/<group>/mcp ROUTE + group-scoped initialize — groups serve).
//
// Phase 4a wired the DATA (groups.yaml → "g:<group>" Bindings + token
// row) but nobody READ it. Phase 4b adds the request path: a sibling
// /g/<group>/mcp route, served by the SAME handler, whose initialize
// keys IntendedParticipants on the group's "g:<group>" snapshot binding —
// so the group's tools/list exposes ONLY its member servers' tools.
//
// THE KEYSTONE assertion is tool-visibility narrowing: a /g/frontend
// session whose group binds {memory, time} (a SUBSET of the live servers)
// exposes ONLY memory__* + time__* tools, never the excluded server's.
//
// Fences carried green (the /clients/ contract must stay byte-identical):
//   - parseClientPathFromURL / the cross-client 401 gate / the full e2e
//     test all live in sibling files and MUST stay green; this file adds
//     coverage WITHOUT touching their assertions.
//
// State-safety: the snapshot + token table are package-level atomic
// pointers; tests publish synthetic ones via resetResolverForTest +
// publishTokenTable and never touch live supervisor / hub state.
//
// Spec: groups/namespaces decision §"DECISION (2026-06-18)" +
// §"Per-group tool visibility" + §"Endpoint / URL shape".
package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"
)

// ----------------------------------------------------------------------
// parseHubPathFromURL — the generalized parser. The /clients/ branch MUST
// stay byte-identical to parseClientPathFromURL; the /g/ branch mirrors
// the strict shape rules.
// ----------------------------------------------------------------------

func TestParseHubPathFromURL(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantKind hubScopeKind
		wantName string
		wantOK   bool
	}{
		// client branch — byte-identical to the old parser.
		{"client-ok", "/clients/claude-code/mcp", kindClient, "claude-code", true},
		{"client-empty-id", "/clients//mcp", kindClient, "", false},
		{"client-no-suffix", "/clients/claude-code", kindClient, "", false},
		{"client-trailing-slash", "/clients/claude-code/", kindClient, "", false},
		{"client-extra-segment", "/clients/foo/mcp/bar", kindClient, "", false},
		{"client-embedded-slash", "/clients/a/b/mcp", kindClient, "", false},
		{"client-bare", "/clients", kindClient, "", false},
		{"client-bare-slash", "/clients/", kindClient, "", false},
		// group branch — same shape rules under the /g/ prefix.
		{"group-ok", "/g/frontend/mcp", kindGroup, "frontend", true},
		{"group-empty-name", "/g//mcp", kindGroup, "", false},
		{"group-no-suffix", "/g/frontend", kindGroup, "", false},
		{"group-trailing-slash", "/g/frontend/", kindGroup, "", false},
		{"group-extra-segment", "/g/a/b/mcp", kindGroup, "", false},
		{"group-bare", "/g", kindGroup, "", false},
		{"group-bare-slash", "/g/", kindGroup, "", false},
		// neither prefix.
		{"unrelated", "/internal/reload-tokens", kindClient, "", false},
		{"root", "/", kindClient, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, name, ok := parseHubPathFromURL(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v want %v (path=%q)", ok, tc.wantOK, tc.path)
			}
			if !ok {
				return
			}
			if kind != tc.wantKind {
				t.Errorf("kind=%v want %v", kind, tc.wantKind)
			}
			if name != tc.wantName {
				t.Errorf("name=%q want %q", name, tc.wantName)
			}
		})
	}
}

// TestParseClientPathFromURLStillByteIdentical pins the byte-identical
// fence directly: parseClientPathFromURL (now a thin wrapper) must return
// EXACTLY what it returned before the generalization for every input the
// Phase 1/2 characterization tests rely on, AND must reject /g/ paths (it
// is the client-only parser).
func TestParseClientPathFromURLStillByteIdentical(t *testing.T) {
	cases := []struct {
		path   string
		wantID string
		wantOK bool
	}{
		{"/clients/claude-code/mcp", "claude-code", true},
		{"/clients/codex-cli/mcp", "codex-cli", true},
		{"/clients//mcp", "", false},
		{"/clients/foo/", "", false},
		{"/clients/foo/mcp/bar", "", false},
		// The client-only parser does NOT recognize the group prefix.
		{"/g/frontend/mcp", "", false},
	}
	for _, tc := range cases {
		got, ok := parseClientPathFromURL(tc.path)
		if got != tc.wantID || ok != tc.wantOK {
			t.Errorf("parseClientPathFromURL(%q) = (%q,%v), want (%q,%v)", tc.path, got, ok, tc.wantID, tc.wantOK)
		}
	}
}

// TestResolveHubScopeKey pins the kind→scopeKey mapping + the known gate.
func TestResolveHubScopeKey(t *testing.T) {
	// Publish a token table with one client + one group so the known
	// predicates have a live record to consult.
	publishTokenTable(HubTokenTable{Tokens: map[string]string{
		"claude-code":  realToken,
		"g:frontend":   realToken,
		"g:bad:forged": realToken, // a forged key; validateGroupName must reject the NAME
	}})
	// gate-2 (isKnownGroup) now reads LoadResolverSnapshot().Groups, NOT the
	// token table, so publish a snapshot whose Groups set is the source of
	// truth — mirroring what the production builder records. Only "frontend"
	// is declared here: "infra" is absent (group-unknown), and the forged
	// "bad:forged" name fails validateGroupName before the Groups lookup.
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{GroupScopeKey("frontend"): true}})

	cases := []struct {
		name      string
		kind      hubScopeKind
		input     string
		wantKey   string
		wantKnown bool
	}{
		{"client-known", kindClient, "claude-code", "claude-code", true},
		{"client-unknown", kindClient, "not-a-client", "not-a-client", false},
		{"group-known", kindGroup, "frontend", "g:frontend", true},
		{"group-unknown", kindGroup, "infra", "g:infra", false},
		// A group NAME containing ':' is rejected by validateGroupName even
		// if some forged row exists, because isKnownGroup validates the name.
		{"group-forged-colon-name", kindGroup, "bad:forged", "g:bad:forged", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, known := resolveHubScopeKey(tc.kind, tc.input)
			if key != tc.wantKey {
				t.Errorf("scopeKey=%q want %q", key, tc.wantKey)
			}
			if known != tc.wantKnown {
				t.Errorf("known=%v want %v", known, tc.wantKnown)
			}
		})
	}
}

// ----------------------------------------------------------------------
// Test helpers for the request-path tests.
// ----------------------------------------------------------------------

// publishGroupTokenTable publishes a token table carrying the standard
// client rows PLUS a "g:<group>" row for each supplied group name, all
// mapped to realToken so authedRequest (which sends realToken) passes
// gate 4 for both kinds.
func publishGroupTokenTable(t *testing.T, groups ...string) {
	t.Helper()
	toks := map[string]string{
		"claude-code": realToken,
		"codex-cli":   realToken,
	}
	for _, g := range groups {
		toks[GroupScopeKey(g)] = realToken
	}
	publishTokenTable(HubTokenTable{Tokens: toks})
}

// frontendSnapshotFixture builds + publishes a snapshot in which:
//   - the group "frontend" binds the {memory, time} member servers,
//   - the client "claude-code" binds ALL THREE servers (memory, time,
//     filesystem) — so the group is provably a SUBSET, and a tools/list
//     on /g/frontend that exposed filesystem would be a narrowing failure.
//
// Returns the three stub daemons (memory, time, filesystem) so the caller
// can assert which were initialized / listed.
func frontendSnapshotFixture(t *testing.T) (memSD, timeSD, fsSD *stubDaemon) {
	t.Helper()
	resetResolverForTest(t)
	memSD = newStubDaemon(t, "sid-memory")
	timeSD = newStubDaemon(t, "sid-time")
	fsSD = newStubDaemon(t, "sid-filesystem")

	memRef := canonicalDaemonRef{Server: "memory", Daemon: "claude-code", Port: memSD.port}
	timeRef := canonicalDaemonRef{Server: "time", Daemon: "claude-code", Port: timeSD.port}
	fsRef := canonicalDaemonRef{Server: "filesystem", Daemon: "claude-code", Port: fsSD.port}

	snap := &ResolverSnapshot{
		Gen: 1,
		Bindings: map[string][]canonicalDaemonRef{
			// Group: SUBSET — only memory + time.
			GroupScopeKey("frontend"): {memRef, timeRef},
			// Client: the SUPERSET — all three. Proves the group narrows.
			"claude-code": {memRef, timeRef, fsRef},
		},
		// The DECLARED-group set the production builder
		// (BuildResolverSnapshotFromManifestsAndGroups) records for every
		// groups.yaml group — the gate-2 source isKnownGroup now reads. This
		// manual fixture must mirror it or a /g/frontend request 404s as
		// "unknown group" before routing.
		Groups: map[string]bool{GroupScopeKey("frontend"): true},
	}
	PublishResolverSnapshot(snap)
	return memSD, timeSD, fsSD
}

// toolNamesFromListResponse decodes a tools/list response body into the
// set of exposed tool names.
func toolNamesFromListResponse(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode tools/list: %v / body=%s", err, string(body))
	}
	names := map[string]bool{}
	for _, tl := range resp.Result.Tools {
		names[tl.Name] = true
	}
	return names
}

// initThenList drives a full gate-ON initialize + tools/list against
// `path` (e.g. /g/frontend/mcp) and returns the exposed tool-name set.
func initThenList(t *testing.T, h *HubMcpHandler, path string) map[string]bool {
	t.Helper()
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, path, initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize %s status=%d want 200; body=%s", path, w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize %s did not return Mcp-Session-Id; body=%s", path, w.Body.String())
	}

	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	req2 := authedRequest(t, http.MethodPost, path, listBody)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("tools/list %s status=%d want 200; body=%s", path, w2.Code, w2.Body.String())
	}
	return toolNamesFromListResponse(t, w2.Body.Bytes())
}

// ----------------------------------------------------------------------
// THE KEYSTONE — /g/frontend tool-visibility narrowing.
// ----------------------------------------------------------------------

// TestGroupsPhase4b_GroupRouteNarrowsToolVisibility is the groups payoff:
// a /g/frontend/mcp session whose group binds {memory, time} exposes ONLY
// memory__* + time__* tools — never the excluded filesystem server's —
// EVEN THOUGH the client claude-code binds all three. This proves the
// group route narrows the tool surface to its member-server subset.
func TestGroupsPhase4b_GroupRouteNarrowsToolVisibility(t *testing.T) {
	memSD, timeSD, fsSD := frontendSnapshotFixture(t)

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/g/frontend/mcp")

	// Stub daemons advertise raw tools "read" + "write"; the hub exposes
	// them namespaced as "<Server>__<RawName>".
	if !names["memory__read"] || !names["memory__write"] {
		t.Errorf("/g/frontend missing memory tools; got %v", names)
	}
	if !names["time__read"] || !names["time__write"] {
		t.Errorf("/g/frontend missing time tools; got %v", names)
	}
	// THE NARROWING ASSERTION: the excluded server's tools MUST NOT appear.
	if names["filesystem__read"] || names["filesystem__write"] {
		t.Errorf("/g/frontend LEAKED excluded filesystem tools; got %v", names)
	}
	if len(names) != 4 {
		t.Errorf("/g/frontend exposed %d tools, want exactly 4 (memory__read/write + time__read/write); got %v", len(names), names)
	}

	// The group fan-out hit ONLY its two member daemons, never the
	// excluded one.
	if memSD.initCount.Load() == 0 || timeSD.initCount.Load() == 0 {
		t.Errorf("group member daemons not initialized: memory=%d time=%d", memSD.initCount.Load(), timeSD.initCount.Load())
	}
	if fsSD.initCount.Load() != 0 {
		t.Errorf("EXCLUDED filesystem daemon was initialized %d times — group fan-out leaked", fsSD.initCount.Load())
	}
}

// TestGroupsPhase4b_GroupScopeKeyOnSession pins that a /g/ initialize
// stores the kind-namespaced scope key on the session (the basis for the
// cross-kind 401 fence) AND populates IntendedParticipants from the
// group's binding.
func TestGroupsPhase4b_GroupScopeKeyOnSession(t *testing.T) {
	memSD, timeSD, _ := frontendSnapshotFixture(t)
	_ = memSD
	_ = timeSD

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	sess, ok := h.sessions.Get(sid)
	if !ok {
		t.Fatalf("session %q not found", sid)
	}
	if sess.ScopeKey != GroupScopeKey("frontend") {
		t.Errorf("sess.ScopeKey=%q want %q", sess.ScopeKey, GroupScopeKey("frontend"))
	}
	if len(sess.IntendedParticipants) != 2 {
		t.Fatalf("IntendedParticipants=%d want 2 (memory+time): %+v", len(sess.IntendedParticipants), sess.IntendedParticipants)
	}
	gotServers := map[string]bool{}
	for _, p := range sess.IntendedParticipants {
		gotServers[p.Server] = true
	}
	if !gotServers["memory"] || !gotServers["time"] || gotServers["filesystem"] {
		t.Errorf("participants servers=%v want exactly {memory,time}", gotServers)
	}
}

// ----------------------------------------------------------------------
// The /clients/ FENCE — byte-identical behavior through the SAME handler.
// ----------------------------------------------------------------------

// TestGroupsPhase4b_ClientRouteUnchangedByGroupWiring drives the SAME
// handler on the SAME snapshot via /clients/claude-code/mcp and asserts
// the client still sees ALL THREE servers' tools — proving the group
// route's narrowing did not bleed into the client path.
func TestGroupsPhase4b_ClientRouteUnchangedByGroupWiring(t *testing.T) {
	frontendSnapshotFixture(t)

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/clients/claude-code/mcp")

	for _, want := range []string{
		"memory__read", "memory__write",
		"time__read", "time__write",
		"filesystem__read", "filesystem__write",
	} {
		if !names[want] {
			t.Errorf("/clients/claude-code missing %q; got %v", want, names)
		}
	}
	if len(names) != 6 {
		t.Errorf("/clients/claude-code exposed %d tools, want 6 (all three servers); got %v", len(names), names)
	}
}

// ----------------------------------------------------------------------
// The unknown-group gate (gate 2 → 404 empty body).
// ----------------------------------------------------------------------

// TestGroupsPhase4b_UnknownGroupReturns404EmptyBody pins the unknown-group
// rejection: a /g/<group>/mcp whose group has NO token row → 404 with an
// empty body, the SAME shape the unknown-client path uses.
func TestGroupsPhase4b_UnknownGroupReturns404EmptyBody(t *testing.T) {
	resetResolverForTest(t)
	h := newTestHandler(t)
	// Publish a token table WITHOUT any group row → "frontend" unknown.
	publishGroupTokenTable(t /* no groups */)

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown group: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("unknown group: body must be empty; got %q", w.Body.String())
	}
}

// TestGroupsPhase4b_MalformedGroupPathReturns404 pins that a malformed
// /g/ path shape (no /mcp suffix, embedded slash, bare prefix) returns the
// same empty-body 404 — the parser rejects the shape before the known gate.
func TestGroupsPhase4b_MalformedGroupPathReturns404(t *testing.T) {
	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	for _, path := range []string{
		"/g/frontend",      // no /mcp suffix
		"/g/a/b/mcp",       // embedded slash
		"/g//mcp",          // empty name
		"/g/frontend/mcp/", // trailing slash
	} {
		req := authedRequest(t, http.MethodPost, path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("malformed %q: got %d, want 404", path, w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("malformed %q: body must be empty; got %q", path, w.Body.String())
		}
	}
}

// ----------------------------------------------------------------------
// Cross-kind session reuse (kind-namespacing keeps the keys unequal → 401).
// ----------------------------------------------------------------------

// TestGroupsPhase4b_CrossKindSessionReuse401_GroupOnClientPath: a session
// minted on /g/frontend/mcp (sess.ScopeKey="g:frontend") replayed on
// /clients/claude-code/mcp (scopeKey="claude-code") → 401 empty body. The
// kind-namespacing makes the keys unequal so the existing cross-client
// fence fires.
func TestGroupsPhase4b_CrossKindSessionReuse401_GroupOnClientPath(t *testing.T) {
	frontendSnapshotFixture(t)
	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	// Mint a session under /g/frontend.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("group initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no Mcp-Session-Id from group initialize")
	}

	// Replay it under /clients/claude-code/mcp → cross-kind 401.
	pingBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	req2 := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", pingBody)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("group-session-on-client-path: got %d, want 401; body=%s", w2.Code, w2.Body.String())
	}
	if w2.Body.Len() != 0 {
		t.Errorf("cross-kind 401 body must be empty; got %q", w2.Body.String())
	}
}

// TestGroupsPhase4b_CrossKindSessionReuse401_ClientOnGroupPath is the
// complement: a session minted on /clients/claude-code/mcp replayed on
// /g/frontend/mcp → 401 empty body.
func TestGroupsPhase4b_CrossKindSessionReuse401_ClientOnGroupPath(t *testing.T) {
	frontendSnapshotFixture(t)
	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	// Mint a session under /clients/claude-code.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("client initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no Mcp-Session-Id from client initialize")
	}

	// Replay it under /g/frontend/mcp → cross-kind 401.
	pingBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	req2 := authedRequest(t, http.MethodPost, "/g/frontend/mcp", pingBody)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("client-session-on-group-path: got %d, want 401; body=%s", w2.Code, w2.Body.String())
	}
	if w2.Body.Len() != 0 {
		t.Errorf("cross-kind 401 body must be empty; got %q", w2.Body.String())
	}
}

// ----------------------------------------------------------------------
// The per-group token gate (gate 4).
// ----------------------------------------------------------------------

// TestGroupsPhase4b_GroupTokenGate: a /g/frontend request whose token does
// NOT match the published "g:frontend" row → 401. Proves gate 4 validates
// against the group's own token row (constant-time compare keyed on the
// "g:<group>" scope string).
func TestGroupsPhase4b_GroupTokenGate(t *testing.T) {
	frontendSnapshotFixture(t)
	h := newTestHandler(t)
	// Publish a DISTINCT token for the group so realToken (sent by
	// authedRequest) does NOT match the group's row, while the group is
	// still "known" (the row exists → gate 2 passes, gate 4 rejects).
	publishTokenTable(HubTokenTable{Tokens: map[string]string{
		"claude-code": realToken,
		"g:frontend":  fakeToken, // != realToken
	}})

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("group wrong-token: got %d, want 401; body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() != 0 {
		t.Errorf("group token-fail 401 body must be empty; got %q", w.Body.String())
	}
}

// TestGroupsPhase4b_GroupTokenRowMatchesAllowsInitialize is the positive
// complement: with the group token row == realToken (what authedRequest
// sends) the initialize passes gate 4 and reaches the aggregate.
func TestGroupsPhase4b_GroupTokenRowMatchesAllowsInitialize(t *testing.T) {
	frontendSnapshotFixture(t)
	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("group right-token initialize: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// ----------------------------------------------------------------------
// Route-mount fence — the /g/ route is actually mounted on the SAME mux
// shape internal/gui/hub_listener.go builds (mux.Handle("/g/", handler) +
// the bare-/g 404 guard + the path.Clean prefilter). Driven through a live
// httptest.Server so a regression in the route wiring surfaces here.
// ----------------------------------------------------------------------

// buildHubMcpMuxForTest reproduces the production mux + path.Clean
// prefilter from internal/gui/hub_listener.go (the /clients/ + /g/ routes,
// their bare-prefix 404 guards, and the muxedHandler wrapper) so a route-
// mount regression is caught at the api layer without standing up the full
// bind pipeline.
func buildHubMcpMuxForTest(t *testing.T, h *HubMcpHandler) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	// Derive from the single-owner constants (deep-review r2 P4-4) so this
	// test-only mux reproduction can never silently drift from the
	// production mux built in internal/gui/hub_listener.go.
	mux.Handle(HubClientPrefix, h)
	mux.Handle(HubGroupPrefix, h)
	mux.HandleFunc(strings.TrimSuffix(HubClientPrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc(strings.TrimSuffix(HubGroupPrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cleaned := path.Clean(r.URL.Path); cleaned != r.URL.Path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// TestGroupsPhase4b_GroupRouteMountedOnMux drives a live server built with
// the production mux shape and asserts:
//   - /g/frontend/mcp reaches the handler and a valid initialize 200s
//     (the route is mounted, not 301/405/unhandled),
//   - /g/unknown/mcp returns the empty-body 404 (gate 2 on the group),
//   - the bare /g returns 404 (no auto-301 to /g/).
func TestGroupsPhase4b_GroupRouteMountedOnMux(t *testing.T) {
	frontendSnapshotFixture(t)
	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	srv := httptest.NewServer(buildHubMcpMuxForTest(t, h))
	t.Cleanup(srv.Close)

	doInit := func(path string) *http.Response {
		t.Helper()
		body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Mcphub-Hub-Token", realToken)
		req.Header.Set("X-Mcphub-Instance-Id", realInstanceID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("init %s: %v", path, err)
		}
		return resp
	}

	// Known group → mounted route → 200 + Mcp-Session-Id.
	respOK := doInit("/g/frontend/mcp")
	defer respOK.Body.Close()
	if respOK.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(respOK.Body)
		t.Fatalf("/g/frontend init: got %d, want 200; body=%s", respOK.StatusCode, string(raw))
	}
	if respOK.Header.Get("Mcp-Session-Id") == "" {
		t.Error("/g/frontend init: no Mcp-Session-Id header — route did not reach the handler")
	}

	// Unknown group → gate-2 empty-body 404.
	respUnknown := doInit("/g/infra/mcp")
	defer respUnknown.Body.Close()
	if respUnknown.StatusCode != http.StatusNotFound {
		t.Errorf("/g/infra init: got %d, want 404", respUnknown.StatusCode)
	}
	raw, _ := io.ReadAll(respUnknown.Body)
	if len(raw) != 0 {
		t.Errorf("/g/infra 404 body must be empty; got %q", string(raw))
	}

	// Bare /g → 404 (no auto-301 to /g/).
	bareReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/g", nil)
	bareReq.Header.Set("X-Mcphub-Hub-Token", realToken)
	bareReq.Header.Set("X-Mcphub-Instance-Id", realInstanceID)
	bareResp, err := http.DefaultClient.Do(bareReq)
	if err != nil {
		t.Fatalf("bare /g: %v", err)
	}
	defer bareResp.Body.Close()
	if bareResp.StatusCode != http.StatusNotFound {
		t.Errorf("bare /g: got %d, want 404 (no auto-301)", bareResp.StatusCode)
	}
}
