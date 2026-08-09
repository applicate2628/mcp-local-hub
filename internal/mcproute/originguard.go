// Package mcproute holds the port-bound, GUI-process-independent pieces of
// the serena+LSP MCP data plane so they can be reused by a standalone
// supervisor-managed front daemon (`mcphub route`) without importing
// internal/gui.
//
// Increment 1 (work-items/decisions/2026-07-25-mcp-data-plane-off-gui-onto-
// supervised-front-daemon.md) scopes this package to the ONE piece of the
// router that the architect verified is trivially parameterizable: the
// DNS-rebind / same-origin guard (internal/gui/csrf.go). It was, before this
// change, the router's only coupling to *gui.Server (via s.effectivePort()).
//
// The request-handling logic itself (serena_router.go / lsp_router.go's
// handler dispatch) was NOT relocated into this package this increment — see
// the "Adjacent findings" section of the Increment-1 implementation report
// for the verified conflict (Go visibility rules make several existing
// internal/gui white-box tests incompatible with a literal file move of
// serena_router_session.go / serena_router_handshake.go, and the handler
// itself has an undeclared hard dependency on internal/gui/serena_router_
// lifecycle.go's JSON-RPC helpers). `mcphub route` instead reuses the
// existing, unchanged gui.Server plumbing directly (internal/cli/route.go),
// which already benefits from this package's port-bound guard via gui's thin
// adapter (internal/gui/csrf.go).
package mcproute

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// AllowedHost reports whether hostport names the loopback host this server
// is bound to at port. Mirrors browsers' behavior of omitting the port
// number for the scheme's default port (":80" for http).
//
// port <= 0 means "not yet bound / not configured" and always rejects — a
// server with no known port can never be a valid same-origin target.
func AllowedHost(hostport string, port int) bool {
	if port <= 0 {
		return false
	}
	host, p, err := net.SplitHostPort(hostport)
	if err != nil {
		// No explicit port: browsers omit :80 for the default HTTP port.
		// Accept the bare-host form only when bound to port 80.
		if port != 80 {
			return false
		}
		host = hostport
	} else if p != strconv.Itoa(port) {
		return false
	}
	return strings.EqualFold(host, "localhost") || host == "127.0.0.1"
}

// AllowedOrigin reports whether origin is this server's own loopback origin
// at port. It is the Origin-header counterpart to AllowedHost: an Origin
// carrying credentials, a query string, a fragment, a non-http scheme, or a
// non-root path is rejected outright; otherwise the host portion is checked
// against AllowedHost.
func AllowedOrigin(origin string, port int) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Path != "" && u.Path != "/" {
		return false
	}
	return AllowedHost(u.Host, port)
}
