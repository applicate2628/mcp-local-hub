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
package gui

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"mcp-local-hub/internal/api"
)

// routeReadOnlySink implements the AuditFn seam
// (func(level, event string, fields map[string]any) error) for the
// GUI-independent, registry/supervisor-intent read-only route daemon. It
// mirrors api.LogHubMcpEvent's envelope shape (ts/level/event merge order,
// RedactToken scrubbing) so an operator sees a familiar structure, but routes
// the line to this process's own stderr instead of the shared hub-mcp.log —
// the route daemon has no shared-state write surface at all, so its own
// diagnostics must never land on one.
func routeReadOnlySink(level, event string, fields map[string]any) error {
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
	redacted := api.RedactToken(string(raw))
	_, err = fmt.Fprintln(os.Stderr, redacted)
	return err
}
