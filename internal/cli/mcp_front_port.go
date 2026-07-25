// internal/cli/mcp_front_port.go
//
// Sub-increment 2a of the MCP front-daemon decision
// (work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md):
// the single place internal/cli resolves the mcp_front.port setting for the
// ONE production call site that wants the graceful fallback —
// newRouteCmd's RunE (internal/cli/route.go, the manual-invocation fallback
// when --port is omitted), a read-only/best-effort consumer per
// api.MCPFrontPortOrDefault's own doc comment. It falls back to
// DefaultRouteDaemonPort on any settings-read/parse/range failure — a
// corrupt gui-preferences.yaml must not stop a manually-invoked route daemon
// from having SOME port to bind.
//
// ensureBuiltinRouteDaemonAtStartup (internal/cli/supervise.go) does NOT use
// this — it is a WRITE path (it durably persists the resolved port into the
// reserved route-daemon row), so it uses resolveMCPFrontPortStrictFn below
// instead (P2-5 fix, adversarial cross-family review): a resolution failure
// there must propagate, not silently substitute the default, or the
// supervisor could canonicalize the row back onto a port no client was ever
// reconciled to.
package cli

import "mcp-local-hub/internal/api"

// resolveMCPFrontPortFn is a package-level test seam (mirrors
// reconcileSpawnFn / setBuiltinRouteSeedingDisabledForTest's style
// elsewhere in this package) so tests can pin a deterministic port without
// touching a real gui-preferences.yaml. Production default reads the
// mcp_front.port setting via api.NewAPI().MCPFrontPortOrDefault(), which
// itself falls back to api.DefaultMCPFrontPort on any read/parse/range
// failure.
var resolveMCPFrontPortFn = func() int {
	return api.NewAPI().MCPFrontPortOrDefault()
}

// resolveMCPFrontPortStrictFn is the STRICT (write-path) counterpart of
// resolveMCPFrontPortFn — a package-level test seam so tests can pin a
// deterministic port/error without touching real settings. Production wires
// it to api.NewAPI().ResolveMCPFrontPort(), which PROPAGATES a
// read/parse/range failure instead of silently substituting
// api.DefaultMCPFrontPort (P2-5 fix): ensureBuiltinRouteDaemonAtStartup is a
// WRITE path and must never canonicalize the reserved route-daemon row onto
// a port the operator did not actually configure.
var resolveMCPFrontPortStrictFn = func() (int, error) {
	return api.NewAPI().ResolveMCPFrontPort()
}
