package oneapirun

import (
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/hubtemp"
	"mcp-local-hub/internal/oneapi"
)

// tempKeys are the environment variable names that point native toolchains
// (icx-cl, link, ml64, …) at a scratch directory for intermediate files.
// Both must point at a WRITABLE dir — the live host inherits TEMP=r:\Temp
// (a RAM disk) which fails icx-cl with "error #10026: error generating
// temporary file".
var tempKeys = []string{"TEMP", "TMP"}

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
		env, source = prependOneAPIToPath(vsEnv, dllDirs), envSourceVcvarsOneAPI
	} else if len(dllDirs) > 0 {
		env, source = prependOneAPIToPath(os.Environ(), dllDirs), envSourceOneAPIOnly
	} else {
		env, source = os.Environ(), envSourcePlain
	}

	// Every env_source path gets a hub-owned writable TEMP/TMP. The child
	// inherits the parent's TEMP (often a RAM disk such as r:\Temp on the
	// live host) which fails icx-cl with "error #10026: error generating
	// temporary file". The working oneapi-shell.cmd wrapper sets TEMP to a
	// writable repo dir + mkdir's it; we do the equivalent here so the agent
	// never has to. Best-effort: if the dir can't be created TEMP is left
	// as-is rather than blocking the run.
	env = withWritableTemp(env)
	return env, source
}

// envValue reads the value of the named env-var KEY from a "KEY=VALUE"
// slice, matching the key case-insensitively (Windows treats env-var names
// case-insensitively, and a vcvars `set` dump can emit "Path" not "PATH").
// Returns ("", false) when no entry matches. The first matching entry wins.
func envValue(env []string, key string) (string, bool) {
	for _, entry := range env {
		k, v, hasEq := strings.Cut(entry, "=")
		if hasEq && strings.EqualFold(k, key) {
			return v, true
		}
	}
	return "", false
}

// resolveCommandPath resolves a bare command NAME against the PATH carried
// in the COMPUTED CHILD env (not the server process's own PATH). This is
// the fix for the classic Go-exec bug: exec.CommandContext resolves a bare
// command via LookPath against the server's os.Getenv("PATH") BEFORE cmd.Env
// is applied, so a tool that only exists on the augmented child PATH (oneAPI
// / VS dirs prepended) fails to start (e.g. icx-cl resolves to "not found"
// even though `where icx-cl` under the child env finds it). By pre-resolving
// against the child PATH and handing exec an absolute path, exec skips its
// own LookPath entirely.
//
// Behavior:
//   - If command already contains a path separator (filepath.Separator or
//     '/'), it is returned unchanged — the caller gave an explicit path.
//   - Otherwise PATH is read from env (case-insensitively) and each dir in
//     filepath.SplitList(PATH) is probed for command + each executable
//     extension (Windows: PATHEXT from the child env, default
//     ".COM;.EXE;.BAT;.CMD"; non-Windows: the bare name). The first existing
//     regular file is returned.
//   - If nothing resolves, command is returned UNCHANGED so exec surfaces a
//     clear "executable file not found" error rather than this helper
//     swallowing the failure.
func resolveCommandPath(command string, env []string) string {
	if strings.ContainsRune(command, filepath.Separator) || strings.ContainsRune(command, '/') {
		return command
	}

	pathVal, ok := envValue(env, oneapi.PathKey)
	if !ok || pathVal == "" {
		return command
	}

	exts := commandExtensions(command, env)
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, command+ext)
			if isRegularFile(candidate) {
				return candidate
			}
		}
	}
	return command
}

// isRegularFile reports whether path names an existing regular file (not a
// directory). Used by resolveCommandPath to confirm a candidate before
// returning it as the resolved command.
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.Mode().IsRegular()
}

// hubTempDir computes the hub-owned writable scratch directory used for the
// child's TEMP/TMP, delegating to the shared hubtemp owner so this server's
// scratch location stays in lockstep with the other MCP servers (e.g.
// drmemory's -logdir). On Windows it resolves to
// %LOCALAPPDATA%\mcp-local-hub\oneapi-run-tmp; see hubtemp.Dir for the full
// per-OS contract and fallbacks. Returns ("", false) only when no candidate
// directory can be derived at all.
func hubTempDir() (string, bool) {
	return hubtemp.Dir("oneapi-run-tmp")
}

// withWritableTemp returns env with TEMP and TMP overridden (case-insensitive
// replace-or-append) to a hub-owned writable directory, creating that
// directory with os.MkdirAll first. It is BEST-EFFORT: if the directory
// cannot be derived or created, env is returned unchanged so a failed temp
// setup never blocks the run. The override (not merely a default-if-absent)
// is deliberate — the inherited TEMP=r:\Temp must be REPLACED, not preserved.
func withWritableTemp(env []string) []string {
	dir, ok := hubTempDir()
	if !ok {
		return env
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		// Best-effort: leave TEMP as-is rather than blocking the run.
		return env
	}
	return setEnvOverride(env, tempKeys, dir)
}

// setEnvOverride returns a copy of env with every key in keys set to val,
// matching existing entries case-insensitively (so an inherited "Temp=..."
// is replaced, not duplicated alongside a new "TEMP="). A key absent from
// env is appended (canonical upper-case form). The casing of a replaced
// entry's existing KEY is preserved. Other entries are kept verbatim and in
// order.
func setEnvOverride(env []string, keys []string, val string) []string {
	out := make([]string, 0, len(env)+len(keys))
	seen := make(map[string]bool, len(keys))

	for _, entry := range env {
		key, _, hasEq := strings.Cut(entry, "=")
		replaced := false
		if hasEq {
			for _, k := range keys {
				if strings.EqualFold(key, k) {
					out = append(out, key+"="+val)
					seen[strings.ToUpper(k)] = true
					replaced = true
					break
				}
			}
		}
		if !replaced {
			out = append(out, entry)
		}
	}

	for _, k := range keys {
		if !seen[strings.ToUpper(k)] {
			out = append(out, k+"="+val)
		}
	}
	return out
}
