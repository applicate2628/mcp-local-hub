// Package vcpkgserver is the vcpkg-mcp server: an MCP server over stdio
// mirroring the framing this repo's other Go MCP surfaces already use
// (internal/godbolt, internal/perftools — github.com/modelcontextprotocol/go-sdk's
// mcp.Server + mcp.StdioTransport, mcp.Tool with a hand-written
// map[string]any InputSchema, and the same errResult/jsonResult shape).
//
// Unlike godbolt/perftools this package is NOT embedded as an mcphub
// subcommand and carries no cobra wrapper — the vcpkg-mcp design
// (work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md,
// "Implementation shape") requires this binary stay strictly
// self-contained: its own package tree, no dependency on hub internals
// other than internal/cmakegraph, and no hub code may import it. Keeping
// this package under cmd/vcpkg-mcp/internal/ (rather than the top-level
// internal/ used by godbolt/perftools) is what makes extraction to a
// standalone repo later a directory move rather than a rewrite.
package vcpkgserver

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VcpkgServer holds the MCP server instance. All three increment-1 tools
// are currently pure functions of their arguments (discovery/lastfailure
// read the filesystem directly via injected default deps; cmakewrap calls
// cmakegraph directly) so there is no shared mutable state yet — this
// struct exists so the registration pattern matches godbolt/perftools and
// has an obvious place to add shared state (e.g. a cache) later.
type VcpkgServer struct {
	server *mcp.Server
}

// Run wires up a fresh VcpkgServer, registers all tools, and serves the
// MCP protocol over stdio until ctx is cancelled or the transport closes.
func Run(ctx context.Context) error {
	vs := &VcpkgServer{}

	vs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "vcpkg-mcp",
		Version: "0.1.0",
	}, nil)

	registerTools(vs)

	if err := vs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("vcpkg-mcp server: %w", err)
	}
	return nil
}
