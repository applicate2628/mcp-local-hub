// internal/api/project_claude_local.go
//
// Per-project-GUI Phase 2b enrichment — claude-code LOCAL scope + .mcp.json
// enabled/disabled reconciliation.
//
// This is a POST-scan, ADDITIVE enrichment of a project ScanResult (the output
// of ScanFrom with a project-scoped ConfigPaths). It is called ONLY by the
// read-only project scan handler (GET /api/projects/scan); the global
// Servers-matrix scan never calls it, so scan.go and the global scan output
// stay byte-identical.
//
// READ-ONLY: it reads ~/.claude.json once (via clients.ReadClaudeLocalScope,
// os.ReadFile only) and mutates the IN-MEMORY ScanResult. It writes no file.
package api

import "mcp-local-hub/internal/clients"

// EnrichProjectClaudeLocalScope folds the claude-code LOCAL scope
// (~/.claude.json → projects.<root>) into a project ScanResult:
//
//   - ScanResult.ProjectScope gets the project's LOCAL-scope server set plus the
//     verbatim disabled/enabledMcpjsonServers toggle arrays (the distinct,
//     private-to-user substrate — separate from the .mcp.json Project scope).
//   - every claude-code .mcp.json (Project-scope) entry gets ScanEntry.
//     ProjectEnabled set to the reconciliation result (enabled unless disabled
//     and not re-enabled). A claude-code entry is identified by a "claude-code"
//     key in its ClientPresence — the same set P2a's ProjectScanConfigPaths
//     feeds the scanner. Non-claude-code entries (cursor/vscode) are left with
//     ProjectEnabled nil (the reconciliation rule is claude-specific).
//
// `root` is the SAME validated project root the project scan ran against. A
// missing ~/.claude.json, missing projects map, or no matching project key all
// leave result untouched except for being a no-op (ProjectScope stays nil,
// ProjectEnabled stays nil) — the normal "no local-scope config" case. A
// genuine read/parse error of ~/.claude.json is returned so the handler can
// surface it; the partial scan result is NOT discarded by this function.
func EnrichProjectClaudeLocalScope(result *ScanResult, root string) error {
	if result == nil {
		return nil
	}

	local, err := clients.ReadClaudeLocalScope(root)
	if err != nil {
		return err
	}

	if local.Matched {
		// Surface the LOCAL scope as a distinct set + the verbatim toggle arrays.
		result.ProjectScope = &ProjectScopeInfo{
			LocalServers:           local.LocalServers,
			DisabledMcpjsonServers: local.Disabled,
			EnabledMcpjsonServers:  local.Enabled,
		}
	}

	// Reconcile every claude-code .mcp.json (Project-scope) entry. The rule is
	// applied even when local.Matched is false: with empty toggle arrays the
	// result is "enabled" for every server, which is the correct default — a
	// project with a .mcp.json but no ~/.claude.json local record has all its
	// .mcp.json servers enabled. (We only set the flag for entries that actually
	// have a claude-code presence, so cursor/vscode-only servers stay nil.)
	for i := range result.Entries {
		if _, isClaude := result.Entries[i].ClientPresence["claude-code"]; !isClaude {
			continue
		}
		enabled := local.IsMcpjsonServerEnabled(result.Entries[i].Name)
		result.Entries[i].ProjectEnabled = &enabled
	}

	return nil
}
