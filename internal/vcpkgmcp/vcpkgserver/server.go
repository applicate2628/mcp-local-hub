// Package vcpkgserver is the vcpkg-mcp server: an MCP server over stdio
// mirroring the framing this repo's other Go MCP surfaces already use
// (internal/godbolt, internal/perftools — github.com/modelcontextprotocol/go-sdk's
// mcp.Server + mcp.StdioTransport, mcp.Tool with a hand-written
// map[string]any InputSchema, and the same errResult/jsonResult shape).
//
// Like godbolt/perftools this package is embedded as an `mcphub vcpkg`
// subcommand (internal/vcpkgmcp.NewCommand, wired in internal/cli/root.go).
// It previously shipped as a standalone `cmd/vcpkg-mcp` executable, deliberately
// kept self-contained under cmd/vcpkg-mcp/internal/ so extraction to its own
// repo would be a directory move. That placement regressed against the
// project's own prior decision — servers/godbolt/manifest.yaml documents the
// identical migration in the opposite direction (Python-per-session process ->
// one hub-managed Go daemon) and names the exact costs the standalone shape
// re-introduced (no build.sh coverage, hand-written client entries with
// machine-specific absolute paths, no supervision, a process per client instead
// of one shared daemon, no hub-upgrade coverage). Decision:
// work-items/decisions/2026-07-26-vcpkg-mcp-must-follow-the-in-hub-server-pattern.md.
// This package now lives under the top-level internal/ tree (internal/vcpkgmcp/vcpkgserver)
// exactly like godbolt/perftools, and internal/vcpkgmcp/... has no dependency on
// hub internals other than internal/cmakegraph — an ordinary internal-to-internal
// import now that both sides live under internal/.
package vcpkgserver

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/vcpkgmcp/lastfailure"
	"mcp-local-hub/internal/vcpkgmcp/reversedepgraph"
)

// VcpkgServer holds the MCP server instance. All three increment-1 tools
// are currently pure functions of their arguments (discovery/lastfailure
// read the filesystem directly via injected default deps; cmakewrap calls
// cmakegraph directly) so there is no shared mutable state yet — this
// struct exists so the registration pattern matches godbolt/perftools and
// has an obvious place to add shared state (e.g. a cache) later.
type VcpkgServer struct {
	server                    *mcp.Server
	lastFailureOnce           sync.Once
	lastFailureSlots          chan struct{}
	lastFailureRun            func(context.Context, lastfailure.Args, lastfailure.Deps) lastfailure.Result
	lastFailureDeps           func() lastfailure.Deps
	reverseDependenciesOnce   sync.Once
	reverseDependenciesSlots  chan struct{}
	reverseDependenciesRun    func(context.Context, reversedepgraph.Args, reversedepgraph.Runner) reversedepgraph.Result
	reverseDependenciesRunner reversedepgraph.Runner
	trustedVcpkgRoot          string
}

func (vs *VcpkgServer) initReverseDependencies() {
	vs.reverseDependenciesOnce.Do(func() {
		vs.reverseDependenciesSlots = make(chan struct{}, 1)
		if vs.reverseDependenciesRun == nil {
			vs.reverseDependenciesRun = reversedepgraph.Analyze
		}
		if vs.reverseDependenciesRunner == nil {
			vs.reverseDependenciesRunner = reversedepgraph.DefaultRunner()
		}
		if vs.trustedVcpkgRoot == "" {
			vs.trustedVcpkgRoot = os.Getenv("VCPKG_ROOT")
		}
	})
}

func (vs *VcpkgServer) initLastFailure() {
	vs.lastFailureOnce.Do(func() {
		vs.lastFailureSlots = make(chan struct{}, 2)
		if vs.lastFailureRun == nil {
			vs.lastFailureRun = lastfailure.LastFailureContext
		}
		if vs.lastFailureDeps == nil {
			vs.lastFailureDeps = lastfailure.DefaultDeps
		}
	})
}

// Run wires up a fresh VcpkgServer, registers all tools, and serves the
// MCP protocol over stdio until ctx is cancelled or the transport closes.
func Run(ctx context.Context) error {
	vs, err := newRegisteredServer(registerTools)
	if err != nil {
		return fmt.Errorf("vcpkg-mcp server: register tool: %w", err)
	}
	if err := vs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("vcpkg-mcp server: %w", err)
	}
	return nil
}

func newRegisteredServer(register func(*VcpkgServer) error) (*VcpkgServer, error) {
	vs := &VcpkgServer{}
	vs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "vcpkg-mcp",
		Version: "0.1.0",
	}, nil)
	if err := register(vs); err != nil {
		return nil, err
	}
	return vs, nil
}
