// hub_mcp_log_redact.go — Phase 2 Task 2.1 (G4 unified hub MCP).
//
// RedactToken is the single redaction choke-point for every emit
// surface that may carry a hub-mcp credential: hub-mcp.log writes,
// `mcphub hub-mcp status` stdout + stderr, `mcphub install` stdout +
// stderr (token-bearing args can appear in error messages), `mcphub
// hub-mcp regenerate-token` stdout + stderr, syscall error wrappers
// (path arg may contain a basename of a token-bearing file), and argv
// echoes in command-not-found / unknown-flag paths.
//
// The helper is intentionally tiny so callers cannot bypass it by
// composing strings; the structured-emit helpers in hub_mcp_log.go
// (Task 2.5) route every event through this function. Phase 4 + 5
// handlers and CLIs inherit the same contract.
//
// Spec: docs/superpowers/specs/2026-05-12-g4-unified-hub-mcp-design-v3.md
// §"Logging hygiene + golden test" (F-S2 closure).
// Plan: docs/superpowers/plans/2026-05-12-g4-unified-hub-mcp.md Task 2.1.

package api

import "regexp"

// hexTokenRE matches the 64-lower-hex form of hub-mcp credentials.
// Both per-client tokens AND the hub instance_id are produced by the
// same `crypto/rand` 32-byte → lower-hex pipeline (see hub_mcp_tokens.go
// + hub_mcp_instance.go), so a single regex covers both surfaces.
//
// Pattern is case-insensitive: `[0-9A-Fa-f]{64}` (codex bot r3 P1
// closure). Defense in depth — even though our writers always emit
// lowercase, user input normalization, shell tooling, or an upstream
// formatter could uppercase the token before it reaches this
// sanitizer. An uppercase 64-hex leak is equivalent to leaking the
// secret (trivially lowercased).
//
// Anchored to word boundaries via a surrounding negative lookbehind
// would be ideal, but RE2 (Go's stdlib regexp engine) does NOT support
// lookbehind. The trade-off: any 64-hex run in the input (either case)
// is treated as a credential and redacted. The golden test in
// hub_mcp_log_redaction_test.go (Task 2.5) exercises every documented
// emit surface and would catch a false negative.
var hexTokenRE = regexp.MustCompile(`[0-9A-Fa-f]{64}`)

// RedactToken replaces every 64-lower-hex substring with `<token>`.
// Apply at every emit site that may carry a token or instance_id:
//
//   - hub-mcp.log writes (handled by LogHubMcpEvent in Task 2.5).
//   - `mcphub hub-mcp status` stdout + stderr (Phase 5 CLI).
//   - `mcphub install` stdout + stderr (Phase 5 reconciler error path).
//   - `mcphub hub-mcp regenerate-token` stdout + stderr (Phase 5).
//   - Syscall error wrappers — path args may include token-bearing
//     basenames (handled by wrapHubMcpFileError in Task 2.5).
//   - argv echoes in command-not-found / unknown-flag paths (handled
//     by redactArgvForLog in Task 2.5).
//
// Callers compose log lines from arbitrary inputs; routing every
// emission through this helper guarantees no plain-token bytes reach
// disk or terminal.
//
// Spec: §"Logging hygiene + golden test".
func RedactToken(s string) string {
	return hexTokenRE.ReplaceAllString(s, "<token>")
}
