// hub_mcp_groups_phase3_test.go — groups/namespaces Phase 3.
//
// CONSUMER-SIDE end-to-end test for the ResolverSnapshot publish path.
//
// The Phase 3 diagnostic (work-items/decisions/2026-06-18-groups-
// namespaces-tool-visibility.md "Phase 3 diagnostic finding") confirmed
// that NOTHING in production publishes a ResolverSnapshot, so in gate-ON
// hub-aggregate mode LoadResolverSnapshot() returns nil at every session
// initialize → the `if snap != nil` block in handleInitialize
// (hub_mcp_handler.go:599) is skipped → sess.IntendedParticipants stays
// EMPTY → AggregateInitialize fans out to nothing → the aggregate
// exposes no tools.
//
// The gui-package test (internal/gui/hub_listener_groups_phase3_test.go)
// exercises the REAL publish path (startHubMcpListener →
// publishResolverSnapshotForHubBind). THIS test pins the CONSUMER side:
// given a published snapshot whose Bindings carry the calling client's
// (server, daemon, port) rows, a full gate-ON HTTP `initialize` MUST
// populate sess.IntendedParticipants from those bindings and the
// aggregate's tools/list MUST expose the bound daemon's tools.
//
// This is the assertion the existing manual-PublishResolverSnapshot unit
// fixtures never made end-to-end through the HTTP handler, which is how
// the dormant-aggregate gap survived.
//
// Spec: groups/namespaces decision §"Phase 3 diagnostic finding".

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"mcp-local-hub/internal/config"
)

// publishSnapshotForStubDaemon builds + publishes a ResolverSnapshot
// that binds `client` to the stub daemon `sd` (server name "fs", daemon
// name "claude-code"), using the stub's dynamically-assigned port so the
// aggregate fan-out reaches the live stub. Returns the snapshot it
// published.
func publishSnapshotForStubDaemon(t *testing.T, client string, sd *stubDaemon) *ResolverSnapshot {
	t.Helper()
	resetResolverForTest(t)
	m := config.ServerManifest{
		Name: "fs",
		Kind: "global",
		Daemons: []config.DaemonSpec{
			{Name: "claude-code", Port: sd.port},
		},
		ClientBindings: []config.ClientBinding{
			{Client: client, Daemon: "claude-code"},
		},
	}
	snap := BuildResolverSnapshotFromManifests([]config.ServerManifest{m})
	PublishResolverSnapshot(snap)
	return snap
}

// TestGroupsPhase3_InitializePopulatesIntendedParticipantsFromSnapshot
// is the fail-before / pass-after consumer-side guard. With the publish
// path wired (snapshot present), a gate-ON initialize for a bound client
// MUST populate sess.IntendedParticipants from snap.Bindings[client].
//
// BEFORE any publish-path wiring AND with the snapshot present, the
// existing `if snap != nil` block already copies the bindings — so this
// test passes today GIVEN a published snapshot. It is the regression
// fence that proves the consumed snapshot reaches IntendedParticipants;
// the gui-side test proves a snapshot actually gets published in
// production. Together they close the dormant-aggregate gap.
func TestGroupsPhase3_InitializePopulatesIntendedParticipantsFromSnapshot(t *testing.T) {
	sd := newStubDaemon(t, "daemon-sid-fs")
	defer sd.server.Close()

	h := newTestHandler(t)
	publishSnapshotForStubDaemon(t, "claude-code", sd)

	// Drive a real gate-ON HTTP initialize for claude-code.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("initialize did not return Mcp-Session-Id; body=%s", w.Body.String())
	}

	sess, ok := h.sessions.Get(sid)
	if !ok {
		t.Fatalf("session %q not found after initialize", sid)
	}
	if len(sess.IntendedParticipants) == 0 {
		t.Fatalf("sess.IntendedParticipants is EMPTY after initialize against a published snapshot — the dormant-aggregate gap")
	}
	if len(sess.IntendedParticipants) != 1 {
		t.Fatalf("IntendedParticipants=%d want 1: %+v", len(sess.IntendedParticipants), sess.IntendedParticipants)
	}
	got := sess.IntendedParticipants[0]
	if got.Server != "fs" || got.Daemon != "claude-code" || got.Port != sd.port {
		t.Errorf("participant=%+v want {Server:fs Daemon:claude-code Port:%d}", got, sd.port)
	}
	// The stub must have actually been initialized via the fan-out.
	if sd.initCount.Load() == 0 {
		t.Errorf("stub daemon initCount=0 — AggregateInitialize did not fan out to the bound participant")
	}
}

// TestGroupsPhase3_ToolsListExposesBoundServerTools proves the full
// consumer chain: published snapshot → IntendedParticipants → aggregate
// initialize → tools/list exposes the bound daemon's tools under the
// "<Server>__<RawName>" namespace. With an EMPTY snapshot (the dormant
// pre-wiring production state) the aggregate would expose zero tools.
func TestGroupsPhase3_ToolsListExposesBoundServerTools(t *testing.T) {
	sd := newStubDaemon(t, "daemon-sid-fs")
	defer sd.server.Close()

	h := newTestHandler(t)
	publishSnapshotForStubDaemon(t, "claude-code", sd)

	// initialize.
	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	req := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", initBody)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	sid := w.Header().Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatalf("no Mcp-Session-Id; body=%s", w.Body.String())
	}

	// tools/list — must merge the stub daemon's tools.
	listBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	req2 := authedRequest(t, http.MethodPost, "/clients/claude-code/mcp", listBody)
	req2.Header.Set("Mcp-Session-Id", sid)
	req2.Header.Set("MCP-Protocol-Version", "2025-11-25")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d want 200; body=%s", w2.Code, w2.Body.String())
	}

	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/list: %v / body=%s", err, w2.Body.String())
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatalf("tools/list exposed ZERO tools against a bound participant — dormant aggregate")
	}
	names := map[string]bool{}
	for _, tl := range resp.Result.Tools {
		names[tl.Name] = true
	}
	// Stub daemon advertises raw tools "read" + "write"; the hub exposes
	// them namespaced as "fs__read" / "fs__write".
	if !names["fs__read"] || !names["fs__write"] {
		t.Errorf("tools/list missing namespaced bound-server tools; got %v", names)
	}
}
