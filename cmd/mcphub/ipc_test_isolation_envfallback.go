//go:build test_state_path_env

// ipc_test_isolation_envfallback.go — child-process supervisor IPC test-pipe
// isolation (PR #300 r3 Finding 1, FLEET-SAFETY).
//
// Compiled ONLY when the binary is built with -tags=test_state_path_env.
// Production binaries never see this file, so a release `mcphub` always
// resolves the SID-based production supervisor pipe (no init runs at all).
//
// THE GAP THIS CLOSES. The supervisor IPC test-pipe discriminator
// (api.EnableSupervisorIPCTestPipeIsolation -> the package var
// supervisorIPCTestPipeDiscriminator read by api.SupervisorIPCAddress on
// Windows) is a per-PROCESS in-memory hook. The internal/cli TestMain installs
// it, but that only affects the PARENT test binary. A cli test that SPAWNS a
// fresh `mcphub` binary as an OS subprocess (gui_integration_test runs
// `go run -tags test_state_path_env ./cmd/mcphub gui ...`; that gui child in
// turn spawns a `mcphub supervise` grandchild) does NOT run the parent's
// TestMain, so in the child the discriminator package var is nil. On Windows,
// api.SupervisorIPCAddress then ignores the stateDir redirect and falls through
// to the PRODUCTION pipe `\\.\pipe\mcphub-supervisor-<SID>` — the LIVE
// supervisor's pipe — even though MCPHUB_STATE_DIR_OVERRIDE correctly redirected
// the child's FILE-SYSTEM state dir. The gui child + supervise grandchild would
// then probe/bind the live SID pipe, a real live-fleet collision hazard.
//
// THE FIX. Install the discriminator in the CHILD itself, at process init
// (which Go runs before main(), so before any cobra command or IPC dial). The
// discriminator api.EnableSupervisorIPCTestPipeIsolation derives the test pipe
// name from MCPHUB_STATE_DIR_OVERRIDE (the same env childStateEnv sets on the
// child's cmd.Env), so once installed the child's api.SupervisorIPCAddress
// resolves `\\.\pipe\mcphub-supervisor-test-<hash(override)>` — a UNIQUE test
// pipe derived from the per-test temp dir, NEVER the production SID pipe. The
// state dir and the IPC pipe then redirect together off the live fleet.
//
//   - We gate on MCPHUB_STATE_DIR_OVERRIDE being non-empty so a tagged binary
//     run WITHOUT the override (e.g. a developer manually invoking a tagged
//     build) keeps the production SID pipe and is not silently redirected to an
//     empty-discriminator dead end. EnableSupervisorIPCTestPipeIsolation's
//     discriminator closure itself returns "" when the override is empty (and
//     SupervisorIPCAddress then takes the SID path), so this guard is belt-and-
//     suspenders, not load-bearing — but it makes the intent explicit and keeps
//     a tagged-but-unredirected run on the production pipe.
//   - api.EnableSupervisorIPCTestPipeIsolation is idempotent: it merely
//     (re)assigns the package-level discriminator closure. Calling it from this
//     init in addition to the parent test's TestMain is harmless.
//   - On POSIX api.EnableSupervisorIPCTestPipeIsolation is a no-op and
//     api.SupervisorIPCAddress already derives the unix-socket path from the
//     redirected stateDir, so the POSIX child is isolated by the state-dir
//     redirect alone; calling the no-op here is harmless.
package main

import (
	"os"
	"strings"

	"mcp-local-hub/internal/api"
)

func init() {
	if strings.TrimSpace(os.Getenv("MCPHUB_STATE_DIR_OVERRIDE")) == "" {
		return
	}
	// Redirect the supervisor IPC pipe (Windows: per-test pipe derived from the
	// override; POSIX: no-op) so a subprocess-spawned child binds the TEST pipe,
	// not the live SID pipe.
	api.EnableSupervisorIPCTestPipeIsolation()
}
