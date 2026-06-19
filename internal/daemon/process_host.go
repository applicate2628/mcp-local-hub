package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"os/exec"

	"mcp-local-hub/internal/process"
)

// ProcessConfig configures a raw NON-MCP companion subprocess run by RunProcess.
type ProcessConfig struct {
	// Command is the absolute (or PATH-resolvable) program to exec.
	Command string
	// Args are the program arguments (manifest base_args + daemon extra_args).
	Args []string
	// Env is appended to os.Environ() for the subprocess (composeChildEnv).
	Env map[string]string
	// UnsetEnv lists env keys REMOVED from the inherited os.Environ() (same
	// skipped-optional-secret semantics as the MCP hosts).
	UnsetEnv []string
	// WorkingDir is the subprocess cwd — for a companion this is REQUIRED (the
	// process must run from its package directory). Empty inherits mcphub's cwd.
	WorkingDir string
	// LogPath receives the subprocess stdout+stderr via a rotating writer
	// (10 MB, 5 rotations). Empty discards output.
	LogPath string
}

// RunProcess runs a companion (kind=companion / transport=process) subprocess to
// completion. Unlike NewHTTPHost / NewStdioHost it wraps NO MCP protocol: the
// child is a plain non-MCP program (e.g. the excalidraw canvas Express server)
// the hub supervises only for lifecycle (correct cwd, restart, orphan-protection
// via the parent mcphub-daemon's Job Object). It execs Command from WorkingDir
// with the composed child env, the console suppressed, and stdout+stderr written
// to LogPath. It BLOCKS until the process exits or ctx is cancelled:
//
//   - ctx cancelled (supervisor stop / graceful shutdown) → CommandContext has
//     already killed the child; return nil (a stop is not a failure).
//   - the process exits on its own (crash OR a clean exit) → return non-nil so
//     the supervisor's restart policy respawns it. A long-lived companion is not
//     expected to self-exit, so even a clean exit is surfaced as an error.
func RunProcess(ctx context.Context, cfg ProcessConfig) error {
	if cfg.Command == "" {
		return fmt.Errorf("companion process: command is empty")
	}
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Dir = cfg.WorkingDir
	if len(cfg.Env) > 0 || len(cfg.UnsetEnv) > 0 {
		cmd.Env = composeChildEnv(cfg.Env, cfg.UnsetEnv)
	}
	process.NoConsole(cmd)

	if cfg.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.LogPath), 0700); err == nil {
			if f, err := os.OpenFile(cfg.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600); err == nil {
				w := &rotatingFileWriter{path: cfg.LogPath, file: f, maxBytes: 10 * 1024 * 1024, keep: 5}
				cmd.Stdout = w
				cmd.Stderr = w
				defer func() { _ = f.Close() }()
			}
		}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("companion process start: %w", err)
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		// Supervisor cancelled (stop / shutdown). The child is already being
		// torn down by CommandContext; a graceful stop is not a failure.
		return nil
	}
	if waitErr != nil {
		return fmt.Errorf("companion process exited: %w", waitErr)
	}
	return fmt.Errorf("companion process exited cleanly (a long-lived companion is not expected to self-exit)")
}
