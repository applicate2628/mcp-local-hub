//go:build windows

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/process"
)

// Closed prepared commands use the raw absolute image consumed by the original
// native creator, not exec.Command's private extension cache or live lookup.
func resolvePreparedStdioCommand(s *StdioLaunchContext, cfg HostConfig) *exec.Cmd {
	cmd := &exec.Cmd{Args: append([]string{cfg.Command}, cfg.Args...)}
	if err := s.valid(); err != nil {
		cmd.Err = err
		return cmd
	}
	image, err := capturedWindowsStdioImage(s, cfg.Command)
	if err != nil {
		cmd.Path, cmd.Err = image, err
		return cmd
	}
	dir := cfg.WorkingDir
	if dir == "" {
		dir = s.cwd
	}
	dir, err = capturedStdioAbsolutePath(s, dir)
	if err != nil {
		cmd.Err = err
		return cmd
	}
	cmd.Path, cmd.Args[0], cmd.Dir = image, image, dir
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
	// Neither drive-relative nor current-volume rooted forms have the admitted
	// parent binding needed for this closed preparation.
	if filepath.VolumeName(path) != "" || strings.Contains(path, ":") ||
		strings.HasPrefix(path, `\`) || strings.HasPrefix(path, "/") {
		return "", errors.New("prepared image has unsupported relative volume semantics")
	}
	return filepath.Join(s.cwd, path), nil
}

func capturedWindowsStdioImage(s *StdioLaunchContext, name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, 0) {
		return "", exec.ErrNotFound
	}
	if strings.ContainsAny(name, `:\/`) {
		image, err := capturedStdioAbsolutePath(s, name)
		if err != nil {
			return "", err
		}
		// CreateProcessW with NULL application name does not append an extension
		// to a path-containing token. Qualify the exact anchored file only.
		if err := capturedWindowsStdioFile(s, image); err != nil {
			return "", err
		}
		return image, preparedWindowsNativeForm(image)
	}
	extensions := capturedStdioPathExtensions(s)
	var dot string
	var dotInfo os.FileInfo
	_, noDefaultDirectory := s.LookupEnvironment("NoDefaultCurrentDirectoryInExePath")
	if !noDefaultDirectory {
		image, err := capturedWindowsStdioCandidate(s, filepath.Join(s.cwd, name), extensions)
		if err == nil {
			if capturedStdioErrDotDisabled(s) {
				return image, nil
			}
			dot, dotInfo = image, nil
			dotInfo, err = os.Lstat(image)
			if err != nil {
				return "", err
			}
		} else if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, exec.ErrNotFound) {
			return "", err
		}
	}
	path, _ := s.LookupEnvironment("PATH")
	for _, directory := range filepath.SplitList(path) {
		if directory == "" {
			continue
		}
		base, err := capturedStdioAbsolutePath(s, directory)
		if err != nil {
			return "", err
		}
		image, err := capturedWindowsStdioCandidate(s, filepath.Join(base, name), extensions)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, exec.ErrNotFound) {
				return "", err
			}
			continue
		}
		if dot != "" {
			info, statErr := os.Lstat(image)
			if statErr != nil || !os.SameFile(dotInfo, info) {
				return dot, &exec.Error{Name: name, Err: exec.ErrDot}
			}
		}
		if !filepath.IsAbs(directory) && !capturedStdioErrDotDisabled(s) {
			if dot == "" {
				dot, dotInfo = image, nil
				dotInfo, err = os.Lstat(image)
				if err != nil {
					return "", err
				}
			}
			continue
		}
		return image, nil
	}
	if dot != "" {
		return dot, &exec.Error{Name: name, Err: exec.ErrDot}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func capturedStdioPathExtensions(s *StdioLaunchContext) []string {
	value, _ := s.LookupEnvironment("PATHEXT")
	if value == "" {
		return []string{".com", ".exe", ".bat", ".cmd"}
	}
	var extensions []string
	for _, extension := range strings.Split(strings.ToLower(value), ";") {
		if extension == "" {
			continue
		}
		if extension[0] != '.' {
			extension = "." + extension
		}
		extensions = append(extensions, extension)
	}
	return extensions
}

func capturedWindowsStdioCandidate(s *StdioLaunchContext, base string, extensions []string) (string, error) {
	if len(extensions) == 0 || filepath.Ext(base) != "" {
		if err := capturedWindowsStdioFile(s, base); err == nil {
			return base, preparedWindowsNativeForm(base)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if len(extensions) == 0 {
			return "", fs.ErrNotExist
		}
	}
	for _, extension := range extensions {
		image := base + extension
		if err := capturedWindowsStdioFile(s, image); err == nil {
			return image, preparedWindowsNativeForm(image)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fs.ErrNotExist
}

func capturedWindowsStdioFile(s *StdioLaunchContext, path string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("prepared image is not a regular file")
	}
	return nil
}

func preparedWindowsNativeForm(path string) error {
	extension := strings.ToLower(filepath.Ext(path))
	if extension != "" && extension != ".exe" && extension != ".com" {
		return errors.New("prepared image requires an unsupported shell or extension form")
	}
	return nil
}
