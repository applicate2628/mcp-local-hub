// Package oneapi detects the Intel oneAPI install root and enumerates the
// per-component runtime DLL directories (mkl / tbb / compiler / …), so the
// supervisor can prepend them to the PATH of the gdb / lldb debugger
// daemons (and the inferior exes they launch).
//
// WHY this exists (hot-prod operator feedback): an MKL-linked .exe fails
// to load its DLLs under the gdb / lldb MCP daemons because those daemons
// — and the inferior they spawn — do NOT inherit the Intel oneAPI runtime
// DLL directories. The operator's documented workaround was to manually
// wrap the debug session in an oneapi-shell.
//
// WHY direct enumeration (this package) vs capturing setvars.bat: these are
// COMPLEMENTARY, used by different consumers. The full build environment
// (PATH + LIB + INCLUDE + MKLROOT/…) — needed to COMPILE and LINK oneAPI code
// — is captured from oneAPI's own setvars.bat by the oneapi-run server (see
// internal/oneapirun); that is the complete, version-correct, vendor-sanctioned
// path. This package instead enumerates ONLY the runtime DLL dirs, for the
// RUNTIME-ONLY consumers that just need a built exe's DLLs on PATH and must NOT
// pay setvars' ~1-3 s subprocess on every spawn: the supervisor's gdb / lldb
// debugger injector and drmemory's instrumented-target spawn. The component
// DLL dirs an MKL-linked .exe needs at runtime are stable and cheap to find:
//
//	<root>\mkl\latest\bin        (mkl_core etc.)
//	<root>\tbb\latest\bin        (tbb12.dll etc.)
//	<root>\compiler\latest\bin   (libiomp5md / svml — OpenMP + SVML runtimes)
//
// where <root> = "C:\Program Files (x86)\Intel\oneAPI". The enumeration is pure
// + fast (a few os.Stat + a cheap *.dll glob per component); no subprocess, no
// caching. (An earlier note here claimed setvars.bat was simply "broken on the
// host". The real story: setvars works, but its component init fails under the
// supervisor's NoDefaultCurrentDirectoryInExePath=1 hardening because it
// invokes each component vars.bat as a bare command via current-dir search;
// oneapi-run's capture clears that var so setvars configures every component —
// see internal/oneapirun/vcvars_windows.go. This direct enumeration is kept for
// the speed/runtime-only consumers above, independent of that capture.)
//
// Platform scope: Windows-focused. On Linux / other platforms this is a
// no-op (DetectRoot returns ("", false), so DLLDirs is never reached with
// a real root; the supervisor never injects).
//
// Failure posture: every failure mode (root missing, no component bin
// dirs) is a CLEAN no-op — the supervisor injects nothing and (when a root
// was found but no DLL dirs exist) logs a warn event. It never blocks or
// fails a daemon spawn.
package oneapi

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DisableEnvVar, when set to "1" in the supervisor's process
// environment, globally opts out of oneAPI PATH injection: DetectRoot
// still locates the root for diagnostics, but the supervisor wiring skips
// both DLLDirs and the inject step. Documented in CLAUDE.md "Supervisor"
// notes.
const DisableEnvVar = "MCPHUB_DISABLE_ONEAPI_PATH"

// PathKey is the normalized key for the PATH variable.
const PathKey = "PATH"

// priorityComponents are the MKL-runtime-essential component directories,
// emitted FIRST (in this order) by DLLDirs: an MKL-linked inferior needs
// the mkl core DLLs, the tbb threading runtime, and the compiler's
// OpenMP / SVML runtimes (libiomp5md / svml). Any other present component
// (mpi / ipp / dnnl / …) follows in sorted order.
var priorityComponents = []string{"mkl", "tbb", "compiler"}

// ---------------------------------------------------------------------------
// Injectable seams (package vars) — let tests drive DetectRoot / DLLDirs
// against a synthetic filesystem without a real oneAPI install.
// ---------------------------------------------------------------------------

// dirExists reports whether path names an existing directory. Overridable
// in tests so DetectRoot / DLLDirs can be exercised against a synthetic
// layout. Production points at the real os.Stat probe (oneapi_<platform>.go).
var dirExists = func(path string) bool { return realDirExists(path) }

// dirHasDLL reports whether dir contains at least one *.dll file.
// Overridable in tests. Production points at the real glob probe.
var dirHasDLL = func(dir string) bool { return realDirHasDLL(dir) }

// listComponentDirs returns the immediate sub-directory NAMES of root
// (the component dirs: "mkl", "tbb", "compiler", "mpi", …). Overridable in
// tests. Production reads the directory entries.
var listComponentDirs = func(root string) []string { return realListComponentDirs(root) }

// ---------------------------------------------------------------------------
// Detection.
// ---------------------------------------------------------------------------

// DetectRoot locates the Intel oneAPI install root.
//
// Probe order (Windows):
//  1. ONEAPI_ROOT env var, if set AND the dir exists.
//  2. "%ProgramFiles(x86)%\Intel\oneAPI"
//  3. "%ProgramFiles%\Intel\oneAPI"
//
// Returns (root, true) for the FIRST candidate that exists on disk as a
// directory; else ("", false). On non-Windows it always returns ("",
// false) (the platform rootProbePaths returns no candidates).
func DetectRoot() (string, bool) {
	for _, cand := range rootProbePaths() {
		if cand == "" {
			continue
		}
		if dirExists(cand) {
			return cand, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// DLL-dir enumeration (pure + fast — no subprocess).
// ---------------------------------------------------------------------------

// DLLDirs enumerates "<root>\<component>\latest\bin" for every component
// directory under root that EXISTS and contains at least one *.dll file,
// returning them in a deterministic order:
//
//	mkl, tbb, compiler FIRST (the MKL runtime essentials, in that order),
//	then any other present component (mpi / ipp / dnnl / …) sorted.
//
// A component is included ONLY when its "latest\bin" dir both exists and
// holds at least one *.dll (a stat + a cheap glob check) — so empty /
// placeholder component layouts are skipped. Returns nil when root holds
// no qualifying component bin dir (the no-op-with-warn path for the
// caller).
//
// This replaces the old setvars-delta capture as the SOURCE of PATH dirs:
// it is exactly what a healthy setvars would add to PATH for runtime DLL
// loading.
func DLLDirs(root string) []string {
	if root == "" {
		return nil
	}

	// Qualify a single component: <root>\<component>\latest\bin must exist
	// and contain at least one *.dll.
	qualify := func(component string) (string, bool) {
		bin := filepath.Join(root, component, "latest", "bin")
		if !dirExists(bin) {
			return "", false
		}
		if !dirHasDLL(bin) {
			return "", false
		}
		return bin, true
	}

	priority := make(map[string]bool, len(priorityComponents))
	for _, c := range priorityComponents {
		priority[strings.ToLower(c)] = true
	}

	var dirs []string

	// Priority components first, in the fixed essential order.
	for _, c := range priorityComponents {
		if bin, ok := qualify(c); ok {
			dirs = append(dirs, bin)
		}
	}

	// Any other present component, sorted, excluding the priority ones
	// (already emitted above).
	var others []string
	for _, name := range listComponentDirs(root) {
		if priority[strings.ToLower(name)] {
			continue
		}
		others = append(others, name)
	}
	sort.Strings(others)
	for _, name := range others {
		if bin, ok := qualify(name); ok {
			dirs = append(dirs, bin)
		}
	}

	return dirs
}

// ---------------------------------------------------------------------------
// Env-list prepend (shared by the runtime-only consumers).
// ---------------------------------------------------------------------------

// PrependEnvList returns a copy of env with dirs prepended (in order) to the
// value of the named env var, joined with the OS path-list separator. key is
// matched case-insensitively: Windows env-var names are case-insensitive and a
// captured `set` dump can emit "Path" rather than "PATH", so a case-sensitive
// compare would miss the entry and the dirs would never reach the real search
// path. When env has no entry for key, a new one is synthesized from dirs
// alone (canonical upper-case key). When dirs is empty, env is returned as a
// fresh copy unchanged. The matched entry's original KEY casing is preserved;
// every other entry is kept verbatim and in order. This is the single owner of
// the prepend logic shared by the runtime-only oneAPI consumers (drmemory's
// instrumented-target spawn; available to the supervisor debugger injector).
func PrependEnvList(env []string, key string, dirs []string) []string {
	if len(dirs) == 0 {
		out := make([]string, len(env))
		copy(out, env)
		return out
	}

	prefix := strings.Join(dirs, string(os.PathListSeparator))

	out := make([]string, 0, len(env)+1)
	found := false
	for _, entry := range env {
		k, v, hasEq := strings.Cut(entry, "=")
		if hasEq && strings.EqualFold(k, key) {
			found = true
			if v == "" {
				out = append(out, k+"="+prefix)
			} else {
				out = append(out, k+"="+prefix+string(os.PathListSeparator)+v)
			}
			continue
		}
		out = append(out, entry)
	}
	if !found {
		out = append(out, key+"="+prefix)
	}
	return out
}

// ---------------------------------------------------------------------------
// Test seam installer.
// ---------------------------------------------------------------------------

// SetSeamsForTest installs fake dir-exists / dll-glob / component-list
// seams and returns a restore func. TEST-ONLY. Any nil argument leaves
// that seam untouched.
func SetSeamsForTest(
	fakeDirExists func(string) bool,
	fakeDirHasDLL func(string) bool,
	fakeListComponentDirs func(string) []string,
) func() {
	prevDirExists := dirExists
	prevDirHasDLL := dirHasDLL
	prevList := listComponentDirs
	if fakeDirExists != nil {
		dirExists = fakeDirExists
	}
	if fakeDirHasDLL != nil {
		dirHasDLL = fakeDirHasDLL
	}
	if fakeListComponentDirs != nil {
		listComponentDirs = fakeListComponentDirs
	}
	return func() {
		dirExists = prevDirExists
		dirHasDLL = prevDirHasDLL
		listComponentDirs = prevList
	}
}
