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
// The supervisor startup seeder does NOT use this mutable setting directly.
// It obtains the typed, leased routing-epoch projection from API so a stable
// or recovering front keeps its admitted port through descriptor persistence.
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
