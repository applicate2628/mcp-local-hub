package secrets

import (
	"os"
	"path/filepath"
)

// DefaultKeyPath resolves .age-key — the age identity file that decrypts
// secrets.age. Moved here from internal/cli so packages that can't
// import cli (e.g. api) can reach the same paths without duplication.
func DefaultKeyPath() string {
	return resolveSecretPath(".age-key")
}

// DefaultVaultPath resolves secrets.age — the encrypted key/value
// vault. Always resolves to the same directory as .age-key.
func DefaultVaultPath() string {
	return resolveSecretPath("secrets.age")
}

// UserDataDir returns the OS-standard per-user data directory for this
// app, creating it if it doesn't exist. Canonical home for .age-key
// and secrets.age — independent of where mcphub.exe or the repo live.
//
//	Windows: %LOCALAPPDATA%\mcp-local-hub
//	Linux:   $XDG_DATA_HOME/mcp-local-hub  (default ~/.local/share/mcp-local-hub)
//	macOS:   ~/Library/Application Support/mcp-local-hub
func UserDataDir() string {
	if v := os.Getenv("LOCALAPPDATA"); v != "" { // Windows
		return filepath.Join(v, "mcp-local-hub")
	}
	if v := os.Getenv("XDG_DATA_HOME"); v != "" { // Linux with XDG override
		return filepath.Join(v, "mcp-local-hub")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "mcp-local-hub" // relative fallback — existing dev-mode behavior
	}
	if info, err := os.Stat(filepath.Join(home, "Library", "Application Support")); err == nil && info.IsDir() {
		return filepath.Join(home, "Library", "Application Support", "mcp-local-hub")
	}
	return filepath.Join(home, ".local", "share", "mcp-local-hub")
}

// resolveSecretPath returns the first existing path among (in order):
//  1. UserDataDir()/<name>               (canonical, OS-standard user data)
//  2. <exe_dir>/<name>                   (legacy: single-dir install)
//  3. <exe_dir>/../<name>                (bin/ layout: secrets one level up)
//  4. ./<name>                           (CWD fallback for dev invocations)
//
// If none exist, returns the canonical path under UserDataDir() — so
// that `mcphub secrets init` creates fresh files in the OS-standard
// location rather than scattering them around the repo.
func resolveSecretPath(name string) string {
	canonical := filepath.Join(UserDataDir(), name)
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		sibling := filepath.Join(exeDir, name)
		if _, err := os.Stat(sibling); err == nil {
			return sibling
		}
		parent := filepath.Join(exeDir, "..", name)
		if _, err := os.Stat(parent); err == nil {
			return parent
		}
	}
	if _, err := os.Stat(filepath.Join(".", name)); err == nil {
		return filepath.Join(".", name)
	}
	return canonical
}
