// route_readonly_sink.go — the read-only `mcphub route` front daemon's own
// diagnostic sink, owned here (internal/api) rather than internal/gui so it
// is reachable from every layer that needs it without a layering violation:
//
//   - internal/gui/serena_router.go + lsp_router.go wire it as their
//     AuditFn (P1-1 fix, adversarial cross-family review of Increment 1) —
//     gui already imports this package, so that direction is fine.
//   - internal/api/serena_routing and internal/api/lsp_routing (via
//     Registry.SetAuditSink / LSPWorkspaceRootTrustedWithAuditSink) need the
//     SAME sink for the shared inode-anchored state-file reader's
//     relax-fallback diagnostic (finding 1,
//     work-items/bugs/2026-07-26-route-daemon-state-read-unhardened-parent-
//     fallback-writes-hub-mcp-log.md). Neither of those packages may import
//     internal/gui (gui already imports api/serena_routing and
//     api/lsp_routing, so the reverse import would be a cycle) — putting the
//     ONE implementation at this shared, lower layer keeps a single owner
//     instead of forking the same "write a redacted JSON line to my own
//     stderr" logic into gui a second time.
//
// internal/gui/route_readonly_audit.go keeps its own (unexported,
// same-named) routeReadOnlySink wrapper that delegates here, so every
// existing gui-package reference stays untouched.
package api

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// RouteReadOnlyStderrSink is the standalone, GUI-independent `mcphub route`
// front daemon's own diagnostic sink. It never touches hub-mcp.log or any
// other shared state file: it writes one redacted, structured JSON line to
// THIS process's own stderr, which the supervisor already captures
// per-daemon, independent of the GUI's hub-mcp.log.
//
// Mirrors LogHubMcpEvent's envelope shape (ts/level/event merge order,
// RedactToken scrubbing) so an operator sees a familiar structure.
func RouteReadOnlyStderrSink(level, event string, fields map[string]any) error {
	if level == "" {
		level = "info"
	}
	rec := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		rec[k] = v
	}
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["level"] = level
	rec["event"] = event
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal route diagnostic event: %w", err)
	}
	redacted := RedactToken(string(raw))
	_, err = fmt.Fprintln(os.Stderr, redacted)
	return err
}
