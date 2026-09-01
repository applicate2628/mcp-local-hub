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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"mcp-local-hub/internal/hubtemp"
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

// VTuneServer holds the MCP server instance plus the injectable seams used by
// the tool handlers:
//
//   - findExe resolves vtune.exe (default: findVTune, probing
//     %VTUNE_PROFILER_DIR% / the known oneAPI install dirs / PATH).
//   - run invokes VTune (collect + report) on a target and returns the raw
//     report CSV + summary text (default: defaultRun).
//   - report re-runs ONLY VTune's report phase against an existing result dir
//     (default: defaultReport), backing vtune_report.
//   - probeVersion resolves+probes vtune for availability/version/SEP-driver
//     readiness (default: defaultVersionProbe), backing vtune_status.
//   - listAnalyses asks vtune for the host's actual supported collect types
//     (default: defaultListAnalyses), backing vtune_list_analyses.
//
// Tests construct a VTuneServer with fakes for the seams so the handlers are
// exercised end-to-end without a real VTune install.
type VTuneServer struct {
	server       *mcp.Server
	findExe      func() (string, error)
	run          runFunc
	report       reportFunc
	probeVersion versionProbeFunc
	listAnalyses listAnalysesFunc
	owner        *vtuneRunOwnerV1
}

// Run wires up a fresh VTuneServer with the production seams, registers the
// profile tool, and serves the MCP protocol over stdio until ctx is
// cancelled or the transport closes. It is the single source of truth for
// every entry point; keep runtime behavior here.
func Run(ctx context.Context) error {
	runRoot, ok := hubtemp.Dir("vtune")
	if !ok {
		return fmt.Errorf("derive vtune run root")
	}
	owner, err := newVTuneRunOwnerV1(runRoot, newDefaultVTunePhaseDriver())
	if err != nil {
		return fmt.Errorf("initialize vtune run owner: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = owner.Close(shutdownCtx)
	}()
	vs := &VTuneServer{
		findExe:      findVTune,
		run:          defaultRun,
		report:       defaultReport,
		probeVersion: defaultVersionProbe,
		listAnalyses: defaultListAnalyses,
		owner:        owner,
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

// registerTools attaches the vtune tools. Called once from Run during startup.
//
// vtune_status and vtune_list_analyses are PURE PROBES of vtune itself (they
// run `vtune --version` / `vtune -collect-list`, never a caller-supplied
// executable), so they register UNCONDITIONALLY — a status/availability probe
// stays useful even when the unsafe profiling surface is off (and matches
// gdb's always-on debugger_status).
//
// vtune_profile and vtune_report run vtune against a caller-controlled
// target / result dir as part of the profiling workflow, so they register ONLY
// after the explicit unsafe opt-in. When the opt-in is absent the daemon still
// serves MCP — it just exposes the two probe tools, not the profiling pair
// (unsafegate.RegisterAllowed logs WHY to stderr so the secure-default is
// observable), so a misconfigured client cannot reach the arbitrary-exec
// surface.
func registerTools(vs *VTuneServer) {
	registerStatusTools(vs)

	if !unsafegate.RegisterAllowed(enableUnsafeVtuneEnv, "vtune") {
		return
	}
	registerProfileTools(vs)
}

// registerStatusTools attaches the always-on probe tools (vtune_status,
// vtune_list_analyses). Neither runs a caller-supplied executable, so neither
// is gated.
func registerStatusTools(vs *VTuneServer) {
	vs.server.AddTool(&mcp.Tool{
		Name: "vtune_status",
		Description: "Report whether the Intel VTune Profiler is available and at what version, plus a " +
			"best-effort note on SEP hardware-sampling-driver readiness. Resolves vtune.exe (via " +
			"%VTUNE_PROFILER_DIR% / the oneAPI install dirs / PATH) and runs `<vtune> --version` via Go " +
			"exec — which works in the console-less mcphub daemon. Runs NO profiling and executes no " +
			"target. Returns JSON {available, vtune_path, version, sep_driver_note}. The sep_driver_note " +
			"is informational: user-mode analyses (hotspots, threading) need no driver; hardware-event " +
			"analyses (memory-access, uarch-exploration, memory-consumption) need the SEP driver / admin.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, vs.statusTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vtune_list_analyses",
		Description: "List the analysis (collect) types the HOST's installed VTune actually supports, by " +
			"running `<vtune> -collect-list` via Go exec. This is the host's real capability set (it " +
			"varies by VTune version and CPU), surfaced alongside this server's validation allowlist — " +
			"vtune_profile still gates analysis_type against the allowlist, but this tool shows what the " +
			"host advertises so a caller can pick a type the install really has. Runs NO profiling and " +
			"executes no target. Returns JSON {available, vtune_path, host_analyses:[...], " +
			"allowed_analyses:[...], raw}.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}, vs.listAnalysesTool)
}

// registerProfileTools attaches the gated profiling pair (vtune_profile,
// vtune_report). Called only after the unsafe opt-in.
func registerProfileTools(vs *VTuneServer) {
	vs.server.AddTool(&mcp.Tool{
		Name: "vtune_profile",
		Description: "Profile a native executable under the Intel VTune Profiler and return its top CPU " +
			"hotspots plus the textual performance summary. VTune samples the ALREADY-BUILT binary at " +
			"runtime (no recompile/relink), so it works on optimized release builds. The default " +
			"analysis_type 'hotspots' uses user-mode sampling and needs no admin/SEP driver. Sampling " +
			"slows the target modestly, so set timeout_sec generously for long-running targets. Set " +
			"keep_result=true to retain the result dir and re-report later with vtune_report (its path " +
			"is returned as result_dir). Returns structured JSON: {exit_code, analysis_type, summary, " +
			"top_hotspots:[{function, module, cpu_time_seconds, metrics}], report_path, result_dir, " +
			"command_line, stderr, timed_out}.",
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
				"keep_result": map[string]any{
					"type": "boolean",
					"description": "When true, the per-run VTune result dir is NOT deleted; its absolute path is " +
						"returned as result_dir so vtune_report can re-generate a report from it without " +
						"re-profiling. Default false (the result dir is deleted after the report is read). Kept " +
						"dirs are still reclaimed by a TTL sweep on a later run, so a forgotten dir does not leak " +
						"forever.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Hard wall-clock limit in seconds for the collect+report run. Default 600 (10 min).",
				},
				"action": map[string]any{
					"type":        "string",
					"description": "Optional lifecycle action: run (default synchronous compatibility mode), start (durable async), status, or stop. Long work must use start because it survives the MCP call.",
				},
				"run_id":          map[string]any{"type": "string", "description": "Durable run id returned by action=start; required for status and stop."},
				"idempotency_key": map[string]any{"type": "string", "description": "Optional key for action=start. Repeating identical input returns the same run; changed input is rejected."},
				"operation_id":    map[string]any{"type": "string", "description": "Required unique operation id for action=stop; repeating it is idempotent."},
				"stop":            map[string]any{"type": "boolean", "description": "Must be true for action=stop; omitted/false never stops a collection."},
				"wait_sec":        map[string]any{"type": "integer", "description": "Reserved bounded status/report wait budget in seconds."},
			},
			// exe is validated by the handler for run/start. status and stop use
			// only run_id, so JSON Schema cannot require exe unconditionally.
		},
	}, vs.profileTool)

	vs.server.AddTool(&mcp.Tool{
		Name: "vtune_report",
		Description: "Re-generate a report from an EXISTING VTune result dir (one a prior vtune_profile run " +
			"left behind via keep_result=true) WITHOUT re-profiling — it re-runs only VTune's read-only " +
			"report phase against result_dir, so it is fast and does NOT execute the target again. Use it " +
			"to get the hotspots table + summary again, or after an interrupted profile that kept its " +
			"result dir. The supplied result dir is read-only here (never mutated or deleted). Returns the " +
			"SAME structured JSON shape as vtune_profile (exit_code is 0 — a re-report has no target exit " +
			"code to forward).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"result_dir": map[string]any{
					"type":        "string",
					"description": "Absolute path to an existing VTune result dir (the result_dir returned by a prior vtune_profile run with keep_result=true).",
				},
				"analysis_type": map[string]any{
					"type": "string",
					"description": "Analysis type the result dir was collected with (default hotspots). Only used to " +
						"select the report name; the universal 'hotspots' report renders against any collect result.",
				},
				"timeout_sec": map[string]any{
					"type":        "integer",
					"description": "Hard wall-clock limit in seconds for the report run. Default 600 (10 min).",
				},
				"run_id": map[string]any{
					"type":        "string",
					"description": "Durable run id returned by vtune_profile action=start. When supplied, reads the finalized receipt; pending runs return a structured snapshot without invoking VTune.",
				},
				"wait_sec": map[string]any{"type": "integer", "description": "Optional bounded wait for run_id settlement before returning its snapshot."},
			},
			// result_dir is required only for raw re-reporting; durable run_id is
			// the alternative identity and is validated by the handler.
		},
	}, vs.reportTool)
}
