package vtune

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// ErrVTuneNotFound is returned by the default path probe when no vtune.exe
// can be located via %VTUNE_PROFILER_DIR%, the known oneAPI install
// locations, or PATH. The message names every place that was searched so
// the operator can fix their install or set the env var.
var ErrVTuneNotFound = fmt.Errorf(
	"vtune not found: looked under %%VTUNE_PROFILER_DIR%%\\bin64, " +
		`"C:\Program Files (x86)\Intel\oneAPI\vtune\latest\bin64", ` +
		`"C:\Program Files (x86)\Intel\oneAPI\vtune\<version>\bin64", and PATH. ` +
		"Install Intel VTune Profiler (part of the Intel oneAPI Base Toolkit) or set " +
		"VTUNE_PROFILER_DIR to its install root (the dir whose bin64 holds vtune.exe)")

// vtuneOneAPIRoots are the canonical Intel oneAPI VTune install roots on
// Windows, probed in order. Under each root, "latest\bin64\vtune.exe" is
// preferred; if absent, every versioned "<version>\bin64\vtune.exe" sibling
// is probed (newest version string first).
var vtuneOneAPIRoots = []string{
	`C:\Program Files (x86)\Intel\oneAPI\vtune`,
}

// findVTune locates vtune.exe. Probe order:
//  1. %VTUNE_PROFILER_DIR%\bin64\vtune.exe
//  2. each known oneAPI vtune root: <root>\latest\bin64\vtune.exe, then
//     <root>\<version>\bin64\vtune.exe for each versioned sibling (newest first)
//  3. PATH (exec.LookPath)
//
// It returns the first existing executable. This is the DEFAULT
// implementation wired into the server's findExe seam; tests inject a fake
// that returns a fixed path (or an error) so they never depend on a real
// VTune install.
func findVTune() (string, error) {
	// 1. VTUNE_PROFILER_DIR explicit override. setvars.bat exports this to
	//    the VTune install root (the dir whose bin64 holds vtune.exe).
	if dir := os.Getenv("VTUNE_PROFILER_DIR"); dir != "" {
		candidate := filepath.Join(dir, "bin64", "vtune.exe")
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}

	// 2. Known oneAPI vtune roots: "latest" first, then versioned dirs.
	for _, root := range vtuneOneAPIRoots {
		latest := filepath.Join(root, "latest", "bin64", "vtune.exe")
		if isExecutableFile(latest) {
			return latest, nil
		}
		for _, ver := range versionedSubdirs(root) {
			candidate := filepath.Join(root, ver, "bin64", "vtune.exe")
			if isExecutableFile(candidate) {
				return candidate, nil
			}
		}
	}

	// 3. PATH fallback.
	if path, err := exec.LookPath("vtune.exe"); err == nil {
		return path, nil
	}
	if path, err := exec.LookPath("vtune"); err == nil {
		return path, nil
	}

	return "", ErrVTuneNotFound
}

// versionedSubdirs returns the immediate sub-directory NAMES of root, sorted
// in reverse (so a newer "2026.2" sorts before "2025.4"), excluding the
// "latest" symlink dir (already probed by the caller). Returns nil when root
// cannot be read (no install) so the caller simply falls through to PATH.
func versionedSubdirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == "latest" {
			continue
		}
		dirs = append(dirs, e.Name())
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	return dirs
}

// isExecutableFile reports whether path exists and is a regular file (not a
// directory). VTune's launcher is a plain .exe; a Stat that resolves to a
// regular file is sufficient on Windows where the .exe extension already
// implies executability.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}
