// hub_mcp_groups_p3c_test.go — per-project-GUI Phase 3c (group↔project binding).
//
// The api-layer half of P3c: the additive groups.yaml `project_path` field
// (design §10.1) and the §5/T3 invariant that project_path is DATA-ONLY — it is
// NOT the scope key and NOT a route segment, so it never perturbs the resolver
// snapshot. Tests:
//
//   - migration / back-compat: an existing groups.yaml with NO project_path key
//     parses with ProjectPath=="" (unbound/global) — version stays 1.
//   - round-trip: a group authored WITH project_path persists + reloads it.
//   - scope-key / route invariant: two groups identical except for project_path
//     build a BYTE-IDENTICAL resolver snapshot — the "g:<name>" scope key,
//     bindings, and Groups set are unchanged by project_path.
//
// State-safety: disk-touching tests go through hubMcpStateTestHelper (hardened
// temp state-dir via daemonStateRootOverride) so no live hub state is touched.

package api

import (
	"reflect"
	"strings"
	"testing"
)

// TestGroups_P3c_NoProjectPathParsesUnbound pins the migration / back-compat
// contract (§10.1): a pre-P3c groups.yaml with NO project_path key parses with
// ProjectPath=="" — the unbound/global meaning, byte-for-byte the prior behavior.
// version stays 1 (no schema bump).
func TestGroups_P3c_NoProjectPathParsesUnbound(t *testing.T) {
	raw := []byte("version: 1\ngroups:\n  - name: frontend\n    servers: [memory]\n")
	cfg, err := parseGroupsConfig(raw)
	if err != nil {
		t.Fatalf("parseGroupsConfig on a pre-P3c (no project_path) groups.yaml: %v", err)
	}
	if cfg.Version != 1 {
		t.Errorf("version=%d, want 1 (no schema bump for the additive field)", cfg.Version)
	}
	if len(cfg.Groups) != 1 {
		t.Fatalf("groups len=%d, want 1", len(cfg.Groups))
	}
	if cfg.Groups[0].ProjectPath != "" {
		t.Errorf("ProjectPath=%q, want \"\" (an absent project_path = unbound/global)", cfg.Groups[0].ProjectPath)
	}
}

// TestGroups_P3c_ProjectPathRoundTrip pins that a group authored WITH a
// project_path persists and reloads it through the single write owner.
func TestGroups_P3c_ProjectPathRoundTrip(t *testing.T) {
	hubMcpStateTestHelper(t)

	want := GroupsConfig{
		Version: 1,
		Groups: []Group{
			{Name: "bound", Servers: []string{"memory"}, ProjectPath: "/dev/proj"},
			{Name: "global", Servers: []string{"time"}}, // no project_path → unbound
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
		t.Fatalf("project_path round-trip mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

// TestGroups_P3c_ProjectPathOmittedWhenEmpty pins the omitempty wire/disk shape:
// a group with ProjectPath=="" serializes WITHOUT a project_path key, so a
// global group keeps the exact pre-P3c groups.yaml bytes (no spurious empty key).
func TestGroups_P3c_ProjectPathOmittedWhenEmpty(t *testing.T) {
	hubMcpStateTestHelper(t)

	if err := WriteGroups(GroupsConfig{Version: 1, Groups: []Group{
		{Name: "global", Servers: []string{"memory"}},
	}}); err != nil {
		t.Fatalf("WriteGroups: %v", err)
	}
	raw, err := readHubMcpStateFile(hubMcpGroupsFileLeaf)
	if err != nil {
		t.Fatalf("read groups.yaml: %v", err)
	}
	if strings.Contains(string(raw), "project_path") {
		t.Errorf("groups.yaml for an UNBOUND group must omit project_path (omitempty), got:\n%s", raw)
	}
}

// TestGroups_P3c_ProjectPathNotInScopeKeyOrSnapshot pins §5/T3: project_path is
// DATA-ONLY. Two groups identical except for project_path build a BYTE-IDENTICAL
// resolver snapshot — the "g:<name>" scope key, the bindings, and the known-group
// set are all unchanged. BuildResolverSnapshotFromManifestsAndGroups never reads
// project_path, so a bind/unbind cannot perturb routing.
func TestGroups_P3c_ProjectPathNotInScopeKeyOrSnapshot(t *testing.T) {
	manifests := twoServerManifests()

	unbound := []Group{{Name: "frontend", Servers: []string{"memory", "time"}}}
	bound := []Group{{Name: "frontend", Servers: []string{"memory", "time"}, ProjectPath: "/dev/proj"}}

	snapUnbound := BuildResolverSnapshotFromManifestsAndGroups(manifests, unbound)
	snapBound := BuildResolverSnapshotFromManifestsAndGroups(manifests, bound)

	// The scope key is unchanged: "g:frontend" present in BOTH, with the SAME
	// bindings.
	keyUnbound, okU := snapUnbound.Bindings[GroupScopeKey("frontend")]
	keyBound, okB := snapBound.Bindings[GroupScopeKey("frontend")]
	if !okU || !okB {
		t.Fatalf("g:frontend scope key missing: unbound=%v bound=%v", okU, okB)
	}
	if !reflect.DeepEqual(keyUnbound, keyBound) {
		t.Errorf("project_path changed the group bindings (it must NOT — data-only):\nunbound=%+v\nbound=%+v", keyUnbound, keyBound)
	}

	// The whole binding map + known-group set are equal modulo the resolver
	// generation counter (which monotonically increments per build).
	snapBound.Gen = snapUnbound.Gen
	if !reflect.DeepEqual(snapUnbound.Bindings, snapBound.Bindings) {
		t.Errorf("project_path perturbed snapshot.Bindings (must be byte-identical):\nunbound=%+v\nbound=%+v", snapUnbound.Bindings, snapBound.Bindings)
	}
	if !reflect.DeepEqual(snapUnbound.Groups, snapBound.Groups) {
		t.Errorf("project_path perturbed snapshot.Groups known-set:\nunbound=%+v\nbound=%+v", snapUnbound.Groups, snapBound.Groups)
	}
	// Ensure the test manifests actually produced the group binding (guard against
	// a vacuous pass if twoServerManifests ever stops binding the members).
	if len(keyUnbound) == 0 {
		t.Fatal("g:frontend produced zero bindings — test is vacuous; check twoServerManifests")
	}
}
