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
//   - LOCAL-SHADOWS-PROJECT precedence (verified against the MCP docs: Local
//     scope > Project scope, matched BY NAME, entire-entry — no merge). A
//     claude-code .mcp.json (Project-scope) entry whose Name also appears in the
//     project's LOCAL-scope mcpServers set is SHADOWED: Claude loads the Local
//     definition, not this .mcp.json one, so the .mcp.json approval is MOOT. Such
//     an entry gets ScanEntry.ProjectShadowedByLocal=true and ProjectEnabled left
//     nil (the approval reconciliation does not apply to a shadowed entry). The
//     Local definition itself is already surfaced once in
//     ProjectScope.LocalServers.
//   - every NON-shadowed claude-code .mcp.json entry gets ScanEntry.
//     ProjectEnabled set to the OPT-IN approval reconciliation result (approved
//     only when enableAll, or explicitly enabled, and not disabled —
//     clients.ClaudeLocalScope.IsMcpjsonServerEnabled). A claude-code entry is
//     identified by a "claude-code" key in its ClientPresence — the same set
//     P2a's ProjectScanConfigPaths feeds the scanner. Non-claude-code entries
//     (cursor/vscode) are left with ProjectEnabled nil (the rule is
//     claude-specific).
//
// `root` is the SAME validated project root the project scan ran against. A
// missing ~/.claude.json, missing projects map, or no matching project key all
// leave result untouched except for being a no-op (ProjectScope stays nil,
// ProjectEnabled stays nil) — the normal "no local-scope config" case. A
// genuine read/parse error of ~/.claude.json is returned so the handler can
// surface it; the partial scan result is NOT discarded by this function.
//
// The symlink-follow policy for the ~/.claude.json read is the SINGLE-OWNER
// predicate OperatorAllowsClientConfigSymlink (the canonical api owner the
// client-config presence gate uses, honoring both the env vars AND the persisted
// supervisor-intent strict_mode bit); it is computed here and injected into the
// clients-package reader, so the presence gate and the local reader can never
// diverge on the policy.
func EnrichProjectClaudeLocalScope(result *ScanResult, root string) error {
	if result == nil {
		return nil
	}

	allow := OperatorAllowsClientConfigSymlink()
	local, err := clients.ReadClaudeLocalScope(root, allow)
	if err != nil {
		return err
	}

	if local.Matched {
		// Surface the LOCAL scope as a distinct set + the verbatim toggle arrays.
		result.ProjectScope = &ProjectScopeInfo{
			LocalServers:               local.LocalServers,
			DisabledMcpjsonServers:     local.Disabled,
			EnabledMcpjsonServers:      local.Enabled,
			EnableAllProjectMcpServers: local.EnableAll,
		}
	}

	// Build the LOCAL-scope name set for the shadow check (Local > Project by
	// name). Empty when there is no matching local record.
	localNames := make(map[string]struct{}, len(local.LocalServers))
	for _, n := range local.LocalServers {
		localNames[n] = struct{}{}
	}

	// Reconcile every claude-code .mcp.json (Project-scope) entry. The OPT-IN
	// approval rule is applied even when local.Matched is false: with no approve
	// list and no enableAll, the result is "not approved" for every server, which
	// is the correct default — a project with a .mcp.json but no ~/.claude.json
	// approval record has all its .mcp.json servers PENDING the trust prompt.
	// (We only touch entries that actually have a claude-code presence, so
	// cursor/vscode-only servers stay nil.)
	for i := range result.Entries {
		if _, isClaude := result.Entries[i].ClientPresence["claude-code"]; !isClaude {
			continue
		}
		if _, shadowed := localNames[result.Entries[i].Name]; shadowed {
			// Local scope wins by name: Claude loads the Local definition, not
			// this .mcp.json one. The .mcp.json approval is moot → leave
			// ProjectEnabled nil and flag the shadow.
			t := true
			result.Entries[i].ProjectShadowedByLocal = &t
			continue
		}
		enabled := local.IsMcpjsonServerEnabled(result.Entries[i].Name)
		result.Entries[i].ProjectEnabled = &enabled
	}

	return nil
}
