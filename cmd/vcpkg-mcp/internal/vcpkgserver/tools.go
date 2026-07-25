package vcpkgserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/cmd/vcpkg-mcp/internal/cmakewrap"
	"mcp-local-hub/cmd/vcpkg-mcp/internal/discovery"
	"mcp-local-hub/cmd/vcpkg-mcp/internal/lastfailure"
)

// registerTools mounts every increment-1 tool handler. Single registration
// point, mirroring internal/perftools's registerTools convention.
func registerTools(vs *VcpkgServer) {
	vs.server.AddTool(&mcp.Tool{
		Name: "vcpkg_discover_root",
		Description: "Resolve the vcpkg root directory and report WHICH discovery rule fired: " +
			"explicit root param > VCPKG_ROOT env var > vcpkg resolved on PATH > a nearby " +
			"vcpkg.json/vcpkg-configuration.json manifest with a co-located vcpkg/ submodule binary > " +
			"labelled heuristic common locations (C:\\vcpkg, C:\\opt\\vcpkg, %USERPROFILE%\\vcpkg, a " +
			"Visual Studio VC\\vcpkg install, /opt/vcpkg, ~/vcpkg, ...). Exactly one candidate -> ok " +
			"with root+rule_fired. Several candidates (only possible at the heuristic tier) -> " +
			"unknown(multiple_candidates) listing ALL of them, never a silent pick. None found -> " +
			"unknown(no_candidates_found) — never reports \"not installed\"; supply root explicitly instead.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root": map[string]any{
					"type":        "string",
					"description": "Optional explicit vcpkg root to validate/use directly (highest-precedence rule).",
				},
			},
		},
	}, vs.discoverRootTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vcpkg_last_failure",
		Description: "Diagnose the last build failure for a vcpkg port. PRIMARY source is the " +
			"vcpkg-native buildtrees layout (<buildtrees-root>/<port>/<phase>-out/err.log) — always " +
			"available, never requires any operator-specific tooling. An OPTIONAL build_failed_log " +
			"(a custom wrapper-script artifact, NOT a vcpkg format) may be supplied for cheap " +
			"invocation-context enrichment (overlay chain, buildtrees root, exit code, which ports " +
			"failed) when the caller has one; absent/malformed wrapper files degrade gracefully and " +
			"never fail the call. Diagnostics are matched by anchored MSVC/GCC/Clang diagnostic " +
			"POSITION shape, never by exit code (libtool and similar wrappers can erase it) and never " +
			"by a bare substring scan (a filename like error_estimator.h must not be mistaken for a " +
			"diagnostic). Returns tri-state ok|failed|unknown(reason) plus log_paths[] so an agent can " +
			"always read further itself, context_source[] naming exactly which sources the answer " +
			"rests on, and overlay_chain[] echoed back (never resolved to a winner — that is the " +
			"separate, out-of-scope vcpkg_port_resolution tool).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{
					"type":        "string",
					"description": "Port name. Optional when build_failed_log names exactly one failed port.",
				},
				"triplet": map[string]any{
					"type":        "string",
					"description": "Optional. Auto-detected from the port's buildtrees directory (stdout-<triplet>.log / <triplet>.vcpkg_abi_info.txt) when omitted.",
				},
				"root": map[string]any{
					"type":        "string",
					"description": "Optional vcpkg root override, used to derive <root>/buildtrees when buildtrees_root is not given directly.",
				},
				"buildtrees_root": map[string]any{
					"type":        "string",
					"description": "Optional explicit buildtrees root override (matches vcpkg's --x-buildtrees-root). Highest precedence for locating logs.",
				},
				"build_failed_log": map[string]any{
					"type":        "string",
					"description": "Optional path to a build_failed.log-shaped wrapper file. Never auto-discovered.",
				},
				"overlays": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional explicit overlay-ports chain, order = precedence. Echoed back only, never resolved.",
				},
			},
		},
	}, vs.lastFailureTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "cmake_include_graph",
		Description: "Thin wrapper over the hub's internal/cmakegraph static resolver: statically " +
			"resolves the CMake include()/add_subdirectory() graph WITHOUT ever invoking cmake. " +
			"Surfaces cmakegraph's tri-state per-edge Status (resolved|dangling|unresolved), the " +
			"Conditional flag (this edge's path IS computed; whether it executes at configure time is " +
			"a separate, genuinely unknown question requiring a real cmake trace), the closed Reason " +
			"enum, and the operator-facing Histogram VERBATIM — this tool does not re-implement or " +
			"re-interpret resolution. Use root to walk every CMakeLists.txt/*.cmake under a tree as " +
			"independent roots (the right mode for an overlay-ports tree with no single top-level " +
			"CMakeLists.txt); use file to walk a single starting file.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root": map[string]any{
					"type":        "string",
					"description": "Directory to walk every entry_names match under as an independent root (cmakegraph.WalkTree). Mutually exclusive with file.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Single file to start the walk from (cmakegraph.Walk). Mutually exclusive with root.",
				},
				"workspace_root": map[string]any{
					"type":        "string",
					"description": "Boundary no resolved path may escape, and the value substituted for ${CMAKE_SOURCE_DIR}. Defaults to root, or file's containing directory.",
				},
				"entry_names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Root mode only. Defaults to [\"CMakeLists.txt\", \"*.cmake\"].",
				},
				"max_depth":      map[string]any{"type": "integer", "description": "Optional traversal depth bound (per root)."},
				"max_nodes":      map[string]any{"type": "integer", "description": "Optional whole-tree node count bound."},
				"max_file_bytes": map[string]any{"type": "integer", "description": "Optional per-file byte cap."},
			},
		},
	}, vs.cmakeIncludeGraphTool)
}

func (vs *VcpkgServer) discoverRootTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		Root string `json:"root"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := discovery.DiscoverRoot(args.Root, discovery.DefaultDeps())
	return jsonResult(res)
}

func (vs *VcpkgServer) lastFailureTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args lastfailure.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := lastfailure.LastFailure(args, lastfailure.DefaultDeps())
	return jsonResult(res)
}

func (vs *VcpkgServer) cmakeIncludeGraphTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args cmakewrap.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := cmakewrap.RunGraph(args)
	return jsonResult(res)
}
