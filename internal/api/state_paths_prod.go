//go:build !test_state_path_env

// state_paths_prod.go — production variant of daemonStateDir.
//
// Per plan §16 v9: when SHGetKnownFolderPath fails, RETURN ERROR.
// No env fallback. The watchdog CLI translates the error into exit
// code 8 (production fail-closed contract). The fallback variant in
// state_paths_envfallback.go is compiled ONLY when the build is
// invoked with -tags=test_state_path_env, so production binaries
// CANNOT consult LOCALAPPDATA / USERPROFILE for state-dir resolution.
//
// testEnvFallbackBuild is exported as a constant so cross-tag-shared
// tests can detect which variant is active and skip assertions that
// only make sense in the production variant.

package api

import "runtime"

// testEnvFallbackBuild is true iff the env-fallback variant of
// daemonStateDir is the one compiled into the test binary. Always
// false in this file; the env-fallback variant defines it true.
const testEnvFallbackBuild = false

// daemonStateDir resolves the per-user state directory path.
//
// Windows:
//   - Calls knownFolderResolverFn (real impl in state_paths_windows.go
//     wraps windows.KnownFolderPath(FOLDERID_LocalAppData)).
//   - On stub failure: returns errKnownFolderUnavailable. NO ENV
//     FALLBACK in production builds.
//
// Linux / darwin: delegates to posixStateDir which honors XDG_DATA_HOME
// (Linux) or ~/Library/Application Support (macOS) per plan §15.
func daemonStateDir() (string, error) {
	if daemonStateRootOverride != "" {
		// Test escape hatch — never set in production.
		return ensureStateRoot(daemonStateRootOverride)
	}
	if runtime.GOOS == "windows" {
		root, err := resolveKnownFolderProduction()
		if err != nil {
			return "", err
		}
		return ensureStateRoot(joinStateRoot(root))
	}
	return posixStateDir()
}

// resolveKnownFolderProduction is the production Windows path.
// knownFolderResolverFn is set by state_paths_windows.go's init();
// tests stub it via installKnownFolderStub. Returns
// errKnownFolderUnavailable (wrapped) on any resolver failure.
func resolveKnownFolderProduction() (string, error) {
	if knownFolderResolverFn == nil {
		return "", errKnownFolderUnavailable
	}
	root, err := knownFolderResolverFn()
	if err != nil {
		// Wrap the resolver error so callers can inspect the cause
		// while errors.Is(..., errKnownFolderUnavailable) keeps
		// matching the production-fail-closed branch.
		return "", joinKnownFolderErr(err)
	}
	if root == "" {
		return "", errKnownFolderUnavailable
	}
	return root, nil
}
