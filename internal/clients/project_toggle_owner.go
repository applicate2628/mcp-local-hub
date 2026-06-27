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

import (
	"fmt"
	"os"
	"path/filepath"
)

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
//     client that (a) has a project-local object-member scope in the ProjectScope
//     registry AND (b) is NOT approval-array-gated (cursor, vscode — NOT
//     claude-code). An unknown client is OwnerUnsupported — the classifier never
//     invents a path for a client the registry does not cover.
//
//     claude-code is DELIBERATELY EXCLUDED from the object-member arm even though
//     it IS in the registry (it stays there for the SCAN/read path, which reads
//     .mcp.json). claude-code's Project-row DISABLE must go through
//     ScopeClaudeLocalMembership (the non-destructive ~/.claude.json approval
//     array-move), NEVER object-member: an object-member disable RFC-6902-removes
//     /mcpServers/<server> from the checked-in, SHARED .mcp.json — data-loss that
//     destroys the server definition for every collaborator. That is the exact
//     destructive path the P3b frontend fix steered AWAY from (the GUI sends
//     claude-local-membership for the claude Project row, shipped #434); narrowing
//     the WRITE classifier here makes the destructive path unreachable for a
//     direct API caller or a future frontend regression, not just the current GUI.
//     cursor/vscode have NO approval array, so object member add/remove IS their
//     correct enable/disable semantic and they KEEP OwnerProjectObjectMember.
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
		// An approval-array-gated client (claude-code) MUST use the
		// ScopeClaudeLocalMembership array-move, never object-member (which would
		// member-delete the shared checked-in definition). Refuse it here so the
		// destructive path is unreachable at the single-owner classifier.
		if clientUsesApprovalArrayToggle(client) {
			return OwnerUnsupported
		}
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

// clientUsesApprovalArrayToggle reports whether `client` disables a Project-scope
// server by MOVING its name in an approval array (the ~/.claude.json
// enabled/disabledMcpjsonServers lists — ScopeClaudeLocalMembership) instead of
// removing the object member from the checked-in config. Only claude-code has
// that approval-array substrate.
//
// It is the predicate the object-member arm of ProjectToggleOwner consults to
// EXCLUDE such a client: an object-member disable RFC-6902-removes the server
// from the SHARED .mcp.json definition (data-loss across collaborators), so an
// approval-array client must route through the array-move owner. cursor/vscode
// return false — they have no approval array, so object member add/remove IS
// their correct disable semantic.
func clientUsesApprovalArrayToggle(client string) bool {
	return client == claudeCodeClientID
}

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
//
// SERIALIZATION + PARENT-CREATE (bot PR #433 findings 3 + 4): the read-modify-
// write is wrapped in withConfigLock — the SAME per-path in-process mutex +
// cross-process advisory flock the adapter decorator (lockingClient) wraps every
// mutating Client method in. Two concurrent /api/projects/toggle on the SAME
// project config no longer torn/lost-update (each reading the same baseline,
// the later write clobbering the earlier). withConfigLock ALSO creates the
// write-target PARENT DIR via the hardened SecureCreateParentDir
// (symlink-/reparse-point-refusing, owner-only, DACL-before-bytes) BEFORE the
// flock — so an ENABLE into a first-time project whose `.cursor`/`.vscode` parent
// does not exist yet succeeds (the production SecureWriteClientConfig opens the
// IMMEDIATE PARENT handle-relative and does NOT create it). NOT a raw os.MkdirAll
// — the same hardened parent-create the global write path uses (config_lock.go).
//
// DISABLE-into-a-missing-parent is a pure no-op: with no config dir there is no
// member to remove and nothing to serialize against (a writer needs the dir to
// exist), so we return nil WITHOUT taking the lock or creating the dir — mirrors
// withConfigReadLock's missing-parent fast path and avoids leaving a stray empty
// `.cursor/` directory behind on a disable. (Finding 4: parent-create is an
// ENABLE-only need.)
func ToggleProjectObjectMember(client, configPath, server string, value any, enable bool) error {
	ps := projectScopeForClient(client)
	if ps == nil {
		return fmt.Errorf("client %q has no project-local object-member config scope", client)
	}
	if !enable {
		// Disable: removing an absent member is a no-op. If the write-target
		// parent dir does not exist, the member cannot exist either — return
		// nil WITHOUT acquiring the lock (the flock file lives in that dir) or
		// creating the dir, so a disable never leaves a stray empty config dir.
		if _, statErr := os.Stat(filepath.Dir(configPath)); statErr != nil && os.IsNotExist(statErr) {
			return nil
		}
	}
	// Serialize the RMW (and, on enable, the hardened parent-create) under the
	// SAME owner the adapter decorator uses for every mutating Client method.
	return withConfigLock(configPath, func() error {
		return mutateJSONObjectMemberPath(configPath, []string{ps.SectionKey}, server, value, !enable)
	})
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
