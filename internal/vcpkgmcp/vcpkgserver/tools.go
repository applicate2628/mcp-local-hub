package vcpkgserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/vcpkgmcp/cmaketrace"
	"mcp-local-hub/internal/vcpkgmcp/cmakewrap"
	"mcp-local-hub/internal/vcpkgmcp/discovery"
	"mcp-local-hub/internal/vcpkgmcp/lastfailure"
	"mcp-local-hub/internal/vcpkgmcp/patchesapply"
	"mcp-local-hub/internal/vcpkgmcp/pinstatus"
	"mcp-local-hub/internal/vcpkgmcp/portresolution"
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
			"Visual Studio VC\\vcpkg install, /opt/vcpkg, ~/vcpkg, ...). " +
			"An EXPLICIT root is TERMINAL: if it holds no vcpkg binary the answer is " +
			"unknown(explicit_root_invalid), or unknown(explicit_root_unreadable) when the probe " +
			"itself failed — it NEVER falls through to another installation the caller did not ask " +
			"about. A HEURISTIC NEVER SELECTS: one hit -> unknown(heuristic_only), several -> " +
			"unknown(multiple_candidates); both list every candidate so the caller can confirm one by " +
			"passing it as root. Only the env / PATH / manifest rules yield ok with root+rule_fired. " +
			"None found -> unknown(no_candidates_found) — never reports \"not installed\"; supply " +
			"root explicitly instead.",
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
			"diagnostic). Recognized shapes cover MSVC with or without a diagnostic code, clang-cl, " +
			"GCC/Clang, link.exe/lld-link and ninja FAILED. Scanned phase logs are extract, patch, " +
			"config, build (build-<triplet>-<cfg>-*.log, written by non-ninja/autotools/NMAKE ports) " +
			"and install. Causeless build-wrapper lines (NMAKE's U-series) are never eligible to be " +
			"the headline. " +
			"RANKING: diagnostics[] is ORDERED by severity (error, then warning, then note) and by " +
			"first occurrence within a severity — the actionable line is always reachable without " +
			"filtering, and first_error carries it directly. Warnings are never dropped, only sorted " +
			"after errors. Per-log capping is per SEVERITY CLASS, so an error trailing a flood of " +
			"repeated warnings can never be squeezed out of the answer. " +
			"COMMANDS: exact_command is the reproducible TOP-LEVEL vcpkg invocation and is recovered " +
			"ONLY from an authoritative record of it (a build_failed_log's `command:` line); it is " +
			"NEVER lifted out of a phase log, which holds a nested build tool's output rather than " +
			"vcpkg's own command line. When none is recoverable the field is omitted and " +
			"exact_command_not_recovered is noted — a wrong command an operator pastes into a shell " +
			"is worse than no command. build_command separately carries the build-layer " +
			"sub-invocation (CMake's \"Run Build Command(s):\") read from the same (phase, " +
			"configuration) build step as the reported diagnostic; diagnostic_log names the log the " +
			"headline diagnostic came from, so both are traceable. " +
			"Returns tri-state ok|failed|unknown(reason) plus log_paths[] so an agent can " +
			"always read further itself, context_source[] naming exactly which sources the answer " +
			"rests on, and overlay_chain[] echoed back (never resolved to a winner — that is the " +
			"separate, out-of-scope vcpkg_port_resolution tool). " +
			"FAILS CLOSED on incomplete evidence: `failed` requires an ERROR-severity diagnostic from a " +
			"primary phase log. A warning-only log -> unknown(no_failure_diagnostic) (that is the normal " +
			"state of a successful C++ build); an error found ONLY in a CMakeConfigureLog.yaml.log " +
			"try_compile dump -> unknown(capability_probe_only) (a failing capability probe is normal " +
			"feature detection); an unreadable relevant log -> unknown(phase_log_unreadable); a log " +
			"only partly examined (over the 32 MiB read bound, or a line the scanner could not take) " +
			"-> unknown(phase_log_size_limit_exceeded), never a confident verdict from the prefix; an " +
			"unreadable root or port directory -> unknown(buildtrees_root_unreadable|port_dir_unreadable), " +
			"NEVER the verified-absence reasons buildtrees_root_absent|port_dir_not_found. root and " +
			"buildtrees_root MUST be absolute (a relative root would bind to the hub daemon's working " +
			"directory) -> unknown(relative_root); port must be one legal vcpkg port name -> " +
			"unknown(invalid_port_name); no root resolvable at all -> unknown(vcpkg_root_not_resolved). " +
			"A build_failed_log's failed_ports list can prove a port did NOT " +
			"fail only when it is provably exhaustive (clean scan AND len(failed_ports) == " +
			"build_failed_count); otherwise the tool falls back to buildtree evidence and notes " +
			"wrapper_failed_ports_list_completeness_unproven. Diagnostics found are returned whatever " +
			"the verdict. " +
			"VOCABULARY RULE: every reason and note names what the tool OBSERVED — a verified fact, " +
			"something not supplied to it, or something not found where it looked — never a " +
			"conclusion about the build. In particular overlay_chain_not_supplied means no chain was " +
			"supplied or found in any consulted source; it does NOT claim the build used no overlays " +
			"(buildtrees records no overlay chain, so this tool cannot know that), and " +
			"buildtrees_root_absent reports a verified absent directory without attributing it to " +
			"--clean-buildtrees-after-build.",
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
					"description": "Optional ABSOLUTE vcpkg root override, used to derive <root>/buildtrees when buildtrees_root is not given directly. A relative value returns unknown(relative_root).",
				},
				"buildtrees_root": map[string]any{
					"type":        "string",
					"description": "Optional explicit ABSOLUTE buildtrees root override (matches vcpkg's --x-buildtrees-root). Highest precedence for locating logs. A relative value returns unknown(relative_root).",
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
		Name: "vcpkg_port_resolution",
		Description: "Determine which port definition wins across overlay ports and builtin ports, and report every location checked. " +
			"When vcpkg_root is omitted, the builtin fallback is NOT checked; the result states only what the supplied overlays established.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{
					"type":        "string",
					"description": "Required port name to resolve.",
				},
				"vcpkg_root": map[string]any{
					"type":        "string",
					"description": "Optional absolute vcpkg root; enables checking its builtin ports fallback.",
				},
				"overlay_ports": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional absolute overlay directories in precedence order; the first matching definition wins.",
				},
			},
		},
	}, vs.portResolutionTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vcpkg_pin_status",
		Description: "Check whether a port's source pin matches what its remote advertises NOW. Verdicts are current or unknown(reason); " +
			"this tool cannot say \"behind\" because git ls-remote cannot prove a differing pin is an ancestor rather than diverged or rebased away. " +
			"It is not a staleness checker. The call carries its OWN status/reason separate from each port's: an omitted or empty port_dirs is " +
			"unknown(no_port_dirs), never an ok result with an empty list. Per-port unknown reasons are a closed enum: not_git_comparable, " +
			"pin_not_at_tip, ref_unresolvable, ref_not_found_on_remote, named_ref_not_comparable, head_ref_unresolvable, remote_query_failed, " +
			"remote_query_timeout, remote_query_canceled, remote_ref_limit, remote_url_credential_bearing, network_disabled, portfile_unparsable, " +
			"guard_unresolvable, multiple_fetch_calls. Remote URLs are redacted on every emitted field, and a credential-bearing remote is " +
			"refused rather than queried (its secret would otherwise appear in the child process's command line).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port_dirs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Absolute port directories, each expected to contain portfile.cmake. Required and non-empty; an empty batch is refused with unknown(no_port_dirs).",
				},
				"disable_network": map[string]any{
					"type":        "boolean",
					"description": "When true, do not query remotes; each requested port reports unknown(network_disabled).",
				},
			},
		},
	}, vs.pinStatusTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vcpkg_patches_apply",
		Description: "Statically analyze a portfile to report which patches WOULD apply for a triplet, in order. It applies nothing. " +
			"Guards that static analysis cannot decide are returned in an undecidable bucket instead of guessed either way. " +
			"Triplet variables are read from the ACTUAL triplet file located via overlay_triplets / " +
			"vcpkg_root, NEVER derived from the triplet NAME (a custom triplet named corp-windows can " +
			"set VCPKG_LIBRARY_LINKAGE static, and a name like `cl` implies nothing at all). With no " +
			"triplet file reachable, every triplet variable is unresolved and the guards depending on " +
			"one land in undecidable — supply var_overrides to decide them explicitly. triplet_file " +
			"reports which file the facts came from. " +
			"Each applied entry carries a tri-state existence (exists|absent|unreadable): only a " +
			"VERIFIED absence is reported in missing[]. Paths the filesystem refused to answer for go " +
			"in unreadable[] and force unknown(triplet_file_unreadable|patch_path_unreadable|" +
			"orphan_scan_incomplete) — every bucket is still returned, so a partial inventory is never " +
			"silently presented as a complete one.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port_dir": map[string]any{
					"type":        "string",
					"description": "Required absolute port directory containing portfile.cmake.",
				},
				"triplet": map[string]any{
					"type":        "string",
					"description": "Required triplet name, used to locate its triplet file and to evaluate patch guards.",
				},
				"vcpkg_root": map[string]any{
					"type":        "string",
					"description": "Optional ABSOLUTE vcpkg root, for $ENV{VCPKG_ROOT} expansion in patch paths and for the builtin triplet lookup (<root>/triplets, <root>/triplets/community).",
				},
				"overlay_triplets": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional ABSOLUTE overlay-triplets directories in precedence order (matches vcpkg's --overlay-triplets); the first directory containing <triplet>.cmake wins, ahead of the builtin triplets.",
				},
				"port_name": map[string]any{
					"type":        "string",
					"description": "Optional override for the ${PORT} builtin; defaults to the port_dir base name.",
				},
				"var_overrides": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Optional explicit CMake variable values used while evaluating guards. Outrank triplet-file facts, and are the way to decide variables vcpkg derives at build time (VCPKG_TARGET_IS_*, VCPKG_CROSSCOMPILING, ...).",
				},
			},
		},
	}, vs.patchesApplyTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vcpkg_cmake_trace",
		Description: "Read an EXISTING cmake --trace-format=json-v1 trace and report the executed CMake lines, expansions, and include order. " +
			"Executed lines are positive evidence only: an absent line means \"not observed in this trace\", never \"unreachable\". It never runs cmake. " +
			"Parsing STREAMS under hard ceilings (total bytes, per-line bytes, records materialized) rather than reading the file whole; any ceiling " +
			"that trips, and any malformed line, is reported in input_incomplete_reasons[] (closed enum: input_malformed, byte_limit, line_limit, " +
			"record_limit) so a bounded read is never mistaken for a complete one. Cancellation is observed and returns unknown(canceled) with no " +
			"partial result. Note truncated (the returned records cap) is a SEPARATE, narrower signal from input_incomplete.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"trace_path": map[string]any{
					"type":        "string",
					"description": "Required absolute path to an existing json-v1 CMake trace.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Optional exact CMake file path filter for returned records.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Optional case-insensitive CMake command filter for returned records.",
				},
				"max_records": map[string]any{
					"type":        "integer",
					"description": "Optional cap for returned records; zero uses the package default.",
				},
			},
		},
	}, vs.cmakeTraceTool)

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
			"CMakeLists.txt); use file to walk a single starting file. Coverage is always reported, " +
			"never assumed: unscanned_files[] carries every hole with a CLOSED reason (byte_cap_exceeded, " +
			"file_unreadable, enumerate_failed, root_outside_workspace, root_enumeration_capped), so a " +
			"subtree that could not be listed appears there rather than being silently omitted from an " +
			"apparently-complete graph. Per-edge, dangling means VERIFIED absence only; a target whose " +
			"existence could not be determined (access denied, sharing violation) is " +
			"unresolved(target_unreadable).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root": map[string]any{
					"type":        "string",
					"description": "Directory to walk every entry_names match under as an independent root (cmakegraph.WalkTree). Mutually exclusive with file — supplying BOTH is refused with unknown(args_invalid), never silently resolved in root's favour.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Single file to start the walk from (cmakegraph.Walk). Mutually exclusive with root — supplying BOTH is refused with unknown(args_invalid).",
				},
				"workspace_root": map[string]any{
					"type":        "string",
					"description": "Boundary no resolved path may escape, and the value substituted for ${CMAKE_SOURCE_DIR}. Honoured in BOTH modes; it may be a parent of root. Defaults to root, or file's containing directory.",
				},
				"entry_names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Root mode only. Defaults to [\"CMakeLists.txt\", \"*.cmake\"].",
				},
				"max_depth":      map[string]any{"type": "integer", "description": "Optional traversal depth bound (per root)."},
				"max_nodes":      map[string]any{"type": "integer", "description": "Optional whole-tree node count bound (root ADMISSION)."},
				"max_file_bytes": map[string]any{"type": "integer", "description": "Optional per-file byte cap."},
				"max_roots":      map[string]any{"type": "integer", "description": "Root mode only. Optional bound on how many candidate roots are ENUMERATED before the tree walk stops (root_enumeration_capped). Separate from max_nodes, which bounds admission, not discovery."},
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

func (vs *VcpkgServer) portResolutionTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args portresolution.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := portresolution.ResolvePort(args, portresolution.DefaultDeps())
	return jsonResult(res)
}

func (vs *VcpkgServer) pinStatusTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args pinstatus.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := pinstatus.PinStatus(ctx, args, pinstatus.DefaultDeps())
	return jsonResult(res)
}

func (vs *VcpkgServer) patchesApplyTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args patchesapply.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := patchesapply.ApplyOrder(args)
	return jsonResult(res)
}

func (vs *VcpkgServer) cmakeTraceTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args cmaketrace.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := cmaketrace.Trace(ctx, args, cmaketrace.DefaultDeps())
	return jsonResult(res)
}

func (vs *VcpkgServer) cmakeIncludeGraphTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args cmakewrap.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
	}
	res := cmakewrap.RunGraph(ctx, args)
	return jsonResult(res)
}
