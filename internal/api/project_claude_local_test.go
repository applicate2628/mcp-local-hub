// internal/api/project_claude_local_test.go
//
// Per-project-GUI Phase 2b: EnrichProjectClaudeLocalScope unit tests (the
// in-package enrichment helper). The end-to-end HTTP-level behavior is covered
// by internal/gui/projects_p2b_test.go; these pin the helper's contract
// directly (nil safety, claude-only targeting, default-enabled when no local
// record).
//
// STATE-SAFETY: t.Setenv HOME + USERPROFILE to a temp dir + synthetic
// ~/.claude.json. The real ~/.claude.json is never touched (os.UserHomeDir()
// resolves to the temp dir under the redirected env).
package api

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func enrichSetHome(t *testing.T, claudeJSON string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if claudeJSON != "" {
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(claudeJSON), 0o600); err != nil {
			t.Fatalf("write claude.json: %v", err)
		}
	}
	return home
}

func enrichKey(root string) string {
	if runtime.GOOS != "windows" {
		return root
	}
	return strings.ReplaceAll(root, `\`, `/`)
}

func enrichRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return `C:\dev\enrichproj`
	}
	return "/dev/enrichproj"
}

// TestEnrichProjectClaudeLocalScope_NilResult: nil result is a safe no-op.
func TestEnrichProjectClaudeLocalScope_NilResult(t *testing.T) {
	enrichSetHome(t, "")
	if err := EnrichProjectClaudeLocalScope(nil, `C:\x`); err != nil {
		t.Errorf("nil result should be a no-op, got: %v", err)
	}
}

// TestEnrichProjectClaudeLocalScope_ClaudeOnlyTargeting: only entries with a
// claude-code presence get ProjectEnabled; others stay nil. ProjectScope is set
// from the matched local record.
func TestEnrichProjectClaudeLocalScope_ClaudeOnlyTargeting(t *testing.T) {
	root := enrichRoot(t)
	key := enrichKey(root)
	body := `{"projects":{"` + strings.ReplaceAll(key, `\`, `\\`) + `":{` +
		`"mcpServers":{"localX":{}},` +
		`"disabledMcpjsonServers":["claudeServer"]}}}`
	enrichSetHome(t, body)

	result := &ScanResult{
		Entries: []ScanEntry{
			{Name: "claudeServer", ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio"}}},
			{Name: "cursorServer", ClientPresence: map[string]ClientEntry{"cursor": {Transport: "http"}}},
		},
	}
	if err := EnrichProjectClaudeLocalScope(result, root); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	if result.ProjectScope == nil {
		t.Fatalf("ProjectScope nil; want matched local record")
	}
	if len(result.ProjectScope.LocalServers) != 1 || result.ProjectScope.LocalServers[0] != "localX" {
		t.Errorf("LocalServers = %v, want [localX]", result.ProjectScope.LocalServers)
	}

	// claudeServer is in disabledMcpjsonServers → ProjectEnabled=false.
	if result.Entries[0].ProjectEnabled == nil {
		t.Fatalf("claudeServer ProjectEnabled nil; want set")
	}
	if *result.Entries[0].ProjectEnabled {
		t.Errorf("claudeServer should be disabled (in disabledMcpjsonServers)")
	}
	// cursorServer has no claude-code presence → ProjectEnabled stays nil.
	if result.Entries[1].ProjectEnabled != nil {
		t.Errorf("cursorServer should NOT be reconciled, ProjectEnabled = %v", *result.Entries[1].ProjectEnabled)
	}
}

// TestEnrichProjectClaudeLocalScope_NoLocalRecord_DefaultNotApproved: no
// ~/.claude.json → ProjectScope nil, and (OPT-IN flip) a claude-code entry gets
// ProjectEnabled=false — with no approval record + no enableAll the .mcp.json
// server is PENDING the trust prompt, i.e. NOT approved.
func TestEnrichProjectClaudeLocalScope_NoLocalRecord_DefaultNotApproved(t *testing.T) {
	enrichSetHome(t, "") // no claude.json
	result := &ScanResult{
		Entries: []ScanEntry{
			{Name: "s", ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio"}}},
		},
	}
	if err := EnrichProjectClaudeLocalScope(result, enrichRoot(t)); err != nil {
		t.Fatalf("enrich: %v", err)
	}
	if result.ProjectScope != nil {
		t.Errorf("ProjectScope should be nil with no local record")
	}
	if result.Entries[0].ProjectEnabled == nil || *result.Entries[0].ProjectEnabled {
		t.Errorf("claude entry with no approval record should default ProjectEnabled=false (opt-IN)")
	}
	if result.Entries[0].ProjectShadowedByLocal != nil {
		t.Errorf("no local record → ProjectShadowedByLocal must stay nil")
	}
}

// TestEnrichProjectClaudeLocalScope_LocalShadowsProject: a .mcp.json
// (Project-scope) entry whose Name also appears in the LOCAL-scope mcpServers set
// is SHADOWED — ProjectShadowedByLocal=true AND ProjectEnabled stays nil (the
// .mcp.json approval is moot; Claude loads the Local definition). A
// non-shadowed claude entry still gets ProjectEnabled set by the opt-IN rule.
func TestEnrichProjectClaudeLocalScope_LocalShadowsProject(t *testing.T) {
	root := enrichRoot(t)
	key := enrichKey(root)
	// LOCAL scope defines "shared" (shadows the .mcp.json "shared") and approves
	// "approved" via enabledMcpjsonServers.
	body := `{"projects":{"` + strings.ReplaceAll(key, `\`, `\\`) + `":{` +
		`"mcpServers":{"shared":{}},` +
		`"enabledMcpjsonServers":["approved"]}}}`
	enrichSetHome(t, body)

	result := &ScanResult{
		Entries: []ScanEntry{
			{Name: "shared", ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio"}}},
			{Name: "approved", ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio"}}},
		},
	}
	if err := EnrichProjectClaudeLocalScope(result, root); err != nil {
		t.Fatalf("enrich: %v", err)
	}

	// "shared" is shadowed by the Local definition.
	if result.Entries[0].ProjectShadowedByLocal == nil || !*result.Entries[0].ProjectShadowedByLocal {
		t.Errorf("shared should be ProjectShadowedByLocal=true (Local scope wins by name)")
	}
	if result.Entries[0].ProjectEnabled != nil {
		t.Errorf("a shadowed entry must leave ProjectEnabled nil (approval moot), got %v", *result.Entries[0].ProjectEnabled)
	}
	// "approved" is NOT shadowed → opt-IN rule applies (explicitly enabled → true).
	if result.Entries[1].ProjectShadowedByLocal != nil {
		t.Errorf("approved is not in LocalServers → ProjectShadowedByLocal must stay nil")
	}
	if result.Entries[1].ProjectEnabled == nil || !*result.Entries[1].ProjectEnabled {
		t.Errorf("approved (in enabledMcpjsonServers) should be ProjectEnabled=true")
	}
}

// TestEnrichProjectClaudeLocalScope_PersistedStrictRefusesSymlink proves the
// Ruling-3 single-owner policy injection end-to-end (claim 6): with the
// MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK opt-in set BUT the persisted
// supervisor-intent strict_mode bit ON, OperatorAllowsClientConfigSymlink (the
// single owner) resolves to FALSE, so EnrichProjectClaudeLocalScope passes
// allowSymlink=false and the reader REFUSES the symlinked ~/.claude.json (empty
// scope, no shadow/enabled set). This is exactly the divergence Ruling 3 closes:
// the clients-local copy used to miss the persisted strict_mode bit. We drive
// strict_mode via the env var (OperatorRequiresSingleUserHome honors EITHER
// input) so the test needs no live supervisor-intent.json.
func TestEnrichProjectClaudeLocalScope_PersistedStrictRefusesSymlink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// Opt-in to symlinks BUT also strict — strict must override (the persisted
	// strict_mode bit is modeled here by the equivalent strict env input that the
	// single-owner predicate also honors).
	t.Setenv("MCPHUB_ALLOW_CLIENT_CONFIG_SYMLINK", "1")
	t.Setenv("MCPHUB_REQUIRE_SINGLE_USER_HOME", "1")

	root := enrichRoot(t)
	key := enrichKey(root)
	body := `{"projects":{"` + strings.ReplaceAll(key, `\`, `\\`) + `":{"mcpServers":{"linked":{}}}}}`
	target := filepath.Join(home, "real-claude.json")
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(home, ".claude.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this host (need privilege/Developer Mode): %v", err)
	}

	// Sanity: the single-owner predicate must compute false under strict.
	if OperatorAllowsClientConfigSymlink() {
		t.Fatalf("test precondition: OperatorAllowsClientConfigSymlink should be false under strict mode")
	}

	result := &ScanResult{
		Entries: []ScanEntry{
			{Name: "linked", ClientPresence: map[string]ClientEntry{"claude-code": {Transport: "stdio"}}},
		},
	}
	if err := EnrichProjectClaudeLocalScope(result, root); err != nil {
		t.Fatalf("enrich must not error on a refused symlink: %v", err)
	}
	if result.ProjectScope != nil {
		t.Errorf("strict mode must REFUSE the symlinked ~/.claude.json → ProjectScope nil, got %+v", result.ProjectScope)
	}
	// The entry got no local record → opt-IN default (not approved), not shadowed.
	if result.Entries[0].ProjectShadowedByLocal != nil {
		t.Errorf("refused symlink → no shadow, ProjectShadowedByLocal must stay nil")
	}
	if result.Entries[0].ProjectEnabled == nil || *result.Entries[0].ProjectEnabled {
		t.Errorf("refused symlink → opt-IN default ProjectEnabled=false")
	}
}

// TestEnrichProjectClaudeLocalScope_MalformedReturnsError: a malformed
// ~/.claude.json surfaces an error; the partial result is not discarded by the
// helper (the caller decides).
func TestEnrichProjectClaudeLocalScope_MalformedReturnsError(t *testing.T) {
	enrichSetHome(t, `{"projects":{`) // truncated
	result := &ScanResult{Entries: []ScanEntry{{Name: "s"}}}
	if err := EnrichProjectClaudeLocalScope(result, enrichRoot(t)); err == nil {
		t.Errorf("malformed ~/.claude.json should return an error")
	}
}
