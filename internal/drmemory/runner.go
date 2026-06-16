package drmemory

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

// runOutput is the result of one Dr. Memory invocation: the target's
// exit code (Dr. Memory forwards the instrumented process's exit code),
// the raw results.txt body, the stderr Dr. Memory itself emitted, the
// resolved path to results.txt (empty when none was produced), and the
// exact drmemory.exe command line that was executed (so a failing launch is
// reproducible / diagnosable from the structured result).
type runOutput struct {
	ExitCode    int
	ResultsText string
	Stderr      string
	ResultsPath string
	CommandLine string
}

// runFunc is the injectable seam that invokes Dr. Memory on a target.
// The default implementation is defaultRun; tests substitute a fake that
// returns a canned results.txt + temp logdir so they never run the real
// (10-50x-slow) instrumented process.
//
// Parameters:
//   - exePath:  drmemory.exe (already resolved by the findExe seam)
//   - target:   the target .exe to instrument
//   - args:     the target's own argv
//   - cwd:      working directory for the target ("" inherits parent)
//   - light:    pass -light (faster, less thorough)
//   - checkUninit: when false pass -no_check_uninitialized
type runFunc func(ctx context.Context, exePath, target string, args []string, cwd string, light, checkUninit bool) (*runOutput, error)

// drMemoryOutputCap bounds the bytes read from results.txt / stderr so a
// pathological report can't blow up memory or overrun the daemon stdio
// scanner. Matches the perftools "tool body cap < scanner cap" convention.
const drMemoryOutputCap = 512 * 1024

// defaultRun is the production runFunc. It builds the command
//
//	drmemory.exe -batch [-light] [-no_check_uninitialized] -logdir <tmp> -- <target> <args...>
//
// runs it, then locates results.txt under <tmp>\DrMemory-<exe>.<pid>.NNN\
// and reads it back. The temp logdir is removed before returning.
func defaultRun(ctx context.Context, exePath, target string, args []string, cwd string, light, checkUninit bool) (*runOutput, error) {
	logdir, err := makeLogdir()
	if err != nil {
		return nil, fmt.Errorf("create logdir: %w", err)
	}
	defer os.RemoveAll(logdir)

	cmdArgs := buildDrMemoryArgs(logdir, symcacheDir(), target, args, light, checkUninit)
	commandLine := formatCommandLine(exePath, cmdArgs)

	cmd := exec.CommandContext(ctx, exePath, cmdArgs...)
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
	// The instrumented target (and Dr. Memory itself) must see the Intel
	// oneAPI runtime DLL dirs on PATH, else an icx-built exe dies with
	// 0xC0000135 (DLL not found) ~65 ms in, BEFORE instrumentation, and
	// drmemory_run returns a useless empty result. The daemon's inherited env
	// does not carry those dirs, so inject them here.
	cmd.Env = oneAPIRuntimeEnv()
	if cwd != "" {
		cmd.Dir = cwd
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// Dr. Memory's own progress chatter and the target's stdout go to
	// stdout; we don't capture stdout here because the authoritative
	// findings live in results.txt. Discarding keeps memory bounded.
	cmd.Stdout = nil

	exitCode := 0
	runErr := cmd.Run()
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			// Non-zero target / Dr. Memory exit is expected when errors
			// are found; it is NOT a runner failure. Capture the code.
			exitCode = ee.ExitCode()
		} else {
			// Genuine spawn failure (binary not executable, DynamoRIO
			// first-run setup failing, ctx cancelled). Return a POPULATED
			// runOutput alongside the error so the handler can surface the
			// command line + captured stderr in the structured result rather
			// than dropping it into an empty answer.
			out := &runOutput{
				ExitCode:    -1,
				Stderr:      truncate(stderr.String(), drMemoryOutputCap),
				CommandLine: commandLine,
			}
			return out, fmt.Errorf("run drmemory: %w (stderr: %s)", runErr, truncate(stderr.String(), 2000))
		}
	}

	resultsPath, body := readResultsTxt(logdir)

	return &runOutput{
		ExitCode:    exitCode,
		ResultsText: body,
		Stderr:      truncate(stderr.String(), drMemoryOutputCap),
		ResultsPath: resultsPath,
		CommandLine: commandLine,
	}, nil
}

// oneAPIRuntimeEnv returns the process environment with the Intel oneAPI
// component DLL dirs prepended to PATH, so the instrumented target (and
// Dr. Memory itself) can load the MKL / TBB / compiler runtime DLLs. An
// icx-built exe otherwise dies with 0xC0000135 (DLL not found) almost
// immediately, before any instrumentation runs. Returns os.Environ() unchanged
// when no oneAPI install is present. Runtime-only (DLL dirs → PATH); drmemory
// instruments a PREBUILT exe — it does not compile or link, so it needs
// neither LIB nor INCLUDE (unlike the oneapi-run server, which captures the
// full setvars.bat environment).
func oneAPIRuntimeEnv() []string {
	root, ok := oneapi.DetectRoot()
	if !ok {
		return os.Environ()
	}
	return oneapi.PrependEnvList(os.Environ(), oneapi.PathKey, oneapi.DLLDirs(root))
}

// makeLogdir creates a fresh per-run logdir for Dr. Memory under the hub-owned
// writable scratch dir (NOT the inherited process TEMP, which on the live host
// is a small RAM disk that makes Dr. Memory log "WARNING: Unable to write to
// the disk" and risks a truncated/missing results.txt on a real target). It
// returns the created directory; the caller os.RemoveAll's it after reading
// results.txt. If the hub scratch base can't be derived or created, it falls
// back to the OS temp dir so a run still proceeds rather than failing outright.
func makeLogdir() (string, error) {
	base, ok := hubtemp.Dir("drmemory")
	if !ok {
		return os.MkdirTemp("", "drmemory-logs-")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return os.MkdirTemp("", "drmemory-logs-")
	}
	return os.MkdirTemp(base, "logs-")
}

// formatCommandLine joins the drmemory.exe path and its argv into a single
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
// space-bearing path (e.g. C:\Program Files (x86)\...) stays one token in
// the surfaced command line.
func quoteIfSpaced(s string) string {
	if strings.ContainsAny(s, " \t") {
		return `"` + s + `"`
	}
	return s
}

// buildDrMemoryArgs assembles the drmemory.exe argv. Kept separate from
// defaultRun so a test can assert the flag wiring without spawning a
// process. When symcacheDir is non-empty it is passed via -symcache_dir so
// Dr. Memory's symbol + auto-generated syscall caches persist on a writable
// disk instead of failing against the read-only install default (see
// symcacheDir for why this matters).
func buildDrMemoryArgs(logdir, symcacheDir, target string, args []string, light, checkUninit bool) []string {
	cmdArgs := []string{"-batch"}
	if light {
		cmdArgs = append(cmdArgs, "-light")
	}
	if !checkUninit {
		cmdArgs = append(cmdArgs, "-no_check_uninitialized")
	}
	if symcacheDir != "" {
		cmdArgs = append(cmdArgs, "-symcache_dir", symcacheDir)
	}
	cmdArgs = append(cmdArgs, "-logdir", logdir, "--", target)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
}

// symcacheDir returns the PERSISTENT directory Dr. Memory uses for its module
// symbol caches and its auto-generated system-call table (-symcache_dir).
// Unlike the per-run logdir (removed after each run), this MUST survive across
// runs: Dr. Memory's default -symcache_dir is <install>\logs\symcache under
// "C:\Program Files (x86)\Dr. Memory\", which a non-admin user cannot write.
// On the live host (an unknown-to-Dr.Memory Windows build) that read-only
// default means Dr. Memory cannot persist the syscall table it auto-generates
// ("Restarting to trigger auto-generation of system call information...") and
// re-generates it on EVERY run — a multi-second tax per invocation. Pointing
// -symcache_dir at a writable hub dir lets the table + symbol caches persist,
// so only the first run pays the auto-generation cost (verified live: run 2
// skipped auto-generation and returned immediately). It is a sibling of the
// ephemeral logs-* dirs under the same hub base, so makeLogdir's per-run
// os.RemoveAll never touches it. Returns "" (caller omits the flag, Dr. Memory
// falls back to its install default) when no writable base can be derived.
//
// NOTE: a residual "WARNING: Unable to write to the disk" still appears in
// Dr. Memory's stderr even with this set — it is a benign DynamoRIO-level
// message (results, symbol caches, and the syscall table all persist correctly
// and the run exits 0; verified live). It is deliberately NOT filtered from the
// surfaced stderr: swallowing it would hide a genuine disk-full / permissions
// failure if one ever arises.
func symcacheDir() string {
	base, ok := hubtemp.Dir("drmemory")
	if !ok {
		return ""
	}
	dir := filepath.Join(base, "symcache")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	return dir
}

// readResultsTxt finds the results.txt Dr. Memory wrote under logdir.
// Dr. Memory creates a per-run subdir DrMemory-<exe>.<pid>.NNN/ holding
// results.txt. When several runs share a logdir the newest subdir wins.
// Returns ("", "") when no results.txt is present (e.g. Dr. Memory failed
// before writing one). The body is capped at drMemoryOutputCap.
func readResultsTxt(logdir string) (path string, body string) {
	entries, err := os.ReadDir(logdir)
	if err != nil {
		return "", ""
	}

	var candidate string
	var candidateMod int64 = -1
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(logdir, e.Name(), "results.txt")
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if mod := info.ModTime().UnixNano(); mod > candidateMod {
			candidateMod = mod
			candidate = p
		}
	}
	if candidate == "" {
		// Some Dr. Memory configurations write results.txt directly under
		// logdir; check that as a fallback.
		direct := filepath.Join(logdir, "results.txt")
		if isExecutableFile(direct) {
			candidate = direct
		} else {
			return "", ""
		}
	}

	raw, err := os.ReadFile(candidate)
	if err != nil {
		return candidate, ""
	}
	return candidate, truncate(string(raw), drMemoryOutputCap)
}

// truncate caps s at n bytes, appending an ellipsis marker so callers and
// operators can tell the output was clipped.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…[truncated]"
}
