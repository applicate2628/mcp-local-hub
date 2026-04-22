package perftools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
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

var errOutputLimitExceeded = errors.New("subprocess output exceeded configured limit")

type cappedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.limit >= 0 && c.buf.Len()+len(p) > c.limit {
		remaining := c.limit - c.buf.Len()
		if remaining > 0 {
			_, _ = c.buf.Write(p[:remaining])
		}
		return len(p), errOutputLimitExceeded
	}
	return c.buf.Write(p)
}

func (c *cappedBuffer) Bytes() []byte {
	return c.buf.Bytes()
}

func (c *cappedBuffer) String() string {
	return c.buf.String()
}

// runCapture spawns cmd under ctx, captures stdout and stderr into
// separate buffers, and reports the exit code. Non-zero exit with an
// *exec.ExitError is NOT a failure (some tools use exit codes to
// signal "found diagnostics"); only returns error on genuine subprocess
// problems (binary not executable, context cancelled, pipe setup
// failure). workingDir is optional — pass "" to inherit parent's cwd.
func runCapture(ctx context.Context, binPath, workingDir string, args []string) (*captureResult, error) {
	return runCaptureLimited(ctx, binPath, workingDir, args, math.MaxInt, math.MaxInt)
}

// runCaptureLimited is runCapture with explicit stdout/stderr limits in bytes.
func runCaptureLimited(ctx context.Context, binPath, workingDir string, args []string, stdoutLimit, stderrLimit int) (*captureResult, error) {
	cmd := exec.CommandContext(ctx, binPath, args...)
	if workingDir != "" {
		cmd.Dir = workingDir
	}
	stdout := cappedBuffer{limit: stdoutLimit}
	stderr := cappedBuffer{limit: stderrLimit}
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
	if errors.Is(err, errOutputLimitExceeded) {
		return nil, errOutputLimitExceeded
	}
	return nil, fmt.Errorf("run %s: %w (stderr: %s)", binPath, err, stderr.String())
}
