// internal/gui/route_readonly_audit.go
//
// P1-1 fix (adversarial cross-family review of Increment 1, mcphub-front-daemon):
// the standalone `mcphub route` front daemon (internal/cli/route.go) is
// documented as READ-ONLY on the registry + supervisor-intent, but both
// serena_router.go and lsp_router.go defaulted their diagnostic emit sites to
// api.LogHubMcpEvent whenever a registered workspace's backend proved
// unreachable (timeout, connection refusal, non-2xx notification forward).
// api.LogHubMcpEvent creates and appends to the SHARED <state-dir>/hub-mcp.log
// (+ its hub-mcp.log.lock sidecar) — a state file the GUI process owns. A
// second, GUI-independent process writing that file is a shared-state WRITE
// exactly as surely as a registry write would be, falsifying the read-only
// invariant the decision record requires.
//
// routeReadOnlySink is the route daemon's OWN diagnostic sink: it never
// touches hub-mcp.log or any other shared state file. It writes one
// redacted, structured JSON line to this process's own stderr — which the
// supervisor already captures per-daemon, independent of the GUI's
// hub-mcp.log (see CLAUDE.md's stderr-sink notes). SetSerenaRouterReadOnly
// and SetLSPRouterReadOnly wire this as their AuditFn; every other
// (production/GUI) construction path is completely unaffected — this file
// adds no new behavior to SetSerenaRouterProduction/SetLSPRouterProduction,
// which continue to default to api.LogHubMcpEvent exactly as before.
//
// The implementation moved to api.RouteReadOnlyStderrSink (finding 1,
// work-items/bugs/2026-07-26-route-daemon-state-read-unhardened-parent-
// fallback-writes-hub-mcp-log.md): internal/api/serena_routing and
// internal/api/lsp_routing need the SAME sink for the shared inode-anchored
// state-file reader's relax-fallback diagnostic, and neither may import this
// gui package (gui already imports both of them, so the reverse import
// would cycle). This unexported wrapper is kept so every existing reference
// in this package (serena_router.go, lsp_router.go) needs no change.
package gui

import (
	"mcp-local-hub/internal/api"
)

// routeReadOnlySink implements the AuditFn seam
// (func(level, event string, fields map[string]any) error) for the
// GUI-independent, registry/supervisor-intent read-only route daemon.
func routeReadOnlySink(level, event string, fields map[string]any) error {
	return api.RouteReadOnlyStderrSink(level, event, fields)
}
