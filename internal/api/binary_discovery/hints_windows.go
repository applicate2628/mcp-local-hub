//go:build windows

package binary_discovery

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultHints returns the ordered list of directories the discoverer
// probes on Windows. The shipped list covers:
//
//   - MSYS2 toolchain bin directories (ucrt64, mingw64, clang64);
//   - LLVM and Go install prefixes under "Program Files";
//   - Visual Studio Build Tools' bundled clang/LLD layout;
//   - per-user toolchain caches (cargo, go/bin, .local/bin, npm prefix);
//   - Node version manager layouts (fnm, nvm-for-windows);
//   - dynamically enumerated %LOCALAPPDATA%\Programs\Python\Python3*\ and
//     the matching \Scripts subdirectory.
//
// The Python entries are enumerated rather than hard-coded because
// official python.org installers ship a versioned directory whose name
// (e.g. Python311, Python312) changes with each minor release; relying
// on a fixed literal would silently break on every Python upgrade
// (M-V4-1: glob, not version-locked literals).
// Hints use `${VAR}` syntax (NOT `%VAR%`) because `Discover` expands
// them through `os.ExpandEnv`, which only understands `$VAR` and
// `${VAR}` forms. The Windows-style `%VAR%` literal is left
// unexpanded by `os.ExpandEnv`, so a hint like `%USERPROFILE%\go\bin`
// would attempt to Stat the literal path "%USERPROFILE%\go\bin" and
// silently miss every per-user toolchain install (cargo, go, npm,
// fnm, nvm). Bot review PR #222 P1 finding (hints_windows.go:37).
func DefaultHints() []string {
	base := []string{
		`C:\msys64\ucrt64\bin`,
		`C:\msys64\mingw64\bin`,
		`C:\msys64\clang64\bin`,
		`C:\Program Files\LLVM\bin`,
		`C:\Program Files\Go\bin`,
		`C:\Program Files\Microsoft Visual Studio\2022\BuildTools\VC\Tools\Llvm\x64\bin`,
		`${USERPROFILE}\.cargo\bin`,
		`${USERPROFILE}\go\bin`,
		`${USERPROFILE}\.local\bin`,
		`${LOCALAPPDATA}\fnm_multishells`,
		`${LOCALAPPDATA}\Programs\fnm`,
		`${LOCALAPPDATA}\nvm`,
		`${APPDATA}\npm`,
	}
	return append(base, pythonProgramsHints()...)
}

// pythonProgramsHints enumerates %LOCALAPPDATA%\Programs\Python\ for
// any entry whose name starts with "Python3" (e.g. Python311, Python312)
// and returns the install directory plus its \Scripts subdirectory for
// each match. The enumeration is best-effort: a missing or unreadable
// %LOCALAPPDATA%\Programs\Python\ yields the empty list.
//
// The HasPrefix check is bounded by Go's strings.HasPrefix length
// guard, so short directory names (e.g. "Py") cannot panic the walk.
func pythonProgramsHints() []string {
	root := os.ExpandEnv(`${LOCALAPPDATA}\Programs\Python`)
	if root == "" {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "Python3") {
			continue
		}
		dir := filepath.Join(root, name)
		out = append(out, dir, filepath.Join(dir, "Scripts"))
	}
	return out
}
