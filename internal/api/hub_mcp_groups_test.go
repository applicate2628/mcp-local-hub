// hub_mcp_groups_test.go — groups/namespaces Phase 4a (DATA layer).
//
// Tests-first contract for the Phase 4a data layer (decision
// work-items/decisions/2026-06-18-groups-namespaces-tool-visibility.md):
//
//   - groups.yaml round-trips (write → load → parse).
//   - invalid group name (contains ':') rejected.
//   - missing groups.yaml → empty GroupsConfig (NOT an error).
//   - a group's servers merge into the published snapshot under the
//     kind-namespaced "g:<group>" key, leaving the bare client keys
//     byte-identical.
//   - a group naming a non-existent server → empty/skipped binding,
//     snapshot still builds (no fault).
//   - the token table gains a "g:<group>" row after EnsureGroupTokens.
//   - kind-namespacing: a group named "claude-code" produces key
//     "g:claude-code" which does NOT collide with the bare client
//     "claude-code" key (both present, distinct).
//   - INERT WITHOUT GROUPS: with nil/empty groups the snapshot is
//     byte-identical to the groups-free build (provably additive).
//
// State-safety: every test that touches disk goes through
// hubMcpStateTestHelper (hardened temp state-dir via
// daemonStateRootOverride) so no live supervisor / hub state is touched.

package api

import (
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/config"
)

// twoServerManifests returns two manifests ("memory" + "time"), each
// with one daemon, plus a per-client binding so the bare-key client path
// is also populated. Used to assert group merge is additive vs the client
// keys.
func twoServerManifests() []config.ServerManifest {
	return []config.ServerManifest{
		{
			Name: "memory",
			Kind: "global",
			Daemons: []config.DaemonSpec{
				{Name: "claude-code", Port: 9301},
			},
			ClientBindings: []config.ClientBinding{
				{Client: "claude-code", Daemon: "claude-code"},
			},
		},
		{
			Name: "time",
			Kind: "global",
			Daemons: []config.DaemonSpec{
				{Name: "claude-code", Port: 9302},
			},
			ClientBindings: []config.ClientBinding{
				{Client: "claude-code", Daemon: "claude-code"},
			},
		},
	}
}

// --- groups.yaml config: round-trip, validation, missing-file ---

func TestGroups_YAMLRoundTrip(t *testing.T) {
	hubMcpStateTestHelper(t)

	want := GroupsConfig{
		Version: 1,
		Groups: []Group{
			{
				Name:        "frontend",
				Description: "JS/TS dev tools",
				Servers:     []string{"memory", "time"},
			},
		},
	}
	if err := WriteGroups(want); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}
	got, err := LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestGroups_LoadMissingFileIsEmptyNotError(t *testing.T) {
	hubMcpStateTestHelper(t)

	// No WriteGroups call — groups.yaml is absent.
	cfg, err := LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups on missing file should be nil error, got: %v", err)
	}
	if len(cfg.Groups) != 0 {
		t.Fatalf("missing file should yield empty groups, got %d: %+v", len(cfg.Groups), cfg.Groups)
	}
}

func TestGroups_InvalidNameRejectedOnParse(t *testing.T) {
	// A group name containing ':' could forge the "g:" kind prefix in the
	// shared scope keyspace — parseGroupsConfig MUST reject it.
	raw := []byte("version: 1\ngroups:\n  - name: \"bad:name\"\n    servers: [memory]\n")
	if _, err := parseGroupsConfig(raw); err == nil {
		t.Fatal("parseGroupsConfig accepted a group name containing ':' — must reject")
	}

	// Empty name also rejected.
	rawEmpty := []byte("version: 1\ngroups:\n  - name: \"\"\n    servers: [memory]\n")
	if _, err := parseGroupsConfig(rawEmpty); err == nil {
		t.Fatal("parseGroupsConfig accepted an empty group name — must reject")
	}
}

func TestGroups_InvalidNameRejectedOnWrite(t *testing.T) {
	hubMcpStateTestHelper(t)

	bad := GroupsConfig{Groups: []Group{{Name: "g:forged", Servers: []string{"memory"}}}}
	if err := WriteGroups(bad); err == nil {
		t.Fatal("WriteGroups persisted a group name containing ':' — must reject before any bytes hit disk")
	}
	// Nothing should have been written: a subsequent load is still empty.
	cfg, err := LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups after rejected write: %v", err)
	}
	if len(cfg.Groups) != 0 {
		t.Fatalf("rejected WriteGroups still wrote groups.yaml: %+v", cfg.Groups)
	}
}

func TestGroups_DuplicateNameRejected(t *testing.T) {
	raw := []byte("version: 1\ngroups:\n  - name: dup\n    servers: [memory]\n  - name: dup\n    servers: [time]\n")
	if _, err := parseGroupsConfig(raw); err == nil {
		t.Fatal("parseGroupsConfig accepted two groups with the same name — must reject")
	}
}

// TestGroups_DuplicateNameCaseInsensitiveOnParse pins C5-case: two groups whose
// names differ ONLY in case ("Frontend" vs "frontend") case-fold onto the same
// uniqueness key, so the parse/Load path rejects them. The error wording makes
// the case-insensitivity explicit.
func TestGroups_DuplicateNameCaseInsensitiveOnParse(t *testing.T) {
	raw := []byte("version: 1\ngroups:\n  - name: Frontend\n    servers: [memory]\n  - name: frontend\n    servers: [time]\n")
	_, err := parseGroupsConfig(raw)
	if err == nil {
		t.Fatal("parseGroupsConfig accepted \"Frontend\" + \"frontend\" — case-insensitive uniqueness must reject")
	}
	if !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("duplicate-name error %q should mention case-insensitivity", err.Error())
	}
}

// TestGroups_DuplicateNameCaseInsensitiveOnWrite pins C5-case at the create/add
// (write) dedup site: creating "frontend" when "Frontend" is already present is
// rejected before any bytes hit disk, and nothing is persisted.
func TestGroups_DuplicateNameCaseInsensitiveOnWrite(t *testing.T) {
	hubMcpStateTestHelper(t)

	collide := GroupsConfig{Version: 1, Groups: []Group{
		{Name: "Frontend", Servers: []string{"memory"}},
		{Name: "frontend", Servers: []string{"time"}},
	}}
	err := WriteGroups(collide)
	if err == nil {
		t.Fatal("WriteGroups persisted \"Frontend\" + \"frontend\" — case-insensitive uniqueness must reject")
	}
	if !strings.Contains(err.Error(), "case-insensitive") {
		t.Fatalf("write-path duplicate-name error %q should mention case-insensitivity", err.Error())
	}
	// Nothing should have been written: a subsequent load is still empty.
	cfg, err := LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups after rejected write: %v", err)
	}
	if len(cfg.Groups) != 0 {
		t.Fatalf("rejected WriteGroups still wrote groups.yaml: %+v", cfg.Groups)
	}
}

// TestGroups_NonCollidingNamesAccepted pins the happy path: distinct names are
// accepted, AND the operator's chosen CASING is preserved verbatim in the
// stored Name (the uniqueness comparison case-folds; the stored value does
// NOT). A round-trip through write → load keeps "Frontend" as "Frontend".
func TestGroups_NonCollidingNamesAccepted(t *testing.T) {
	hubMcpStateTestHelper(t)

	cfg := GroupsConfig{Version: 1, Groups: []Group{
		{Name: "Frontend", Servers: []string{"memory"}},
		{Name: "backend", Servers: []string{"time"}},
	}}
	if err := WriteGroups(cfg); err != nil {
		t.Fatalf("WriteGroups rejected two non-colliding names %+v: %v", cfg.Groups, err)
	}
	got, err := LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("expected 2 groups after round-trip, got %d: %+v", len(got.Groups), got.Groups)
	}
	// Stored casing preserved: "Frontend" must NOT be normalized to "frontend".
	if got.Groups[0].Name != "Frontend" {
		t.Fatalf("stored name lost its casing: got %q, want %q (uniqueness case-folds, storage does not)", got.Groups[0].Name, "Frontend")
	}
	if got.Groups[1].Name != "backend" {
		t.Fatalf("stored name changed: got %q, want %q", got.Groups[1].Name, "backend")
	}
}

func TestGroups_UnknownYAMLFieldRejected(t *testing.T) {
	// KnownFields(true): a typo'd field surfaces rather than silently
	// dropping config.
	raw := []byte("version: 1\ngroups:\n  - name: frontend\n    serverz: [memory]\n")
	if _, err := parseGroupsConfig(raw); err == nil {
		t.Fatal("parseGroupsConfig accepted an unknown YAML field — KnownFields(true) must reject")
	}
}

// TestGroups_UnsupportedVersionRejected pins C1 (consultant): parseGroupsConfig
// must reject a version this binary does not understand. version 0 (absent) and
// version 1 are accepted; anything else (a config from a newer binary) is a
// hard error rather than a silent misread.
func TestGroups_UnsupportedVersionRejected(t *testing.T) {
	// A future v2 — this binary can only read v1, so reject.
	raw := []byte("version: 2\ngroups:\n  - name: frontend\n    servers: [memory]\n")
	if _, err := parseGroupsConfig(raw); err == nil {
		t.Fatal("parseGroupsConfig accepted version 2 — must reject an unsupported version")
	}
	// version 1 explicit is accepted.
	if _, err := parseGroupsConfig([]byte("version: 1\ngroups: []\n")); err != nil {
		t.Fatalf("parseGroupsConfig rejected version 1: %v", err)
	}
	// version 0 / absent is accepted (the default form; writeGroupsLocked
	// stamps it to 1 on write).
	if _, err := parseGroupsConfig([]byte("groups:\n  - name: frontend\n    servers: [memory]\n")); err != nil {
		t.Fatalf("parseGroupsConfig rejected an absent version (default v0): %v", err)
	}
}

// TestGroups_UnsupportedVersionWithUnknownFieldYieldsFriendlyError pins the
// P4 deep-review reachability fix: a groups.yaml written by a NEWER mcphub
// that both bumped the version AND added a field this binary doesn't know
// about must surface the FRIENDLY "unsupported version, upgrade mcphub"
// message, not the cryptic KnownFields(true) "field not found" decode
// error. Before the fix, the strict decode ran before the version check,
// so this exact case (new field + new version together — the realistic
// forward-compat scenario) hit the unknown-field branch first and the
// friendly version message was unreachable.
func TestGroups_UnsupportedVersionWithUnknownFieldYieldsFriendlyError(t *testing.T) {
	raw := []byte("version: 2\nfuture_field: something\ngroups:\n  - name: frontend\n    servers: [memory]\n")
	_, err := parseGroupsConfig(raw)
	if err == nil {
		t.Fatal("parseGroupsConfig accepted version 2 with an unknown field — must reject")
	}
	if !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("parseGroupsConfig error = %q; want the friendly \"unsupported version\" message, not a raw decode error (reachability regression)", err.Error())
	}
}

// TestGroups_NameLengthCapRejected pins C5-length (consultant): a group name
// longer than maxGroupNameLen (64) is rejected; a 64-char name is accepted.
func TestGroups_NameLengthCapRejected(t *testing.T) {
	atCap := strings.Repeat("a", maxGroupNameLen)
	if err := validateGroupName(atCap); err != nil {
		t.Fatalf("validateGroupName rejected a %d-char name (at the cap): %v", maxGroupNameLen, err)
	}
	overCap := strings.Repeat("a", maxGroupNameLen+1)
	if err := validateGroupName(overCap); err == nil {
		t.Fatalf("validateGroupName accepted a %d-char name — must reject names longer than %d", maxGroupNameLen+1, maxGroupNameLen)
	}
}

func TestGroups_EmptyInputYieldsEmptyConfig(t *testing.T) {
	cfg, err := parseGroupsConfig([]byte("   \n  "))
	if err != nil {
		t.Fatalf("parseGroupsConfig on whitespace-only input: %v", err)
	}
	if len(cfg.Groups) != 0 {
		t.Fatalf("whitespace-only input should yield empty config, got %+v", cfg.Groups)
	}
}

// --- snapshot merge: kind-namespaced, additive, missing-server skip ---

// TestGroups_MergeIntoSnapshotKindNamespaced is the load-bearing
// assertion: a group "frontend" naming [memory, time] produces
// Bindings["g:frontend"] = the memory+time daemon refs, AND the bare
// client binding ("claude-code") is byte-identical to the groups-free
// build.
func TestGroups_MergeIntoSnapshotKindNamespaced(t *testing.T) {
	resetResolverForTest(t)
	manifests := twoServerManifests()

	// Baseline: groups-free snapshot — capture the exact client binding.
	baseline := BuildResolverSnapshotFromManifests(manifests)
	wantClient := baseline.Bindings["claude-code"]
	if len(wantClient) != 2 {
		t.Fatalf("baseline client binding should have 2 refs (memory+time), got %d: %+v", len(wantClient), wantClient)
	}

	groups := []Group{{Name: "frontend", Servers: []string{"memory", "time"}}}
	snap := BuildResolverSnapshotFromManifestsAndGroups(manifests, groups)

	// The bare client key must be byte-identical to the groups-free build.
	gotClient := snap.Bindings["claude-code"]
	if !reflect.DeepEqual(gotClient, wantClient) {
		t.Fatalf("client binding changed by group merge:\n got=%+v\nwant=%+v", gotClient, wantClient)
	}

	// The group key carries the memory+time daemon refs.
	groupRefs := snap.Bindings[GroupScopeKey("frontend")]
	if len(groupRefs) != 2 {
		t.Fatalf("g:frontend should bind 2 daemons (memory+time), got %d: %+v", len(groupRefs), groupRefs)
	}
	gotServers := map[string]int{}
	for _, r := range groupRefs {
		gotServers[r.Server] = r.Port
	}
	if gotServers["memory"] != 9301 || gotServers["time"] != 9302 {
		t.Fatalf("g:frontend daemon refs wrong: %+v (want memory:9301 time:9302)", groupRefs)
	}

	// The group key MUST be the namespaced form, not a bare "frontend".
	if _, ok := snap.Bindings["frontend"]; ok {
		t.Fatal("group bound under bare 'frontend' key — must be kind-namespaced 'g:frontend'")
	}
	if _, ok := snap.Bindings["g:frontend"]; !ok {
		t.Fatal("group NOT bound under 'g:frontend' — kind-namespacing broken")
	}
}

// TestGroups_NonExistentServerSkippedNoFault: a group naming a server
// with no manifest degrades to an empty/partial binding, snapshot still
// builds.
func TestGroups_NonExistentServerSkippedNoFault(t *testing.T) {
	resetResolverForTest(t)
	manifests := twoServerManifests()

	// "ghost" has no manifest; "memory" does.
	groups := []Group{{Name: "mixed", Servers: []string{"ghost", "memory"}}}
	snap := BuildResolverSnapshotFromManifestsAndGroups(manifests, groups)

	groupRefs := snap.Bindings[GroupScopeKey("mixed")]
	// Only "memory" resolves; "ghost" is skipped.
	if len(groupRefs) != 1 {
		t.Fatalf("g:mixed should bind only the resolvable 'memory' (1 ref), got %d: %+v", len(groupRefs), groupRefs)
	}
	if groupRefs[0].Server != "memory" {
		t.Fatalf("g:mixed sole ref should be 'memory', got %+v", groupRefs[0])
	}

	// A group naming ONLY a non-existent server → empty/absent binding,
	// still no fault.
	groupsAllGhost := []Group{{Name: "phantom", Servers: []string{"ghost"}}}
	snap2 := BuildResolverSnapshotFromManifestsAndGroups(manifests, groupsAllGhost)
	if refs := snap2.Bindings[GroupScopeKey("phantom")]; len(refs) != 0 {
		t.Fatalf("g:phantom (all-missing-server) should bind nothing, got %+v", refs)
	}
}

// TestGroups_KindNamespacingNoCollision is the explicit kind-namespacing
// proof: a group named "claude-code" (same as a real client) produces key
// "g:claude-code", which does NOT collide with the bare client
// "claude-code" key — both present, distinct, in the SAME Bindings map.
func TestGroups_KindNamespacingNoCollision(t *testing.T) {
	resetResolverForTest(t)
	manifests := twoServerManifests() // client "claude-code" is bound

	// A group whose NAME equals a client name.
	groups := []Group{{Name: "claude-code", Servers: []string{"memory"}}}
	snap := BuildResolverSnapshotFromManifestsAndGroups(manifests, groups)

	clientRefs, clientOK := snap.Bindings["claude-code"]
	groupRefs, groupOK := snap.Bindings["g:claude-code"]

	if !clientOK {
		t.Fatal("bare client key 'claude-code' missing — group merge clobbered the client keyspace")
	}
	if !groupOK {
		t.Fatal("group key 'g:claude-code' missing")
	}
	// Distinct by construction: client binds memory+time (2 refs via its
	// ClientBindings); group binds only memory (1 ref).
	if len(clientRefs) != 2 {
		t.Fatalf("client 'claude-code' should bind 2 refs, got %d: %+v", len(clientRefs), clientRefs)
	}
	if len(groupRefs) != 1 {
		t.Fatalf("group 'g:claude-code' should bind 1 ref (memory only), got %d: %+v", len(groupRefs), groupRefs)
	}
	if reflect.DeepEqual(clientRefs, groupRefs) {
		t.Fatal("client and group bindings are equal — kind-namespacing failed to keep them disjoint")
	}
}

// TestGroups_InertWithoutGroups proves additive-by-omission: with nil
// groups, the snapshot is byte-identical to the groups-free build (same
// Bindings map). resolverGen is reset between the two builds so the Gen
// counters match too.
func TestGroups_InertWithoutGroups(t *testing.T) {
	manifests := twoServerManifests()

	resetResolverForTest(t)
	groupsFree := BuildResolverSnapshotFromManifests(manifests)

	resetResolverForTest(t)
	withNilGroups := BuildResolverSnapshotFromManifestsAndGroups(manifests, nil)

	if !reflect.DeepEqual(groupsFree.Bindings, withNilGroups.Bindings) {
		t.Fatalf("nil-groups build differs from groups-free build:\n groupsFree=%+v\n withNil=%+v",
			groupsFree.Bindings, withNilGroups.Bindings)
	}
	if groupsFree.Gen != withNilGroups.Gen {
		t.Fatalf("Gen mismatch after identical reset: groupsFree=%d withNil=%d", groupsFree.Gen, withNilGroups.Gen)
	}

	// Empty (non-nil) groups slice is equally inert.
	resetResolverForTest(t)
	withEmptyGroups := BuildResolverSnapshotFromManifestsAndGroups(manifests, []Group{})
	if !reflect.DeepEqual(groupsFree.Bindings, withEmptyGroups.Bindings) {
		t.Fatalf("empty-groups build differs from groups-free build:\n groupsFree=%+v\n withEmpty=%+v",
			groupsFree.Bindings, withEmptyGroups.Bindings)
	}
}

// --- per-group token row (the §D auth seam) ---

func TestGroups_EnsureGroupTokensAddsRow(t *testing.T) {
	hubMcpStateTestHelper(t)

	groups := []Group{{Name: "frontend", Servers: []string{"memory"}}}
	keys := GroupScopeKeys(groups)
	if len(keys) != 1 || keys[0] != "g:frontend" {
		t.Fatalf("GroupScopeKeys wrong: %+v (want [g:frontend])", keys)
	}

	tbl, err := EnsureGroupTokens(keys)
	if err != nil {
		t.Fatalf("EnsureGroupTokens: %v", err)
	}
	tok, ok := tbl.Tokens["g:frontend"]
	if !ok {
		t.Fatal("token table has no 'g:frontend' row after EnsureGroupTokens")
	}
	if !isValidHexToken(tok) {
		t.Fatalf("g:frontend token is not a valid 64-hex token: %q", tok)
	}

	// The live snapshot must carry it too (published by ensureHubTokensLocked).
	live := CurrentTokenTable()
	if _, ok := live.Tokens["g:frontend"]; !ok {
		t.Fatal("live token snapshot missing 'g:frontend' after EnsureGroupTokens")
	}
}

// TestGroups_TokenKindNamespacingNoCollision: a group named "claude-code"
// and a real client "claude-code" each get their OWN token row — the
// group's under "g:claude-code", the client's under "claude-code" — so
// they never overwrite each other.
func TestGroups_TokenKindNamespacingNoCollision(t *testing.T) {
	hubMcpStateTestHelper(t)

	// Client row first.
	if _, err := EnsureHubTokens([]string{"claude-code"}); err != nil {
		t.Fatalf("EnsureHubTokens: %v", err)
	}
	// Group row with the SAME bare name.
	if _, err := EnsureGroupTokens([]string{GroupScopeKey("claude-code")}); err != nil {
		t.Fatalf("EnsureGroupTokens: %v", err)
	}

	tbl := CurrentTokenTable()
	clientTok, clientOK := tbl.Tokens["claude-code"]
	groupTok, groupOK := tbl.Tokens["g:claude-code"]
	if !clientOK {
		t.Fatal("client 'claude-code' token row missing — group ensure clobbered it")
	}
	if !groupOK {
		t.Fatal("group 'g:claude-code' token row missing")
	}
	if clientTok == groupTok {
		t.Fatal("client and group share the same token — kind-namespacing failed (they must be distinct rows)")
	}
}
