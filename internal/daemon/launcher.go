package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"mcp-local-hub/internal/process"
)

// LaunchSpec describes one child-process invocation.
type LaunchSpec struct {
	// Command is the program to run (looked up on PATH unless absolute).
	Command string
	// Args are passed verbatim to the child process.
	Args []string
	// Env adds/overrides environment for the child. Hub secrets are resolved before this struct.
	Env map[string]string
	// WorkingDir is optional; empty means inherit.
	WorkingDir string
	// LogPath receives both stdout and stderr. Rotated (if large) before each launch.
	LogPath string
	// MaxLogSize controls rotation threshold in bytes. Zero means 10 MB default.
	MaxLogSize int64
	// LogKeep controls how many rotated siblings to retain. Zero means 5 default.
	LogKeep int
}

// Launch executes the spec, tees stdout+stderr into a log file, and returns the
// child's exit code. This is a blocking call — intended to be used as the entire
// body of a scheduler-triggered `mcp daemon` invocation.
func Launch(spec LaunchSpec) (int, error) {
	if spec.Command == "" {
		return -1, errors.New("Launch: Command is required")
	}
	if spec.LogPath == "" {
		return -1, errors.New("Launch: LogPath is required")
	}
	maxSize := spec.MaxLogSize
	if maxSize == 0 {
		maxSize = 10 * 1024 * 1024
	}
	keep := spec.LogKeep
	if keep == 0 {
		keep = 5
	}
	if err := os.MkdirAll(filepath_Dir(spec.LogPath), 0755); err != nil {
		return -1, fmt.Errorf("mkdir log dir: %w", err)
	}
	if err := RotateIfLarge(spec.LogPath, maxSize, keep); err != nil {
		// Rotation failure is non-fatal — we still try to launch.
		fmt.Fprintf(os.Stderr, "warn: rotate failed: %v\n", err)
	}
	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return -1, fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(spec.Command, spec.Args...)
	process.NoConsole(cmd) // suppress per-child console pop on windowsgui parents
	cmd.Stdout = io.MultiWriter(logFile, os.Stdout)
	cmd.Stderr = io.MultiWriter(logFile, os.Stderr)
	cmd.Dir = spec.WorkingDir
	cmd.Env = os.Environ()
	for k, v := range spec.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}

// filepath_Dir is a tiny local wrapper to avoid importing path/filepath here;
// kept as a single-purpose helper so this file has no conditional deps.
func filepath_Dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[:i]
		}
	}
	return "."
}
