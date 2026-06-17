package api

import (
	"os"
	"testing"
)

// TestMain installs the supervisor IPC test-pipe discriminator for the whole
// internal/api test binary. Without it, SupervisorIPCAddress resolves the
// kernel-authoritative user SID first (supervisor_ipc_address_windows.go) and
// only falls back to USERNAME on a SID-query failure. On a real Windows host
// the SID always resolves, so the IPC tests that LISTEN bind the PRODUCTION
// pipe `\\.\pipe\mcphub-supervisor-<SID>` — which collides with the live
// running supervisor and fails `winio.ListenPipe` with "Access is denied".
// (The legacy `t.Setenv("USERNAME", ...)` isolation in
// supervisor_ipc_status_client_windows_test.go was silently dead-ended by the
// PR #212 SID migration: USERNAME no longer participates in the pipe name on
// the SID-resolving happy path.)
//
// EnableSupervisorIPCTestPipeIsolation installs a runtime discriminator keyed
// off MCPHUB_STATE_DIR_OVERRIDE; the LISTEN helper sets that env var per-test
// to a unique temp dir so every test binds `\\.\pipe\mcphub-supervisor-test-<hash>`
// instead of the SID/production pipe. POSIX is a no-op (the unix socket already
// derives from the per-test stateDir). Mirrors internal/cli's TestMain
// (settings_registry_test.go). This is a runtime hook, not a build tag, so it
// takes effect in both the default untagged `go test ./...` build and the
// -tags=test_state_path_env build, and is absent from release binaries (which
// never call it).
//
// TestMain ALSO installs a global state-root fence (test-state-leak hygiene).
// Many api paths emit
// observability events (LogHubMcpEvent → hub-mcp.log, serialized on the BLOCKING
// hub-mcp.log.lock flock) or write managed-entries.json, resolving the target
// through DaemonStateDir() = daemonStateRootOverride first. Tests that forget to
// redirect the state dir would write those into the operator's real
// %LOCALAPPDATA%\mcp-local-hub and contend cross-process with internal/cli +
// internal/gui under parallel `go test ./...` (the blocking hub-mcp.log.lock has
// no timeout, so a held lock from one package stalls another's emit past its
// -timeout). Defaulting daemonStateRootOverride to a throwaway temp dir for the
// whole api test binary fences every such emit. Mirrors internal/cli +
// internal/gui TestMain.
//
// SAFE COMPOSITION. daemonStateRootOverride is the in-memory seam that EVERY
// state-redirecting api test already drives through statePathsHelper(t) (which
// saves the CURRENT value — now this global default — and restores TO it on
// cleanup, so per-test overrides nest correctly). The only tests that need the
// override EMPTY are the resolver-chain tests (state_paths_test.go,
// state_paths_envfallback_test.go) which assert daemonStateDir()'s real
// LOCALAPPDATA/KnownFolder behavior; statePathsHelper(t) now clears the override
// at entry so those exercise the real resolver while still restoring the global
// default afterward. This is in-memory ONLY — the global does NOT set the
// MCPHUB_STATE_DIR_OVERRIDE env var, so the supervisor IPC test-pipe
// discriminator (EnableSupervisorIPCTestPipeIsolation, keyed off that env per
// LISTEN test) and the env-honoring resolver tests are untouched.
//
// os.Exit-safety: defers do NOT run after os.Exit, so cleanup is performed
// explicitly after capturing m.Run()'s exit code.
func TestMain(m *testing.M) {
	EnableSupervisorIPCTestPipeIsolation()

	tmp, err := os.MkdirTemp("", "mcphub-api-test-state-*")
	if err != nil {
		panic("internal/api TestMain: create global test-state temp dir: " + err.Error())
	}
	prevOverride := daemonStateRootOverride
	daemonStateRootOverride = tmp

	code := m.Run()

	daemonStateRootOverride = prevOverride
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
