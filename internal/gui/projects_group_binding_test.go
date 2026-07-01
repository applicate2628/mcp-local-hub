// internal/gui/projects_group_binding_test.go
//
// Per-project-GUI Phase 3c: POST /api/projects/group-binding (the bind/unbind
// write) + the /api/projects per-project binding FILTER (design §10.1). Tests:
//   - bind write: project_path persisted CanonicalProjectKey-normalized.
//   - unbind (empty project_path) → ProjectPath cleared (global).
//   - validation: relative / traversal project_path → 400; a not-yet-existing
//     ABSOLUTE path → ACCEPTED (no require-exist — §10.1).
//   - 404 on an absent group; no write on not-found.
//   - filter: a bound group shows ONLY in its project; an unbound group shows in
//     ALL projects.
//   - golden: a bind/unbind does NOT perturb the global DefaultScanConfigPaths
//     scan (scan.go isolation).
//
// State-safety: isolateHome(t) fences HOME/LOCALAPPDATA; api.SetDaemonStateRootForTest
// gives each test its own state dir; groups are seeded via api.WriteGroups into
// that sandbox — NEVER the live groups.yaml.
package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/clients"
)

// postGroupBinding issues a same-origin POST /api/projects/group-binding.
func postGroupBinding(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/projects/group-binding", strings.NewReader(body))
	req.Host = "127.0.0.1:9081"
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.port.Store(9081)
	s.mux.ServeHTTP(rec, req)
	return rec
}

// seedOneGroup writes a single group (server "memory") into the sandboxed state
// dir via the single write owner, optionally pre-bound to projectPath.
func seedOneGroup(t *testing.T, name, projectPath string) {
	t.Helper()
	g := api.Group{Name: name, Servers: []string{"memory"}}
	if projectPath != "" {
		g.ProjectPath = projectPath
	}
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{g}}); err != nil {
		t.Fatalf("seed group %q: %v", name, err)
	}
}

// loadGroupProjectPath reads the persisted ProjectPath of one group from the
// sandboxed groups.yaml.
func loadGroupProjectPath(t *testing.T, name string) (string, bool) {
	t.Helper()
	cfg, err := api.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	for _, g := range cfg.Groups {
		if g.Name == name {
			return g.ProjectPath, true
		}
	}
	return "", false
}

// TestGroupBinding_BindThenUnbind: bind a group to a project (persisted as the
// CanonicalProjectKey-normalized path), then unbind it (cleared → global).
func TestGroupBinding_BindThenUnbind(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})

	seedOneGroup(t, "frontend", "")

	root := t.TempDir() // an existing absolute path (existence is NOT required, but a real one is convenient)
	wantKey := clients.CanonicalProjectKey(root)

	// BIND.
	rec := postGroupBinding(t, s, `{"group":"frontend","project_path":"`+jsonEscPath(root)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp projectGroupBindingResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ProjectPath != wantKey {
		t.Errorf("response project_path=%q, want canonical %q", resp.ProjectPath, wantKey)
	}
	if got, ok := loadGroupProjectPath(t, "frontend"); !ok || got != wantKey {
		t.Errorf("persisted ProjectPath=%q (ok=%v), want canonical %q", got, ok, wantKey)
	}

	// UNBIND (empty project_path).
	rec = postGroupBinding(t, s, `{"group":"frontend","project_path":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("unbind status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got, ok := loadGroupProjectPath(t, "frontend"); !ok || got != "" {
		t.Errorf("after unbind ProjectPath=%q (ok=%v), want \"\" (global)", got, ok)
	}
}

// TestGroupBinding_NotYetExistingAbsolutePathAccepted: §10.1 — a group may be
// bound to a project that is NOT registered / does NOT exist yet (clean ABS path,
// never stat'd). The bind succeeds and persists the canonical key.
func TestGroupBinding_NotYetExistingAbsolutePathAccepted(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})
	seedOneGroup(t, "frontend", "")

	// A clean absolute path that does NOT exist on disk.
	var ghost string
	if runtime.GOOS == "windows" {
		ghost = `C:/dev/not-yet-created-xyz`
	} else {
		ghost = "/dev/not-yet-created-xyz"
	}
	wantKey := clients.CanonicalProjectKey(ghost)

	rec := postGroupBinding(t, s, `{"group":"frontend","project_path":"`+jsonEscPath(ghost)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind to not-yet-existing path status=%d body=%s (must be accepted — no require-exist)", rec.Code, rec.Body.String())
	}
	if got, ok := loadGroupProjectPath(t, "frontend"); !ok || got != wantKey {
		t.Errorf("persisted ProjectPath=%q (ok=%v), want %q", got, ok, wantKey)
	}
}

// TestGroupBinding_RelativeAndTraversalRejected: a relative project_path and a
// traversal-bearing path are both 400 PROJECT_GROUP_BINDING_INVALID and write
// nothing.
func TestGroupBinding_RelativeAndTraversalRejected(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})
	seedOneGroup(t, "frontend", "")

	cases := []struct {
		name string
		path string
	}{
		{"relative", "dev/proj"},
		{"traversal-rel", "../../etc"},
	}
	// A traversal segment in an ABSOLUTE path must also be rejected (not silently
	// collapsed). On Windows use a drive-rooted form; on POSIX a leading-slash one.
	if runtime.GOOS == "windows" {
		cases = append(cases, struct{ name, path string }{"traversal-abs", `C:/dev/../etc`})
	} else {
		cases = append(cases, struct{ name, path string }{"traversal-abs", "/dev/../etc"})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := postGroupBinding(t, s, `{"group":"frontend","project_path":"`+jsonEscPath(tc.path)+`"}`)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d, want 400 for %q; body=%s", rec.Code, tc.path, rec.Body.String())
			}
			var env struct {
				Code string `json:"code"`
			}
			_ = json.NewDecoder(rec.Body).Decode(&env)
			if env.Code != "PROJECT_GROUP_BINDING_INVALID" {
				t.Errorf("code=%q, want PROJECT_GROUP_BINDING_INVALID", env.Code)
			}
			// Nothing persisted: the group stays UNBOUND.
			if got, ok := loadGroupProjectPath(t, "frontend"); !ok || got != "" {
				t.Errorf("rejected bind still wrote ProjectPath=%q (ok=%v)", got, ok)
			}
		})
	}
}

// TestGroupBinding_MissingGroupIs404: binding a non-existent group is a 404 and
// writes nothing.
func TestGroupBinding_MissingGroupIs404(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})
	seedOneGroup(t, "frontend", "")

	root := t.TempDir()
	rec := postGroupBinding(t, s, `{"group":"nope","project_path":"`+jsonEscPath(root)+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&env)
	if env.Code != "PROJECT_GROUP_BINDING_NOT_FOUND" {
		t.Errorf("code=%q, want PROJECT_GROUP_BINDING_NOT_FOUND", env.Code)
	}
	// The seeded group is untouched.
	if got, ok := loadGroupProjectPath(t, "frontend"); !ok || got != "" {
		t.Errorf("a not-found binding perturbed the existing group: ProjectPath=%q (ok=%v)", got, ok)
	}
}

// TestGroupBinding_MissingGroupNameIs400: an empty/absent group name is a 400.
func TestGroupBinding_MissingGroupNameIs400(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})

	rec := postGroupBinding(t, s, `{"group":"","project_path":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGroupBinding_FilterBoundShowsOnlyInItsProject: the /api/projects aggregate
// filter (groupVisibleInProject) shows a BOUND group ONLY in its own project,
// and an UNBOUND group in ALL projects.
func TestGroupBinding_FilterBoundShowsOnlyInItsProject(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	t.Cleanup(api.SetClientWriteFallbackForTest())

	rootA := t.TempDir()
	rootB := t.TempDir()
	keyA := clients.CanonicalProjectKey(rootA)

	// Two registered projects (Model A entries).
	injectWorkspaceRegistry(t, []api.WorkspaceEntry{
		{WorkspaceKey: "a", WorkspacePath: rootA, Language: "go", Backend: "mcp-language-server"},
		{WorkspaceKey: "b", WorkspacePath: rootB, Language: "go", Backend: "mcp-language-server"},
	})

	// One group BOUND to project A; one UNBOUND (global).
	if err := api.WriteGroups(api.GroupsConfig{Version: 1, Groups: []api.Group{
		{Name: "bound-a", Servers: []string{"memory"}, ProjectPath: keyA},
		{Name: "global", Servers: []string{"time"}},
	}}); err != nil {
		t.Fatalf("seed groups: %v", err)
	}

	s := NewServer(Config{})
	rec := getProjectsAggregate(t, s)
	if rec.Code != http.StatusOK {
		t.Fatalf("aggregate status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out projectsAggregateResponse
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	byKey := map[string]projectAggregateDTO{}
	for _, p := range out.Projects {
		byKey[p.Key] = p
	}
	pa, okA := byKey[keyA]
	pb, okB := byKey[clients.CanonicalProjectKey(rootB)]
	if !okA || !okB {
		t.Fatalf("expected both projects in aggregate; keys=%v", func() []string {
			ks := []string{}
			for k := range byKey {
				ks = append(ks, k)
			}
			return ks
		}())
	}

	// Project A sees BOTH bound-a (bound to it) AND global.
	if got := groupNames(pa.Groups); !reflect.DeepEqual(sortedCopy(got), []string{"bound-a", "global"}) {
		t.Errorf("project A groups=%v, want [bound-a global]", got)
	}
	// Project B sees ONLY global (bound-a is bound to A, not B).
	if got := groupNames(pb.Groups); !reflect.DeepEqual(sortedCopy(got), []string{"global"}) {
		t.Errorf("project B groups=%v, want [global] (bound-a must NOT leak into B)", got)
	}

	// Each rendered group carries its project_path so the UI can label it.
	for _, g := range pa.Groups {
		if g.Name == "bound-a" && g.ProjectPath != keyA {
			t.Errorf("bound-a in project A has project_path=%q, want %q", g.ProjectPath, keyA)
		}
		if g.Name == "global" && g.ProjectPath != "" {
			t.Errorf("global group has project_path=%q, want \"\"", g.ProjectPath)
		}
	}
}

// TestGroupVisibleInProject_PreFoldPersistedStillMatches pins bot PR #474 P2:
// groupVisibleInProject re-normalizes the STORED ProjectPath through
// CanonicalProjectKey at compare time, so a groups.yaml entry persisted on a
// case-folding platform (Windows / macOS) by an OLDER binary that stored a
// CASE-PRESERVING ProjectPath still matches the now-folded project key. On a
// non-folding platform (Linux) the same mixed-case stored value is genuinely a
// DIFFERENT project and must NOT match — the predicate is driven by the same
// caseFoldsProjectKey decision, so this asserts both directions per GOOS.
func TestGroupVisibleInProject_PreFoldPersistedStillMatches(t *testing.T) {
	// A stored binding in the OLD case-preserving form (what a pre-fold binary
	// would have written on macOS), and the lookup key in the NEW folded form
	// the current aggregate produces.
	var storedPreFold, foldedKey string
	if runtime.GOOS == "windows" {
		storedPreFold, foldedKey = "C:/Dev/Proj", "c:/dev/proj"
	} else {
		storedPreFold, foldedKey = "/Dev/Proj", "/dev/proj"
	}

	g := api.Group{Name: "frontend", ProjectPath: storedPreFold}
	got := groupVisibleInProject(g, foldedKey)
	// The expected outcome is DERIVED from the single owner's actual fold
	// behavior on this GOOS, not a re-typed predicate: the pre-fold stored
	// value matches iff CanonicalProjectKey reduces it to the same key (true
	// where the platform folds, false where it preserves case). Driving the
	// expectation off the owner keeps the invariant single-sourced.
	wantVisible := clients.CanonicalProjectKey(storedPreFold) == clients.CanonicalProjectKey(foldedKey)
	if got != wantVisible {
		t.Errorf("groupVisibleInProject(stored=%q, key=%q) on GOOS=%s = %v, want %v (a pre-fold-persisted binding must still match where the platform folds, and must NOT match where it does not)",
			storedPreFold, foldedKey, runtime.GOOS, got, wantVisible)
	}

	// An UNBOUND group is always visible regardless of platform/fold.
	if !groupVisibleInProject(api.Group{Name: "global"}, foldedKey) {
		t.Errorf("unbound group must be visible in every project")
	}

	// Sanity: an already-canonical bound value still matches (no regression for
	// freshly-written entries).
	if !groupVisibleInProject(api.Group{Name: "fresh", ProjectPath: foldedKey}, foldedKey) {
		t.Errorf("a freshly-written canonical binding must still match its own project key")
	}
}

func groupNames(gs []groupDTO) []string {
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

func sortedCopy(in []string) []string {
	out := append([]string{}, in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// TestGroupBinding_GlobalScanUnchanged: a bind/unbind write does NOT perturb the
// global DefaultScanConfigPaths scan (scan.go isolation — the binding only
// touches groups.yaml, never the global client scan).
func TestGroupBinding_GlobalScanUnchanged(t *testing.T) {
	isolateHome(t)
	t.Cleanup(api.SetDaemonStateRootForTest(t.TempDir()))
	s := NewServer(Config{})
	seedOneGroup(t, "frontend", "")

	before, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("baseline global scan: %v", err)
	}

	root := t.TempDir()
	if rec := postGroupBinding(t, s, `{"group":"frontend","project_path":"`+jsonEscPath(root)+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("bind status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := postGroupBinding(t, s, `{"group":"frontend","project_path":""}`); rec.Code != http.StatusOK {
		t.Fatalf("unbind status=%d body=%s", rec.Code, rec.Body.String())
	}

	after, err := api.NewAPI().ScanFrom(api.ScanOpts{ConfigPaths: api.DefaultScanConfigPaths()})
	if err != nil {
		t.Fatalf("post global scan: %v", err)
	}
	before.At = after.At
	sortEntriesByName(before.Entries)
	sortEntriesByName(after.Entries)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("global scan changed across a group bind/unbind:\nbefore=%+v\nafter=%+v", before, after)
	}
}
