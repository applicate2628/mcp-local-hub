// project_toggle_owner.go — per-project-GUI Phase 3a.
//
// ProjectToggleOwner is the SINGLE classifier that maps a per-project toggle's
// (client, scope) to its WRITE owner (design decision 5: "ONE classifier
// dispatches; the GUI never re-derives ownership"). It is a PURE mapping — it
// performs no I/O and imports no api/gui package — so it can live in the
// clients package alongside the per-client project-scope registry it shares a
// vocabulary with, while the actual invocation of the resolved owner happens in
// the gui composition root (which can import both api and clients). Keeping the
// ownership DERIVATION here and the owner INVOCATION at the top means the GUI
// switches on a resolved kind instead of re-deriving "which writer handles
// cursor's project config" inline.
package clients

import "fmt"

// ProjectToggleScope is the per-project substrate a toggle targets. It is the
// disambiguator the classifier needs ON TOP OF the client id, because a single
// client (claude-code) has TWO distinct project substrates (the checked-in
// .mcp.json Project scope and the private ~/.claude.json Local scope) toggled
// by DIFFERENT owners (decision 5 / §10.2). Non-claude clients use only the
// object-member scopes; A and C use their own scope constants.
type ProjectToggleScope string

const (
	// ScopeWorkspaceLSP — Model A: a workspace-scoped LSP tool registration.
	// enable=register / disable=unregister the workspace daemon.
	ScopeWorkspaceLSP ProjectToggleScope = "workspace-lsp"

	// ScopeProjectObjectMember — Model B: add/remove an object member in a
	// project-local JSON config (cursor .cursor/mcp.json, vscode .vscode/mcp.json,
	// claude-code .mcp.json Project scope). The substrate IS the gate.
	ScopeProjectObjectMember ProjectToggleScope = "project-object-member"

	// ScopeClaudeLocalMembership — Model B-claude Local: MOVE the name between
	// ~/.claude.json projects.<key>.{enabled,disabled}McpjsonServers (NEVER
	// delete from mcpServers — decision 5).
	ScopeClaudeLocalMembership ProjectToggleScope = "claude-local-membership"

	// ScopeGroupServers — Model C: add/remove from a group's servers list in
	// groups.yaml.
	ScopeGroupServers ProjectToggleScope = "group-servers"
)

// ProjectToggleOwnerKind is the resolved write owner for a (client, scope)
// toggle — the enum the gui dispatcher switches on to invoke the correct
// backend (api.Register/Unregister, mutateJSONObjectMemberPath,
// ToggleClaudeMcpjsonMembership, or api.ReadModifyWriteGroups).
type ProjectToggleOwnerKind int

const (
	// OwnerUnsupported — no write owner for this (client, scope). The handler
	// must refuse (no write) rather than guess.
	OwnerUnsupported ProjectToggleOwnerKind = iota
	// OwnerWorkspaceRegister — api.Register/Unregister (Model A).
	OwnerWorkspaceRegister
	// OwnerProjectObjectMember — mutateJSONObjectMemberPath at the project
	// config path (Model B; cursor/vscode/claude-Project).
	OwnerProjectObjectMember
	// OwnerClaudeLocalMembership — ToggleClaudeMcpjsonMembership (Model B-claude
	// Local array-move).
	OwnerClaudeLocalMembership
	// OwnerGroupServers — api.ReadModifyWriteGroups (Model C).
	OwnerGroupServers
)

// ProjectToggleOwner resolves the single write owner for a per-project toggle.
//
//   - scope ScopeWorkspaceLSP        → OwnerWorkspaceRegister (client ignored;
//     A is per-workspace, not per-client-config).
//   - scope ScopeProjectObjectMember → OwnerProjectObjectMember, but ONLY for a
//     client that actually has a project-local object-member scope in the
//     ProjectScope registry (cursor, vscode, claude-code). An unknown client is
//     OwnerUnsupported — the classifier never invents a path for a client the
//     registry does not cover.
//   - scope ScopeClaudeLocalMembership → OwnerClaudeLocalMembership, ONLY for
//     client "claude-code" (the Local substrate is claude-specific). Any other
//     client is OwnerUnsupported.
//   - scope ScopeGroupServers        → OwnerGroupServers (client ignored; the
//     "client" of a group toggle is the group name, carried separately).
//   - any other scope                → OwnerUnsupported.
//
// It is the ONE place ownership is derived, so the GUI cannot branch on a client
// name inline and drift from this mapping.
func ProjectToggleOwner(client string, scope ProjectToggleScope) ProjectToggleOwnerKind {
	switch scope {
	case ScopeWorkspaceLSP:
		return OwnerWorkspaceRegister
	case ScopeProjectObjectMember:
		if projectScopeForClient(client) != nil {
			return OwnerProjectObjectMember
		}
		return OwnerUnsupported
	case ScopeClaudeLocalMembership:
		if client == claudeCodeClientID {
			return OwnerClaudeLocalMembership
		}
		return OwnerUnsupported
	case ScopeGroupServers:
		return OwnerGroupServers
	default:
		return OwnerUnsupported
	}
}

// claudeCodeClientID is the registry client id for claude-code. Single owner so
// the classifier and any consumer reference one literal (it matches the
// projectScopeRegistry row in project_scope.go).
const claudeCodeClientID = "claude-code"

// projectScopeForClient returns the ProjectScope registry row for client, or nil
// when the client has no project-local object-member scope. It is the lookup the
// classifier and the object-member writer share so the set of object-member
// clients has one owner (the projectScopeRegistry).
func projectScopeForClient(client string) *ProjectScope {
	for i := range projectScopeRegistry {
		if projectScopeRegistry[i].Supported && projectScopeRegistry[i].Client == client {
			return &projectScopeRegistry[i]
		}
	}
	return nil
}

// ToggleProjectObjectMember is the Model B leaf writer: it adds (enable) or
// removes (disable) the `server` object member under the project-local config
// file for `client` at the given absolute config path, through the SAME
// comment-preserving hujson + WriteConfigFile → SecureWriteClientConfig pipeline
// the global adapter writes use (mutateJSONObjectMemberPath). The section key
// (mcpServers for cursor/claude, servers for vscode) is resolved from the
// ProjectScope registry — never re-derived — so the writer can never write the
// wrong top-level key.
//
// configPath MUST be the registry-resolved, path-safety-validated project config
// path (from ProjectScanConfigPaths, which returns realRoot-contained, fixed-
// RelFile-joined, symlink-checked paths). This writer does NOT re-validate the
// path — that is the caller's contract (the gui handler resolves it via
// ProjectScanConfigPaths before dispatch); it only owns the byte mutation.
//
// On enable, `value` is the object-member value to set (the same shape the
// global scan/adapters use for this client). On disable, value is ignored and
// the member is removed (idempotent: removing an absent member is a no-op).
func ToggleProjectObjectMember(client, configPath, server string, value any, enable bool) error {
	ps := projectScopeForClient(client)
	if ps == nil {
		return fmt.Errorf("client %q has no project-local object-member config scope", client)
	}
	return mutateJSONObjectMemberPath(configPath, []string{ps.SectionKey}, server, value, !enable)
}

// ProjectObjectMemberPresent is the Model-B read-back: it reports whether
// `server` is currently a member under client's section key in the project
// config file at configPath. It parses the same JSONC the writer preserved and
// looks up <sectionKey>.<server>. An absent file (the common no-config case)
// reports false with a nil error. It is the inverse-confirming read the toggle
// handler uses so the response reflects the PERSISTED state, not the request.
func ProjectObjectMemberPresent(client, configPath, server string) (bool, error) {
	ps := projectScopeForClient(client)
	if ps == nil {
		return false, fmt.Errorf("client %q has no project-local object-member config scope", client)
	}
	original, err := readRawConfig(configPath) // absent → (nil, nil)
	if err != nil {
		return false, err
	}
	if len(original) == 0 {
		return false, nil
	}
	m, err := parseJSONCBytes(original)
	if err != nil {
		return false, err
	}
	section, _ := m[ps.SectionKey].(map[string]any)
	if section == nil {
		return false, nil
	}
	_, present := section[server]
	return present, nil
}
