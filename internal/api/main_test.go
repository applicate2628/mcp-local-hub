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
func TestMain(m *testing.M) {
	EnableSupervisorIPCTestPipeIsolation()
	os.Exit(m.Run())
}
