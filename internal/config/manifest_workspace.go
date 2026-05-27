package config

import "strings"

// WorkspacePathToken is the literal token expanded inside a manifest's
// daemon_template.extra_args_template entries when the supervisor
// fan-out (Phase D.2) materializes one SupervisorDaemon per registered
// workspace. The validator (D.1) requires at least one args entry to
// contain this token; the fan-out (D.2) substitutes the registered
// workspace's canonical absolute path into every occurrence.
//
// Exported so both internal/config (validator) and internal/api
// (fan-out) reference the same literal — operators reading either
// surface see the same magic string.
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.1 + §D.2.
const WorkspacePathToken = "${workspace.path}"

// ExpandWorkspacePathTokens returns a fresh slice in which every
// occurrence of the literal substring "${workspace.path}" (see
// WorkspacePathToken) is replaced with workspacePath. The input slice
// is NOT mutated — Phase D.2 fan-out builds one expanded args slice
// per registered workspace, and the caller may reuse the manifest's
// template multiple times.
//
// Substring-match preserves composite tokens (e.g.
// "--project=${workspace.path}/src" expands to
// "--project=/abs/path/src"). Multiple occurrences inside one arg
// (e.g. "${workspace.path}/a:${workspace.path}/b") are all replaced.
// Args that do not contain the token are copied verbatim.
//
// nil input -> nil output; empty workspacePath is accepted (the
// caller is responsible for not feeding garbage in — the fan-out
// reads from `WorkspaceEntry.WorkspacePath` which is the canonical
// absolute path persisted by `mcphub workspace register`).
//
// Plan ref: docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md §D.2.
func ExpandWorkspacePathTokens(args []string, workspacePath string) []string {
	if args == nil {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, WorkspacePathToken, workspacePath)
	}
	return out
}
