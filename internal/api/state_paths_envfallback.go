//go:build test_state_path_env

// state_paths_envfallback.go — test-build variant of daemonStateDir.
//
// Compiled ONLY when the build is invoked with
// -tags=test_state_path_env. Production binaries never see this code
// path (compile-time guarantee per plan §16 v9). The env fallback
// exists so unit tests can stub knownFolderResolverFn to fail and
// then verify the LOCALAPPDATA / USERPROFILE fallback chain — the
// same fallback chain used historically before the watchdog work
// tightened production to KnownFolder-only.

package api

import (
	"path/filepath"
	"runtime"

	"os"
)

// testEnvFallbackBuild is true in this build so cross-tag-shared
// tests can switch behavior. In state_paths_prod.go the constant
// is false.
const testEnvFallbackBuild = true

// daemonStateDir is the env-fallback variant. Differs from
// state_paths_prod.go ONLY in the Windows path: when the resolver
// fails, the fallback chain attempts LOCALAPPDATA and then
// USERPROFILE\AppData\Local before surrendering with
// errKnownFolderUnavailable.
//
// Linux / darwin paths are identical to the production variant.
// (Splitting them into one shared helper would mean importing this
// file from state_paths_prod.go which we cannot do because of the
// mutually-exclusive build tags. Duplicating the GOOS switch is
// cheaper than threading another helper through.)
func daemonStateDir() (string, error) {
	if daemonStateRootOverride != "" {
		return ensureStateRoot(daemonStateRootOverride)
	}
	if runtime.GOOS == "windows" {
		root, err := resolveKnownFolderWithEnvFallback()
		if err != nil {
			return "", err
		}
		return ensureStateRoot(joinStateRoot(root))
	}
	return posixStateDir()
}

func daemonStateDirReadOnly() (string, error) {
	if daemonStateRootOverride != "" {
		return daemonStateRootOverride, nil
	}
	if runtime.GOOS == "windows" {
		root, err := resolveKnownFolderWithEnvFallback()
		if err != nil {
			return "", err
		}
		return joinStateRoot(root), nil
	}
	parent, err := posixParentDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, stateDirName), nil
}

// resolveKnownFolderWithEnvFallback runs the resolver first; if it
// fails OR returns an empty string, consults LOCALAPPDATA, then
// USERPROFILE\AppData\Local. Returns errKnownFolderUnavailable when
// every fallback is empty.
func resolveKnownFolderWithEnvFallback() (string, error) {
	if knownFolderResolverFn != nil {
		if root, err := knownFolderResolverFn(); err == nil && root != "" {
			return root, nil
		}
	}
	if v := os.Getenv("LOCALAPPDATA"); v != "" {
		return v, nil
	}
	if up := os.Getenv("USERPROFILE"); up != "" {
		return filepath.Join(up, "AppData", "Local"), nil
	}
	return "", errKnownFolderUnavailable
}
