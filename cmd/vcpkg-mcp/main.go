// cmd/vcpkg-mcp is the vcpkg MCP server: a standalone, static Go binary
// exposing read-only vcpkg/CMake diagnostic tools over the MCP stdio
// transport. See work-items/decisions/2026-07-25-vcpkg-mcp-tool-contracts.md
// for the accepted tool contracts and behavioural invariants.
//
// This binary is deliberately NOT embedded as an mcphub subcommand and
// carries no cobra wrapper: the design's binding obligation is that this
// whole directory stays self-contained (its own package tree under
// cmd/vcpkg-mcp/internal/, no dependency on hub internals other than
// internal/cmakegraph, and no hub code may import it) so a future
// extraction to a standalone repository is a directory move, not a
// rewrite.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/vcpkgserver"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if err := vcpkgserver.Run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "vcpkg-mcp:", err)
		os.Exit(1)
	}
}
