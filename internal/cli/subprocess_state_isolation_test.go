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
//     does NOT read the LOCALAPPDATA env var. So NO env var can redirect a
//     production Windows child — setting LOCALAPPDATA on cmd.Env is inert.
//   - The -tags test_state_path_env build compiles state_paths_envfallback.go
//     instead, whose resolveKnownFolderWithEnvFallback() honors LOCALAPPDATA
//     (then USERPROFILE\AppData\Local) on Windows.
//   - On POSIX both build variants resolve via posixStateDir() ->
//     posixParentDir(), which on Linux honors XDG_DATA_HOME (NOT
//     XDG_STATE_HOME — that var only redirects the daemon *log* base dir in
//     daemon.go:logBaseDir). macOS production ignores env entirely
//     (~/Library/Application Support via $HOME), so on darwin the only env
//     lever is HOME; we deliberately do NOT clobber HOME (broad side effects),
//     and the darwin subprocess tests are skipped/unaffected in practice.
//
// THE ISOLATION RECIPE both subprocess tests must apply:
//  1. Build the child WITH the env-fallback tag (goRunArgsWithTestTag /
//     the -tags flag on go build) so the child honors env-based redirection.
//  2. Set the child's state env to a per-test temp dir on cmd.Env
//     (childStateEnv): LOCALAPPDATA (Windows) + XDG_DATA_HOME (Linux state
//     dir) + XDG_STATE_HOME (daemon log base dir). Setting all three keeps
//     the test platform-portable.
//
// A grandchild matters too: the gui child spawns `mcphub supervise` as
// os.Executable() (the same env-fallback-tagged temp binary `go run` built)
// and inherits the gui child's full environment, so the grandchild resolves
// the same temp state dir. One env+tag treatment on the gui spawn covers the
// whole subtree.

// childStateEnv returns the env-var assignments that redirect a
// test_state_path_env-tagged `mcphub` child's daemon state dir (and daemon
// log base dir) to the per-test temp dir `tmp`, on Windows AND POSIX. Append
// the result to os.Environ() when building a subprocess's cmd.Env.
//
//   - LOCALAPPDATA   -> Windows daemonStateDir() (env-fallback build only) and
//     logBaseDir().
//   - XDG_DATA_HOME  -> Linux daemonStateDir() (posixParentDir).
//   - XDG_STATE_HOME -> POSIX logBaseDir() (daemon log destination).
//
// All three point at the same `tmp` so the child's resolved
// <tmp>/mcp-local-hub leaf is stable and assertable across platforms.
func childStateEnv(tmp string) []string {
	return []string{
		"LOCALAPPDATA=" + tmp,
		"XDG_DATA_HOME=" + tmp,
		"XDG_STATE_HOME=" + tmp,
	}
}

// childStateLeaf returns the per-user state leaf path a child redirected via
// childStateEnv(tmp) resolves to (<tmp>/mcp-local-hub). Tests assert the
// child created its pidport/state UNDER this leaf — proving the subprocess
// used the temp dir and never touched the real %LOCALAPPDATA%\mcp-local-hub.
func childStateLeaf(tmp string) string {
	return filepath.Join(tmp, "mcp-local-hub")
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
