// internal/gui/route_adapter.go
//
// RouteHandler is the thin adapter Increment 1 of the MCP-front-daemon
// decision (work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-
// supervised-front-daemon.md) adds so a standalone `mcphub route` process
// (internal/cli/route.go) can serve EXACTLY the serena+LSP MCP data-plane
// routes this Server already registers for the GUI, and nothing else of the
// GUI's API surface (dashboard, settings, secrets, migration, ...).
//
// This is purely additive: registerSerenaRouterRoutes / registerLSPRouterRoutes
// still mount the same handlers on s.mux exactly as before (unchanged), so
// every existing GUI test keeps exercising the identical code path. This
// function builds a SEPARATE, narrower http.ServeMux referencing the same
// handler closures, then wraps it in the same requireAllowedHost guard the
// full GUI mux uses.
package gui

import "net/http"

// RouteHandler returns an http.Handler exposing ONLY /serena/mcp and
// /lsp/<language>/mcp, guarded by the same DNS-rebind (Host) + same-origin
// (Origin / Sec-Fetch-Site) checks as the full GUI mux. Intended for a
// front daemon that must not expose the rest of the GUI's HTTP API.
func (s *Server) RouteHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/serena/mcp", s.requireSameOrigin(s.serenaRouterHandler))
	mux.HandleFunc("/lsp/", s.requireSameOrigin(s.lspRouterHandler))
	return s.requireAllowedHost(mux)
}
