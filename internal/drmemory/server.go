// Package drmemory implements an MCP server that runs a Windows
// executable under Dr. Memory (the DynamoRIO runtime memory checker) and
// returns parsed memory-error findings. It is the runtime counterpart to
// AddressSanitizer for builds that cannot be recompiled/relinked under
// ASan — e.g. /Qmkl static-CRT binaries where ASan's instrumentation
// triggers LNK2005. Dr. Memory instruments the ALREADY-BUILT binary at
// runtime (no relink), catching buffer-overflow / use-after-free /
// uninitialized-read / leak (the gate7-class bugs).
//
// The server mirrors the godbolt / perftools CLI-wrapper MCP-server
// pattern in this repo: a struct holding the server plus the injectable
// seams, a Run(ctx) entry point shared by every binary shape, and one
// tool handler. The drmemory.exe path probe and the actual subprocess
// runner are both injectable so tests never invoke the real (10-50x slow)
// instrumented process.
package drmemory

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/unsafegate"
)

// enableUnsafeDrmemoryEnv gates registration of drmemory_run. The tool runs a
// caller-supplied executable under Dr. Memory — the same arbitrary-local-code-
// execution class as oneapi-run's run_in_oneapi_env (the client picks any exe
// and it runs with the user's privileges), so it is secure-by-default and
// registered only when the operator opts in by setting this to "1".
const enableUnsafeDrmemoryEnv = "MCP_LOCAL_HUB_ENABLE_UNSAFE_DRMEMORY"

// drmemoryEnabled reports whether the operator opted in (enableUnsafeDrmemoryEnv
// == "1"). Thin wrapper over the shared unsafegate owner; pure, for tests.
func drmemoryEnabled() bool {
	return unsafegate.Enabled(enableUnsafeDrmemoryEnv)
}

// DrMemoryServer holds the MCP server instance plus the two injectable
// seams used by the run handler:
//
//   - findExe resolves drmemory.exe (default: findDrMemory, probing the
//     known install dirs / %DRMEMORY_HOME% / PATH).
//   - run invokes Dr. Memory on a target and returns the raw results.txt
//     (default: defaultRun).
//
// Tests construct a DrMemoryServer with fakes for both so the handler is
// exercised end-to-end without a real Dr. Memory install.
type DrMemoryServer struct {
	server  *mcp.Server
	findExe func() (string, error)
	run     runFunc
}

// Run wires up a fresh DrMemoryServer with the production seams,
// registers the run tool, and serves the MCP protocol over stdio until
// ctx is cancelled or the transport closes. It is the single source of
// truth for every entry point; keep runtime behavior here.
func Run(ctx context.Context) error {
	ds := &DrMemoryServer{
		findExe: findDrMemory,
		run:     defaultRun,
	}

	ds.server = mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-local-hub-drmemory",
		Version: "1.0.0",
	}, nil)

	registerTools(ds)

	if err := ds.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("drmemory server run: %w", err)
	}
	return nil
}

// registerTools attaches the drmemory_run tool, but ONLY after an explicit
// unsafe opt-in. Called once from Run during startup. When the opt-in is
// absent the daemon still serves MCP — it just exposes no tool
// (unsafegate.RegisterAllowed logs WHY to stderr so the secure-default is
// observable), so a misconfigured client cannot reach the arbitrary-exec
// surface.
func registerTools(ds *DrMemoryServer) {
	if !unsafegate.RegisterAllowed(enableUnsafeDrmemoryEnv, "drmemory") {
		return
	}

	ds.server.AddTool(&mcp.Tool{
		Name: "drmemory_run",
		Description: "Run a Windows executable under Dr. Memory (the DynamoRIO runtime memory checker) " +
			"and return parsed memory-error findings. Dr. Memory instruments the ALREADY-BUILT binary at " +
			"runtime — no recompile/relink — so it works on binaries that cannot be built under " +
			"AddressSanitizer (e.g. /Qmkl static-CRT builds where ASan triggers LNK2005). It catches " +
			"buffer-overflow (UNADDRESSABLE ACCESS), use-after-free, uninitialized reads (UNINITIALIZED " +
			"READ), and memory leaks (LEAK / POSSIBLE LEAK). Instrumentation is 10-50x slower than a native " +
			"run, so set timeout_sec generously. Returns structured JSON: {exit_code, error_count, " +
			"leak_count, errors:[{type, count, location, full_stack}], summary, results_path, duration_ms, " +
			"truncated}.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"exe": map[string]any{
					"type":        "string",
					"description": "Path to the target .exe to instrument and run.",
				},
				"args": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional argv passed to the target executable.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Optional working directory for the target process. Inherits the server's cwd when omitted.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Hard wall-clock limit in seconds for the instrumented run. Default 1200 (20 min). Instrumentation is 10-50x slower than native — raise for long-running targets.",
				},
				"check_uninitialized": map[string]any{
					"type":        "boolean",
					"description": "When true (default) Dr. Memory checks for uninitialized reads. Set false to pass -no_check_uninitialized (faster, drops the UNINITIALIZED READ class).",
				},
				"light": map[string]any{
					"type":        "boolean",
					"description": "When true pass -light: a faster, less thorough mode that still catches unaddressable accesses but skips the heavier uninitialized-read / leak analysis. Default false.",
				},
			},
			"required": []string{"exe"},
		},
	}, ds.runTool)
}
