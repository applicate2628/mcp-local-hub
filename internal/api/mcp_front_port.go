// internal/api/mcp_front_port.go
//
// Sub-increment 2a of the MCP front-daemon decision
// (work-items/decisions/2026-07-25-increment2-mcp-front-port-ownership.md):
// the single-owned settings key that names the client-facing port for the
// supervisor-managed `mcphub route` front daemon (internal/cli/route.go).
// The GUI keeps its own gui_server.port (default 9125, config.ReservedGUIPort)
// for the web UI — this is a SEPARATE, dedicated port so serena+LSP MCP
// traffic survives GUI death without needing the GUI's own port/lifecycle at
// all.
//
// This is the DORMANT half of Increment 2a: the setting + a resolver exist,
// but nothing in this file writes any client config. The operator-gated
// `mcphub install --reconcile-mcp-front` (internal/cli) is the only writer.
package api

import (
	"fmt"
	"strconv"
	"strings"
)

// MCPFrontPortSettingKey is the settings-registry key owning the MCP front
// daemon's client-facing port.
const MCPFrontPortSettingKey = "mcp_front.port"

// DefaultMCPFrontPort mirrors internal/cli.DefaultRouteDaemonPort (9137).
// The two constants are independently declared — internal/api is the LOWER
// layer internal/cli depends on (internal/cli imports internal/api;  the
// reverse would cycle), so this package cannot import cli.DefaultRouteDaemonPort
// directly. This is the "hard package-boundary" case of a mechanically
// drift-gated duplicate (see the shared architecture-layering-hygiene C1
// note): internal/cli's TestMCPFrontPortSettingDefaultMatchesRouteDaemonPort
// (internal/cli/mcp_front_port_test.go, which CAN import both packages)
// mechanically re-verifies this constant, DefaultRouteDaemonPort, and the
// SettingsRegistry default string all agree on every build.
const DefaultMCPFrontPort = 9137

// ResolveMCPFrontPort reads the mcp_front.port setting and validates it is
// in [1024,65535]. It returns an error on any read/parse/range failure —
// intended for WRITE paths (the reconcile-mcp-front command) that must not
// silently substitute a different port than what the operator configured.
func (a *API) ResolveMCPFrontPort() (int, error) {
	setting, err := a.SettingsGet(MCPFrontPortSettingKey)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", MCPFrontPortSettingKey, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(setting))
	if err != nil || n < 1024 || n > 65535 {
		return 0, fmt.Errorf("%s resolved to invalid value %q", MCPFrontPortSettingKey, setting)
	}
	return n, nil
}

// MCPFrontPortOrDefault is the graceful-fallback counterpart of
// ResolveMCPFrontPort, for READ-ONLY / best-effort consumers (the scan
// classifier, the route daemon's own flag default) where a resolution
// failure should degrade to the compiled default rather than abort the
// whole call.
func (a *API) MCPFrontPortOrDefault() int {
	if port, err := a.ResolveMCPFrontPort(); err == nil {
		return port
	}
	return DefaultMCPFrontPort
}
