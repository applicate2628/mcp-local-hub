// Package vcpkgmcp is the mcphub entry point for the vcpkg MCP server —
// read-only vcpkg/CMake diagnostic tools (build-failure triage, overlay-port
// resolution, pin status, patch-apply order, CMake trace/include-graph
// reading) over the MCP stdio transport. The actual server + tool
// registration lives in the vcpkgserver subpackage and the sibling
// evidence/discovery/lastfailure/cmakewrap/pinstatus/portresolution/
// patchesapply/cmaketrace packages; this file only owns the cobra command,
// matching the internal/godbolt, internal/perftools, internal/drmemory
// convention.
//
// This server previously shipped as a standalone cmd/vcpkg-mcp executable.
// That placement regressed against the pattern every other in-house MCP
// server here follows and cost real, measured deploy friction (build.sh
// never built it, client entries were hand-written with machine-specific
// absolute paths, it was unsupervised and absent from `mcphub status`, and a
// hub upgrade never updated it). Decision:
// work-items/decisions/2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md.
package vcpkgmcp

import (
	"github.com/spf13/cobra"

	"mcp-local-hub/internal/vcpkgmcp/vcpkgserver"
)

// NewCommand returns a cobra.Command that runs the vcpkg MCP server over
// stdio, embedded as the `mcphub vcpkg` subcommand.
func NewCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "vcpkg",
		Short:  "vcpkg/CMake diagnostic MCP server (stdio)",
		Hidden: true, // internal transport helper when embedded
		RunE: func(cmd *cobra.Command, args []string) error {
			return vcpkgserver.Run(cmd.Context())
		},
	}
}
