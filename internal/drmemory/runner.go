package drmemory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"mcp-local-hub/internal/process"
)

// runOutput is the result of one Dr. Memory invocation: the target's
// exit code (Dr. Memory forwards the instrumented process's exit code),
// the raw results.txt body, the stderr Dr. Memory itself emitted, and the
// resolved path to results.txt (empty when none was produced).
type runOutput struct {
	ExitCode    int
	ResultsText string
	Stderr      string
	ResultsPath string
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
	logdir, err := os.MkdirTemp("", "drmemory-logs-")
	if err != nil {
		return nil, fmt.Errorf("create logdir: %w", err)
	}
	defer os.RemoveAll(logdir)

	cmdArgs := buildDrMemoryArgs(logdir, target, args, light, checkUninit)

	cmd := exec.CommandContext(ctx, exePath, cmdArgs...)
	process.NoConsole(cmd) // suppress console flash on windowsgui parent
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
			// Genuine spawn failure (binary not executable, ctx cancelled).
			return nil, fmt.Errorf("run drmemory: %w (stderr: %s)", runErr, truncate(stderr.String(), 2000))
		}
	}

	resultsPath, body := readResultsTxt(logdir)

	return &runOutput{
		ExitCode:    exitCode,
		ResultsText: body,
		Stderr:      truncate(stderr.String(), drMemoryOutputCap),
		ResultsPath: resultsPath,
	}, nil
}

// buildDrMemoryArgs assembles the drmemory.exe argv. Kept separate from
// defaultRun so a test can assert the flag wiring without spawning a
// process.
func buildDrMemoryArgs(logdir, target string, args []string, light, checkUninit bool) []string {
	cmdArgs := []string{"-batch"}
	if light {
		cmdArgs = append(cmdArgs, "-light")
	}
	if !checkUninit {
		cmdArgs = append(cmdArgs, "-no_check_uninitialized")
	}
	cmdArgs = append(cmdArgs, "-logdir", logdir, "--", target)
	cmdArgs = append(cmdArgs, args...)
	return cmdArgs
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
