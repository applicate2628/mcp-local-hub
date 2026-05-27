// Package api - Phase D.2 supervisor-intent build helpers.
//
// D.2 introduces a per-workspace fan-out for serena-style dynamic-pool
// manifests: given a kind: workspace-scoped manifest with a non-nil
// DaemonTemplate, and the current workspace registry, materialize one
// SupervisorDaemon descriptor per registered serena workspace.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"mcp-local-hub/internal/config"
)

// SerenaTaskNamePrefix is the canonical leading-backslash prefix for
// every serena dynamic-pool task name. The 8-hex workspace key is
// appended verbatim to produce a "\mcp-local-hub-serena-<wsKey>"
// supervisor-intent task name.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
const SerenaTaskNamePrefix = `\mcp-local-hub-serena-`

// SerenaTaskNameForWorkspace returns the canonical leading-backslash
// task name for a per-workspace serena daemon. The 8-hex-chars suffix
// comes from WorkspaceKey(workspacePath) which is the same SHA-256[:8]
// hex hash used everywhere else in the codebase for workspace
// identification. See internal/api/workspace_path.go:167.
//
// Hash is deterministic - same canonical path always yields the same
// task name. Collisions inside one user's registry are not a concern:
// birthday bound for 8-hex (32-bit) collisions sits at ~77k workspaces
// for 50% probability and real users carry <100.
//
// Callers MUST pass a CANONICAL absolute path.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
func SerenaTaskNameForWorkspace(workspacePath string) string {
	return SerenaTaskNamePrefix + WorkspaceKey(workspacePath)
}

// IsSerenaTaskName reports whether taskName looks like the canonical
// supervisor-intent form for a serena per-workspace daemon. Accepts
// both the bare (no leading backslash) and canonical (leading
// backslash) forms. The suffix MUST be exactly 8 lowercase hex chars
// (the WorkspaceKey shape) - any other suffix shape returns false.
//
// The 8-hex requirement is intentional and load-bearing: D.3 will use
// this predicate to classify supervisor-intent.json entries as
// "serena, owned by registry" vs "serena orphan, prune candidate" vs
// "non-serena, leave alone". A permissive predicate that accepted
// "\mcp-local-hub-serena-foo" would mis-classify hand-edited
// descriptors.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
func IsSerenaTaskName(taskName string) bool {
	const bareSerenaTaskNamePrefix = "mcp-local-hub-serena-"
	suffix := ""
	switch {
	case strings.HasPrefix(taskName, SerenaTaskNamePrefix):
		suffix = strings.TrimPrefix(taskName, SerenaTaskNamePrefix)
	case strings.HasPrefix(taskName, bareSerenaTaskNamePrefix):
		suffix = strings.TrimPrefix(taskName, bareSerenaTaskNamePrefix)
	default:
		return false
	}
	if len(suffix) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		c := suffix[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// BuildSupervisorDaemonsForSerena materializes one SupervisorDaemon
// descriptor per registered serena workspace, given a parsed manifest
// with a non-nil DaemonTemplate and the snapshot of serena rows from
// the workspace registry.
//
// Inputs:
//
//   - m            - parsed ServerManifest. MUST satisfy
//     m.Kind == config.KindWorkspaceScoped AND m.DaemonTemplate != nil.
//     If either gate fails the function returns nil.
//   - workspaces   - registry snapshot returned by
//     (*Registry).SerenaEntries(). Empty or nil -> nil result (NOT
//     an error; an empty pool is a valid steady state).
//   - manifestHash - opaque content hash of the parsed manifest.
//     Empty string is fine for unit tests.
//
// Outputs:
//
//   - one SupervisorDaemon per entry in workspaces, in input order.
//
// Behavior:
//
//   - TaskName follows the canonical form (leading backslash +
//     mcp-local-hub-serena- + 8-hex-chars-of-WorkspaceKey).
//   - Server is m.Name.
//   - Daemon is m.DaemonTemplate.Context.
//   - Command is m.Command verbatim.
//   - Args is the concatenation m.BaseArgs ++ ExtraArgsTemplate
//     followed by token expansion. Both halves are expanded.
//   - Env is a CLONE of m.Env (each descriptor owns its own map).
//   - Env values are passed verbatim. Secret-placeholder expansion
//     (`secret:KEY` references resolved against the vault) is the
//     caller's responsibility - this helper is pure and does NOT
//     consult any vault or runtime state.
//   - Workspace is the canonical absolute path from the registry row.
//   - Port is the per-workspace port persisted on the registry row.
//     The fan-out does NOT re-allocate.
//   - ManifestHash is the supplied manifestHash.
//
// Determinism: same inputs -> same outputs. Order matches the input
// workspaces slice.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md D.2.
func BuildSupervisorDaemonsForSerena(
	m *config.ServerManifest,
	workspaces []WorkspaceEntry,
	manifestHash string,
) []SupervisorDaemon {
	if m == nil || m.DaemonTemplate == nil {
		return nil
	}
	if m.Kind != config.KindWorkspaceScoped {
		return nil
	}
	if len(workspaces) == 0 {
		return nil
	}

	templateArgs := make([]string, 0, len(m.BaseArgs)+len(m.DaemonTemplate.ExtraArgsTemplate))
	templateArgs = append(templateArgs, m.BaseArgs...)
	templateArgs = append(templateArgs, m.DaemonTemplate.ExtraArgsTemplate...)

	out := make([]SupervisorDaemon, 0, len(workspaces))
	for _, ws := range workspaces {
		if ws.Language != SerenaLanguageSentinel {
			continue
		}
		if ws.WorkspacePath == "" {
			continue
		}

		expanded := config.ExpandWorkspacePathTokens(templateArgs, ws.WorkspacePath)

		out = append(out, SupervisorDaemon{
			TaskName:     SerenaTaskNameForWorkspace(ws.WorkspacePath),
			Server:       m.Name,
			Daemon:       m.DaemonTemplate.Context,
			Command:      m.Command,
			Args:         expanded,
			Env:          cloneStringMap(m.Env),
			Workspace:    ws.WorkspacePath,
			Port:         ws.Port,
			ManifestHash: manifestHash,
		})
	}
	return out
}

// cloneStringMap returns a shallow copy of m, or nil when m is nil.
// Used by BuildSupervisorDaemonsForSerena so each per-workspace
// descriptor owns an independent env map.
func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// hashWorkspacePathForTest exposes the suffix-only hash helper for the
// fan-out tests so a deterministic vs. collision assertion can compare
// against the same byte sequence WorkspaceKey produces. Internal, NOT
// part of the public API surface.
func hashWorkspacePathForTest(path string) string {
	sum := sha256.Sum256([]byte(path))
	return hex.EncodeToString(sum[:])[:8]
}
