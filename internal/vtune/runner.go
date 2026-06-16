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
	"time"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/oneapi"
	"mcp-local-hub/internal/process"
)

// waitDelayAfterKill bounds how long cmd.Wait blocks after the context deadline
// kills vtune.exe / the profiled target, so a grandchild holding the pipe can't
// hang Wait past the timeout (mirrors oneapirun / drmemory). Paired with
// process.RunUnderKillJob, which reaps the grandchild via KILL_ON_JOB_CLOSE.
const waitDelayAfterKill = 2 * time.Second

// staleRunDirTTL bounds how long an abandoned per-run result dir may linger
// before a later run sweeps it. Far above the default vtune timeout so a live
// run's dir is never swept.
const staleRunDirTTL = time.Hour

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
	// ResultDir is the absolute path to the per-run VTune result dir, populated
	// ONLY when the caller passed keep_result=true (otherwise the dir is deleted
	// before returning and this stays ""). An agent feeds this path back to
	// vtune_report to re-generate a report without re-profiling.
	ResultDir string
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
//   - keepResult: when true the per-run result dir is NOT deleted and its
//     absolute path is returned in runOutput.ResultDir so a later vtune_report
//     call can re-report against it without re-profiling.
type runFunc func(ctx context.Context, exePath, target string, args []string, cwd, analysis string, keepResult bool) (*runOutput, error)

// reportFunc is the injectable seam that re-runs only VTune's report phase
// against an EXISTING result dir (one a prior keep_result profile left
// behind), producing a fresh CSV table + summary WITHOUT re-profiling. The
// default implementation is defaultReport; tests substitute a fake so they
// never invoke the real reporter.
//
// Parameters:
//   - exePath:   vtune.exe (already resolved by the findExe seam)
//   - resultDir: the absolute path to the existing VTune result dir (-r)
//   - analysis:  the validated analysis type (only used to choose the report
//     name, which is always "hotspots" today — see reportName)
type reportFunc func(ctx context.Context, exePath, resultDir, analysis string) (*runOutput, error)

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
func defaultRun(ctx context.Context, exePath, target string, args []string, cwd, analysis string, keepResult bool) (*runOutput, error) {
	base, ok := hubtemp.Dir("vtune")
	if !ok {
		return nil, fmt.Errorf("derive vtune scratch dir")
	}
	// 0o700: owner-only, so a co-resident user on a multi-tenant POSIX host
	// cannot read another user's VTune result DB (function names, hot paths) or
	// plant a symlink at the report path (the readReportFile Lstat guard is the
	// second layer).
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create vtune scratch base: %w", err)
	}
	// Reap result dirs abandoned by an abnormally-terminated prior run (timeout
	// force-kill, daemon restart, reboot mid-run); they hold a full result DB
	// (often 50-500 MB) so unbounded accumulation is real. Prefix-scoped to
	// "run-", so any shared sibling is untouched. A keep_result dir is subject
	// to the same TTL sweep, so a kept-but-forgotten dir is reclaimed after
	// staleRunDirTTL rather than leaking forever.
	hubtemp.SweepStale(base, "run-", staleRunDirTTL)
	// runDir holds the per-run result dir + the report output files. VTune
	// REQUIRES the result dir (-r) to be FRESH/nonexistent (it creates it), so
	// we hand it a not-yet-created subpath of a freshly-made run dir.
	runDir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return nil, fmt.Errorf("create vtune run dir: %w", err)
	}
	cleanupActive, err := hubtemp.MarkActive(runDir)
	if err != nil {
		_ = os.RemoveAll(runDir)
		return nil, fmt.Errorf("mark vtune run dir active: %w", err)
	}
	defer cleanupActive()
	// keep_result keeps the per-run result dir on disk and returns its path so
	// vtune_report can re-report against it without re-profiling. The default
	// (keepResult=false) deletes the whole run dir on return, as before.
	if !keepResult {
		defer os.RemoveAll(runDir)
	}

	resultDir := filepath.Join(runDir, "result")
	csvPath := filepath.Join(runDir, "report.csv")
	summaryPath := filepath.Join(runDir, "summary.txt")

	env := vtuneEnv()
	var stderr bytes.Buffer
	var cmdLines []string

	// keptDir is the result-dir path surfaced to the caller only when
	// keepResult is set (so a default run never advertises a path it just
	// deleted).
	keptDir := ""
	if keepResult {
		keptDir = resultDir
	}

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
				ResultDir:   keptDir,
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
				ResultDir:   keptDir,
			}, fmt.Errorf("run vtune collect: %w (stderr: %s)", err, truncate(stderr.String(), 2000))
		}
	}

	// --- Phases 2 & 3: report (CSV table) + summary (human-readable text) ---
	out, err := runReportPhases(ctx, exePath, resultDir, csvPath, summaryPath, analysis, cwd, env, &stderr, &cmdLines)
	out.ExitCode = exitCode
	out.ResultDir = keptDir
	return out, err
}

// runReportPhases runs VTune's two read-only report phases (the CSV hotspots
// table + the human-readable summary) against an EXISTING resultDir, reads the
// outputs back, and returns a populated runOutput. It is shared by defaultRun
// (after a fresh collect) and defaultReport (re-reporting a kept result dir),
// so the report-phase flag wiring + non-fatal-failure + timeout handling live
// in ONE place. It does NOT set ExitCode/ResultDir — the caller owns those (a
// re-report has no target exit code to forward). cmdLines is appended in place
// so the caller's command-line accumulator captures every phase. A returned
// error is non-nil ONLY on a timeout (DeadlineExceeded); a plain report
// failure is folded into stderr and returned with a nil error, since a report
// that fails to render is still a successful "we tried" result the caller
// surfaces.
func runReportPhases(ctx context.Context, exePath, resultDir, csvPath, summaryPath, analysis, cwd string, env []string, stderr *bytes.Buffer, cmdLines *[]string) (*runOutput, error) {
	// --- report (CSV table) -------------------------------------------------
	reportArgs := buildReportArgs(reportName(analysis), resultDir, csvPath)
	*cmdLines = append(*cmdLines, formatCommandLine(exePath, reportArgs))
	// A report failure is non-fatal: collect already succeeded, so surface
	// whatever we have. The error is folded into stderr for diagnosis.
	if err := runVTunePhase(ctx, exePath, reportArgs, cwd, env, stderr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &runOutput{
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(*cmdLines, "\n"),
				TimedOut:    true,
			}, err
		}
		fmt.Fprintf(stderr, "\nvtune report (%s) failed: %v\n", reportName(analysis), err)
	}

	// --- summary (human-readable text) --------------------------------------
	summaryArgs := buildReportArgs("summary", resultDir, summaryPath)
	*cmdLines = append(*cmdLines, formatCommandLine(exePath, summaryArgs))
	if err := runVTunePhase(ctx, exePath, summaryArgs, cwd, env, stderr); err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return &runOutput{
				ReportCSV:   readReportFile(csvPath),
				ReportPath:  reportPathIfPresent(csvPath),
				Stderr:      truncate(stderr.String(), vtuneOutputCap),
				CommandLine: strings.Join(*cmdLines, "\n"),
				TimedOut:    true,
			}, err
		}
		fmt.Fprintf(stderr, "\nvtune report (summary) failed: %v\n", err)
	}

	return &runOutput{
		ReportCSV:   readReportFile(csvPath),
		Summary:     readReportFile(summaryPath),
		Stderr:      truncate(stderr.String(), vtuneOutputCap),
		ReportPath:  reportPathIfPresent(csvPath),
		CommandLine: strings.Join(*cmdLines, "\n"),
	}, nil
}

// defaultReport is the production reportFunc. It re-runs ONLY VTune's report
// phases (no collect, no target execution) against an EXISTING result dir a
// prior keep_result profile left behind, writing the CSV + summary into a
// FRESH per-run scratch dir (always cleaned up — re-reporting never keeps its
// own output dir). The supplied resultDir is read-only here; this never
// mutates or deletes the caller's kept result dir.
func defaultReport(ctx context.Context, exePath, resultDir, analysis string) (*runOutput, error) {
	if info, err := os.Stat(resultDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("result_dir is not an existing VTune result directory: %s", resultDir)
	}

	base, ok := hubtemp.Dir("vtune")
	if !ok {
		return nil, fmt.Errorf("derive vtune scratch dir")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, fmt.Errorf("create vtune scratch base: %w", err)
	}
	// The report outputs (CSV + summary) go into a throwaway dir, NOT the
	// caller's result dir, so the kept result dir stays pristine for further
	// re-reports. "report-" prefix keeps it out of the "run-" stale sweep.
	outDir, err := os.MkdirTemp(base, "report-")
	if err != nil {
		return nil, fmt.Errorf("create vtune report dir: %w", err)
	}
	defer os.RemoveAll(outDir)

	csvPath := filepath.Join(outDir, "report.csv")
	summaryPath := filepath.Join(outDir, "summary.txt")

	env := vtuneEnv()
	var stderr bytes.Buffer
	var cmdLines []string

	out, err := runReportPhases(ctx, exePath, resultDir, csvPath, summaryPath, analysis, "", env, &stderr, &cmdLines)
	// A re-report has no target exit code to forward, so ExitCode stays 0.
	return out, err
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
	cmd.WaitDelay = waitDelayAfterKill
	cmd.Env = env
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stderr = stderr
	cmd.Stdout = nil
	// RunUnderKillJob reaps the profiled target's subtree on a timeout kill so a
	// grandchild can't orphan + hold its pipe/port; WaitDelay bounds the Wait.
	return process.RunUnderKillJob(cmd)
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
	// RuntimeEnv is the full setvars PATH MINUS build-only keys (INCLUDE/LIB/…):
	// VTune profiles a PREBUILT exe, so the target needs the runtime DLLs on PATH
	// but never the build toolchain's search paths (least privilege for the
	// arbitrary profiled target — it no longer sees the host's INCLUDE/LIB layout).
	if env := oneapi.RuntimeEnv(); env != nil {
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
// `-report <name>` CSV-table phase. CRITICAL: VTune's -report names are a
// FIXED closed set (summary, hotspots, hw-events, callstacks, top-down,
// gprof-cc, gpu-computing-tasks) — they are NOT the -collect analysis types.
// `vtune -report memory-access` errors "Cannot find report `memory-access'"
// (verified live), so an identity mapping silently produced an EMPTY
// top_hotspots table for every analysis except "hotspots". "hotspots" is the
// universal function-level table report and renders against ANY collect result
// (verified live: `-report hotspots` on a threading collect yields a populated
// CSV). So the structured function table always uses "hotspots"; the
// analysis-specific metrics come through the separate hard-coded "summary"
// report. analysis is accepted (and ignored) so this stays the single mapping
// owner if a future analysis ever needs a different table report.
func reportName(analysis string) string {
	_ = analysis
	return "hotspots"
}

// readReportFile reads a VTune report output file, capping it at
// vtuneOutputCap. Returns "" when the file is absent (the report phase failed
// before writing it) or unreadable.
func readReportFile(path string) string {
	// Lstat-guard before ReadFile: a co-resident TOCTOU attacker who planted a
	// symlink at path (between VTune's write and this read, on a host where the
	// scratch dir is not owner-only) would otherwise leak the symlink target's
	// bytes into the structured result. Read only a real regular file the run
	// itself wrote; 0o700 on the scratch base is the first layer.
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return ""
	}
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
	// Lstat (not isExecutableFile, whose os.Stat follows symlinks): report only a
	// real regular file the run wrote, never a symlink planted at the path.
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
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
