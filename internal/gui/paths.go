package gui

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// appDataDirPath resolves the per-user mcp-local-hub directory path WITHOUT
// creating it. AppDataDir adds the MkdirAll; read-only probes that must have no
// filesystem side effect (e.g. a dry-run install checking for a live GUI) use
// this via PidportPathNoCreate instead.
func appDataDirPath() (string, error) {
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
	return filepath.Join(base, "mcp-local-hub"), nil
}

// AppDataDir returns the per-user writeable directory for mcp-local-hub
// runtime artifacts (pidport, gui-preferences.yaml). On Windows:
// %LOCALAPPDATA%\mcp-local-hub. On Linux/macOS: $XDG_STATE_HOME or
// $HOME/.local/state/mcp-local-hub. Creates the directory 0700 on first call.
func AppDataDir() (string, error) {
	dir, err := appDataDirPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	return dir, nil
}

// PidportPath returns the absolute path to the single-instance pidport
// file. Format: ASCII "<PID> <PORT>\n" — read by second-instance probe.
// Creates the per-user directory (use PidportPathNoCreate for read-only probes).
func PidportPath() (string, error) {
	dir, err := AppDataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gui.pidport"), nil
}

// PidportPathNoCreate returns the pidport path WITHOUT creating the per-user
// directory — for read-only probes (e.g. a dry-run install checking whether a
// live GUI is running) that must have no filesystem side effect. If the GUI has
// never run, the directory/file simply won't exist and the caller's probe
// reports no live instance.
func PidportPathNoCreate() (string, error) {
	dir, err := appDataDirPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "gui.pidport"), nil
}
