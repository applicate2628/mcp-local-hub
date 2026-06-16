// Package vtune implements an MCP server that profiles a native executable
// under the Intel VTune Profiler CLI (vtune.exe) and returns parsed
// performance findings (top hotspots + the textual summary). It is the
// CPU-profiling counterpart to the drmemory memory-checker server in this
// repo: where drmemory instruments an already-built binary for memory
// errors, vtune samples an already-built binary for where it spends its
// CPU time (user-mode hotspots, threading, memory-access, uarch, etc.).
//
// The server mirrors the drmemory / godbolt / oneapi-run CLI-wrapper
// MCP-server pattern in this repo: a struct holding the server plus the
// injectable seams, a Run(ctx) entry point shared by every binary shape,
// and one tool handler. The vtune.exe path probe and the actual two-phase
// (collect + report) subprocess runner are both injectable so tests never
// invoke the real (slow, target-requiring) profiler.
package vtune

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/unsafegate"
)

// enableUnsafeVtuneEnv gates registration of vtune_profile. The tool runs a
// caller-supplied executable under VTune — the same arbitrary-local-code-
// execution class as oneapi-run / drmemory (the client picks any exe and it
// runs with the user's privileges), so it is secure-by-default and registered
// only when the operator opts in by setting this to "1".
const enableUnsafeVtuneEnv = "MCP_LOCAL_HUB_ENABLE_UNSAFE_VTUNE"

// vtuneEnabled reports whether the operator opted in (enableUnsafeVtuneEnv ==
// "1"). Thin wrapper over the shared unsafegate owner; pure, for tests.
func vtuneEnabled() bool {
	return unsafegate.Enabled(enableUnsafeVtuneEnv)
}

// VTuneServer holds the MCP server instance plus the two injectable seams
// used by the profile handler:
//
//   - findExe resolves vtune.exe (default: findVTune, probing
//     %VTUNE_PROFILER_DIR% / the known oneAPI install dirs / PATH).
//   - run invokes VTune (collect + report) on a target and returns the raw
//     report CSV + summary text (default: defaultRun).
//
// Tests construct a VTuneServer with fakes for both so the handler is
// exercised end-to-end without a real VTune install.
type VTuneServer struct {
	server  *mcp.Server
	findExe func() (string, error)
	run     runFunc
}

// Run wires up a fresh VTuneServer with the production seams, registers the
// profile tool, and serves the MCP protocol over stdio until ctx is
// cancelled or the transport closes. It is the single source of truth for
// every entry point; keep runtime behavior here.
func Run(ctx context.Context) error {
	vs := &VTuneServer{
		findExe: findVTune,
		run:     defaultRun,
	}

	vs.server = mcp.NewServer(&mcp.Implementation{
		Name:    "mcp-local-hub-vtune",
		Version: "1.0.0",
	}, nil)

	registerTools(vs)

	if err := vs.server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("vtune server run: %w", err)
	}
	return nil
}

// registerTools attaches the vtune_profile tool, but ONLY after an explicit
// unsafe opt-in. Called once from Run during startup. When the opt-in is
// absent the daemon still serves MCP — it just exposes no tool
// (unsafegate.RegisterAllowed logs WHY to stderr so the secure-default is
// observable), so a misconfigured client cannot reach the arbitrary-exec
// surface.
func registerTools(vs *VTuneServer) {
	if !unsafegate.RegisterAllowed(enableUnsafeVtuneEnv, "vtune") {
		return
	}

	vs.server.AddTool(&mcp.Tool{
		Name: "vtune_profile",
		Description: "Profile a native executable under the Intel VTune Profiler and return its top CPU " +
			"hotspots plus the textual performance summary. VTune samples the ALREADY-BUILT binary at " +
			"runtime (no recompile/relink), so it works on optimized release builds. The default " +
			"analysis_type 'hotspots' uses user-mode sampling and needs no admin/SEP driver. Sampling " +
			"slows the target modestly, so set timeout_sec generously for long-running targets. Returns " +
			"structured JSON: {exit_code, analysis_type, summary, top_hotspots:[{function, module, " +
			"cpu_time_seconds, metrics}], report_path, command_line, stderr, timed_out}.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"exe": map[string]any{
					"type":        "string",
					"description": "Path to the target .exe to profile (absolute path recommended).",
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
				"analysis_type": map[string]any{
					"type": "string",
					"description": "VTune analysis type. One of: hotspots (default), memory-access, threading, " +
						"uarch-exploration, memory-consumption. Unknown values are rejected with a clear error. " +
						"hotspots and threading collect in USER MODE (no admin/SEP driver). memory-access, " +
						"uarch-exploration and memory-consumption need the hardware sampling (SEP) driver / admin; " +
						"without it the collect phase returns a structured error (exit_code, stderr) rather than data. " +
						"The top_hotspots function table is rendered via VTune's universal 'hotspots' report for every " +
						"type; analysis-specific metrics appear in the summary.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Hard wall-clock limit in seconds for the collect+report run. Default 600 (10 min).",
				},
			},
			"required": []string{"exe"},
		},
	}, vs.profileTool)
}
