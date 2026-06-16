// Package oneapirun implements the "run any command under the fully
// initialized Visual-Studio + Intel-oneAPI environment" MCP server over
// stdio. It exposes a single tool, run_in_oneapi_env, that executes an
// arbitrary native command with an environment composed of:
//
//   - the Visual-Studio toolchain environment captured from vcvars64.bat
//     (compiler / linker / INCLUDE / LIB / WindowsSdk* vars), plus
//   - the Intel oneAPI component DLL directories (mkl / tbb / compiler / …)
//     prepended to PATH so MKL/TBB-linked executables load their runtime
//     DLLs without a manual oneapi-shell wrap.
//
// WHY this exists (hot-prod operator feedback): gdb / lldb / a freshly
// built .exe / ctest / ASan builds fail to find the VS toolchain DLLs and
// the oneAPI MKL/TBB runtimes without a manual `vcvars64 && oneapi-shell`
// wrap, and Git-Bash mangles the backslash paths the native toolchain
// expects. This tool runs the command directly via the OS (no shell
// interpretation) with the correct environment so the agent never has to
// reconstruct that shell dance.
//
// It is consumed as a library from two entry points (mirroring godbolt):
//   - internal/oneapirun.NewCommand, embedded as the `mcphub oneapi-run`
//     subcommand (wired in internal/cli/root.go in a later serialized step)
//   - a possible standalone cmd/oneapi-run binary
//
// Both share the same handlers, SDK setup, and stdio transport via
// Run(ctx), so there is no behavior drift between shapes.
package oneapirun

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// OneAPIRunServer holds the MCP server instance plus the injectable
// environment-computation seams (vcvars capture + oneAPI DLL-dir
// enumeration). The seams let tests drive the env-merge and run logic
// against synthetic data without invoking the real ~1-2s vcvars64.bat
// subprocess.
type OneAPIRunServer struct {
	server *mcp.Server

	// captureVSEnv returns the Visual-Studio environment as a slice of
	// "KEY=VALUE" strings and a bool reporting whether vcvars64.bat was
	// found and captured. When ok is false the caller falls back to
	// os.Environ() and reports env_source accordingly. Cached once per
	// process in production (vcvars is slow); overridable in tests.
	captureVSEnv func() (env []string, ok bool)

	// oneAPIDLLDirs returns the ordered Intel oneAPI component DLL
	// directories to prepend to PATH (empty when no oneAPI install is
	// found). Overridable in tests.
	oneAPIDLLDirs func() []string
}

// Run wires up a fresh OneAPIRunServer with the production env-computation
// seams, registers the run_in_oneapi_env tool, and serves the MCP protocol
// over stdio until ctx is cancelled or the transport closes. Single source
// of truth for both entry points — keep runtime behavior here.
func Run(ctx context.Context) error {
	rs := &OneAPIRunServer{
		captureVSEnv:  captureVSEnvCached,
		oneAPIDLLDirs: detectOneAPIDLLDirs,
	}

	rs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "oneapi-run",
		Version: "1.0.0",
	}, nil)

	registerTools(rs)

	if err := rs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("oneapi-run server: %w", err)
	}
	return nil
}
