package cli

import (
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// TestPerServerInstalledSetFiltersNonServerTaskFamilies pins the
// codex bot phase5 r10 P2 closure on PR #160: --reconcile-hub-mode's
// "installed servers" set MUST exclude every non-per-server
// `mcp-local-hub-*` task family (watchdog, hub-wide / workspace-
// wide weekly-refresh, lazy-proxy LSP). Real per-server daemon
// tasks remain included.
func TestPerServerInstalledSetFiltersNonServerTaskFamilies(t *testing.T) {
	tasks := []scheduler.TaskStatus{
		// Singleton watchdog — must be filtered.
		{Name: `\mcp-local-hub-watchdog`},
		// Hub-wide weekly-refresh of all daemons — must be filtered.
		{Name: `\mcp-local-hub-weekly-refresh`},
		// Hub-wide workspace-scoped weekly-refresh
		// (api.WeeklyRefreshTaskName) — parseTaskName returns
		// ("workspace", "weekly-refresh"); must be filtered.
		{Name: `\mcp-local-hub-workspace-weekly-refresh`},
		// Workspace-scoped LSP lazy-proxy
		// (api.IsLazyProxyTaskName) — must be filtered. The
		// workspaceKey is exactly 8 lowercase hex chars per
		// parseLazyProxyTaskName (status_enrich.go).
		{Name: `\mcp-local-hub-lsp-deadbeef-go`},
		// Real per-server daemon — must be INCLUDED.
		{Name: `\mcp-local-hub-serena-default`},
		// Per-server weekly-refresh — must be INCLUDED (it's
		// still a signal that the server was installed).
		{Name: `\mcp-local-hub-memory-weekly-refresh`},
		// Hyphenated server name with a single-word daemon —
		// must be INCLUDED.
		{Name: `\mcp-local-hub-paper-search-mcp-default`},
	}
	got := perServerInstalledSet(tasks)
	want := map[string]bool{
		"serena":           true,
		"memory":           true,
		"paper-search-mcp": true,
	}
	if len(got) != len(want) {
		t.Errorf("got %d installed servers, want %d: %v vs %v", len(got), len(want), got, want)
	}
	for srv := range want {
		if !got[srv] {
			t.Errorf("missing expected installed server %q in %v", srv, got)
		}
	}
	for srv := range got {
		if !want[srv] {
			t.Errorf("unexpected entry %q in installed set (non-server task name leaked through filter)", srv)
		}
	}
}
