//go:build test_state_path_env

// state_paths_envfallback.go — test-build variant of daemonStateDir.
//
// Compiled ONLY when the build is invoked with
// -tags=test_state_path_env. Production binaries never see this code
// path (compile-time guarantee per plan §16 v9). Two test-only
// mechanisms live here, BOTH excluded from shipped binaries by the
// build tag:
//
//   - MCPHUB_STATE_DIR_OVERRIDE: a cross-process env override checked
//     BEFORE the platform resolver, mirroring the in-memory
//     daemonStateRootOverride but surviving an exec boundary so a
//     subprocess-spawned `mcphub` child redirects off the live fleet on
//     EVERY GOOS (including a real Windows host where the resolver
//     succeeds). See daemonStateDir for the full rationale.
//   - The LOCALAPPDATA / USERPROFILE resolver-failure fallback chain:
//     unit tests stub knownFolderResolverFn to fail and then verify the
//     fallback — the same chain used historically before the watchdog
//     work tightened production to KnownFolder-only.

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
// state_paths_prod.go in two test-only ways:
//
//   - A cross-process MCPHUB_STATE_DIR_OVERRIDE short-circuit checked
//     BEFORE the GOOS switch (on every platform), so a tagged
//     subprocess child redirects off the live fleet even on a real
//     Windows host where the resolver succeeds (see below).
//   - On the Windows resolver-FAILURE path only: the fallback chain
//     attempts LOCALAPPDATA and then USERPROFILE\AppData\Local before
//     surrendering with errKnownFolderUnavailable.
//
// When neither test mechanism fires the Linux / darwin paths are
// identical to the production variant. (Splitting them into one shared
// helper would mean importing this file from state_paths_prod.go which
// we cannot do because of the mutually-exclusive build tags.
// Duplicating the GOOS switch is cheaper than threading another helper
// through.)
func daemonStateDir() (string, error) {
	if daemonStateRootOverride != "" {
		return ensureStateRoot(daemonStateRootOverride)
	}
	// Cross-process env override (test build ONLY — this file is compiled
	// solely under -tags=test_state_path_env, never into a production
	// binary). It mirrors the in-memory daemonStateRootOverride above but
	// survives an exec boundary: a subprocess-spawned `mcphub` child cannot
	// inherit the parent process's in-memory package var, so cli subprocess
	// tests (gui_integration_test, daemon_reliability_test) set this env on
	// the child's cmd.Env to fence it off the live %LOCALAPPDATA%\mcp-local-hub
	// fleet. It MUST be checked BEFORE the GOOS switch / resolver chain so it
	// wins on a real Windows host where SHGetKnownFolderPath succeeds and the
	// LOCALAPPDATA fallback below would otherwise never fire (PR #300 r2 P1:
	// the resolver-first chain made LOCALAPPDATA inert on real hosts). Same env
	// name the supervisor IPC test-pipe discriminator reads
	// (EnableSupervisorIPCTestPipeIsolation), so ONE env redirects state dir
	// AND IPC pipe together. The existing resolveKnownFolderWithEnvFallback
	// LOCALAPPDATA → USERPROFILE chain is left UNCHANGED below so the
	// resolver-failure fallback unit tests keep passing.
	if v := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); v != "" {
		return ensureStateRoot(v)
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
	// See daemonStateDir for the rationale: cross-process env override wins
	// before the resolver chain so a tagged subprocess child redirects off
	// the live fleet on every GOOS, including a real Windows host.
	if v := os.Getenv("MCPHUB_STATE_DIR_OVERRIDE"); v != "" {
		return v, nil
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
