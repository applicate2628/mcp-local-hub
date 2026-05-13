// install_hub_reconcile_test.go — Phase 5 Task 5.1 (G4 unified hub MCP).
//
// Tests for BuildHubReconcilePlan (full-reconcile planner that owns the
// gate ON/OFF transition) + ApplyHubReconcileInOrder (add-before-remove
// crash-safe ordering per spec §"Crash-safe reconcile ordering") + the
// per-server install path's invariant that it MUST NOT emit a
// `Remove EntryName="mcphub-hub"` even though both planners share the
// extended ClientUpdatePlan shape.

package api

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/config"
)

// reconcileTwoServerManifests builds two minimal global manifests that
// together cover the four shipped clients (claude-code, codex-cli,
// cursor, plus opt-in vscode used to exercise the per-client union).
// The bindings overlap so the per-client union has multiple
// (server, daemon) refs for claude-code.
func reconcileTwoServerManifests() []config.ServerManifest {
	return []config.ServerManifest{
		{
			Name:      "alpha",
			Kind:      config.KindGlobal,
			Transport: config.TransportNativeHTTP,
			Command:   "uvx",
			Daemons: []config.DaemonSpec{
				{Name: "claude", Port: 9301},
				{Name: "codex", Port: 9302},
			},
			ClientBindings: []config.ClientBinding{
				{Client: "claude-code", Daemon: "claude", URLPath: "/mcp"},
				{Client: "codex-cli", Daemon: "codex", URLPath: "/mcp"},
			},
		},
		{
			Name:      "beta",
			Kind:      config.KindGlobal,
			Transport: config.TransportNativeHTTP,
			Command:   "uvx",
			Daemons: []config.DaemonSpec{
				{Name: "claude", Port: 9303},
			},
			ClientBindings: []config.ClientBinding{
				{Client: "claude-code", Daemon: "claude", URLPath: "/mcp"},
				{Client: "cursor", Daemon: "claude", URLPath: "/mcp"},
			},
		},
	}
}

// TestBuildHubReconcilePlanGateOnAddsMcphubHubAndRemovesPerDaemon pins
// the ON-transition contract per spec §"Bidirectional install
// reconciler": exactly one AddReplace `mcphub-hub` entry per client
// with a non-empty participating-daemon set + one Remove for every
// previously-installed per-(server, client) entry.
func TestBuildHubReconcilePlanGateOnAddsMcphubHubAndRemovesPerDaemon(t *testing.T) {
	manifests := reconcileTwoServerManifests()
	endpoint := HubEndpoint{
		Port:       9180,
		InstanceID: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		PID:        4242,
		StartedAt:  "2026-05-12T00:00:00.000000000Z",
	}
	tokens := HubTokenTable{Tokens: map[string]string{
		"claude-code": "11111111111111111111111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"codex-cli":   "22222222222222222222222222222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"cursor":      "33333333333333333333333333333333cccccccccccccccccccccccccccccccc",
	}}

	plan, err := BuildHubReconcilePlan(manifests, endpoint, tokens, HubReconcileOpts{GateOn: true})
	if err != nil {
		t.Fatalf("BuildHubReconcilePlan: %v", err)
	}

	// Group ops by client + action.
	type key struct {
		client string
		action ClientUpdateAction
		entry  string
	}
	got := map[key]ClientUpdatePlan{}
	for _, p := range plan {
		k := key{p.Client, p.Action, p.EntryName}
		if _, dup := got[k]; dup {
			t.Errorf("duplicate plan op for %+v: existing=%+v new=%+v", k, got[k], p)
		}
		got[k] = p
	}

	// Each of the three bound clients gets exactly one AddReplace
	// `mcphub-hub` entry with the spec'd URL + token + instance id.
	for _, client := range []string{"claude-code", "codex-cli", "cursor"} {
		k := key{client, ClientUpdateAddReplace, "mcphub-hub"}
		op, ok := got[k]
		if !ok {
			t.Errorf("gate ON: missing AddReplace mcphub-hub for client %q", client)
			continue
		}
		wantURL := "http://127.0.0.1:9180/clients/" + client + "/mcp"
		if op.URL != wantURL {
			t.Errorf("gate ON %s: URL = %q, want %q", client, op.URL, wantURL)
		}
		if op.Headers["X-Mcphub-Hub-Token"] != tokens.Tokens[client] {
			t.Errorf("gate ON %s: X-Mcphub-Hub-Token = %q, want %q", client, op.Headers["X-Mcphub-Hub-Token"], tokens.Tokens[client])
		}
		if op.Headers["X-Mcphub-Instance-Id"] != endpoint.InstanceID {
			t.Errorf("gate ON %s: X-Mcphub-Instance-Id = %q, want %q", client, op.Headers["X-Mcphub-Instance-Id"], endpoint.InstanceID)
		}
	}

	// claude-code has TWO per-(server, daemon) refs (alpha+beta), each
	// of which gets a Remove. codex-cli has one (alpha). cursor has
	// one (beta).
	wantRemoves := map[string][]string{
		"claude-code": {"alpha", "beta"},
		"codex-cli":   {"alpha"},
		"cursor":      {"beta"},
	}
	for client, names := range wantRemoves {
		for _, name := range names {
			k := key{client, ClientUpdateRemove, name}
			op, ok := got[k]
			if !ok {
				t.Errorf("gate ON: missing Remove %s for client %q", name, client)
				continue
			}
			if op.URL != "" {
				t.Errorf("gate ON %s Remove %s: URL = %q, want empty", client, name, op.URL)
			}
		}
	}
}

// TestBuildHubReconcilePlanGateOffRemovesAggregateForAllSupportedClients
// pins the codex deep-sec phase5 r11 P2 closure on PR #160 (protocol
// lane): gate-OFF reconcile MUST emit a `Remove mcphub-hub` op for
// every supported client adapter, NOT just clients that currently
// have manifest bindings. The pre-r11 code skipped supported clients
// without bindings, leaving a stale aggregate entry behind whenever
// the operator uninstalled every server that bound to a particular
// client (or whenever the manifest set changed between gate ON and
// gate OFF). Antigravity is intentionally excluded
// (hubReconcileSkipClients).
func TestBuildHubReconcilePlanGateOffRemovesAggregateForAllSupportedClients(t *testing.T) {
	// Only one manifest with one client binding — three other supported
	// clients (vscode, gemini-cli, qwen-cli) have NO bindings in this set
	// but MUST still receive a `Remove mcphub-hub` op.
	manifests := []config.ServerManifest{{
		Name:      "alpha",
		Kind:      config.KindGlobal,
		Transport: config.TransportNativeHTTP,
		Command:   "uvx",
		Daemons:   []config.DaemonSpec{{Name: "default", Port: 9100}},
		ClientBindings: []config.ClientBinding{
			{Client: "claude-code", Daemon: "default", URLPath: "/mcp"},
		},
	}}
	endpoint := HubEndpoint{Port: 9180, InstanceID: "irrelevant-for-gate-off"}
	tokens := HubTokenTable{Tokens: map[string]string{}}

	plan, err := BuildHubReconcilePlan(manifests, endpoint, tokens, HubReconcileOpts{GateOn: false})
	if err != nil {
		t.Fatalf("BuildHubReconcilePlan: %v", err)
	}

	// Collect clients that got a Remove mcphub-hub op.
	gotRemove := map[string]bool{}
	for _, op := range plan {
		if op.Action == ClientUpdateRemove && op.EntryName == "mcphub-hub" {
			gotRemove[op.Client] = true
		}
	}

	// Every supported client EXCEPT antigravity (skip-listed) must
	// be present in gotRemove, regardless of whether it had a
	// binding in `manifests`.
	for _, c := range clients.SupportedClientNames() {
		if hubReconcileSkipClients[c] {
			if gotRemove[c] {
				t.Errorf("client %q is in hubReconcileSkipClients but got a Remove op — skip list ignored", c)
			}
			continue
		}
		if !gotRemove[c] {
			t.Errorf("gate-OFF: missing Remove mcphub-hub for supported client %q (stale aggregate would persist)", c)
		}
	}

	// Sanity: claude-code (with binding) also got AddReplace for the
	// per-server entry.
	gotAdd := false
	for _, op := range plan {
		if op.Client == "claude-code" && op.Action == ClientUpdateAddReplace && op.EntryName == "alpha" {
			gotAdd = true
		}
	}
	if !gotAdd {
		t.Error("claude-code with binding must still get AddReplace for the alpha per-server entry")
	}
}

// TestBuildHubReconcilePlanGateOnFailsFastOnMissingToken pins the
// codex bot phase5 r7 P1 closure on PR #160: gate-ON reconcile MUST
// reject the plan upfront when any participating client lacks a
// token entry. Empty `tokens.Tokens[client]` would otherwise produce
// an aggregate `mcphub-hub` entry with a blank X-Mcphub-Hub-Token
// header that the 7-check auth gate rejects as 401 on every request,
// silently bricking the client until the operator finds the empty
// header in disk config.
func TestBuildHubReconcilePlanGateOnFailsFastOnMissingToken(t *testing.T) {
	manifests := reconcileTwoServerManifests()
	endpoint := HubEndpoint{Port: 9180, InstanceID: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"}
	// claude-code and codex-cli have tokens; cursor is missing. The
	// planner must reject the entire plan rather than emit a partial
	// one that bricks cursor.
	tokens := HubTokenTable{Tokens: map[string]string{
		"claude-code": "11111111111111111111111111111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"codex-cli":   "22222222222222222222222222222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}}
	_, err := BuildHubReconcilePlan(manifests, endpoint, tokens, HubReconcileOpts{GateOn: true})
	if err == nil {
		t.Fatal("BuildHubReconcilePlan: expected fail-fast on missing per-client token; got nil error")
	}
	if !strings.Contains(err.Error(), "cursor") {
		t.Errorf("error message must name the missing client; got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "hub-mcp-tokens.json") {
		t.Errorf("error message must reference hub-mcp-tokens.json for operator guidance; got %q", err.Error())
	}
}

// TestBuildHubReconcilePlanGateOffRemovesMcphubHubAndRestoresPerDaemon
// is the symmetric OFF-transition: AddReplace each per-(server,
// client) entry pointing at the per-daemon HTTP URL + Remove the
// `mcphub-hub` aggregate.
func TestBuildHubReconcilePlanGateOffRemovesMcphubHubAndRestoresPerDaemon(t *testing.T) {
	manifests := reconcileTwoServerManifests()
	endpoint := HubEndpoint{Port: 9180, InstanceID: "doesnotmatterforgateoff"}
	tokens := HubTokenTable{Tokens: map[string]string{}}

	plan, err := BuildHubReconcilePlan(manifests, endpoint, tokens, HubReconcileOpts{GateOn: false})
	if err != nil {
		t.Fatalf("BuildHubReconcilePlan: %v", err)
	}

	type key struct {
		client string
		action ClientUpdateAction
		entry  string
	}
	got := map[key]ClientUpdatePlan{}
	for _, p := range plan {
		k := key{p.Client, p.Action, p.EntryName}
		got[k] = p
	}

	// Each client gets a `Remove mcphub-hub` op.
	for _, client := range []string{"claude-code", "codex-cli", "cursor"} {
		k := key{client, ClientUpdateRemove, "mcphub-hub"}
		op, ok := got[k]
		if !ok {
			t.Errorf("gate OFF: missing Remove mcphub-hub for client %q", client)
			continue
		}
		if op.URL != "" {
			t.Errorf("gate OFF %s Remove mcphub-hub: URL = %q, want empty", client, op.URL)
		}
	}

	// claude-code gets AddReplace alpha + AddReplace beta with the
	// per-daemon URLs; codex-cli gets AddReplace alpha; cursor gets
	// AddReplace beta.
	wantAdds := map[string]map[string]string{
		"claude-code": {"alpha": "http://localhost:9301/mcp", "beta": "http://localhost:9303/mcp"},
		"codex-cli":   {"alpha": "http://localhost:9302/mcp"},
		"cursor":      {"beta": "http://localhost:9303/mcp"},
	}
	for client, names := range wantAdds {
		for entryName, wantURL := range names {
			k := key{client, ClientUpdateAddReplace, entryName}
			op, ok := got[k]
			if !ok {
				t.Errorf("gate OFF: missing AddReplace %s for client %q", entryName, client)
				continue
			}
			if op.URL != wantURL {
				t.Errorf("gate OFF %s %s: URL = %q, want %q", client, entryName, op.URL, wantURL)
			}
			// Per-daemon Headers must be empty (token routing is only
			// for the aggregate entry).
			if len(op.Headers) != 0 {
				t.Errorf("gate OFF %s %s: Headers = %+v, want empty", client, entryName, op.Headers)
			}
		}
	}
}

// TestApplyHubReconcileAddsBeforeRemoves pins spec §"Crash-safe
// reconcile ordering": within a single client config rewrite, ALL
// AddReplace ops apply before any Remove op. The ordering matters
// because if a crash occurs mid-sequence and only Removes have hit
// disk, the client is left with no entry at all; if AddReplace went
// first, the client at worst has a stale-but-functional entry until
// the operator re-runs.
//
// We assert ordering by routing applyOpsForClient through a fake that
// records the order it sees ops.
func TestApplyHubReconcileAddsBeforeRemoves(t *testing.T) {
	// Mixed plan for a single client: one AddReplace + one Remove.
	plan := []ClientUpdatePlan{
		{Client: "claude-code", Path: "fake", Action: ClientUpdateRemove, EntryName: "alpha"},
		{Client: "claude-code", Path: "fake", Action: ClientUpdateAddReplace, EntryName: "mcphub-hub", URL: "http://127.0.0.1:9180/clients/claude-code/mcp"},
	}

	// Capture the order applyOpsForClient is called with.
	type seen struct {
		client  string
		actions []ClientUpdateAction
	}
	var calls []seen
	prev := applyOpsForClientForTest
	applyOpsForClientForTest = func(client string, ops []ClientUpdatePlan) error {
		var acts []ClientUpdateAction
		for _, o := range ops {
			acts = append(acts, o.Action)
		}
		calls = append(calls, seen{client: client, actions: acts})
		return nil
	}
	t.Cleanup(func() { applyOpsForClientForTest = prev })

	report := ApplyHubReconcileInOrder(plan)
	if len(report.Failed) != 0 {
		t.Errorf("Failed = %+v, want empty", report.Failed)
	}
	if len(calls) != 2 {
		t.Fatalf("applyOpsForClient called %d times, want 2 (one for adds, one for removes)", len(calls))
	}
	// First batch MUST be adds only.
	for _, a := range calls[0].actions {
		if a != ClientUpdateAddReplace {
			t.Errorf("first batch contained non-AddReplace op %q (full=%+v)", a, calls[0].actions)
		}
	}
	// Second batch MUST be removes only.
	for _, a := range calls[1].actions {
		if a != ClientUpdateRemove {
			t.Errorf("second batch contained non-Remove op %q (full=%+v)", a, calls[1].actions)
		}
	}
}

// TestPerServerInstallSkipsHubEntryRemoval pins codex r3 general F2
// closure: `mcphub install --server X` MUST NOT emit a Remove on
// EntryName="mcphub-hub". That entry is owned by the full-reconcile
// pipeline; per-server installs only touch their own per-(server,
// client) bindings.
func TestPerServerInstallSkipsHubEntryRemoval(t *testing.T) {
	m := serenaLikeManifest()
	p, err := BuildPlanWithOpts(m, BuildPlanOpts{IncludeAllClients: true})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, u := range p.ClientUpdates {
		if u.Action == ClientUpdateRemove {
			t.Errorf("per-server install emitted a Remove op for client %s entry %s — only the full-reconcile path may remove entries", u.Client, u.EntryName)
		}
		if u.EntryName == "mcphub-hub" {
			t.Errorf("per-server install emitted a mcphub-hub op (entry=%s, action=%s) — the aggregate is owned by BuildHubReconcilePlan only", u.EntryName, u.Action)
		}
		// Sanity: every per-server op carries the server name in
		// EntryName so the reconciler can compute the union from
		// previously-installed entries.
		if u.EntryName != m.Name {
			t.Errorf("per-server install op EntryName = %q, want %q (manifest server name) — needed so BuildHubReconcilePlan can identify per-(server,client) removals)", u.EntryName, m.Name)
		}
	}
	// Sanity check on the suite-wide invariant: action constants exist
	// and the string forms match the spec wire format. Catches a
	// future refactor that accidentally renames the constant.
	if string(ClientUpdateAddReplace) != "add/replace" {
		t.Errorf("ClientUpdateAddReplace = %q, want %q (wire format)", string(ClientUpdateAddReplace), "add/replace")
	}
	if string(ClientUpdateRemove) != "remove" {
		t.Errorf("ClientUpdateRemove = %q, want %q (wire format)", string(ClientUpdateRemove), "remove")
	}
	// Defensive: the printer surface in install.go expects %s of
	// ClientUpdateAction to be the wire form (this is the bridge
	// between the spec field type and human-readable CLI output —
	// any stringer regression would silently change CLI output).
	if !strings.HasPrefix(strings.ToLower(string(ClientUpdateAddReplace)), "add") {
		t.Errorf("ClientUpdateAddReplace stringer regression: %q", ClientUpdateAddReplace)
	}
}
