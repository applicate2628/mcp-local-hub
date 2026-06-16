//go:build windows

package oneapi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"

	"mcp-local-hub/internal/process"
)

// setvarsProbePaths returns the ordered list of candidate setvars.bat
// paths for Windows (see DetectSetvars doc for the probe order):
//
//  1. ONEAPI_ROOT (if set) → "<ONEAPI_ROOT>\setvars.bat"
//  2. "%ProgramFiles(x86)%\Intel\oneAPI\setvars.bat"
//  3. "%ProgramFiles%\Intel\oneAPI\setvars.bat"
//
// Empty / unset env candidates are returned as "" so DetectSetvars skips
// them; the real host's oneAPI 2026.0 lives under ProgramFiles(x86).
func setvarsProbePaths() []string {
	var out []string
	if root := os.Getenv("ONEAPI_ROOT"); root != "" {
		out = append(out, filepath.Join(root, "setvars.bat"))
	}
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		out = append(out, filepath.Join(pf86, "Intel", "oneAPI", "setvars.bat"))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out, filepath.Join(pf, "Intel", "oneAPI", "setvars.bat"))
	}
	return out
}

// realFileExists reports whether path is an existing regular-ish file
// (Stat succeeds and it is not a directory).
func realFileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// realBaselineEnv returns the current process environment as the diff
// baseline.
func realBaselineEnv() []string { return os.Environ() }

// realSetvarsRunner runs the oneAPI env shell and returns the stdout of
// the trailing `set` dump. It builds:
//
//	cmd /c ""<setvars>" > NUL 2>&1 && set"
//
// so setvars' banner is redirected to NUL (it prints version info to
// stdout that would otherwise pollute the `set` dump), and the trailing
// `set` enumerates the resulting environment to stdout, which we capture.
//
// process.NoConsole suppresses any console-window flash (CREATE_NO_WINDOW
// + HideWindow), matching the rest of the repo's subprocess discipline
// (e.g. internal/api/serena_port_owner_windows.go). The context timeout is
// applied via exec.CommandContext so a hung setvars is force-killed.
func realSetvarsRunner(ctx context.Context, setvarsPath string) (string, error) {
	// The whole command is passed to cmd /c as ONE argument. Quote the
	// setvars path (it contains spaces: "Program Files (x86)"). The OUTER
	// pair of double-quotes around the entire command string is the cmd.exe
	// convention required when the command itself contains quoted tokens.
	inner := `""` + setvarsPath + `" > NUL 2>&1 && set"`
	cmd := exec.CommandContext(ctx, "cmd", "/c", inner)
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
