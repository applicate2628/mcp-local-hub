package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
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

// TestGroups_AllActiveTokensPresentPublishSkipsTokenReload verifies the no-op
// publish path for an already-active group whose live token row is present and
// valid. The corrupt on-disk table is a tripwire: this publish has nothing to
// add or rotate, so it must not reload or rewrite hub-mcp-tokens.json.
func TestGroups_AllActiveTokensPresentPublishSkipsTokenReload(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:backend"
	publishWithGroups(t, []Group{{Name: "backend", Servers: []string{"memory"}}})
	seeded := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(seeded) {
		t.Fatalf("seeded token invalid: %q", seeded)
	}

	corrupt := []byte(`{"tokens":{"g:backend":"short"}}`)
	if err := writeHubMcpStateFile(hubMcpTokensFileLeaf, corrupt); err != nil {
		t.Fatalf("install corrupt reload tripwire: %v", err)
	}
	before, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatalf("read corrupt tripwire: %v", err)
	}

	publishCurrentGroups(t)

	after, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatalf("read after publish: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("no-op publish rewrote token file: before %q after %q", before, after)
	}
	if got := CurrentTokenTable().Tokens[key]; got != seeded {
		t.Fatalf("live token changed on no-op publish: got %q want %q", got, seeded)
	}
}

// TestGroups_CleanRestartDeclaredGroupKeepsTokenWithoutTombstone pins the
// cold-start rule: a normal restart has the group declared in groups.yaml and
// its token row present. With no prior in-memory publish in this process, that
// declared row is authoritative active state and must survive.
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

// TestGroups_KnownGoodEmptySnapshotRotatesReintroducedRow distinguishes a
// real, known-good empty active-set from process cold start. A prior publish
// that authoritatively declared no groups is enough history to classify a
// pre-existing row as reintroduced, so the next declared publish must rotate it.
func TestGroups_KnownGoodEmptySnapshotRotatesReintroducedRow(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	stale := strings.Repeat("d", 64)
	writeGroupTokenRowForTest(t, key, stale)

	publishWithGroups(t, []Group{})
	if snap := LoadResolverSnapshot(); snap == nil || snap.Groups[key] {
		t.Fatalf("known-good empty publish precondition failed, snap=%+v", snap)
	}

	publishWithGroups(t, []Group{{Name: "frontend", Servers: []string{"memory"}}})
	after := CurrentTokenTable().Tokens[key]
	if !isValidHexToken(after) {
		t.Fatalf("after reintroduced publish: %q row missing/invalid (got %q)", key, after)
	}
	if after == stale {
		t.Fatalf("known-good empty active-set skipped needed rotation; stale token %q survived", stale)
	}
}

// TestGroups_IndeterminateActiveSetPreservesPreexistingRow pins the transient
// failure edge: once the prior active-set is known indeterminate, a declared
// pre-existing row must not be rotated on doubt. The publish surfaces the
// uncertainty so callers can retry / require restart instead of serving an
// unproven rotate-or-preserve decision.
func TestGroups_IndeterminateActiveSetPreservesPreexistingRow(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	seeded := strings.Repeat("e", 64)
	writeGroupTokenRowForTest(t, key, seeded)
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{}})
	storeResolverGroupActiveSetStatus(resolverGroupActiveSetIndeterminate)
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}

	err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer)
	if !errors.Is(err, errGroupTokenActiveSetIndeterminate) {
		t.Fatalf("PublishGroupsSnapshotLocked err = %v, want errGroupTokenActiveSetIndeterminate", err)
	}
	raw, rerr := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if rerr != nil {
		t.Fatalf("read token table after indeterminate publish: %v", rerr)
	}
	var tbl HubTokenTable
	if jerr := json.Unmarshal(raw, &tbl); jerr != nil {
		t.Fatalf("unmarshal token table after indeterminate publish: %v", jerr)
	}
	if got := tbl.Tokens[key]; got != seeded {
		t.Fatalf("indeterminate active-set rotated token; got %q want preserved %q", got, seeded)
	}
	if snap := LoadResolverSnapshot(); snap == nil || snap.Groups[key] {
		t.Fatalf("indeterminate token decision must not publish %q as routable, snap=%+v", key, snap)
	}
}

// TestGroups_StaleActiveSnapshotDoesNotSkipTokenEnsure verifies that a stale
// in-memory active snapshot is not authoritative for the token-publish skip.
// The live token table alone is not durable state; if the disk row is missing,
// the current cfg.Groups publish must still ensure it.
func TestGroups_StaleActiveSnapshotDoesNotSkipTokenEnsure(t *testing.T) {
	hubMcpStateTestHelper(t)
	resetResolverForTest(t)

	const key = "g:frontend"
	publishTokenTable(HubTokenTable{Tokens: map[string]string{key: strings.Repeat("f", 64)}})
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{key: true}})
	storeResolverGroupActiveSetStatus(resolverGroupActiveSetIndeterminate)
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}

	if err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer); err != nil {
		t.Fatalf("PublishGroupsSnapshotLocked: %v", err)
	}
	raw, err := readHubMcpStateFile(hubMcpTokensFileLeaf)
	if err != nil {
		t.Fatalf("token row was not durably ensured: %v", err)
	}
	var tbl HubTokenTable
	if err := json.Unmarshal(raw, &tbl); err != nil {
		t.Fatalf("unmarshal ensured token table: %v", err)
	}
	if tok := tbl.Tokens[key]; !isValidHexToken(tok) {
		t.Fatalf("ensured token invalid/missing for %q: %q", key, tok)
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
	stale := strings.Repeat("c", 64)
	writeGroupTokenRowForTest(t, key, stale)
	PublishResolverSnapshot(&ResolverSnapshot{Gen: 1, Groups: map[string]bool{GroupScopeKey("old"): true}})
	storeResolverGroupActiveSetStatus(resolverGroupActiveSetKnownGood)
	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{{Name: "frontend", Servers: []string{"memory"}}}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}

	prev := postRenameVerifyFailHook
	t.Cleanup(func() { postRenameVerifyFailHook = prev })
	postRenameVerifyFailHook = func() error {
		return errors.New("synthetic token rotation write failure")
	}

	err := PublishGroupsSnapshotLocked(context.Background(), scanOneServer)
	if err == nil {
		t.Fatal("PublishGroupsSnapshotLocked must fail when group token rotation cannot be written")
	}
	if !strings.Contains(err.Error(), "rotate reintroduced group token rows") {
		t.Fatalf("error should surface rotation failure, got: %v", err)
	}
	if snap := LoadResolverSnapshot(); snap == nil || snap.Groups[key] {
		t.Fatalf("failed rotation must not publish %q as routable, snap=%+v", key, snap)
	}
}

// TestGroups_DeletePruneFailureDoesNotPersistTombstone verifies that groups.yaml
// remains the single durable source of truth when a delete commits but token
// pruning fails. The caller still sees ErrTokenPruneFailed so it can force
// restart_required, but no second marker file is written.
func TestGroups_DeletePruneFailureDoesNotPersistTombstone(t *testing.T) {
	dir := hubMcpStateTestHelper(t)
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

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(strings.ToLower(entry.Name()), "orphan") {
			t.Fatalf("delete prune failure wrote legacy tombstone file %q", entry.Name())
		}
	}
}
