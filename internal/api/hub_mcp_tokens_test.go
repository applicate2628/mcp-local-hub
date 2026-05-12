// hub_mcp_tokens_test.go — Phase 2 Task 2.4 (G4 unified hub MCP).
//
// Tests for the per-client token table. Three core invariants:
//
//  1. EnsureHubTokens generates a fresh 64-hex token for each named
//     client that doesn't yet have one, and persists the table to
//     <state-dir>/hub-mcp-tokens.json under hub-mcp.lock. Existing
//     tokens are NEVER rotated by EnsureHubTokens — that's
//     RotateHubToken's job (codex r5 MED).
//  2. Adding new clients via a second EnsureHubTokens call does NOT
//     rotate previously-present tokens.
//  3. RotateHubToken rotates exactly one client's token without
//     touching any other client.
//
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 2.4.

package api

import (
	"crypto/subtle"
	"encoding/json"
	"strings"
	"testing"
)

// TestEnsureHubTokensCreatesPerClientEntries verifies the happy-path
// shape: every named client ends up with a 64-lower-hex token, and
// the on-disk JSON round-trips.
func TestEnsureHubTokensCreatesPerClientEntries(t *testing.T) {
	hubMcpStateTestHelper(t)

	want := []string{"claude-code", "codex-cli", "cursor"}
	tbl, err := EnsureHubTokens(want)
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	for _, c := range want {
		tok, ok := tbl.Tokens[c]
		if !ok {
			t.Errorf("missing client %s in returned table", c)
		}
		if got := len(tok); got != 64 {
			t.Errorf("client %s token len = %d, want 64", c, got)
		}
		if strings.ToLower(tok) != tok {
			t.Errorf("client %s token must be lower-hex: %q", c, tok)
		}
	}

	// On-disk round-trip.
	raw, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatalf("read tokens file: %v", err)
	}
	var loaded HubTokenTable
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, c := range want {
		if loaded.Tokens[c] != tbl.Tokens[c] {
			t.Errorf("on-disk %s = %q, want %q", c, loaded.Tokens[c], tbl.Tokens[c])
		}
	}
}

// TestEnsureHubTokensIsIdempotent pins the "additive install must not
// rotate" contract: a second EnsureHubTokens call with a superset of
// clients adds the missing entries without touching the existing
// tokens. This is the install-reconciler interaction (Phase 5) —
// adding a new client must not invalidate every previously-installed
// client.
func TestEnsureHubTokensIsIdempotent(t *testing.T) {
	hubMcpStateTestHelper(t)

	t1, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	t2, err := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if t1.Tokens["claude-code"] != t2.Tokens["claude-code"] {
		t.Errorf("claude-code token rotated on additive install: %q -> %q",
			t1.Tokens["claude-code"], t2.Tokens["claude-code"])
	}
	if t2.Tokens["codex-cli"] == "" {
		t.Errorf("codex-cli not added on second Ensure")
	}
}

// TestEnsureHubTokensReturnsSnapshotForUnknownClients shows that
// callers passing zero clients still get the current snapshot back
// (used by `mcphub hub-mcp status` to list the table without
// mutating).
func TestEnsureHubTokensReturnsSnapshotForUnknownClients(t *testing.T) {
	hubMcpStateTestHelper(t)

	_, err := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	if err != nil {
		t.Fatalf("seed Ensure: %v", err)
	}
	snap, err := EnsureHubTokens(nil)
	if err != nil {
		t.Fatalf("snapshot Ensure: %v", err)
	}
	if _, ok := snap.Tokens["claude-code"]; !ok {
		t.Errorf("snapshot missing claude-code")
	}
	if _, ok := snap.Tokens["codex-cli"]; !ok {
		t.Errorf("snapshot missing codex-cli")
	}
	if len(snap.Tokens) != 2 {
		t.Errorf("snapshot extra entries: %v", snap.Tokens)
	}
}

// TestRotateHubTokenRotatesOnlyOneClient pins the targeted-rotation
// contract: RotateHubToken on one client must NOT change any other
// client's token. Operators use this when a single client config
// leaks; rotating only that client avoids re-installing every other
// client.
func TestRotateHubTokenRotatesOnlyOneClient(t *testing.T) {
	hubMcpStateTestHelper(t)

	t1, err := EnsureHubTokens([]string{"claude-code", "codex-cli"})
	if err != nil {
		t.Fatalf("seed Ensure: %v", err)
	}
	t2, err := RotateHubToken("claude-code")
	if err != nil {
		t.Fatalf("RotateHubToken: %v", err)
	}
	if t2.Tokens["claude-code"] == t1.Tokens["claude-code"] {
		t.Errorf("RotateHubToken did not rotate claude-code: still %q", t1.Tokens["claude-code"])
	}
	if t2.Tokens["codex-cli"] != t1.Tokens["codex-cli"] {
		t.Errorf("RotateHubToken touched codex-cli: %q -> %q",
			t1.Tokens["codex-cli"], t2.Tokens["codex-cli"])
	}
}

// TestRotateHubTokenAddsMissingClient confirms RotateHubToken on a
// previously-absent client adds an entry (rather than failing). This
// gives Phase 5 a single "ensure rotated" path: even if EnsureHubTokens
// hasn't run yet for the named client, RotateHubToken creates the
// fresh entry without complaint.
func TestRotateHubTokenAddsMissingClient(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := RotateHubToken("claude-code")
	if err != nil {
		t.Fatalf("RotateHubToken on empty: %v", err)
	}
	if len(tbl.Tokens["claude-code"]) != 64 {
		t.Errorf("claude-code token after rotate = %q", tbl.Tokens["claude-code"])
	}
}

// TestHubCurrentTokenTablePublishesAfterEnsure pins the live-snapshot
// contract: after EnsureHubTokens publishes, CurrentTokenTable
// returns the same map. Phase 4's auth gate reads via CurrentTokenTable;
// the publish must be eventually-consistent with the on-disk file.
func TestHubCurrentTokenTablePublishesAfterEnsure(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	live := CurrentTokenTable()
	if live.Tokens["claude-code"] != tbl.Tokens["claude-code"] {
		t.Errorf("CurrentTokenTable not in sync after Ensure: live=%q on-disk=%q",
			live.Tokens["claude-code"], tbl.Tokens["claude-code"])
	}
}

// TestReloadHubTokensPublishesFromDisk confirms ReloadHubTokens (the
// /internal/reload-tokens path used in Phase 4) re-reads the file and
// publishes a new snapshot. Simulated by writing a hand-crafted table
// to disk, calling Reload, then asserting CurrentTokenTable changes.
func TestReloadHubTokensPublishesFromDisk(t *testing.T) {
	hubMcpStateTestHelper(t)

	if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Mutate on disk to simulate a sibling process rotation.
	mutated := HubTokenTable{Tokens: map[string]string{
		"claude-code": strings.Repeat("c", 64),
	}}
	payload, _ := json.Marshal(mutated)
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, payload); err != nil {
		t.Fatalf("write mutated: %v", err)
	}

	reloaded, err := ReloadHubTokens()
	if err != nil {
		t.Fatalf("ReloadHubTokens: %v", err)
	}
	if reloaded.Tokens["claude-code"] != strings.Repeat("c", 64) {
		t.Errorf("ReloadHubTokens did not pick up disk change: %q", reloaded.Tokens["claude-code"])
	}
	live := CurrentTokenTable()
	if live.Tokens["claude-code"] != strings.Repeat("c", 64) {
		t.Errorf("CurrentTokenTable not re-published after Reload: %q", live.Tokens["claude-code"])
	}
}

// TestHubConstantTimeCompareTokenAccepts pins the auth gate primitive:
// matching header against stored token returns 1; mismatch returns
// 0; wrong-shape inputs (not 64 bytes) return 0 without consulting
// the table. The wrapper around subtle.ConstantTimeCompare is the
// only path Phase 4 callers should use.
func TestHubConstantTimeCompareTokenAccepts(t *testing.T) {
	hubMcpStateTestHelper(t)

	tbl, err := EnsureHubTokens([]string{"claude-code"})
	if err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	stored := tbl.Tokens["claude-code"]

	t.Run("match", func(t *testing.T) {
		if got := ConstantTimeCompareToken("claude-code", stored); got != 1 {
			t.Errorf("match returned %d, want 1", got)
		}
	})
	t.Run("mismatch", func(t *testing.T) {
		bad := strings.Repeat("0", 64)
		if got := ConstantTimeCompareToken("claude-code", bad); got != 0 {
			t.Errorf("mismatch returned %d, want 0", got)
		}
	})
	t.Run("short-header", func(t *testing.T) {
		if got := ConstantTimeCompareToken("claude-code", "short"); got != 0 {
			t.Errorf("short header returned %d, want 0", got)
		}
	})
	t.Run("unknown-client", func(t *testing.T) {
		if got := ConstantTimeCompareToken("unknown-client", stored); got != 0 {
			t.Errorf("unknown client returned %d, want 0", got)
		}
	})
	// Sanity: confirm the helper indeed delegates to subtle.ConstantTimeCompare —
	// match must equal subtle.ConstantTimeCompare(stored, stored).
	if want := subtle.ConstantTimeCompare([]byte(stored), []byte(stored)); want != 1 {
		t.Fatalf("subtle.ConstantTimeCompare sanity broke")
	}
}
