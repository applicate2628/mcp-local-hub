// internal/cli/mcp_front_port.go
//
// Sub-increment 2a of the MCP front-daemon decision
// (work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md):
// the single place internal/cli resolves the mcp_front.port setting for the
// two production call sites that need it — ensureBuiltinRouteDaemonAtStartup
// (internal/cli/supervise.go, the supervisor's own seeded --port argv) and
// newRouteCmd's RunE (internal/cli/route.go, the manual-invocation fallback
// when --port is omitted). Both fall back to DefaultRouteDaemonPort on any
// settings-read/parse/range failure — a corrupt gui-preferences.yaml must
// not stop the route daemon from having SOME port to bind.
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
