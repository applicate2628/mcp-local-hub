package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// AppDataDir returns the per-user writeable directory for mcp-local-hub
// runtime artifacts (pidport, gui-preferences.yaml). On Windows:
// %LOCALAPPDATA%\mcp-local-hub. On Linux/macOS: $XDG_STATE_HOME or
// $HOME/.local/state/mcp-local-hub. Creates the directory 0700 on first call.
func AppDataDir() (string, error) {
	var base string
	switch runtime.GOOS {
	case "windows":
		base = os.Getenv("LOCALAPPDATA")
		if base == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home: %w", err)
			}
			base = filepath.Join(home, "AppData", "Local")
		}
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			base = x
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("resolve home: %w", err)
			}
			base = filepath.Join(home, ".local", "state")
		}
	}
	dir := filepath.Join(base, "mcp-local-hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// PidportPath returns the absolute path to the single-instance pidport
// file. Format: ASCII "<PID> <PORT>\n" — read by second-instance probe.
func PidportPath() (string, error) {
	dir, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gui.pidport"), nil
}
