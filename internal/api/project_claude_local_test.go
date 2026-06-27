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

// TestEnrichProjectClaudeLocalScope_NoLocalRecord_DefaultEnabled: no
// ~/.claude.json → ProjectScope nil, but claude-code entries still get
// ProjectEnabled=true (the default).
func TestEnrichProjectClaudeLocalScope_NoLocalRecord_DefaultEnabled(t *testing.T) {
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
	if result.Entries[0].ProjectEnabled == nil || !*result.Entries[0].ProjectEnabled {
		t.Errorf("claude entry with no local record should default ProjectEnabled=true")
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
