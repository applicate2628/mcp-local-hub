// Package binary_discovery resolves required tool binaries (python3, node,
// rustc, etc.) against an ordered list of hint directories.
//
// The package is best-effort: missing binaries map to "" rather than
// producing an error, so a manifest declaring multiple RequiredBinaries
// can be partially satisfied without aborting the caller.
//
// Discovery semantics:
//
//   - Hints are walked in order; the first hint directory that contains
//     the binary wins for that binary.
//   - On Windows, each candidate name is probed as "<bin>.exe" first and
//     then "<bin>" — many tools (python, node) ship as ".exe" on Windows,
//     but the manifest writer uses the bare stem.
//   - Environment-variable references inside hint strings are expanded
//     via `os.ExpandEnv` before each Stat. `os.ExpandEnv` understands
//     `$VAR` and `${VAR}` syntax ONLY — Windows-style `%VAR%`
//     placeholders are left as literals (see hints_windows.go for the
//     correct `${USERPROFILE}` / `${LOCALAPPDATA}` spelling). The
//     per-OS hint catalogue lives in DefaultHints().
//   - ctx.Err() is consulted between binaries so a cancelled context
//     terminates discovery promptly.
package binary_discovery

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
)

// Discover walks the hint directories in order and returns a mapping from
// each requested binary stem to the absolute path where it was found, or
// "" if the binary was not present in any hint.
//
// The function never returns an error for missing binaries; the only
// error returned is ctx.Err() if the caller's context is cancelled during
// the walk.
func Discover(ctx context.Context, requiredBinaries []string, hints []string) (map[string]string, error) {
	out := make(map[string]string, len(requiredBinaries))
	for _, bin := range requiredBinaries {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out[bin] = resolveOne(bin, hints)
	}
	return out, nil
}

// resolveOne walks hints in order and returns the first absolute path
// where bin (or its platform-specific variants) is found. Empty string if
// nothing matches.
func resolveOne(bin string, hints []string) string {
	for _, hint := range hints {
		expanded := os.ExpandEnv(hint)
		if expanded == "" {
			continue
		}
		for _, candidate := range candidateNames(bin) {
			full := filepath.Join(expanded, candidate)
			if fi, err := os.Stat(full); err == nil && !fi.IsDir() {
				return full
			}
		}
	}
	return ""
}

// candidateNames returns the platform-specific filename variants to try
// for a given binary stem. On Windows we try "<bin>.exe" first because
// that is the dominant on-disk form for the tools we care about
// (python.exe, node.exe, rustc.exe); falling back to bare "<bin>"
// covers anomalies like rare extension-less helpers.
func candidateNames(bin string) []string {
	if runtime.GOOS == "windows" {
		return []string{bin + ".exe", bin}
	}
	return []string{bin}
}
