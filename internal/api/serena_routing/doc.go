// Package serena_routing implements the path-aware + sticky-session routing
// primitives for the dynamic-pool serena MCP architecture described in
// docs/superpowers/specs/2026-05-20-serena-dynamic-pool.md and the v10
// implementation plan docs/superpowers/plans/2026-05-20-serena-supervisor-unified.md.
//
// Two concerns live here, each scoped to one type:
//
//   - WorkspaceResolver maps an inbound MCP tool argument (relative_path,
//     file_path, name_path, etc.) to a registered per-workspace serena
//     entry from the api.Registry. Absolute paths use an ancestor-walk to
//     .serena/project.yml; relative paths are tried against each
//     registered serena workspace in deterministic order.
//
//   - SessionRouter remembers which workspace a given MCP session id was
//     last bound to (per Mode 2 sticky-session semantics). Path-aware
//     calls bind the session; subsequent no-path calls look it up. TTL
//     cleanup removes stale entries after 24h.
//
// Both types are concurrency-safe. The package is fully isolated -- it
// imports api for WorkspaceEntry / Registry / SerenaLanguageSentinel only;
// no api -> serena_routing back-reference exists (callers wire WorkspaceResolver
// + SessionRouter explicitly).
package serena_routing
