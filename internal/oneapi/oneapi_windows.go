//go:build windows

package oneapi

import (
	"os"
	"path/filepath"
)

// rootProbePaths returns the ordered list of candidate oneAPI install-root
// directories for Windows (see DetectRoot doc for the probe order):
//
//  1. ONEAPI_ROOT (if set) → "<ONEAPI_ROOT>"
//  2. "%ProgramFiles(x86)%\Intel\oneAPI"
//  3. "%ProgramFiles%\Intel\oneAPI"
//
// Empty / unset env candidates are returned as "" so DetectRoot skips
// them; the real host's oneAPI lives under ProgramFiles(x86).
func rootProbePaths() []string {
	var out []string
	if root := os.Getenv("ONEAPI_ROOT"); root != "" {
		out = append(out, root)
	}
	if pf86 := os.Getenv("ProgramFiles(x86)"); pf86 != "" {
		out = append(out, filepath.Join(pf86, "Intel", "oneAPI"))
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		out = append(out, filepath.Join(pf, "Intel", "oneAPI"))
	}
	return out
}

// realDirExists reports whether path is an existing directory.
func realDirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// realDirHasDLL reports whether dir contains at least one *.dll file. The
// glob is case-insensitive on Windows (the OS filesystem folds case), so
// "*.dll" matches "mkl_core.DLL" too. A glob error (bad pattern, dir gone)
// is treated as "no DLL" — clean no-op.
func realDirHasDLL(dir string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, "*.dll"))
	if err != nil {
		return false
	}
	return len(matches) > 0
}

// realListComponentDirs returns the immediate sub-directory names under
// root (the component dirs). A read error yields nil — DLLDirs then only
// emits the priority components it can stat directly, which is the
// MKL-runtime-essential set anyway.
func realListComponentDirs(root string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}
