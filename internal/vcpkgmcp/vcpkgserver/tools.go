package vcpkgserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/cmakegraph"
	"mcp-local-hub/internal/vcpkgmcp/cmaketrace"
	"mcp-local-hub/internal/vcpkgmcp/cmakewrap"
	"mcp-local-hub/internal/vcpkgmcp/discovery"
	"mcp-local-hub/internal/vcpkgmcp/lastfailure"
	"mcp-local-hub/internal/vcpkgmcp/patchesapply"
	"mcp-local-hub/internal/vcpkgmcp/pinstatus"
	"mcp-local-hub/internal/vcpkgmcp/portresolution"
	"mcp-local-hub/internal/vcpkgmcp/publicresult"
)

const resultProjectionDescription = " Every successful result is measured as indented JSON and is bounded to 256 KiB; an oversized complete result retains its package-owned causal core and adds result_projection with explicit omissions."

func pinStatusReasonVocabularies() (perPort, batch, noPortDirs, tooManyPortDirs, relativePortDir, commitPinAbbreviated, refUnresolvable, networkDisabled string) {
	registry := pinstatus.PublicReasonRegistry()
	perPortReasons := registry.PerPort()
	perPortValues := make([]string, len(perPortReasons))
	for i, reason := range perPortReasons {
		perPortValues[i] = string(reason)
	}
	batchReasons := registry.Batch()
	batchValues := make([]string, len(batchReasons))
	for i, reason := range batchReasons {
		batchValues[i] = string(reason)
	}
	findPerPort := func(want pinstatus.Reason) string {
		if reason, ok := registry.LookupPerPort(want); ok {
			return string(reason)
		}
		return ""
	}
	findBatch := func(want pinstatus.BatchReason) string {
		if reason, ok := registry.LookupBatch(want); ok {
			return string(reason)
		}
		return ""
	}
	return strings.Join(perPortValues, ", "), strings.Join(batchValues, ", "),
		findBatch(pinstatus.BatchReasonNoPortDirs), findBatch(pinstatus.BatchReasonTooManyPortDirs),
		findPerPort(pinstatus.ReasonRelativePortDir), findPerPort(pinstatus.ReasonCommitPinAbbreviated),
		findPerPort(pinstatus.ReasonRefUnresolvable), findPerPort(pinstatus.ReasonNetworkDisabled)
}

func pinStatusToolDescription() string {
	perPortReasons, batchReasons, noPortDirs, tooManyPortDirs, relativePortDir, commitPinAbbreviated, refUnresolvable, _ := pinStatusReasonVocabularies()
	return fmt.Sprintf("Check whether a port's source pin matches what its remote advertises NOW. Valid-input verdicts are current or unknown(reason); "+
		"this tool cannot say \"behind\" because git ls-remote cannot prove a differing pin is an ancestor rather than diverged or rebased away. "+
		"It is not a staleness checker. The call carries its OWN status/reason separate from each port's: an omitted or empty port_dirs is "+
		"unknown(%s), never an ok result with an empty list. A batch over the package limit is unknown(%s) before filesystem, clock, allocation, or remote work. Batch reasons are a closed enum: %s. Every port_dirs entry must be absolute; a relative entry is "+
		"failed(%s) before filesystem or network access. Per-port reasons are a closed enum: %s. "+
		"An ABBREVIATED commit pin (7..39 hex, pin.shape commit_abbrev) is reported as "+
		"unknown(%s) — an unresolvable COMMIT, never as a missing tag/branch: ls-remote advertises only full 40-hex SHAs, "+
		"so an abbreviation has nothing to be matched against. A ${VARIABLE} REF assigned only inside an if()/foreach()/macro() body is "+
		"unknown(%s), never resolved from a branch that may not have executed. Remote URLs are redacted on every emitted field "+
		"— including ones url.Parse rejects — and a credential-bearing remote is "+
		"refused rather than queried (its secret would otherwise appear in the child process's command line). "+
		"A remote-query lifecycle failure also includes failure.id and a fixed safe failure.detail; status/reason remain the compatibility verdict.", noPortDirs, tooManyPortDirs, batchReasons, relativePortDir, perPortReasons, commitPinAbbreviated, refUnresolvable) + resultProjectionDescription
}

func pinStatusPortDirsDescription() string {
	_, _, noPortDirs, tooManyPortDirs, relativePortDir, _, _, _ := pinStatusReasonVocabularies()
	return fmt.Sprintf("Absolute port directories, each expected to contain portfile.cmake. At most %d entries are admitted; an oversize batch returns unknown(%s) before filesystem, clock, allocation, or remote work. A relative entry returns failed(%s) before filesystem or network access. Required and non-empty; an empty batch is refused with unknown(%s).", pinstatus.MaxPortDirs, tooManyPortDirs, relativePortDir, noPortDirs)
}

func pinStatusDisableNetworkDescription() string {
	_, _, _, _, _, _, _, networkDisabled := pinStatusReasonVocabularies()
	return fmt.Sprintf("When true, do not query remotes; each requested valid absolute port reports unknown(%s).", networkDisabled)
}

func discoverRootAdmissionDescription() string {
	return fmt.Sprintf("A relative explicit root is terminal unknown(%s) before every filesystem, environment, PATH, manifest, or heuristic probe. ", discovery.ReasonExplicitRootRelative)
}

func patchesRootAdmissionDescription() string {
	return fmt.Sprintf("More than the admitted overlay root maximum fails as failed(%s) before I/O; every nonblank relative overlay root fails as failed(%s), and a relative vcpkg_root fails as failed(%s), also before I/O. ", patchesapply.ReasonTooManyOverlayTripletRoots, patchesapply.ReasonRelativeOverlayTripletRoot, patchesapply.ReasonRelativeVcpkgRoot)
}

func patchesVcpkgRootDescription() string {
	return fmt.Sprintf("Optional ABSOLUTE vcpkg root, for $ENV{VCPKG_ROOT} expansion in patch paths and for the builtin triplet lookup (<root>/triplets, <root>/triplets/community). A relative value returns failed(%s) before I/O.", patchesapply.ReasonRelativeVcpkgRoot)
}

func patchesOverlayTripletsDescription() string {
	return fmt.Sprintf("Optional ABSOLUTE overlay-triplets directories in precedence order (matches vcpkg's --overlay-triplets); the first directory containing <triplet>.cmake wins, ahead of the builtin triplets. More than %d roots returns failed(%s), and any nonblank relative root returns failed(%s), before I/O.", patchesapply.MaxOverlayTripletRoots, patchesapply.ReasonTooManyOverlayTripletRoots, patchesapply.ReasonRelativeOverlayTripletRoot)
}

type projectableToolOutcome struct {
	invalidArgument error
	err             error
	result          publicresult.Projectable
}

type projectableToolHandler func(context.Context, *mcp.CallToolRequest) projectableToolOutcome

func registerProjectableTool(vs *VcpkgServer, tool *mcp.Tool, handler projectableToolHandler) error {
	resolved, err := strictResolvedSchema(tool)
	if err != nil {
		return fmt.Errorf("%s: %w", tool.Name, err)
	}
	vs.server.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args := map[string]any{}
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
			}
		}
		if err := resolved.Validate(&args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err)), nil
		}
		outcome := handler(ctx, req)
		if outcome.invalidArgument != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", outcome.invalidArgument)), nil
		}
		if outcome.err != nil {
			return nil, outcome.err
		}
		if outcome.result == nil {
			return errResult("internal invariant: nil projectable result"), nil
		}
		return jsonResult(outcome.result)
	})
	return nil
}

// strictResolvedSchema preserves nested intentional maps while making the
// advertised top-level object contract reject misspelled arguments before any
// semantic handler can observe its zero-value interpretation.
func strictResolvedSchema(tool *mcp.Tool) (*jsonschema.Resolved, error) {
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object["type"] != "object" {
		return nil, fmt.Errorf("input schema must be a top-level object")
	}
	object["additionalProperties"] = false
	raw, err = json.Marshal(object)
	if err != nil {
		return nil, err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, err
	}
	resolved, err := schema.Resolve(&jsonschema.ResolveOptions{ValidateDefaults: true})
	if err != nil {
		return nil, err
	}
	tool.InputSchema = &schema
	return resolved, nil
}

// registerTools mounts every increment-1 tool handler. Single registration
// point, mirroring internal/perftools's registerTools convention.
func registerTools(vs *VcpkgServer) error {
	if err := registerProjectableTool(vs, &mcp.Tool{
		Name: "vcpkg_discover_root",
		Description: "Resolve the vcpkg root directory and report WHICH discovery rule fired: " +
			"explicit root param > VCPKG_ROOT env var > vcpkg resolved on PATH > a nearby " +
			"vcpkg.json/vcpkg-configuration.json manifest with a co-located vcpkg/ submodule binary > " +
			"labelled heuristic common locations (C:\\vcpkg, C:\\opt\\vcpkg, %USERPROFILE%\\vcpkg, a " +
			"Visual Studio VC\\vcpkg install, /opt/vcpkg, ~/vcpkg, ...). " +
			discoverRootAdmissionDescription() +
			"An EXPLICIT absolute root is TERMINAL: if it holds no vcpkg binary the answer is " +
			"unknown(explicit_root_invalid), or unknown(explicit_root_unreadable) when the probe " +
			"itself failed — it NEVER falls through to another installation the caller did not ask " +
			"about. A HEURISTIC NEVER SELECTS: one hit -> unknown(heuristic_only), several -> " +
			"unknown(multiple_candidates); both list every candidate so the caller can confirm one by " +
			"passing it as root. Only the env / PATH / manifest rules yield ok with root+rule_fired. " +
			"None found -> unknown(no_candidates_found) — never reports \"not installed\"; supply " +
			"root explicitly instead." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root": map[string]any{
					"type":        "string",
					"description": "Optional explicit ABSOLUTE vcpkg root to validate/use directly (highest-precedence rule). " + discoverRootAdmissionDescription(),
				},
			},
		},
	}, vs.discoverRootTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
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
			"and install. Causeless build-wrapper lines (NMAKE's U-series) carry no cause at all and " +
			"are never returned as diagnostics — a stricter exclusion than the aggregate tier below, " +
			"which IS returned. " +
			"RANKING: every diagnostic carries tier=specific|aggregate. An AGGREGATE only summarises " +
			"other diagnostics, carrying a count or a sub-tool exit code instead of a cause (LNK1120 " +
			"\"N unresolved externals\", LNK1169, \"clang-cl: error: linker command failed with exit " +
			"code N\", ninja's \"FAILED: <target>\"); a SPECIFIC names a cause (LNK2019/LNK2001/LNK2005, " +
			"\"lld-link: error: undefined symbol: X\", any file:line diagnostic). diagnostics[] is " +
			"ORDERED by severity (error, then warning, then note), then by tier (specific before " +
			"aggregate), then by first occurrence — so a driver's \"linker command failed\" never " +
			"outranks the undefined symbol that caused it. first_error carries that headline error " +
			"directly and always equals diagnostics[0] when any error exists; diagnostic_log names the " +
			"log the headline actually came from. When EVERY error is an aggregate the aggregate is " +
			"still the headline — it is better than nothing. Warnings are never filtered by class; a " +
			"reported producer/response budget may drop only the lowest-ranked tail and reports the count. " +
			"Per-log capping is per SEVERITY CLASS, so an error trailing a flood of " +
			"repeated warnings can never be squeezed out of the answer. " +
			"COMMANDS: exact_command is the reproducible TOP-LEVEL vcpkg invocation and is recovered " +
			"ONLY from an authoritative record of it (a build_failed_log's `command:` line); it is " +
			"NEVER lifted out of a phase log, which holds a nested build tool's output rather than " +
			"vcpkg's own command line. When none is recoverable the field is omitted and " +
			"exact_command_not_recovered is noted — a wrong command an operator pastes into a shell " +
			"is worse than no command. Credential-bearing command fields are emitted as REDACTED " +
			"rather than copying secrets into the result. build_command separately carries the build-layer " +
			"sub-invocation (CMake's \"Run Build Command(s):\") read from the same (phase, " +
			"configuration) build step as the reported diagnostic; diagnostic_log names the log the " +
			"headline diagnostic came from, so both are traceable. " +
			"RESOURCE BOUNDS: producer work is capped before Result construction (1024 directory entries, " +
			"64 relevant logs, 32 MiB per log, 256 MiB total metadata/log work, bounded diagnostic cells), " +
			"the inner JSON is at most 256 KiB, and at most two calls scan concurrently. Any incomplete " +
			"evidence returns unknown(artifact_limit_exceeded|metadata_limit_exceeded|resource_busy|resource_cancelled); " +
			"resources.completeness, resources.omitted and resources.high_water report what was bounded. " +
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
			"buildtrees_root and build_failed_log MUST be absolute (a relative path would bind to the hub daemon's working " +
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
			"--clean-buildtrees-after-build." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{
					"type":        "string",
					"maxLength":   lastfailure.MaxPortNameBytes,
					"description": "Port name. Optional when build_failed_log names exactly one failed port.",
				},
				"triplet": map[string]any{
					"type":        "string",
					"maxLength":   lastfailure.MaxInputScalarBytes,
					"description": "Optional. Auto-detected from the port's buildtrees directory (stdout-<triplet>.log / <triplet>.vcpkg_abi_info.txt) when omitted.",
				},
				"root": map[string]any{
					"type":        "string",
					"maxLength":   lastfailure.MaxInputScalarBytes,
					"description": "Optional ABSOLUTE vcpkg root override, used to derive <root>/buildtrees when buildtrees_root is not given directly. A relative value returns unknown(relative_root).",
				},
				"buildtrees_root": map[string]any{
					"type":        "string",
					"maxLength":   lastfailure.MaxInputScalarBytes,
					"description": "Optional explicit ABSOLUTE buildtrees root override (matches vcpkg's --x-buildtrees-root). Highest precedence for locating logs. A relative value returns unknown(relative_root).",
				},
				"build_failed_log": map[string]any{
					"type":        "string",
					"maxLength":   lastfailure.MaxInputScalarBytes,
					"description": "Optional ABSOLUTE path to a build_failed.log-shaped wrapper file. Never auto-discovered. A relative value returns unknown(relative_root).",
				},
				"overlays": map[string]any{
					"type":        "array",
					"maxItems":    lastfailure.MaxInputOverlayEntries,
					"items":       map[string]any{"type": "string", "maxLength": lastfailure.MaxInputScalarBytes},
					"description": "Optional explicit overlay-ports chain, order = precedence. Echoed back only, never resolved.",
				},
			},
		},
	}, vs.lastFailureTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
		Name: "vcpkg_port_resolution",
		Description: "Determine which port definition wins across overlay ports and builtin ports, and report every location checked. " +
			"When vcpkg_root is omitted, the builtin fallback is NOT checked; the result states only what the supplied overlays established. " +
			"port must be ONE legal vcpkg port-name segment (lowercase ASCII letters, digits and hyphens, not leading/trailing) AND its joined path " +
			"must stay beneath each root -> failed(invalid_port_name), with the rejected value echoed in invalid_port. A traversal name is refused " +
			"before the join, never normalised into a directory outside the roots the caller granted." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port": map[string]any{
					"type":        "string",
					"description": "Required port name to resolve. Must be one legal vcpkg port-name segment; a path-traversal value returns failed(invalid_port_name).",
				},
				"vcpkg_root": map[string]any{
					"type":        "string",
					"description": "Optional absolute vcpkg root; enables checking its builtin ports fallback.",
				},
				"overlay_ports": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    portresolution.MaxOverlayRoots,
					"description": "Optional absolute overlay directories in precedence order; the first matching definition wins.",
				},
			},
		},
	}, vs.portResolutionTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
		Name:        "vcpkg_pin_status",
		Description: pinStatusToolDescription(),
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port_dirs": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    pinstatus.MaxPortDirs,
					"description": pinStatusPortDirsDescription(),
				},
				"disable_network": map[string]any{
					"type":        "boolean",
					"description": pinStatusDisableNetworkDescription(),
				},
			},
		},
	}, vs.pinStatusTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
		Name: "vcpkg_patches_apply",
		Description: "Statically analyze a portfile to report which patches WOULD apply for a triplet, in order. It applies nothing. " +
			patchesRootAdmissionDescription() +
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
			"silently presented as a complete one. " +
			"portfile.cmake is read through a bounded stream: a too-large file returns " +
			"unknown(portfile_size_limit_exceeded), never a parsed prefix. Orphan scan limits, " +
			"directory unreadability, and cancellation return unknown(orphan_scan_incomplete) with " +
			"orphan_scan_stop_cause. PATCHES inside a function() or macro() body, or carried through a declared " +
			"function/macro invocation such as ${ARGN}, returns unknown(patches_deferred_command_body); declaration " +
			"bodies and calls are not modeled. " +
			"port_dir MUST be absolute (a relative one would bind to the hub daemon's working directory and " +
			"answer about a different port) -> failed(relative_port_dir). An unreadable port_dir is " +
			"unknown(port_dir_unreadable), NEVER the verified-absence reason port_dir_missing. " +
			"A patch reached through a set() made under an undecided guard lands in undecidable[] exactly like " +
			"a conditionally appended list item — the scalar and list shapes carry the same tri-state." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"port_dir": map[string]any{
					"type":        "string",
					"description": "Required ABSOLUTE port directory containing portfile.cmake. A relative value returns failed(relative_port_dir).",
				},
				"triplet": map[string]any{
					"type":        "string",
					"description": "Required triplet name, used to locate its triplet file and to evaluate patch guards.",
				},
				"vcpkg_root": map[string]any{
					"type":        "string",
					"description": patchesVcpkgRootDescription(),
				},
				"overlay_triplets": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    patchesapply.MaxOverlayTripletRoots,
					"description": patchesOverlayTripletsDescription(),
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
	}, vs.patchesApplyTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
		Name: "vcpkg_cmake_trace",
		Description: "Read an EXISTING cmake --trace-format=json-v1 trace and report the executed CMake lines, expansions, and include order. " +
			"Executed lines are positive evidence only: an absent line means \"not observed in this trace\", never \"unreachable\". It never runs cmake. " +
			"Parsing STREAMS under hard ceilings (total bytes, per-line bytes, records materialized) rather than reading the file whole; any ceiling " +
			"that trips, and any malformed line, is reported in input_incomplete_reasons[] (closed enum: input_malformed, byte_limit, line_limit, " +
			"record_limit) so a bounded read is never mistaken for a complete one. Cancellation is observed and returns unknown(canceled) with no " +
			"partial result. Note truncated (the returned records cap) is a SEPARATE, narrower signal from input_incomplete. " +
			"An omitted or empty trace_path is unknown(trace_path_not_supplied) — a fact about the CALL, never " +
			"unknown(trace_not_found), which is reserved for a path that WAS supplied and verified absent. A relative path is " +
			"failed(relative_trace_path) before filesystem access. An explicit trace header with a major other than json-v1 is " +
			"unknown(unsupported_trace_version), with no partial records returned." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"trace_path": map[string]any{
					"type":        "string",
					"description": "Required absolute path to an existing json-v1 CMake trace. A relative value returns failed(relative_trace_path) before filesystem access; omitting it returns unknown(trace_path_not_supplied), never trace_not_found.",
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
	}, vs.cmakeTraceTool); err != nil {
		return err
	}

	if err := registerProjectableTool(vs, &mcp.Tool{
		Name: "cmake_include_graph",
		Description: "Thin wrapper over the hub's internal/cmakegraph static resolver: statically " +
			"resolves the CMake include()/add_subdirectory() graph WITHOUT ever invoking cmake. " +
			"Surfaces cmakegraph's per-edge Status (resolved|dangling|optional_absent|unresolved), the " +
			"Conditional flag (this edge's path IS computed; whether it executes at configure time is " +
			"a separate, genuinely unknown question requiring a real cmake trace), the closed Reason " +
			"enum, and the operator-facing Histogram VERBATIM — this tool does not re-implement or " +
			"re-interpret resolution. Use root to walk every CMakeLists.txt/*.cmake under a tree as " +
			"independent roots (the right mode for an overlay-ports tree with no single top-level " +
			"CMakeLists.txt); use file to walk a single starting file. Coverage is always reported, " +
			"never assumed: unscanned_files[] carries every hole with a CLOSED reason (byte_cap_exceeded, " +
			"file_unreadable, enumerate_failed, root_outside_workspace, root_enumeration_capped, symlink_directory_skipped, edge_cap_exceeded), so a " +
			"subtree that could not be listed appears there rather than being silently omitted from an " +
			"apparently-complete graph. Per-edge, dangling means VERIFIED absence only; a target whose " +
			"existence could not be determined (access denied, sharing violation) is " +
			"unresolved(target_unreadable)." + resultProjectionDescription,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"root": map[string]any{
					"type":        "string",
					"description": "Absolute directory to walk every entry_names match under as an independent root (cmakegraph.WalkTree). A relative root returns unknown(args_invalid) before traversal. Mutually exclusive with file — supplying BOTH is refused with unknown(args_invalid), never silently resolved in root's favour.",
				},
				"file": map[string]any{
					"type":        "string",
					"description": "Absolute single file to start the walk from (cmakegraph.Walk). A relative file returns unknown(args_invalid) before traversal. Mutually exclusive with root — supplying BOTH is refused with unknown(args_invalid).",
				},
				"workspace_root": map[string]any{
					"type":        "string",
					"description": "When explicitly supplied, an absolute boundary no resolved path may escape; a relative workspace_root returns unknown(args_invalid) before traversal. It is the value substituted for ${CMAKE_SOURCE_DIR}, honoured in BOTH modes; it may be a parent of root. Defaults to root, or file's containing directory.",
				},
				"entry_names": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"maxItems":    cmakegraph.MaxEntryFilters,
					"description": "Root mode only. Defaults to [\"CMakeLists.txt\", \"*.cmake\"].",
				},
				"max_depth":      map[string]any{"type": "integer", "maximum": cmakegraph.MaxDepthLimit, "description": "Optional traversal depth bound (per root)."},
				"max_nodes":      map[string]any{"type": "integer", "maximum": cmakegraph.MaxNodesLimit, "description": "Optional whole-tree node count bound (root ADMISSION)."},
				"max_file_bytes": map[string]any{"type": "integer", "maximum": cmakegraph.MaxFileBytesLimit, "description": "Optional per-file byte cap."},
				"max_roots":      map[string]any{"type": "integer", "maximum": cmakegraph.MaxRootsLimit, "description": "Root mode only. Optional bound on how many candidate roots are ENUMERATED before the tree walk stops (root_enumeration_capped). Separate from max_nodes, which bounds admission, not discovery."},
			},
		},
	}, vs.cmakeIncludeGraphTool); err != nil {
		return err
	}
	return nil
}

func (vs *VcpkgServer) discoverRootTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args struct {
		Root string `json:"root"`
	}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := discovery.DiscoverRoot(args.Root, discovery.DefaultDeps())
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) lastFailureTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args lastfailure.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	vs.initLastFailure()
	if ctx.Err() != nil {
		return projectableToolOutcome{result: lastfailure.ResourceResult(lastfailure.ReasonResourceCancelled)}
	}
	select {
	case vs.lastFailureSlots <- struct{}{}:
		defer func() { <-vs.lastFailureSlots }()
	default:
		return projectableToolOutcome{result: lastfailure.ResourceResult(lastfailure.ReasonResourceBusy)}
	}
	res := vs.lastFailureRun(ctx, args, vs.lastFailureDeps())
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) portResolutionTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args portresolution.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := portresolution.ResolvePortContext(ctx, args, portresolution.DefaultDeps())
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) pinStatusTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args pinstatus.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := pinstatus.PinStatus(ctx, args, pinstatus.DefaultDeps())
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) patchesApplyTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args patchesapply.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := patchesapply.ApplyOrderContext(ctx, args)
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) cmakeTraceTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args cmaketrace.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := cmaketrace.Trace(ctx, args, cmaketrace.DefaultDeps())
	return projectableToolOutcome{result: res}
}

func (vs *VcpkgServer) cmakeIncludeGraphTool(ctx context.Context, req *mcp.CallToolRequest) projectableToolOutcome {
	var args cmakewrap.Args
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return projectableToolOutcome{invalidArgument: err}
		}
	}
	res := cmakewrap.RunGraph(ctx, args)
	return projectableToolOutcome{result: res}
}
