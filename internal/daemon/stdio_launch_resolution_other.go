//go:build !windows

package daemon

import (
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/process"
)

func resolvePreparedStdioCommand(s *StdioLaunchContext, cfg HostConfig) *exec.Cmd {
	if err := s.valid(); err != nil {
		return &exec.Cmd{Err: err}
	}
	image, err := capturedUnixStdioImage(s, cfg.Command)
	if err != nil {
		return &exec.Cmd{Path: image, Err: err}
	}
	dir := cfg.WorkingDir
	if dir == "" {
		dir = s.cwd
	}
	dir, err = capturedStdioAbsolutePath(s, dir)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	// An absolute path bypasses PATH/current-directory lookup in the public
	// Go API while preserving its original context cancellation owner.
	cmd := exec.CommandContext(s.ctx, image, cfg.Args...)
	cmd.Args[0] = cfg.Command
	cmd.Dir = dir
	unset := append(append([]string{}, cfg.UnsetEnv...), LaunchCapabilityHandleEnv)
	cmd.Env = composeChildEnvFrom(s.environment, cfg.Env, unset)
	process.NoConsole(cmd)
	process.SetParentDeathSignal(cmd)
	return cmd
}

func capturedStdioAbsolutePath(s *StdioLaunchContext, path string) (string, error) {
	if err := s.valid(); err != nil {
		return "", err
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(s.cwd, path), nil
}

func capturedUnixStdioImage(s *StdioLaunchContext, name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) {
		return "", exec.ErrNotFound
	}
	if strings.Contains(name, "/") {
		image, err := capturedStdioAbsolutePath(s, name)
		if err != nil {
			return "", err
		}
		return exec.LookPath(image)
	}
	path, _ := s.LookupEnvironment("PATH")
	for _, directory := range filepath.SplitList(path) {
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		if directory == "" {
			directory = "."
		}
		base, err := capturedStdioAbsolutePath(s, directory)
		if err != nil {
			return "", err
		}
		image, err := exec.LookPath(filepath.Join(base, name))
		if err == nil {
			if !filepath.IsAbs(directory) && !capturedStdioErrDotDisabled(s) {
				return image, &exec.Error{Name: name, Err: exec.ErrDot}
			}
			return image, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}
