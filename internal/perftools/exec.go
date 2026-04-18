package perftools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// captureResult is the shared subprocess output bundle used by every
// per-tool handler. stdout / stderr are captured separately (unlike a
// merged buffer) so parsers that expect diagnostics on a specific
// stream (clang-tidy writes to stdout, iwyu to stderr) can pick the
// right one. exitCode distinguishes "tool emitted suggestions" (non-
// zero is legitimate for some tools like iwyu / clang-tidy) from
// "subprocess crashed" (err != nil and not *exec.ExitError).
type captureResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// runCapture spawns cmd under ctx, captures stdout and stderr into
// separate buffers, and reports the exit code. Non-zero exit with an
// *exec.ExitError is NOT a failure (some tools use exit codes to
// signal "found diagnostics"); only returns error on genuine subprocess
// problems (binary not executable, context cancelled, pipe setup
// failure). workingDir is optional — pass "" to inherit parent's cwd.
func runCapture(ctx context.Context, binPath, workingDir string, args []string) (*captureResult, error) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := &captureResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err == nil {
		return res, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return nil, fmt.Errorf("run %s: %w (stderr: %s)", binPath, err, stderr.String())
}
