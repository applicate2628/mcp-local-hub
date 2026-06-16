// Package toolchain detects the directory holding the native debugger binaries
// (gdb / lldb) so the supervisor can put it on the PATH of the gdb / lldb MCP
// daemons.
//
// WHY this exists: the GDB-MCP server decides a debugger is "available" with a
// BARE PATH probe (`subprocess.run(['gdb', '--version'])`) and ignores any
// explicit gdb_path / lldb_path until that probe passes. But an MCP daemon does
// NOT reliably inherit the interactive user PATH — a supervisor launched by the
// Task-Scheduler logon task gets a reduced environment that can be missing the
// MSYS2 `…\ucrt64\bin` dir where gdb/lldb live — so the daemon reports
// "debugger not available" even though the binaries are installed and on the
// operator's own PATH. mcphub's native lldb-bridge worked around this with a
// hardcoded absolute path; this package replaces that hardcode with detection
// and also gives the GDB-MCP daemon the dir it needs.
//
// Detection is FILESYSTEM-PROBED (os.Stat), never PATH-resolved, precisely so it
// still works when the supervisor's own PATH is the thing that's missing the
// dir. Returns empty (a clean no-op) when nothing is found — e.g. POSIX hosts
// where gdb/lldb are already on the system PATH, or a host with no MSYS2 install.
package toolchain

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// OverrideEnvVar lets an operator point at the debugger bin dir(s) explicitly
// (os.PathListSeparator-separated), overriding / supplementing detection — for a
// non-standard MSYS2 install, a Linux toolchain dir, or a custom LLVM build.
const OverrideEnvVar = "MCPHUB_DEBUGGER_TOOLCHAIN_DIR"

// debuggerExeNames are the binaries whose presence qualifies a candidate bin dir.
func debuggerExeNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"gdb.exe", "lldb.exe"}
	}
	return []string{"gdb", "lldb"}
}

// msys2Subenvs are the MSYS2 sub-environments probed on Windows, in preference
// order. ucrt64 is first (the UCRT-based toolchain that matches MSVC's Universal
// C Runtime and is the project's default debug toolchain).
var msys2Subenvs = []string{"ucrt64", "clang64", "mingw64"}

// DebuggerDirs returns the directories to prepend to the gdb / lldb daemon PATH
// so a bare `gdb` / `lldb` resolves. Order: the explicit override env first,
// then (Windows) the MSYS2 sub-environments that actually contain a debugger.
// Every returned dir is confirmed on disk to hold gdb or lldb. Empty when
// nothing is detected — the caller treats empty as a clean no-op.
func DebuggerDirs() []string {
	var out []string
	seen := map[string]bool{}
	add := func(dir string) {
		if dir == "" {
			return
		}
		key := dir
		if runtime.GOOS == "windows" {
			key = strings.ToLower(dir)
		}
		if seen[key] {
			return
		}
		if dirHasAnyDebugger(dir) {
			seen[key] = true
			out = append(out, dir)
		}
	}

	// 1. Explicit operator override (cross-platform; wins on order).
	if v := os.Getenv(OverrideEnvVar); v != "" {
		for _, d := range filepath.SplitList(v) {
			add(d)
		}
	}

	// 2. Windows MSYS2 detection. MSYS2_ROOT overrides the conventional location.
	if runtime.GOOS == "windows" {
		root := os.Getenv("MSYS2_ROOT")
		if root == "" {
			root = `C:\msys64`
		}
		for _, sub := range msys2Subenvs {
			add(filepath.Join(root, sub, "bin"))
		}
	}
	return out
}

// dirHasAnyDebugger reports whether dir contains gdb or lldb (a regular file).
func dirHasAnyDebugger(dir string) bool {
	for _, name := range debuggerExeNames() {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
			return true
		}
	}
	return false
}

// DefaultLldbPath returns an absolute lldb path for the lldb-bridge — the first
// detected debugger dir's lldb — replacing a previously hardcoded absolute path.
// Falls back to the bare "lldb" / "lldb.exe" (system-PATH resolution) only when
// nothing is detected, preserving the prior bare-name behavior on POSIX.
func DefaultLldbPath() string {
	exe := "lldb"
	if runtime.GOOS == "windows" {
		exe = "lldb.exe"
	}
	for _, dir := range DebuggerDirs() {
		cand := filepath.Join(dir, exe)
		if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
			return cand
		}
	}
	return exe
}
