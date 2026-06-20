package api

import (
	"context"
	"testing"

	"mcp-local-hub/internal/config"
)

// scanOneServer returns a fixed single-server manifest set for the
// PublishGroupsSnapshotLocked scan closure. Group-token rotation does not
// depend on binding resolution, so a minimal stable scan is enough.
func scanOneServer() ([]config.ServerManifest, error) {
	return []config.ServerManifest{
		{Name: "memory", Kind: "global", Daemons: []config.DaemonSpec{{Name: "claude-code", Port: 9451}}},
	}, nil
}

// publishWithGroups writes the supplied groups to groups.yaml then runs
// PublishGroupsSnapshotLocked, so the published snapshot's active-group
// set + the token table reflect exactly `groups`.
func publishWithGroups(t *testing.T, groups []Group) {
	t.Helper()
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: groups}); err != nil {
		t.Fatalf("WriteGroups(%+v): %v", groups, err)
	}
	if err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer); err != nil {
		t.Fatalf("PublishGroupsSnapshotLocked: %v", err)
	}
}

// TestGroups_ReusedTokenRotatesOnRecreateOverOrphan pins the
// "stale token reused on group re-create" fix: when a deleted group's
// "g:<name>" token row is left behind (the ErrTokenPruneFailed orphan
// case) and a group of the same name is later RE-CREATED, the recreate
// publish must ROTATE the orphaned row to a FRESH token rather than
// reuse the stale (possibly leaked) one.
func TestGroups_ReusedTokenRotatesOnRecreateOverOrphan(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"

	// 1. Create "frontend" → it becomes ACTIVE (in the published snapshot)
	//    and gets a token row.
	publishWithGroups(t, []Group{{Name: "frontend", Servers: []string{"memory"}}})
	tok0, ok := CurrentTokenTable().Tokens[key]
	if !ok || !isValidHexToken(tok0) {
		t.Fatalf("after create: %q row missing/invalid (got ok=%v tok=%q)", key, ok, tok0)
	}

	// 2. Simulate a delete whose token-prune FAILED: the group leaves
	//    groups.yaml (so the next publish drops it from the active snapshot)
	//    but its token row is LEFT BEHIND on disk (the orphan). We model the
	//    failed-prune residue by publishing an EMPTY groups set (snapshot no
	//    longer declares "frontend" active) while leaving the token row in
	//    place — re-asserting the row directly so the prune appears to have
	//    failed.
	publishWithGroups(t, []Group{}) // frontend now NOT active in the snapshot
	if _, err := EnsureGroupTokens([]string{key}); err != nil {
		t.Fatalf("re-assert orphan token row: %v", err)
	}
	orphanTok := CurrentTokenTable().Tokens[key]
	if orphanTok != tok0 {
		t.Fatalf("orphan row should still carry the original token (got %q want %q)", orphanTok, tok0)
	}
	// Confirm the snapshot no longer declares frontend active (the orphan
	// precondition the rotation keys on).
	if snap := LoadResolverSnapshot(); snap != nil && snap.Groups[key] {
		t.Fatalf("precondition failed: %q is still active in the snapshot; orphan rotation would be skipped", key)
	}

	// 3. RE-CREATE "frontend". The recreate publish must ROTATE the orphaned
	//    row (frontend is NOT in the prior active set) to a fresh token.
	publishWithGroups(t, []Group{{Name: "frontend", Servers: []string{"memory"}}})
	tok1, ok := CurrentTokenTable().Tokens[key]
	if !ok || !isValidHexToken(tok1) {
		t.Fatalf("after recreate: %q row missing/invalid (got ok=%v tok=%q)", key, ok, tok1)
	}
	if tok1 == orphanTok {
		t.Fatalf("recreate REUSED the stale orphan token %q — it must be rotated to a fresh secret", orphanTok)
	}
}

// TestGroups_StillActiveTokenNotRotated pins the safety half: a group that
// is ALREADY active (in the published snapshot) must KEEP its token across
// a re-publish — rotating it would break live /g/ sessions. Only the
// orphan-recreate case rotates.
func TestGroups_StillActiveTokenNotRotated(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:backend"

	// Create "backend" → active + token row.
	publishWithGroups(t, []Group{{Name: "backend", Servers: []string{"memory"}}})
	tok0, ok := CurrentTokenTable().Tokens[key]
	if !ok || !isValidHexToken(tok0) {
		t.Fatalf("after create: %q row missing/invalid", key)
	}

	// Re-publish WITHOUT deleting it (e.g. an unrelated manifest change). The
	// group stays active in the snapshot, so its token must be preserved.
	publishWithGroups(t, []Group{{Name: "backend", Servers: []string{"memory"}}})
	tok1, ok := CurrentTokenTable().Tokens[key]
	if !ok {
		t.Fatalf("after re-publish: %q row vanished", key)
	}
	if tok1 != tok0 {
		t.Fatalf("a still-active group's token was rotated (%q -> %q) — live sessions would break", tok0, tok1)
	}
}

// TestGroups_ColdStartDoesNotRotateExistingTokens pins the cold-start
// guard: on the FIRST publish of the process (no prior in-memory snapshot)
// the on-disk token rows are legitimate active-group tokens that MUST
// survive a clean restart. Orphan rotation is skipped on the first publish,
// so an existing token row is reused, not rotated.
func TestGroups_ColdStartDoesNotRotateExistingTokens(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:shared"

	// Seed a token row on disk WITHOUT ever publishing a snapshot (models a
	// prior process having created the group, then this process cold-starting
	// with the row already on disk and the in-memory snapshot nil).
	if _, err := EnsureGroupTokens([]string{key}); err != nil {
		t.Fatalf("seed token row: %v", err)
	}
	seeded := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(seeded) {
		t.Fatalf("seeded token invalid: %q", seeded)
	}

	// Reset the in-memory snapshot to nil to model a true cold start (the
	// token row stays on disk).
	resetResolverForTest(t)

	// First publish of the process WITH the group declared. Because there is
	// no prior published snapshot, orphan rotation is skipped — the existing
	// token must be preserved (reused), not rotated.
	publishWithGroups(t, []Group{{Name: "shared", Servers: []string{"memory"}}})
	after := CurrentTokenTable().Tokens[key]
	if after != seeded {
		t.Fatalf("cold-start publish rotated an existing token (%q -> %q) — it must survive a clean restart", seeded, after)
	}
}
