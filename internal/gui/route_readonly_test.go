// internal/gui/route_readonly_test.go
//
// Falsifying test for bot/architect review finding F1 (Increment-1,
// 2026-07-25): the standalone `mcphub route` front daemon must be READ-ONLY
// on the registry + supervisor-intent — an unregistered-but-trusted
// workspace path must fail loud (503) WITHOUT creating a registry row,
// because new-workspace registration stays a GUI-owned concern.
//
// Mutation-proven (see the Increment-1 F1 fix report for the terminal
// transcript): temporarily re-wiring SetSerenaRouterReadOnly's AutoRegisterFn
// to a non-nil stub that performs a real registry write makes
// TestSetSerenaRouterReadOnly_UnregisteredWorkspace_Returns503WithNoRegistryMutation
// fail at the FIRST assertion it reaches — the status-code check — because
// t.Fatalf halts the test there; the mutation diverts the request onto a
// forward-attempt path instead of the canonical 503, so this specific
// mutation never even reaches the registry-entry-count assertions. That is
// not a gap in the test: each assertion independently guards its own
// property. Assertion 1 alone catches any status-shape regression even if
// the registry stayed untouched; assertions 2-3 would independently catch a
// registry mutation that happened to preserve the 503 status (e.g. a write
// added AFTER the response was already written). Together they are exactly
// what the F1 fix depends on.
package gui

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
)

// TestSetSerenaRouterReadOnly_UnregisteredWorkspace_Returns503WithNoRegistryMutation
// is the F1 falsifying test. It uses a REAL on-disk registry + a REAL,
// well-formed serena project directory (a genuine ".serena/project.yml"
// marker — the same fixture TestSerenaRouter_RealResolverIntegration_
// RoutesPathArgToCorrectWorkspace uses) that is NOT yet registered, and
// proves three things about the read-only wiring:
//
//  1. The tool-call for the unregistered path gets the canonical
//     "register workspace first" 503 — the SAME shape as the
//     routing-layer-not-wired 503 (TestSerenaRouter_RoutingLayerNotWired_
//     Returns503), because SetSerenaRouterReadOnly's nil AutoRegisterFn hits
//     the exact same attemptSerenaAutoRegister nil-guard.
//  2. NO row is added to the on-disk registry — reloaded fresh from disk
//     after the request, independent of the Registry handle the resolver
//     holds in memory, so this cannot pass by observing a stale in-memory
//     view.
//  3. NO row is added under EITHER a workspace-key or workspace-path lookup,
//     guarding against a mutation that writes under an unexpected key.
func TestSetSerenaRouterReadOnly_UnregisteredWorkspace_Returns503WithNoRegistryMutation(t *testing.T) {
	resetSerenaRouterTestSeam(t)
	serenaRouterTestSeam = nil

	root := t.TempDir()
	ws := makeSerenaWorkspace(t, root, "Trusted")
	toolFile := writeWorkspaceFile(t, ws, "src", "main.go")

	regPath := filepath.Join(root, "workspaces.yaml")
	reg := api.NewRegistry(regPath)
	// Deliberately NO PutSerena call: the registry starts (and, per this
	// test's assertion, must STAY) empty. Save + Load establishes a real
	// on-disk empty registry file, mirroring the real-resolver integration
	// test's setup so the resolver's ResolveByPath genuinely misses.
	if err := reg.Save(); err != nil {
		t.Fatalf("Save empty registry: %v", err)
	}
	if err := reg.Load(); err != nil {
		t.Fatalf("Load empty registry: %v", err)
	}

	resolver := serena_routing.NewWorkspaceResolver(reg, regPath)
	sessions := serena_routing.NewSessionRouter()

	// Port 9125 matches postSerena's hardcoded Origin header (every other
	// serena_router_test.go case uses the same port for the same reason).
	s := NewServer(Config{Port: 9125, Version: "test", PID: 1, ReadOnlyRouterMode: true})
	s.SetSerenaRouterReadOnly(resolver, sessions)

	body := buildToolCallBody(t, "find_symbol", map[string]any{"relative_path": toolFile})
	rr := postSerena(t, s, body, nil)

	// Assertion 1: the canonical unregistered-workspace 503, not a nil-panic
	// and not a forward-attempt status (502/504/200).
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (register workspace first); body=%s", rr.Code, rr.Body.String())
	}
	var resp notFoundJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode 503 body: %v; body=%s", err, rr.Body.String())
	}
	if resp.PhaseEStatus != "deferred" {
		t.Errorf("phase_e_status = %q, want %q (canonical unregistered-workspace shape)", resp.PhaseEStatus, "deferred")
	}
	if resp.HintCommand == "" {
		t.Errorf("hint_command empty, want the register-workspace hint (canonical shape)")
	}

	// Assertion 2 + 3: reload the registry FRESH from disk (not the resolver's
	// in-memory handle) and confirm it is still empty — no row under any key.
	freshReg := api.NewRegistry(regPath)
	if err := freshReg.Load(); err != nil {
		t.Fatalf("reload registry after request: %v", err)
	}
	entries := freshReg.SerenaEntries()
	if len(entries) != 0 {
		t.Fatalf("registry mutated: %d serena entries after an unregistered-workspace tool-call via the read-only wiring, want 0: %+v", len(entries), entries)
	}
	if entry, ok := freshReg.GetSerena(api.WorkspaceKey(ws)); ok {
		t.Fatalf("registry mutated: found a serena entry for %q after the read-only forward, want none: %+v", ws, entry)
	}
}

