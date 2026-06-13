package cli

import "path/filepath"

// Subprocess state-dir isolation helpers (PR #300 r1 P1).
//
// THE GAP THE GLOBAL TestMain DID NOT CLOSE. The cli TestMain
// (settings_registry_test.go) installs a PROCESS-GLOBAL
// daemonStateRootOverride via api.SetDaemonStateRootForTest. That override
// is an IN-MEMORY package variable: it only redirects state-dir resolution
// inside the CURRENT test process. CLI tests that SPAWN a fresh `mcphub`
// binary as an OS subprocess (gui_integration_test.go runs
// `go run ./cmd/mcphub gui ...`; daemon_reliability_test.go builds + runs
// the daemon command) do NOT inherit that in-memory variable. The child
// reaches ensureSupervisorRunning -> api.DaemonStateDir() and resolves the
// REAL per-user state/IPC path — so the live-fleet-touch hazard the
// TestMain redirect was meant to eliminate is still reachable through a
// subprocess.
//
// PRODUCTION-BUILD vs TEST-TAG-BUILD DISTINCTION (verified against
// internal/api/state_paths_prod.go, state_paths_envfallback.go,
// state_paths_windows.go, state_paths_unix.go):
//
//   - A `go run ./cmd/mcphub` child WITHOUT -tags test_state_path_env is a
//     PRODUCTION build. On Windows production, daemonStateDir() resolves the
//     state root via windows.KnownFolderPath(FOLDERID_LocalAppData), which
//     does NOT read ANY env var. So NO env var can redirect a production
//     Windows child — its daemon state dir is the real
//     %LOCALAPPDATA%\mcp-local-hub regardless of cmd.Env.
//   - The -tags test_state_path_env build compiles state_paths_envfallback.go
//     instead. THAT variant honors MCPHUB_STATE_DIR_OVERRIDE BEFORE the
//     platform resolver (state_paths_envfallback.go daemonStateDir), so a
//     tagged child inheriting that env redirects its daemon state dir to the
//     override on Windows AND POSIX. (The older LOCALAPPDATA → USERPROFILE
//     chain in that file fires ONLY on resolver FAILURE, which never happens
//     on a real Windows host where SHGetKnownFolderPath succeeds — so
//     LOCALAPPDATA alone is INERT for the daemon state dir on a real host;
//     PR #300 r2 P1.)
//   - On POSIX both build variants resolve the daemon state dir via
//     posixStateDir() -> posixParentDir(), which on Linux honors
//     XDG_DATA_HOME (NOT XDG_STATE_HOME — that var only redirects the daemon
//     *log* base dir in daemon.go:logBaseDir). macOS production ignores env
//     for the state dir entirely (~/Library/Application Support via $HOME);
//     we deliberately do NOT clobber HOME (broad side effects), and the
//     MCPHUB_STATE_DIR_OVERRIDE short-circuit (env-fallback build) covers the
//     daemon state dir on darwin too without touching HOME.
//
// SEPARATE consumer of LOCALAPPDATA: the GUI single-instance pidport path
// (gui.AppDataDir / PidportPath, internal/gui/paths.go) and the daemon log
// base dir (logBaseDir) BOTH read LOCALAPPDATA directly (NOT via
// daemonStateDir), so they redirect under the env-fallback AND production
// build. This is why LOCALAPPDATA must STILL be set (for logs + pidport) even
// though the daemon state dir now redirects via MCPHUB_STATE_DIR_OVERRIDE.
//
// THE ISOLATION RECIPE both subprocess tests must apply:
//  1. Build the child WITH the env-fallback tag (goRunArgsWithTestTag /
//     the -tags flag on go build) so the child honors MCPHUB_STATE_DIR_OVERRIDE
//     in daemonStateDir.
//  2. Set the child's state env to a per-test temp dir on cmd.Env
//     (childStateEnv): MCPHUB_STATE_DIR_OVERRIDE (daemon state dir + supervisor
//     IPC pipe discriminator, the authoritative redirect on every GOOS),
//     LOCALAPPDATA (Windows GUI pidport + log base dir), XDG_DATA_HOME (Linux
//     daemon state dir belt-and-suspenders), XDG_STATE_HOME (POSIX log base
//     dir).
//
// A grandchild matters too: the gui child spawns `mcphub supervise` as
// os.Executable() (the same env-fallback-tagged temp binary `go run` built)
// and inherits the gui child's full environment, so the grandchild resolves
// the same temp state dir AND the same test IPC pipe. One env+tag treatment on
// the gui spawn covers the whole subtree.

// childStateDirOverrideLeaf returns the daemon-state-dir leaf a child
// redirected via childStateEnv(tmp) resolves through api.DaemonStateDir().
// This is a DISTINCT subdir of `tmp` (NOT <tmp>/mcp-local-hub) so existence
// of this dir is CONCLUSIVE proof the child's daemonStateDir() honored
// MCPHUB_STATE_DIR_OVERRIDE — the GUI pidport path (gui.AppDataDir) and the
// log base dir create <tmp>/mcp-local-hub from LOCALAPPDATA and would mask a
// shared leaf, so we keep the daemon-state-dir override on its own subdir to
// keep the proof unambiguous (PR #300 r2 P2).
func childStateDirOverrideLeaf(tmp string) string {
	return filepath.Join(tmp, "supervisor-state")
}

// childStateEnv returns the env-var assignments that fence a
// test_state_path_env-tagged `mcphub` child off the live fleet, on Windows
// AND POSIX. Append the result to os.Environ() when building a subprocess's
// cmd.Env.
//
//   - MCPHUB_STATE_DIR_OVERRIDE -> daemonStateDir() (env-fallback build,
//     BEFORE the platform resolver) on EVERY GOOS, AND the supervisor IPC
//     test-pipe discriminator (EnableSupervisorIPCTestPipeIsolation /
//     SupervisorIPCAddress) so state dir + IPC pipe redirect together. Points
//     at a DISTINCT subdir (childStateDirOverrideLeaf) so the daemon-state-dir
//     proof is unambiguous vs the LOCALAPPDATA-derived <tmp>/mcp-local-hub.
//   - LOCALAPPDATA   -> Windows GUI pidport (gui.AppDataDir) + logBaseDir().
//   - XDG_DATA_HOME  -> Linux daemonStateDir() (posixParentDir) belt-and-suspenders.
//   - XDG_STATE_HOME -> POSIX logBaseDir() (daemon log destination).
func childStateEnv(tmp string) []string {
	return []string{
		"MCPHUB_STATE_DIR_OVERRIDE=" + childStateDirOverrideLeaf(tmp),
		"LOCALAPPDATA=" + tmp,
		"XDG_DATA_HOME=" + tmp,
		"XDG_STATE_HOME=" + tmp,
	}
}

// goRunArgsWithTestTag builds the argv for `go run` with the
// test_state_path_env build tag applied to the compiled child, followed by
// the package path and the child's own arguments. The -tags flag is a `go
// run` build flag and MUST precede the package path (verified: `go run
// [build flags] package [arguments...]`).
//
// Example: goRunArgsWithTestTag("./cmd/mcphub", "gui", "--no-browser")
// yields {"run", "-tags", "test_state_path_env", "./cmd/mcphub", "gui",
// "--no-browser"}. Building with the tag makes the child honor the
// childStateEnv redirection (see the production-vs-test-tag note above).
func goRunArgsWithTestTag(pkgAndArgs ...string) []string {
	args := []string{"run", "-tags", "test_state_path_env"}
	return append(args, pkgAndArgs...)
}
