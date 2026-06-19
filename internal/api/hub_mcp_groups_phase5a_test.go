// hub_mcp_groups_phase5a_test.go — groups/namespaces Phase 5a
// (FINE-GRAINED per-tool visibility filter for group sessions).
//
// Phase 4a parsed Group.ToolsHidden but left it INERT; Phase 4b added the
// /g/<group>/mcp route + whole-server-subset narrowing. Phase 5a applies
// the per-tool grain ON TOP: for a group session, a tool the group hides
// (`tools_hidden: {server: [rawname]}`) is DROPPED at the tools/list MERGE
// step — it never enters the merged response NOR the session RouteMap, so
// a tools/call for it returns the existing -32601 "Method not found".
//
// The data path: Group.ToolsHidden folds into the published
// ResolverSnapshot.ToolsHidden (scopeKey → server → hidden raw names) in
// the SAME atomic build as Bindings, so a session captures a consistent
// (bindings, filter) pair at initialize. For a CLIENT scope key there is
// no ToolsHidden entry → nil → NO filtering (the byte-identical fence).
//
// THE KEYSTONE assertion (per-tool grain, not whole-server exclusion):
// a /g/frontend session with tools_hidden {memory:[write]} shows
// memory__read + time__read + time__write but NOT memory__write — the
// OTHER memory tool and ALL time tools survive, proving the filter is
// per-tool, not whole-server.
//
// State-safety: snapshot + token table are package-level atomic pointers;
// tests publish synthetic ones via resetResolverForTest + publishTokenTable
// and never touch live supervisor / hub state.
//
// Spec: groups/namespaces decision §"DECISION (2026-06-18)" operator
// decision 3 (FINE-GRAINED per-tool filter in v1) + §"Per-group tool
// visibility" layer 2.
package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// frontendSnapshotWithHiddenFixture mirrors frontendSnapshotFixture but
// additionally publishes a ToolsHidden filter on the "g:frontend" scope
// key hiding the supplied (server → raw tool names) entries. The client
// "claude-code" scope key carries NO filter, so the client-route fence
// stays byte-identical. Returns the three stub daemons.
func frontendSnapshotWithHiddenFixture(t *testing.T, hidden map[string][]string) (memSD, timeSD, fsSD *stubDaemon) {
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
			GroupScopeKey("frontend"): {memRef, timeRef},
			"claude-code":             {memRef, timeRef, fsRef},
		},
		// Per-tool filter ONLY on the group scope key. The client key has
		// no entry → no filtering (the fence).
		ToolsHidden: map[string]map[string][]string{
			GroupScopeKey("frontend"): hidden,
		},
		// The DECLARED-group set the production builder records for every
		// groups.yaml group — the gate-2 source isKnownGroup now reads; the
		// manual fixture mirrors it (else /g/frontend 404s before routing).
		Groups: map[string]bool{GroupScopeKey("frontend"): true},
	}
	PublishResolverSnapshot(snap)
	return memSD, timeSD, fsSD
}

// ----------------------------------------------------------------------
// THE KEYSTONE — per-tool grain on a group session.
// ----------------------------------------------------------------------

// TestGroupsPhase5a_PerToolHideNarrowsWithinServer is the Phase 5a payoff:
// a /g/frontend session that hides {memory:[write]} exposes memory__read +
// time__read + time__write but NOT memory__write. The OTHER memory tool +
// ALL time tools survive — proving the filter is PER-TOOL, not
// whole-server exclusion (memory itself stays in the group).
func TestGroupsPhase5a_PerToolHideNarrowsWithinServer(t *testing.T) {
	memSD, timeSD, fsSD := frontendSnapshotWithHiddenFixture(t, map[string][]string{
		"memory": {"write"},
	})

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/g/frontend/mcp")

	// The hidden tool is ABSENT.
	if names["memory__write"] {
		t.Errorf("/g/frontend LEAKED hidden tool memory__write; got %v", names)
	}
	// The OTHER memory tool survives — proving memory is still a group
	// member and only the named tool was dropped (per-tool grain).
	if !names["memory__read"] {
		t.Errorf("/g/frontend dropped memory__read (only memory__write should be hidden); got %v", names)
	}
	// ALL time tools survive — the filter named no time entry.
	if !names["time__read"] || !names["time__write"] {
		t.Errorf("/g/frontend dropped a time tool (no time filter was set); got %v", names)
	}
	// Exactly 3 tools: memory__read + time__read + time__write.
	if len(names) != 3 {
		t.Errorf("/g/frontend exposed %d tools, want exactly 3 (memory__read + time__read + time__write); got %v", len(names), names)
	}

	// Both member daemons were still listed (memory is NOT excluded as a
	// server — only one of its tools is filtered).
	if memSD.listCount.Load() == 0 || timeSD.listCount.Load() == 0 {
		t.Errorf("group member daemons not listed: memory=%d time=%d", memSD.listCount.Load(), timeSD.listCount.Load())
	}
	if fsSD.initCount.Load() != 0 {
		t.Errorf("EXCLUDED filesystem daemon was initialized %d times — group fan-out leaked", fsSD.initCount.Load())
	}
}

// TestGroupsPhase5a_HiddenToolCallReturns32601 pins the dispatch-path
// claim: a tools/call for a hidden tool returns -32601 "Method not found"
// because the filtered tool never entered the session RouteMap. The
// dispatch path (resolveToolsCallRoute) is UNCHANGED — it just can't find
// a route that was never published.
func TestGroupsPhase5a_HiddenToolCallReturns32601(t *testing.T) {
	frontendSnapshotWithHiddenFixture(t, map[string][]string{
		"memory": {"write"},
	})

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	// initialize + tools/list to build the (filtered) RouteMap.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")

	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	reqL := authedRequest(t, http.MethodPost, "/g/frontend/mcp", listBody)
	reqL.Header.Set("Mcp-Session-Id", sid)
	reqL.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wL := httptest.NewRecorder()
	h.ServeHTTP(wL, reqL)
	if wL.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d want 200; body=%s", wL.Code, wL.Body.String())
	}

	// tools/call for the HIDDEN tool → -32601 (not in RouteMap).
	callHidden := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory__write","arguments":{}}}`)
	reqC := authedRequest(t, http.MethodPost, "/g/frontend/mcp", callHidden)
	reqC.Header.Set("Mcp-Session-Id", sid)
	reqC.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wC := httptest.NewRecorder()
	h.ServeHTTP(wC, reqC)
	if wC.Code != http.StatusOK {
		t.Fatalf("tools/call status=%d want 200 (JSON-RPC error envelope); body=%s", wC.Code, wC.Body.String())
	}
	assertJSONRPCErrorCode(t, wC.Body.Bytes(), -32601)

	// A VISIBLE tool on the same group session still routes (proves the
	// RouteMap is populated, the hidden tool is the only one missing).
	callVisible := []byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"memory__read","arguments":{}}}`)
	reqV := authedRequest(t, http.MethodPost, "/g/frontend/mcp", callVisible)
	reqV.Header.Set("Mcp-Session-Id", sid)
	reqV.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wV := httptest.NewRecorder()
	h.ServeHTTP(wV, reqV)
	if wV.Code != http.StatusOK {
		t.Fatalf("tools/call memory__read status=%d want 200; body=%s", wV.Code, wV.Body.String())
	}
	if hasJSONRPCError(wV.Body.Bytes()) {
		t.Errorf("visible tool memory__read returned an error; body=%s", wV.Body.String())
	}
}

// TestGroupsPhase5a_RepublishedHiddenToolRevokesExistingSession closes the
// stale-session revocation gap: a group session that listed memory__write
// before the operator hid it must not keep calling the stale RouteMap entry
// after the live ResolverSnapshot is republished with the same daemon binding
// but a new ToolsHidden policy.
func TestGroupsPhase5a_RepublishedHiddenToolRevokesExistingSession(t *testing.T) {
	memSD, timeSD, _ := frontendSnapshotWithHiddenFixture(t, nil)

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	// initialize + tools/list while memory__write is visible, so the
	// session RouteMap contains a route for memory__write.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/g/frontend/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")

	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	reqL := authedRequest(t, http.MethodPost, "/g/frontend/mcp", listBody)
	reqL.Header.Set("Mcp-Session-Id", sid)
	reqL.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wL := httptest.NewRecorder()
	h.ServeHTTP(wL, reqL)
	if wL.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d want 200; body=%s", wL.Code, wL.Body.String())
	}
	if !toolNamesFromListResponse(t, wL.Body.Bytes())["memory__write"] {
		t.Fatalf("pre-republish tools/list did not expose memory__write; body=%s", wL.Body.String())
	}

	// Republish the current resolver snapshot with the SAME group daemon
	// bindings but a new tools_hidden filter. daemonStillBound remains true;
	// only the per-tool visibility revalidation should reject the stale route.
	PublishResolverSnapshot(&ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			GroupScopeKey("frontend"): {
				{Server: "memory", Daemon: "claude-code", Port: memSD.port},
				{Server: "time", Daemon: "claude-code", Port: timeSD.port},
			},
		},
		ToolsHidden: map[string]map[string][]string{
			GroupScopeKey("frontend"): {"memory": {"write"}},
		},
		Groups: map[string]bool{GroupScopeKey("frontend"): true},
	})

	// REGRESSION (list/call symmetry): a SECOND tools/list on the SAME session
	// after the republish must NOT re-advertise memory__write. Before the
	// hiddenToolsForScope live-snapshot fix, tools/list filtered off the
	// init-captured snapshot and kept listing (and re-routing) memory__write
	// even though the very next tools/call rejected it with -32601 — a
	// list-says-yes / call-says-no split that only self-healed on reconnect.
	reqL2 := authedRequest(t, http.MethodPost, "/g/frontend/mcp", listBody)
	reqL2.Header.Set("Mcp-Session-Id", sid)
	reqL2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wL2 := httptest.NewRecorder()
	h.ServeHTTP(wL2, reqL2)
	if wL2.Code != http.StatusOK {
		t.Fatalf("post-republish tools/list status=%d want 200; body=%s", wL2.Code, wL2.Body.String())
	}
	if toolNamesFromListResponse(t, wL2.Body.Bytes())["memory__write"] {
		t.Fatalf("post-republish tools/list still advertises hidden memory__write; list and call disagree; body=%s", wL2.Body.String())
	}

	callHidden := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory__write","arguments":{}}}`)
	reqC := authedRequest(t, http.MethodPost, "/g/frontend/mcp", callHidden)
	reqC.Header.Set("Mcp-Session-Id", sid)
	reqC.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wC := httptest.NewRecorder()
	h.ServeHTTP(wC, reqC)
	if wC.Code != http.StatusOK {
		t.Fatalf("tools/call status=%d want 200 (JSON-RPC error envelope); body=%s", wC.Code, wC.Body.String())
	}
	assertJSONRPCErrorCode(t, wC.Body.Bytes(), -32601)

	if memSD.callCount.Load() != 0 {
		t.Fatalf("stale hidden tool reached memory daemon %d time(s)", memSD.callCount.Load())
	}
}

// TestGroupsPhase5a_RepublishedMemberRemovalDropsFromList closes the
// member-removal half of the list/call split (Codex #376). When a group
// edit REMOVES a member server, the republished snapshot no longer carries
// that server's tools_hidden entry, so a hidden-filter-only fix would
// re-advertise + re-route its tools on the next tools/list while tools/call
// rejects them via daemonStillBound. tools/list must apply the live
// membership check too, so list and call agree for member-removal edits.
func TestGroupsPhase5a_RepublishedMemberRemovalDropsFromList(t *testing.T) {
	memSD, timeSD, _ := frontendSnapshotWithHiddenFixture(t, nil)

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

	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	listNames := func() map[string]bool {
		r := authedRequest(t, http.MethodPost, "/g/frontend/mcp", listBody)
		r.Header.Set("Mcp-Session-Id", sid)
		r.Header.Set("MCP-Protocol-Version", "2025-11-25")
		ww := httptest.NewRecorder()
		h.ServeHTTP(ww, r)
		if ww.Code != http.StatusOK {
			t.Fatalf("tools/list status=%d want 200; body=%s", ww.Code, ww.Body.String())
		}
		return toolNamesFromListResponse(t, ww.Body.Bytes())
	}
	if !listNames()["memory__write"] {
		t.Fatalf("pre-removal tools/list missing memory__write")
	}

	// Republish with memory REMOVED from the group (only time remains).
	// daemonStillBound now returns false for the memory daemon.
	PublishResolverSnapshot(&ResolverSnapshot{
		Gen: 2,
		Bindings: map[string][]canonicalDaemonRef{
			GroupScopeKey("frontend"): {
				{Server: "time", Daemon: "claude-code", Port: timeSD.port},
			},
		},
		Groups: map[string]bool{GroupScopeKey("frontend"): true},
	})

	// tools/list must DROP the removed member's tools, KEEP the surviving
	// member's — list now agrees with the daemonStillBound call fence.
	names := listNames()
	if names["memory__write"] || names["memory__read"] {
		t.Fatalf("post-removal tools/list still advertises removed-member memory tools; got %v", names)
	}
	if !names["time__read"] {
		t.Fatalf("post-removal tools/list dropped surviving-member time tools; got %v", names)
	}

	// tools/call on the removed member → -32601, never reaches the daemon.
	callRemoved := []byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"memory__write","arguments":{}}}`)
	rc := authedRequest(t, http.MethodPost, "/g/frontend/mcp", callRemoved)
	rc.Header.Set("Mcp-Session-Id", sid)
	rc.Header.Set("MCP-Protocol-Version", "2025-11-25")
	wc := httptest.NewRecorder()
	h.ServeHTTP(wc, rc)
	assertJSONRPCErrorCode(t, wc.Body.Bytes(), -32601)
	if memSD.callCount.Load() != 0 {
		t.Fatalf("removed-member tool reached memory daemon %d time(s)", memSD.callCount.Load())
	}
}

// ----------------------------------------------------------------------
// The /clients/ FENCE — a CLIENT route is NEVER filtered.
// ----------------------------------------------------------------------

// TestGroupsPhase5a_ClientRouteUnfilteredByGroupHidden drives the SAME
// snapshot (which DOES carry a g:frontend tools_hidden filter) via
// /clients/claude-code/mcp and asserts the client still sees ALL SIX tools
// including memory__write — the per-tool filter is keyed on the group
// scope key, so it never touches a client scope key. Byte-identical fence.
func TestGroupsPhase5a_ClientRouteUnfilteredByGroupHidden(t *testing.T) {
	frontendSnapshotWithHiddenFixture(t, map[string][]string{
		"memory": {"write"},
	})

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/clients/claude-code/mcp")

	for _, want := range []string{
		"memory__read", "memory__write", // memory__write MUST be present for the client
		"time__read", "time__write",
		"filesystem__read", "filesystem__write",
	} {
		if !names[want] {
			t.Errorf("/clients/claude-code missing %q (group hidden filter leaked into client route); got %v", want, names)
		}
	}
	if len(names) != 6 {
		t.Errorf("/clients/claude-code exposed %d tools, want 6 (all three servers, unfiltered); got %v", len(names), names)
	}
}

// ----------------------------------------------------------------------
// Degenerate filters — empty / missing / non-existent → harmless no-op.
// ----------------------------------------------------------------------

// TestGroupsPhase5a_EmptyHiddenBehavesLikePhase4b: a group with an EMPTY
// tools_hidden map (or nil) behaves EXACTLY like Phase 4b — all member
// tools present, none dropped.
func TestGroupsPhase5a_EmptyHiddenBehavesLikePhase4b(t *testing.T) {
	// nil filter on the group scope key.
	frontendSnapshotWithHiddenFixture(t, nil)

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/g/frontend/mcp")

	for _, want := range []string{"memory__read", "memory__write", "time__read", "time__write"} {
		if !names[want] {
			t.Errorf("empty filter dropped %q; got %v", want, names)
		}
	}
	if len(names) != 4 {
		t.Errorf("empty filter exposed %d tools, want 4 (Phase-4b-identical); got %v", len(names), names)
	}
}

// TestGroupsPhase5a_HiddenForNonMemberServerOrTool_NoOp: a tools_hidden
// entry naming a server NOT in the group's servers (filesystem — excluded
// already) OR a tool that does not exist (memory:[nonexistent]) is a
// HARMLESS no-op — no fault, and no over-broad drop.
func TestGroupsPhase5a_HiddenForNonMemberServerOrTool_NoOp(t *testing.T) {
	frontendSnapshotWithHiddenFixture(t, map[string][]string{
		// filesystem is NOT a member of g:frontend → filtering it is moot.
		"filesystem": {"read"},
		// "nonexistent" is not a real memory tool → naming it drops nothing.
		"memory": {"nonexistent"},
	})

	h := newTestHandler(t)
	publishGroupTokenTable(t, "frontend")

	names := initThenList(t, h, "/g/frontend/mcp")

	// All four member tools survive — neither degenerate entry dropped
	// anything real.
	for _, want := range []string{"memory__read", "memory__write", "time__read", "time__write"} {
		if !names[want] {
			t.Errorf("degenerate filter dropped real tool %q; got %v", want, names)
		}
	}
	if len(names) != 4 {
		t.Errorf("degenerate filter exposed %d tools, want 4 (no real drop); got %v", len(names), names)
	}
}

// ----------------------------------------------------------------------
// Snapshot-builder unit coverage — ToolsHidden folds in from Group.
// ----------------------------------------------------------------------

// TestGroupsPhase5a_BuilderFoldsToolsHidden pins that
// BuildResolverSnapshotFromManifestsAndGroups copies each group's
// ToolsHidden into snap.ToolsHidden under the SAME "g:<group>" scope key
// as its Bindings, and that a group with NO tools_hidden contributes no
// entry (so the client/bare-key path stays free of the new map).
func TestGroupsPhase5a_BuilderFoldsToolsHidden(t *testing.T) {
	resetResolverForTest(t)
	manifests := twoServerManifests() // memory + time, both bound to claude-code
	groups := []Group{
		{
			Name:    "frontend",
			Servers: []string{"memory", "time"},
			ToolsHidden: map[string][]string{
				"memory": {"write"},
			},
		},
		{
			Name:    "infra",
			Servers: []string{"time"},
			// no ToolsHidden
		},
	}
	snap := BuildResolverSnapshotFromManifestsAndGroups(manifests, groups)

	gKey := GroupScopeKey("frontend")
	got, ok := snap.ToolsHidden[gKey]
	if !ok {
		t.Fatalf("snap.ToolsHidden missing %q entry; got %+v", gKey, snap.ToolsHidden)
	}
	if len(got["memory"]) != 1 || got["memory"][0] != "write" {
		t.Errorf("snap.ToolsHidden[%q][memory]=%v want [write]", gKey, got["memory"])
	}
	// The group with no ToolsHidden contributes NO entry.
	if _, ok := snap.ToolsHidden[GroupScopeKey("infra")]; ok {
		t.Errorf("group with no tools_hidden produced a ToolsHidden entry: %+v", snap.ToolsHidden)
	}
	// The bare client key never gets a filter.
	if _, ok := snap.ToolsHidden["claude-code"]; ok {
		t.Errorf("client scope key got a ToolsHidden entry: %+v", snap.ToolsHidden)
	}
}

// TestGroupsPhase5a_NilGroupsNilToolsHidden pins the additive-by-omission
// invariant at the builder: with nil groups, snap.ToolsHidden is nil/empty
// (the groups-free build never allocates a filter), so the client path is
// byte-identical.
func TestGroupsPhase5a_NilGroupsNilToolsHidden(t *testing.T) {
	resetResolverForTest(t)
	manifests := twoServerManifests()
	snap := BuildResolverSnapshotFromManifests(manifests) // nil groups
	if len(snap.ToolsHidden) != 0 {
		t.Errorf("groups-free build produced a non-empty ToolsHidden: %+v", snap.ToolsHidden)
	}
}

// assertJSONRPCErrorCode decodes a JSON-RPC envelope and fails unless it
// carries an error object with the expected code.
func assertJSONRPCErrorCode(t *testing.T, body []byte, wantCode int) {
	t.Helper()
	var env struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode JSON-RPC envelope: %v; body=%s", err, string(body))
	}
	if env.Error == nil {
		t.Fatalf("expected JSON-RPC error code=%d, got no error object; body=%s", wantCode, string(body))
	}
	if env.Error.Code != wantCode {
		t.Errorf("JSON-RPC error code=%d want %d (msg=%q)", env.Error.Code, wantCode, env.Error.Message)
	}
}

// hasJSONRPCError reports whether the envelope carries an error object.
func hasJSONRPCError(body []byte) bool {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return false
	}
	return len(env.Error) > 0 && string(env.Error) != "null"
}
