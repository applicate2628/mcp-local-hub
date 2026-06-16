package oneapirun

import (
	"os"
	"strings"

	"mcp-local-hub/internal/oneapi"
)

// envSource labels which environment composition the run used, so the
// caller can tell whether the command saw the full VS toolchain, only the
// oneAPI runtime, or neither.
const (
	// envSourceVcvarsOneAPI: VS env captured from vcvars64.bat AND oneAPI
	// DLL dirs prepended to PATH — the fully-initialized environment.
	envSourceVcvarsOneAPI = "vcvars64+oneapi"
	// envSourceOneAPIOnly: vcvars64.bat not found, but oneAPI DLL dirs
	// were prepended onto the inherited os.Environ() PATH.
	envSourceOneAPIOnly = "oneapi-only"
	// envSourcePlain: neither vcvars nor any oneAPI dir available — the
	// command runs with the inherited os.Environ() unchanged.
	envSourcePlain = "plain"
)

// detectOneAPIDLLDirs is the production oneAPIDLLDirs seam: it reuses the
// internal/oneapi package's DetectRoot + DLLDirs to enumerate the component
// runtime DLL directories (mkl / tbb / compiler / …). Returns nil on any
// non-Windows host or when no oneAPI install is found — the no-op path.
func detectOneAPIDLLDirs() []string {
	root, ok := oneapi.DetectRoot()
	if !ok {
		return nil
	}
	return oneapi.DLLDirs(root)
}

// pathKeyMatches reports whether an env-var KEY (the part before '=' in a
// "KEY=VALUE" entry) is the PATH variable, case-insensitively. Windows
// treats env-var names case-insensitively and vcvars64's `set` dump can
// emit "Path" rather than "PATH", so a case-sensitive compare would miss
// the dumped PATH and the oneAPI dirs would never reach the real search
// path. The returned canonical key preserves whatever casing the source
// used so the merged env keeps a single PATH entry.
func pathKeyMatches(key string) bool {
	return strings.EqualFold(key, oneapi.PathKey)
}

// prependOneAPIToPath returns a copy of baseEnv with the oneAPI DLL
// directories prepended (in order) to the PATH entry's value, joined with
// the OS path-list separator. It is the core env-merge used by the run
// handler and is deliberately platform-agnostic so tests can exercise it
// on any OS with a synthetic VS-env map + fake oneAPI dirs:
//
//   - baseEnv is a slice of "KEY=VALUE" strings (the captured VS env, or
//     os.Environ()).
//   - dllDirs is the ordered oneAPI DLL directories.
//
// If baseEnv has no PATH entry, a new one is synthesized from the dllDirs
// alone (so an MKL inferior still finds its DLLs even if the captured env
// somehow lacked PATH). If dllDirs is empty, baseEnv is returned unchanged
// (a fresh copy). The PATH entry's KEY casing from baseEnv is preserved;
// when synthesizing, the canonical upper-case "PATH" is used. All other
// entries are kept verbatim and in order.
func prependOneAPIToPath(baseEnv []string, dllDirs []string) []string {
	if len(dllDirs) == 0 {
		out := make([]string, len(baseEnv))
		copy(out, baseEnv)
		return out
	}

	prefix := strings.Join(dllDirs, string(os.PathListSeparator))

	out := make([]string, 0, len(baseEnv)+1)
	foundPath := false
	for _, entry := range baseEnv {
		key, val, hasEq := strings.Cut(entry, "=")
		if hasEq && pathKeyMatches(key) {
			foundPath = true
			if val == "" {
				out = append(out, key+"="+prefix)
			} else {
				out = append(out, key+"="+prefix+string(os.PathListSeparator)+val)
			}
			continue
		}
		out = append(out, entry)
	}
	if !foundPath {
		// Synthesize a PATH from the oneAPI dirs alone.
		out = append(out, oneapi.PathKey+"="+prefix)
	}
	return out
}

// computeRunEnv builds the final environment for a run, returning the
// "KEY=VALUE" env slice and the env_source label. The composition:
//
//  1. VS env captured (vcvars64.bat) → prepend oneAPI dirs → "vcvars64+oneapi"
//     (env_source stays "vcvars64+oneapi" even when no oneAPI dir is present,
//     because the VS toolchain — the load-bearing half — WAS captured).
//  2. vcvars not found, oneAPI dirs present → os.Environ()+oneAPI →
//     "oneapi-only".
//  3. neither → os.Environ() unchanged → "plain".
//
// captureVS and oneAPIDirs are passed in (the server's injectable seams)
// so this is fully testable without the real subprocess. It never errors —
// it always yields a runnable environment, only the label changes.
func computeRunEnv(captureVS func() ([]string, bool), oneAPIDirs func() []string) (env []string, source string) {
	dllDirs := oneAPIDirs()

	if vsEnv, ok := captureVS(); ok {
		return prependOneAPIToPath(vsEnv, dllDirs), envSourceVcvarsOneAPI
	}

	if len(dllDirs) > 0 {
		return prependOneAPIToPath(os.Environ(), dllDirs), envSourceOneAPIOnly
	}

	return os.Environ(), envSourcePlain
}
