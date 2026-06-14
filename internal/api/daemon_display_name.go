package api

import "strings"

// ComputeDaemonDisplayName derives a human-readable label for a daemon row
// from its already-resolved fields. It is DISPLAY-ONLY: the returned string
// never feeds routing, task-name parsing, registry lookups, or any identity
// decision — task_name remains the canonical key. CLI/GUI render this when
// non-empty and fall back to the plain task/server name otherwise.
//
// The single owner of the hash→name presentation rule. Both row-builders —
// enrichStatusWithRegistry (the scheduler-scan / StatusWithOpts path) and
// decodeSupervisorIPCStatusResult (the default supervisor-IPC path) — call
// this so the two surfaces never drift.
//
// Mapping (workspace is the canonical workspace ROOT path the supervisor /
// registry already carries for the row; the 8-hex task-name hash is NOT
// reversed — the literal path is used directly):
//
//	workspace-scoped serena  → "serena · <workspace-basename>"
//	   task: mcp-local-hub-serena-<8hex>; server=="serena"; workspace!=""
//	   e.g. (\mcp-local-hub-serena-6935d24c, server=serena, ws=d:\dev\VFEM)
//	        → "serena · VFEM"
//
//	workspace-scoped LSP     → "<language> @ <workspace-basename>"
//	   task: mcp-local-hub-lsp-<8hex>-<language>; workspace!=""
//	   e.g. (\mcp-local-hub-lsp-b133f336-go, ws=d:\dev\mcp-local-hub)
//	        → "go @ mcp-local-hub"
//
//	everything else (global daemons memory/time/wolfram/…, maintenance
//	rows, any row whose workspace is unknown) → "" (caller keeps the
//	plain name).
//
// Returning "" rather than a bare-hash strip for the unresolved-workspace
// case is deliberate: a name without the project it serves is no clearer than
// the task name the operator already sees, so we leave the plain name intact.
func ComputeDaemonDisplayName(taskName, server, daemon, workspace string) string {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		// No workspace root to render. Global daemons and any row whose
		// workspace the producer could not resolve keep their plain name.
		return ""
	}
	proj := basenameAcrossSeparators(ws)
	if proj == "" {
		return ""
	}

	// LSP lazy-proxy row: language comes from the canonical task-name parse,
	// not from a registry field (which can be empty when enrichment is
	// skipped, e.g. on the IPC path).
	if _, lang, ok := parseLazyProxyTaskName(taskName); ok && lang != "" {
		return lang + " @ " + proj
	}

	// Workspace-scoped serena row. The legacy/global serena daemon
	// (mcp-local-hub-serena-claude) and the serena weekly-refresh row both
	// carry an empty workspace, so the ws!="" gate above already excludes
	// them; the server=="serena" check keeps the mapping intentional rather
	// than matching any future server that happens to be workspace-scoped.
	if strings.TrimSpace(server) == "serena" {
		return "serena · " + proj
	}

	return ""
}
