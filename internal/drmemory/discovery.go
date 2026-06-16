package drmemory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/process"
)

// ErrDrMemoryNotFound is returned by the default path probe when no
// drmemory.exe can be located via any of the known install locations,
// %DRMEMORY_HOME%, or PATH. The message names every place that was
// searched so the operator can fix their install or set the env var.
var ErrDrMemoryNotFound = fmt.Errorf(
	"drmemory.exe not found: looked under %%DRMEMORY_HOME%%, " +
		`"C:\Program Files (x86)\Dr. Memory\bin64", ` +
		`"C:\Program Files (x86)\Dr. Memory\bin", and PATH. ` +
		"Install Dr. Memory (https://drmemory.org) or set DRMEMORY_HOME to its install root")

// drMemoryInstallRoots are the canonical Dr. Memory install locations on
// Windows, probed in order. bin64 is preferred over bin (a 64-bit target
// is the common case; Dr. Memory itself dispatches to the right bitness
// once invoked, but the launcher under bin64 is the documented default).
var drMemoryInstallRoots = []string{
	`C:\Program Files (x86)\Dr. Memory`,
}

// drMemoryBinSubdirs are the per-root subdirectories that hold the
// drmemory.exe launcher, probed in order (bin64 first, then bin).
var drMemoryBinSubdirs = []string{"bin64", "bin"}

// findDrMemory locates drmemory.exe. Probe order:
//  1. %DRMEMORY_HOME%\bin64\drmemory.exe, then %DRMEMORY_HOME%\bin\drmemory.exe
//  2. each known install root + bin64, then + bin
//  3. PATH (exec.LookPath)
//
// It returns the first existing executable. This is the DEFAULT
// implementation wired into the server's findExe seam; tests inject a
// fake that returns a fixed path (or an error) so they never depend on a
// real Dr. Memory install.
func findDrMemory() (string, error) {
	// 1. DRMEMORY_HOME explicit override.
	if home := os.Getenv("DRMEMORY_HOME"); home != "" {
		for _, sub := range drMemoryBinSubdirs {
			candidate := filepath.Join(home, sub, "drmemory.exe")
			if isExecutableFile(candidate) {
				return candidate, nil
			}
		}
	}

	// 2. Known install roots.
	for _, root := range drMemoryInstallRoots {
		for _, sub := range drMemoryBinSubdirs {
			candidate := filepath.Join(root, sub, "drmemory.exe")
			if isExecutableFile(candidate) {
				return candidate, nil
			}
		}
	}

	// 3. PATH fallback.
	if path, err := exec.LookPath("drmemory.exe"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("drmemory"); err == nil {
		return path, nil
	}

	return "", ErrDrMemoryNotFound
}

// defaultVersionProbe is the production versionProbeFunc backing drmemory_status.
// It resolves drmemory.exe via findDrMemory, then runs `<drmemory> -version`
// (Dr. Memory's single-dash version flag) via Go exec — which works in the
// console-less daemon. The reported availability/version follow these rules:
//
//   - findDrMemory fails (no install) → ("", "", false): Dr. Memory is not
//     available; the caller's install-guidance comes from drmemory_run.
//   - drmemory.exe resolves AND `-version` succeeds → (path, firstLine, true).
//   - drmemory.exe resolves but `-version` fails (older Dr. Memory that does not
//     support -version, or it prints to stderr) → (path, "", true): a usable
//     drmemory.exe was located, so available stays true; version is just unknown.
//     Reporting available:false here would wrongly claim a present install is
//     missing, since resolution already proved the binary exists.
func defaultVersionProbe() (string, string, bool) {
	path, err := findDrMemory()
	if err != nil {
		return "", "", false
	}
	cmd := exec.Command(path, "-version")
	process.NoConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		// -version unsupported / wrote to stderr: the binary is still present,
		// so report it as available with an unknown version rather than masking
		// a real install behind available:false.
		return path, "", true
	}
	return path, firstNonEmptyLine(string(out)), true
}

// firstNonEmptyLine returns the first non-blank line of s (trimmed), or "" when
// s holds no non-blank line. Dr. Memory's `-version` output leads with the
// version banner; the rest is noise for an availability probe.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// isExecutableFile reports whether path exists and is a regular file
// (not a directory). Dr. Memory's launcher is a plain .exe; a Stat that
// resolves to a regular file is sufficient on Windows where the .exe
// extension already implies executability.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
