package drmemory

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
