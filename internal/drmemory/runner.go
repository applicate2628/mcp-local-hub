package drmemory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/oneapi"
	"mcp-local-hub/internal/process"
)

// waitDelayAfterKill bounds how long cmd.Wait blocks AFTER the context deadline
// kills the direct child, waiting for the target's stdout/stderr pipes to close.
// Without it a killed target whose GRANDCHILD still holds the pipe hangs Wait
// until the grandchild exits on its own, defeating timeout_sec (mirrors
// oneapirun.waitDelayAfterKill). Paired with process.RunUnderKillJob, which then
// reaps that grandchild via KILL_ON_JOB_CLOSE.
const waitDelayAfterKill = 2 * time.Second

// staleRunDirTTL bounds how long an abandoned per-run logdir may linger before a
// later run sweeps it. Far above the default drmemory timeout (20 min) so a live
// run's dir is never swept.
const staleRunDirTTL = time.Hour

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
	cmd.WaitDelay = waitDelayAfterKill
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
	// RunUnderKillJob binds the whole target subtree to a KILL_ON_JOB_CLOSE job
	// so a grandchild that outlives the timeout-killed direct child is reaped
	// instead of orphaning + holding its pipe/port. WaitDelay above bounds the
	// Wait; the job reaps the orphan.
	runErr := process.RunUnderKillJob(cmd)
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

// oneAPIRuntimeEnv returns the environment the instrumented target (and
// Dr. Memory itself) runs under, so an icx-built target loads the oneAPI
// runtime DLLs it links instead of dying with 0xC0000135 (DLL not found)
// before instrumentation.
//
// It prefers the COMPLETE setvars.bat environment — the SAME env the oneapi-run
// command runner uses — so the target's PATH covers EVERY oneAPI component dir
// setvars adds (compiler redist, clang runtime, mpi, …), not just the
// "<component>\latest\bin" dirs the fast DLLDirs enumeration covers. That
// matters for a heavier target whose linked DLLs live outside those bin dirs.
// When setvars can't be captured (no oneAPI install, or a non-Windows host) it
// falls back to os.Environ() + the hand-enumerated DLLDirs on PATH (runtime
// PATH only — drmemory instruments a PREBUILT exe, so it never needs LIB or
// INCLUDE), and to a plain os.Environ() when there is no oneAPI at all.
func oneAPIRuntimeEnv() []string {
	// RuntimeEnv is the full setvars PATH MINUS the build-only keys (INCLUDE /
	// LIB / …): drmemory instruments a PREBUILT exe, so the target needs the
	// runtime DLLs on PATH but never the build toolchain's search paths
	// (least-privilege for the arbitrary instrumented target).
	if env := oneapi.RuntimeEnv(); env != nil {
		return env
	}
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
	// 0o700: owner-only, so a co-resident user on a multi-tenant POSIX host
	// can't read another user's Dr. Memory output (module paths, findings).
	if err := os.MkdirAll(base, 0o700); err != nil {
		return os.MkdirTemp("", "drmemory-logs-")
	}
	// Reap logdirs abandoned by an abnormally-terminated prior run (timeout
	// force-kill, daemon restart, reboot mid-run) so they don't accumulate; the
	// shared "symcache" sibling is left untouched (prefix-scoped to "logs-").
	hubtemp.SweepStale(base, "logs-", staleRunDirTTL)
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
// ephemeral logs-* dirs on Windows, and under the user's cache directory on
// non-Windows so persistent cache files are not stored below a shared temp base.
// Returns "" (caller omits the flag, Dr. Memory falls back to its install
// default) when no writable base can be derived.
//
// NOTE: a residual "WARNING: Unable to write to the disk" still appears in
// Dr. Memory's stderr even with this set — it is a benign DynamoRIO-level
// message (results, symbol caches, and the syscall table all persist correctly
// and the run exits 0; verified live). It is deliberately NOT filtered from the
// surfaced stderr: swallowing it would hide a genuine disk-full / permissions
// failure if one ever arises.
func symcacheDir() string {
	base, ok := symcacheBaseDir()
	if !ok {
		return ""
	}
	dir := filepath.Join(base, "symcache")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	if runtime.GOOS != "windows" {
		// Tighten permissions on pre-existing cache dirs so persistent symbol and
		// syscall caches are not exposed to other local users.
		if err := os.Chmod(dir, 0o700); err != nil {
			return ""
		}
	}
	return dir
}

func symcacheBaseDir() (string, bool) {
	if runtime.GOOS == "windows" {
		return hubtemp.Dir("drmemory")
	}
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return "", false
	}
	return filepath.Join(cache, "mcp-local-hub", "drmemory"), true
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
