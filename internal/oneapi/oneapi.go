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
// WHY direct enumeration instead of setvars.bat (the earlier design):
// setvars.bat is BROKEN on the live host for oneAPI components — it adds
// only the Visual-Studio dirs to PATH and FAILS to run the per-component
// `vars.bat` scripts ("'vars.bat' is not recognized"), so it does NOT add
// the oneAPI mkl/tbb/compiler bin dirs and does NOT set
// MKLROOT/TBBROOT/CMPLR_ROOT. Under the supervisor's env it even exits 1.
// The live deploy logged "oneapi-env-capture-failed" and injected NOTHING.
// The component DLL dirs that an MKL-linked .exe actually needs at runtime
// DO exist and are reliable:
//
//	<root>\mkl\latest\bin        (mkl_core etc.)
//	<root>\tbb\latest\bin        (tbb12.dll etc.)
//	<root>\compiler\latest\bin   (libiomp5md / svml — OpenMP + SVML runtimes)
//
// where <root> = "C:\Program Files (x86)\Intel\oneAPI". So we enumerate
// those DLL dirs directly — exactly what a HEALTHY setvars WOULD add to
// PATH for runtime DLL loading, minus the broken-install dependency and
// the slow / flaky subprocess. The enumeration is pure + fast (a few
// os.Stat + a cheap *.dll glob per component); no subprocess, no caching.
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
