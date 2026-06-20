// hub_mcp_groups_round2_test.go — groups/namespaces bot round-2 fixes.
//
// Two regression guards on assembleToolsListResponse for findings the bot
// raised AFTER Phase 5a landed:
//
//   - F2 (empty group ≠ all-failed): a declared-but-empty group binds ZERO
//     servers, so a /g/<group>/mcp tools/list has zero INTENDED participants.
//     That is NOT the all-failed (-32000) case — nothing was attempted, so
//     the route exposes a SUCCESSFUL empty tool list (decision claim 5), not
//     a "-32000 all participating daemons failed" envelope.
//
//   - F4 (hide-before-collision leak): a tool a group hides must be excluded
//     from collision detection too. Otherwise a hidden tool that ALSO collides
//     between two same-server daemons is dropped from result.tools (Pass 2)
//     yet still named in a partialFailures collision row — leaking the hidden
//     tool's existence via diagnostics.
//
// State-safety: these build synthetic sessions + (for F4) publish a synthetic
// resolver snapshot via resetResolverForTest; they never touch live state.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestGroupsRound2_EmptyGroupReturnsEmptyToolsNotMinus32000 pins F2: a group
// session with ZERO IntendedParticipants (a declared-but-empty group) returns
// a SUCCESS envelope with an empty tools array — not the -32000 all-failed
// envelope. A normal client always binds ≥1 server, so only an empty group
// reaches this branch.
func TestGroupsRound2_EmptyGroupReturnsEmptyToolsNotMinus32000(t *testing.T) {
	sess := &hubSession{
		ClientSessionID:  "client-sid-empty-group",
		ScopeKey:         GroupScopeKey("empty"),
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           time.Now(),
		LastUsedAt:       time.Now(),
		// IntendedParticipants intentionally empty — the declared-but-empty
		// group binds no servers.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// AggregateInitialize over zero participants is a no-op that leaves
	// InitSuccesses empty; tools/list then fans out over nothing.
	if _, err := AggregateInitialize(ctx, sess, json.RawMessage(`1`)); err != nil {
		t.Fatalf("init: %v", err)
	}

	body, err := AggregateToolsList(ctx, sess, json.RawMessage(`7`), "")
	if err != nil {
		t.Fatalf("AggregateToolsList: %v", err)
	}

	var env struct {
		Error  *json.RawMessage `json:"error"`
		Result *struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("parse response: %v body=%s", uerr, body)
	}
	if env.Error != nil {
		t.Fatalf("empty group returned an error envelope (want success with empty tools): %s", body)
	}
	if env.Result == nil {
		t.Fatalf("empty group returned no result object: %s", body)
	}
	if len(env.Result.Tools) != 0 {
		t.Errorf("empty group result.tools=%d want 0 (no member servers ⇒ no tools): %s", len(env.Result.Tools), body)
	}
	// The empty route map must be published so a concurrent tools/call sees a
	// (present-but-empty) map rather than a nil one.
	if rm := sess.RouteMap.Load(); rm == nil {
		t.Errorf("RouteMap not published for empty-group success (want an empty map, got nil)")
	} else if len(*rm) != 0 {
		t.Errorf("RouteMap for empty group has %d entries, want 0", len(*rm))
	}
}

// TestGroupsRound3_ZeroBindingClientReturnsMinus32000NotEmptySuccess pins B2
// (bot R3): the empty-success branch is restricted to GROUP scopes. A /clients/
// session with ZERO IntendedParticipants (a client with no bindings — a startup
// publish failure or a client absent from the snapshot) must keep the -32000
// all-failed envelope, NOT empty-success. Returning empty-success there would
// mask a broken hub config and violate the byte-identical client contract
// (pre-groups a zero-binding client got -32000). Only a group scope (ScopeKey
// prefixed "g:") reaches empty-success; a bare client scope never does.
func TestGroupsRound3_ZeroBindingClientReturnsMinus32000NotEmptySuccess(t *testing.T) {
	sess := &hubSession{
		ClientSessionID: "client-sid-zero-binding",
		// Bare client scope key (NO "g:" prefix) — a normal /clients/ session.
		ScopeKey:         "claude-code",
		ProtocolVersion:  "2025-11-25",
		InitSuccesses:    map[canonicalDaemonRef]string{},
		DaemonProtoVer:   map[canonicalDaemonRef]string{},
		InFlightRequests: map[requestIDKey]inflightEntry{},
		InitAt:           time.Now(),
		LastUsedAt:       time.Now(),
		// IntendedParticipants intentionally empty — a zero-binding client.
	}

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
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
		Result *json.RawMessage `json:"result"`
	}
	if uerr := json.Unmarshal(body, &env); uerr != nil {
		t.Fatalf("parse response: %v body=%s", uerr, body)
	}
	if env.Error == nil {
		t.Fatalf("zero-binding CLIENT returned success (want -32000 all-failed envelope): %s", body)
	}
	if env.Error.Code != -32000 {
		t.Errorf("zero-binding CLIENT Error.Code=%d want -32000: %s", env.Error.Code, body)
	}
}

// TestGroupsRound2_HiddenCollidingToolNeverLeaksViaPartialFailures pins F4: a
// group that HIDES a tool which ALSO collides between two same-server daemons
// must drop that tool from BOTH result.tools AND partialFailures. Before the
// fix the collision row leaked the hidden tool's existence even though the
// tool itself was filtered out of the merged response.
func TestGroupsRound2_HiddenCollidingToolNeverLeaksViaPartialFailures(t *testing.T) {
	resetResolverForTest(t)

	// Two daemons under the SAME server "srv1": both expose "read" (collides)
	// and a unique tool ("write" / "format"). The group hides srv1's "read".
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

	scope := GroupScopeKey("frontend")
	d1Ref := canonicalDaemonRef{Server: "srv1", Daemon: "daemon-a", Port: d1.port}
	d2Ref := canonicalDaemonRef{Server: "srv1", Daemon: "daemon-b", Port: d2.port}

	// The snapshot the session captures at init: bindings for the group +
	// a ToolsHidden filter hiding srv1's "read" (the colliding raw name).
	snap := &ResolverSnapshot{
		Gen:      1,
		Bindings: map[string][]canonicalDaemonRef{scope: {d1Ref, d2Ref}},
		ToolsHidden: map[string]map[string][]string{
			scope: {"srv1": {"read"}},
		},
		Groups: map[string]bool{scope: true},
	}
	PublishResolverSnapshot(snap)

	sess := &hubSession{
		ClientSessionID:      "client-sid-hide-collide",
		ScopeKey:             scope,
		ProtocolVersion:      "2025-11-25",
		SnapshotAtInit:       snap,
		InitSuccesses:        map[canonicalDaemonRef]string{},
		DaemonProtoVer:       map[canonicalDaemonRef]string{},
		InFlightRequests:     map[requestIDKey]inflightEntry{},
		InitAt:               time.Now(),
		LastUsedAt:           time.Now(),
		IntendedParticipants: []canonicalDaemonRef{d1Ref, d2Ref},
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

	// 1) result.tools must contain ONLY the unique tools — never the hidden
	//    colliding srv1__read.
	names := make(map[string]int)
	for _, raw := range env.Result.Tools {
		var m struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(raw, &m)
		names[m.Name]++
	}
	if names["srv1__read"] != 0 {
		t.Errorf("hidden colliding tool srv1__read leaked into result.tools (count=%d)", names["srv1__read"])
	}
	if names["srv1__write"] != 1 || names["srv1__format"] != 1 {
		t.Errorf("non-colliding tools missing: write=%d format=%d (want 1 each): %s", names["srv1__write"], names["srv1__format"], body)
	}

	// 2) partialFailures must NOT name the hidden tool — the leak this fix
	//    closes. No collision row may reference srv1__read.
	for _, f := range env.Result.Meta.Mcphub.PartialFailures {
		if strings.Contains(f.Err, "srv1__read") || strings.Contains(f.Err, "namespace collision") {
			t.Errorf("hidden tool leaked via partialFailures collision row: %+v", f)
		}
	}

	// 3) RouteMap must not carry the hidden tool either.
	if rm := sess.RouteMap.Load(); rm != nil {
		if _, ok := (*rm)["srv1__read"]; ok {
			t.Errorf("RouteMap retains hidden colliding key srv1__read")
		}
	}
}
