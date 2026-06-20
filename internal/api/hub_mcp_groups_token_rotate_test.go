package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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
	publishCurrentGroups(t)
}

func publishCurrentGroups(t *testing.T) {
	t.Helper()
	if err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer); err != nil {
		t.Fatalf("PublishGroupsSnapshotLocked: %v", err)
	}
}

func writeGroupTokenRowForTest(t *testing.T, key, token string) {
	t.Helper()
	payload, err := json.Marshal(HubTokenTable{Tokens: map[string]string{key: token}})
	if err != nil {
		t.Fatalf("marshal token table: %v", err)
	}
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, payload); err != nil {
		t.Fatalf("write token table: %v", err)
	}
}

func writeGroupTokenOrphanTombstoneForTest(t *testing.T, keys ...string) {
	t.Helper()
	orphans := make(map[string]bool, len(keys))
	for _, key := range keys {
		orphans[key] = true
	}
	payload, err := json.Marshal(struct {
		Orphans map[string]bool `json:"orphans"`
	}{Orphans: orphans})
	if err != nil {
		t.Fatalf("marshal group-token orphan tombstone: %v", err)
	}
	if err := writeHubMcpStateFile(hubMcpGroupTokenOrphansFileLeaf, payload); err != nil {
		t.Fatalf("write group-token orphan tombstone: %v", err)
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

// TestGroups_FirstPublishRecreatedOrphanTombstoneRotatesToken pins the
// offline / gate-off recreate case: a prior delete's token-prune failure
// persisted the deletion but left a token row behind, then the group was
// re-added before this process published any resolver snapshot. The persisted
// orphan tombstone is the durable discriminator; prev==nil must not make that
// stale row look like a clean-restart active token.
func TestGroups_FirstPublishRecreatedOrphanTombstoneRotatesToken(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:shared"
	stale := strings.Repeat("a", 64)
	writeGroupTokenRowForTest(t, key, stale)
	writeGroupTokenOrphanTombstoneForTest(t, key)

	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "shared", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}

	// First publish of the process (nil resolver snapshot) must still rotate
	// the tombstoned row before making the group routable.
	publishCurrentGroups(t)
	after := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(after) {
		t.Fatalf("after first publish: %q token invalid: %q", key, after)
	}
	if after == stale {
		t.Fatalf("first publish reused tombstoned orphan token %q; it must rotate before publish", stale)
	}
	if snap := LoadResolverSnapshot(); snap == nil || !snap.Groups[key] {
		t.Fatalf("group should publish after successful orphan rotation, snap=%+v", snap)
	}
}

// TestGroups_CleanRestartDeclaredGroupKeepsTokenWithoutTombstone pins the
// safety half of the persisted discriminator: a normal restart has the group
// declared in groups.yaml and its token row present, but no orphan tombstone.
// That token must survive the first publish of the new process.
func TestGroups_CleanRestartDeclaredGroupKeepsTokenWithoutTombstone(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:shared"

	// Prior process created and published the group, leaving both groups.yaml
	// and the token row on disk.
	publishWithGroups(t, []Group{{Name: "shared", Servers: []string{"memory"}}})
	seeded := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(seeded) {
		t.Fatalf("seeded token invalid: %q", seeded)
	}

	// New process: resolver snapshot is nil, but the durable state is a clean
	// active group, not a prune-failed orphan.
	resetResolverForTest(t)

	publishCurrentGroups(t)
	after := CurrentTokenTable().Tokens[key]
	if after != seeded {
		t.Fatalf("cold-start publish rotated an existing token (%q -> %q) — it must survive a clean restart", seeded, after)
	}
}

// TestGroups_StalePreviousSnapshotDoesNotSuppressTombstonedRecreate pins the
// failed-delete-publish edge case: groups.yaml durably deleted the group and
// token prune failed, but the previous resolver snapshot was never republished
// and still says the group is active. The persisted orphan tombstone must win
// over that stale in-memory snapshot.
func TestGroups_StalePreviousSnapshotDoesNotSuppressTombstonedRecreate(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	stale := strings.Repeat("b", 64)
	writeGroupTokenRowForTest(t, key, stale)
	writeGroupTokenOrphanTombstoneForTest(t, key)
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{key: true}})

	publishWithGroups(t, []Group{{Name: "frontend", Servers: []string{"memory"}}})
	after := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(after) {
		t.Fatalf("after recreate: %q token invalid: %q", key, after)
	}
	if after == stale {
		t.Fatalf("stale previous snapshot suppressed orphan rotation; token is still %q", stale)
	}
}

// TestGroups_RotationFailureDoesNotPublishStaleGroup pins fail-closed publish:
// if the orphan-rotation step errors, PublishGroupsSnapshotLocked must return
// the error without swapping in a resolver snapshot that makes the group
// routable with the old token row.
func TestGroups_RotationFailureDoesNotPublishStaleGroup(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	writeGroupTokenOrphanTombstoneForTest(t, key)
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, []byte(`{"tokens":{"g:frontend":"malformed"}}`)); err != nil {
		t.Fatalf("write malformed token table: %v", err)
	}
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{GroupScopeKey("old"): true}})
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}

	err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer)
	if err == nil {
		t.Fatal("PublishGroupsSnapshotLocked must fail when orphan token rotation fails")
	}
	if !strings.Contains(err.Error(), "rotate orphaned group token rows") {
		t.Fatalf("error should surface rotation failure, got: %v", err)
	}
	if snap := LoadResolverSnapshot(); snap == nil || snap.Groups[key] {
		t.Fatalf("failed rotation must not publish %q as routable, snap=%+v", key, snap)
	}
}

// TestGroups_DeletePruneFailurePersistsOrphanTombstone verifies the producer
// side of the durable discriminator: when the groups.yaml delete commits but
// token pruning fails, the deleted group key is recorded for a future recreate
// publish to rotate before serving.
func TestGroups_DeletePruneFailurePersistsOrphanTombstone(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("seed groups.yaml: %v", err)
	}
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, []byte(`{"tokens":{"g:frontend":"malformed"}}`)); err != nil {
		t.Fatalf("write malformed token table: %v", err)
	}

	err := ReadModifyWriteGroups(func(cfg *GroupsConfig) ([]string, error) {
		cfg.Groups = nil
		return []string{"frontend"}, nil
	})
	if !errors.Is(err, ErrTokenPruneFailed) {
		t.Fatalf("delete should return ErrTokenPruneFailed, got %v", err)
	}

	raw, err := readHubMcpStateFile(hubMcpGroupTokenOrphansFileLeaf)
	if err != nil {
		t.Fatalf("read group-token orphan tombstone: %v", err)
	}
	var tombstone struct {
		Orphans map[string]bool `json:"orphans"`
	}
	if err := json.Unmarshal(raw, &tombstone); err != nil {
		t.Fatalf("unmarshal group-token orphan tombstone: %v", err)
	}
	if !tombstone.Orphans[key] {
		t.Fatalf("expected tombstone for %q, got %+v", key, tombstone.Orphans)
	}
}
