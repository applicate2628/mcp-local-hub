package vtune

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/oneapi"
	"mcp-local-hub/internal/process"
)

// runOutput is the result of one VTune collect+report invocation: the
// target's exit code (VTune forwards the profiled process's exit code from
// the collect phase), the raw report CSV body, the report SUMMARY text, the
// combined stderr VTune emitted across both phases, the resolved path to the
// CSV report file (empty when none was produced), the exact vtune.exe
// command line(s) executed (so a failing launch is reproducible /
// diagnosable from the structured result), and whether the run timed out.
type runOutput struct {
	ExitCode    int
	ReportCSV   string
	Summary     string
	Stderr      string
	ReportPath  string
	CommandLine string
	TimedOut    bool
}

// runFunc is the injectable seam that profiles a target under VTune. The
// default implementation is defaultRun; tests substitute a fake that returns
// a canned report CSV + summary so they never run the real (slow,
// target-requiring) profiler.
//
// Parameters:
//   - exePath: vtune.exe (already resolved by the findExe seam)
//   - target:  the target .exe to profile
//   - args:    the target's own argv
//   - cwd:     working directory for the target ("" inherits parent)
//   - analysis: the validated analysis type (e.g. "hotspots")
type runFunc func(ctx context.Context, exePath, target string, args []string, cwd, analysis string) (*runOutput, error)

// vtuneOutputCap bounds the bytes read from the report CSV / summary / stderr
// so a pathological report can't blow up memory or overrun the daemon stdio
// scanner. Matches the drmemory "tool body cap < scanner cap" convention.
const vtuneOutputCap = 512 * 1024

// defaultRun is the production runFunc. It runs VTune in two phases against a
// FRESH per-run result dir:
//
//	collect: vtune.exe -collect <analysis> -r <resultdir> -- <target> <args...>
//	report:  vtune.exe -report <reportName(analysis)> -r <resultdir>
//	             -format=csv -report-output <csvfile>
//	         vtune.exe -report summary -r <resultdir> -report-output <sumfile>
//
// then reads the CSV + summary files back. The per-run result dir (which
// VTune itself creates and populates with its result DB) is removed before
// returning. A non-zero collect exit is captured (the profiled target failed
// or returned non-zero), NOT treated as a runner failure — the report is
// still generated from whatever data was collected.
func defaultRun(ctx context.Context, exePath, target string, args []string, cwd, analysis string) (*runOutput, error) {
	base, ok := hubtemp.Dir("vtune")
	if !ok {
		return nil, fmt.Errorf("derive vtune scratch dir")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("create vtune scratch base: %w", err)
	}
	// runDir holds the per-run result dir + the report output files. VTune
	// REQUIRES the result dir (-r) to be FRESH/nonexistent (it creates it), so
	// we hand it a not-yet-created subpath of a freshly-made run dir.
	runDir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return nil, fmt.Errorf("create vtune run dir: %w", err)
	}
	defer os.RemoveAll(runDir)

	resultDir := filepath.Join(runDir, "result")
	csvPath := filepath.Join(runDir, "report.csv")
	summaryPath := filepath.Join(runDir, "summary.txt")

	env := vtuneEnv()
	var stderr bytes.Buffer
	var cmdLines []string

	// --- Phase 1: collect ---------------------------------------------------
	collectArgs := buildCollectArgs(analysis, resultDir, target, args)
	cmdLines = append(cmdLines, formatCommandLine(exePath, collectArgs))

	exitCode := 0
	if err := runVTunePhase(ctx, exePath, collectArgs, cwd, env, &stderr); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Non-zero collect exit means the profiled target (or VTune) exited
			// non-zero — expected for a target that itself returns non-zero. It
			// is NOT a runner failure; capture the code and still try to report.
			exitCode = ee.ExitCode()
		} else if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &runOutput{
				ExitCode:    -1,
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(cmdLines, "\n"),
				TimedOut:    true,
			}, err
		} else {
			// Genuine spawn failure (vtune.exe not executable, missing oneAPI
			// runtime, ctx cancelled). Return a POPULATED runOutput alongside
			// the error so the handler can surface the command line + captured
			// stderr in the structured result rather than dropping it.
			return &runOutput{
				ExitCode:    -1,
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(cmdLines, "\n"),
			}, fmt.Errorf("run vtune collect: %w (stderr: %s)", err, truncate(stderr.String(), 2000))
		}
	}

	// --- Phase 2: report (CSV table) ----------------------------------------
	reportArgs := buildReportArgs(reportName(analysis), resultDir, csvPath)
	cmdLines = append(cmdLines, formatCommandLine(exePath, reportArgs))
	// A report failure is non-fatal: collect already succeeded, so surface
	// whatever we have. The error is folded into stderr for diagnosis.
	if err := runVTunePhase(ctx, exePath, reportArgs, cwd, env, &stderr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &runOutput{
				ExitCode:    exitCode,
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(cmdLines, "\n"),
				TimedOut:    true,
			}, err
		}
		fmt.Fprintf(&stderr, "\nvtune report (%s) failed: %v\n", reportName(analysis), err)
	}

	// --- Phase 3: summary (human-readable text) -----------------------------
	summaryArgs := buildReportArgs("summary", resultDir, summaryPath)
	cmdLines = append(cmdLines, formatCommandLine(exePath, summaryArgs))
	if err := runVTunePhase(ctx, exePath, summaryArgs, cwd, env, &stderr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &runOutput{
				ExitCode:    exitCode,
				ReportCSV:   readReportFile(csvPath),
				ReportPath:  reportPathIfPresent(csvPath),
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(cmdLines, "\n"),
				TimedOut:    true,
			}, err
		}
		fmt.Fprintf(&stderr, "\nvtune report (summary) failed: %v\n", err)
	}

	return &runOutput{
		ExitCode:    exitCode,
		ReportCSV:   readReportFile(csvPath),
		Summary:     readReportFile(summaryPath),
		Stderr:      truncate(stderr.String(), vtuneOutputCap),
		ReportPath:  reportPathIfPresent(csvPath),
		CommandLine: strings.Join(cmdLines, "\n"),
	}, nil
}

// runVTunePhase runs one vtune.exe invocation, appending its stderr to the
// shared buffer. VTune's progress chatter and the target's stdout go to
// stdout; we discard stdout because the authoritative report lives in the
// -report-output file. The Intel oneAPI runtime env (env) is applied so an
// icx-built / MKL-linked target loads its DLLs instead of dying with
// 0xC0000135 (DLL not found) before profiling.
func runVTunePhase(ctx context.Context, exePath string, args []string, cwd string, env []string, stderr *bytes.Buffer) error {
	cmd := exec.CommandContext(ctx, exePath, args...)
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stderr = stderr
	cmd.Stdout = nil
	return cmd.Run()
}

// vtuneEnv returns the environment VTune (and the profiled target) runs
// under, so an icx-built target loads the oneAPI runtime DLLs it links
// instead of dying with 0xC0000135 (DLL not found) before profiling.
//
// It prefers the COMPLETE setvars.bat environment — the SAME env the
// oneapi-run command runner and drmemory's instrumented-target spawn use —
// so the target's PATH covers EVERY oneAPI component dir setvars adds
// (compiler redist, clang runtime, mpi, …), not just the
// "<component>\latest\bin" dirs the fast DLLDirs enumeration covers. That
// matters for a heavier target whose linked DLLs live outside those bin
// dirs. When setvars can't be captured (no oneAPI install, or a non-Windows
// host) it falls back to os.Environ() + the hand-enumerated DLLDirs on PATH
// (runtime PATH only — VTune profiles a PREBUILT exe, so it never needs LIB
// or INCLUDE), and to a plain os.Environ() when there is no oneAPI at all.
// Mirrors drmemory/runner.go's oneAPIRuntimeEnv.
func vtuneEnv() []string {
	if env, ok := oneapi.SetvarsEnv(); ok {
		return env
	}
	root, ok := oneapi.DetectRoot()
	if !ok {
		return os.Environ()
	}
	return oneapi.PrependEnvList(os.Environ(), oneapi.PathKey, oneapi.DLLDirs(root))
}

// buildCollectArgs assembles the vtune.exe collect-phase argv. Kept separate
// from defaultRun so a test can assert the flag wiring without spawning a
// process. The "--" separates VTune's own flags from the target's argv so a
// target argument that looks like a VTune flag is not consumed by VTune.
func buildCollectArgs(analysis, resultDir, target string, args []string) []string {
	cmdArgs := []string{"-collect", analysis, "-r", resultDir, "--", target}
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

// buildReportArgs assembles a vtune.exe report-phase argv that writes a CSV
// (or, for the "summary" report, text) report into outPath. -report-output
// is LOAD-BEARING: without it VTune interleaves its "vtune: Executing actions
// …" progress chatter into the same stdout stream as the report body, so a
// stdout capture would mix chatter into the CSV. Writing to a file yields a
// clean report we read back. -format=csv yields the TAB-delimited table the
// parser consumes (the "summary" report ignores -format=csv and prints text,
// which is exactly what we want for the human-readable summary).
func buildReportArgs(reportName, resultDir, outPath string) []string {
	return []string{"-report", reportName, "-r", resultDir, "-format=csv", "-report-output", outPath}
}

// knownAnalysisTypes is the allowlist of VTune analysis types this server
// accepts (mapped to true). The handler validates analysis_type against this
// set and rejects anything else with a clear error, so an unknown/mistyped
// analysis never reaches vtune.exe.
var knownAnalysisTypes = map[string]bool{
	"hotspots":           true,
	"memory-access":      true,
	"threading":          true,
	"uarch-exploration":  true,
	"memory-consumption": true,
}

// defaultAnalysisType is applied when the caller omits analysis_type.
const defaultAnalysisType = "hotspots"

// reportName maps an analysis type to the VTune report name used in the
// `-report <name>` phase. VTune's report name matches the analysis type for
// the types we support (hotspots→hotspots, threading→threading, …), so this
// is identity today; it exists as the single mapping owner so a future
// analysis whose report name differs from its collect name is handled in one
// place rather than scattered.
func reportName(analysis string) string {
	return analysis
}

// readReportFile reads a VTune report output file, capping it at
// vtuneOutputCap. Returns "" when the file is absent (the report phase failed
// before writing it) or unreadable.
func readReportFile(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return truncate(string(raw), vtuneOutputCap)
}

// reportPathIfPresent returns path when it names an existing regular file,
// else "" — so the structured result's report_path is empty when VTune never
// wrote the CSV (a swallowed report failure the handler surfaces explicitly).
func reportPathIfPresent(path string) string {
	if isExecutableFile(path) {
		return path
	}
	return ""
}

// formatCommandLine joins the vtune.exe path and its argv into a single
// human-readable command line for the structured result, quoting any token
// that contains whitespace so a reader can paste it back into a shell.
func formatCommandLine(exePath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteIfSpaced(exePath))
	for _, a := range args {
		parts = append(parts, quoteIfSpaced(a))
	}
	return strings.Join(parts, " ")
}

// quoteIfSpaced wraps s in double quotes when it contains whitespace, so a
// space-bearing path (e.g. C:\Program Files (x86)\...) stays one token in the
// surfaced command line.
func quoteIfSpaced(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// truncate caps s at n bytes, appending an ellipsis marker so callers and
// operators can tell the output was clipped.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
