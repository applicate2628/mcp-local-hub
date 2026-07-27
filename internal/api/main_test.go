package api

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/clients"
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
	// caller_start_time oracle child fast-path. Sentinel-gated, short-circuits
	// BEFORE m.Run() so the child never runs the suite. It emits one
	// intent-audit row into a parent-chosen state dir after a deliberate delay,
	// giving TestIntentAudit_CallerStartTimeAgainstIndependentOracle a process
	// start time it can check against the PARENT's clock rather than against
	// the helper under test. Body lives in intent_audit_caller_oracle_test.go.
	if stateDir := os.Getenv(callerStartTimeChildStateDirEnv); stateDir != "" {
		runCallerStartTimeOracleChild(stateDir)
	}

	EnableSupervisorIPCTestPipeIsolation()

	// Default-disable the MiMoCode ~/.claude.json import + home-.mimocode read
	// (clients.MimoCodeDisableClaudeImportEnv) for the whole api package, so no
	// api test that runs a mimo scan/shadow walk without redirecting HOME ever
	// reads the developer's REAL ~/.claude.json / ~/.mimocode. The B6 resolver is
	// HOME||USERPROFILE, and not every cleanup/scan test overrides HOME, so this
	// is the single-owner state-safety default; the import-specific tests opt back
	// in with t.Setenv(MimoCodeDisableClaudeImportEnv, "") + a temp HOME.
	os.Setenv("MIMOCODE_DISABLE_CLAUDE_CODE_MCP", "1")

	tmp, err := os.MkdirTemp("", "mcphub-api-test-state-*")
	if err != nil {
		panic("internal/api TestMain: create global test-state temp dir: " + err.Error())
	}
	prevOverride := daemonStateRootOverride
	daemonStateRootOverride = tmp

	// ── PRIMARY seal: the process-lookup / wmic family ──────────────────────
	// lookupProcess + lookupProcessBatch are wired at processes.go init() to real
	// `netstat -ano` + `wmic` shell-outs on Windows (~31s per wmic call on Win11
	// 24H2). killByPortFn / forceKillByPortFn / status enrichment all funnel
	// through them, so a test that seeds a real manifest port and reaches a
	// kill/enrich path shells out to the live OS process table — and a seeded port
	// can COLLIDE with a live daemon on the developer host (e.g. serena on 19150),
	// touching the live fleet. Nil-ing them makes the Windows test binary behave
	// like the always-green ubuntu-latest CI lane (where these are already nil) and
	// makes every port-kill seam short-circuit to portKillLookupUnavailable/nil
	// (install.go ~3461). This is the dominant timeout cost; several tests already
	// nil lookupProcess manually (restart_supervisor_test.go, status_workspace_test.go).
	prevLookupProcess := lookupProcess
	prevLookupProcessBatch := lookupProcessBatch
	lookupProcess = nil
	lookupProcessBatch = nil

	// ── Belt-and-suspenders seal: the supervisor-IPC dial seams ─────────────
	// The daemonStateRootOverride temp-dir pin above already makes these dials
	// fast-fail (no supervisor.lock.owner.json in the temp dir → the dial returns
	// the unavailable shape before touching the pipe). This default closes the
	// narrow edge where a test clears the override (statePathsHelper) without
	// stubbing the KnownFolder resolver — which would otherwise resolve the real
	// %LOCALAPPDATA% and dial the developer's LIVE supervisor pipe (on Windows the
	// pipe name keys off the process SID, not the state dir). Each default returns
	// exactly the shape a no-supervisor host produces, so no caller branch flips.
	// Tests needing a specific response override + restore to this default.
	prevRegisterReconcile := registerSupervisorReconcileFn
	prevReconcileApply := supervisorReconcileApplyFn
	prevRestartRespawn := supervisorRestartRespawnFn
	prevStatusInternalDial := statusInternalDialFn
	prevSupervisorIPCStatus := supervisorIPCStatusFn
	registerSupervisorReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	supervisorReconcileApplyFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	// NOTE: respawn returns a POPULATED result with a nil error — matching
	// DialSupervisorIPCRespawn's own no-owner-sidecar contract
	// (supervisor_ipc_respawn_client.go: RespawnResult{Code:"SUPERVISOR_UNAVAILABLE"},
	// nil). Its callers branch on result.Success / result.Code, NEVER the err; an
	// ErrSupervisorIPCUnavailable error return here would manufacture an error row
	// the real no-supervisor path never produces.
	supervisorRestartRespawnFn = func(context.Context, string, bool, int) (RespawnResult, error) {
		return RespawnResult{Code: "SUPERVISOR_UNAVAILABLE", Message: "test default: no supervisor"}, nil
	}
	statusInternalDialFn = func(context.Context) ([]DaemonStatus, error) {
		return nil, ErrSupervisorIPCUnavailable
	}
	supervisorIPCStatusFn = func(context.Context) ([]DaemonStatus, error) {
		return nil, ErrSupervisorIPCUnavailable
	}
	// installSupervisorRunningProbeFn is LEFT REAL: it is a non-blocking flock
	// TryLock on the already-fenced temp state dir (fast, returns not-running), and
	// several tests acquire a real lock under their own state dir and rely on it
	// returning true. SupervisorIPCStatusFn (health.go) is LEFT NIL: nil already
	// means "no IPC seam wired" (falls to the scheduler-scan default).

	// ── Additional live-fleet / real-I/O seams (fable-review, second pass) ──
	// serenaWakeReconcileFn is an 8th supervisor-IPC dial. loopbackPortOwnerFn
	// shells out to netstat. installAutostart* run a REAL `schtasks /Run` of the
	// live supervisor task (a fleet-touch NOT covered by lookupProcess=nil; the
	// call sites warn-and-continue on error). proxyReadinessFn is a 10s HTTP retry.
	// taskkillProcessTreeByPIDFn is sealed defense-in-depth so the default test
	// path can NEVER taskkill a real process tree even if a future kill site
	// bypasses the lookupProcess short-circuit.
	prevSerenaWakeReconcile := serenaWakeReconcileFn
	prevAutoRegisterReconcile := autoRegisterReconcileFn
	prevLoopbackPortOwner := loopbackPortOwnerFn
	prevAutostartOwnerStart := installAutostartOwnerStartFn
	prevAutostartBackendFactory := installAutostartBackendFactoryFn
	prevProxyReadiness := proxyReadinessFn
	prevAutoRegisterReadiness := autoRegisterReadinessFn
	prevTaskkillTree := taskkillProcessTreeByPIDFn
	prevStopForceKillPID := stopForceKillPIDFn
	prevSnapshotProcesses := snapshotProcessesFn
	serenaWakeReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	autoRegisterReconcileFn = func(context.Context, bool) (ReconcileResponse, error) {
		return ReconcileResponse{}, ErrSupervisorIPCUnavailable
	}
	loopbackPortOwnerFn = func(int) (int, bool, error) { return 0, false, nil }
	installAutostartOwnerStartFn = func() error {
		return errors.New("autostart owner start sealed by TestMain default-stub")
	}
	installAutostartBackendFactoryFn = func() (autostart.Backend, error) {
		return nil, errors.New("autostart backend sealed by TestMain default-stub")
	}
	proxyReadinessFn = func(int, time.Duration) error {
		return errors.New("proxy readiness sealed by TestMain default-stub")
	}
	// Serena auto-register has its OWN readiness seam (verifyProxyReady, up to a 20s
	// poll) distinct from proxyReadinessFn.
	autoRegisterReadinessFn = func(int, time.Duration) error {
		return errors.New("auto-register readiness sealed by TestMain default-stub")
	}
	// serenaWakeReadinessFn is deliberately NOT sealed here: the WakeIdleSerena
	// tests integration-exercise the real wake-readiness flow with their OWN
	// controlled supervisor-PID / port-owner deps (which ARE sealed) plus a fake
	// serena listener, so a global seal would short-circuit the exact logic they
	// test. Its slow deps (supervisorIPCStatusFn / loopbackPortOwnerFn) are sealed
	// above, so a test that does NOT control them cannot reach a live poll.
	taskkillProcessTreeByPIDFn = func(int) error {
		return errors.New("taskkill sealed by TestMain default-stub (a test must not reap a real process tree)")
	}
	// POSIX force-kill goes through a SEPARATE seam (process.TreeKillByPID) that the
	// Windows-only taskkill seam does not cover — seal it too so a portless
	// force-stop test can never SIGKILL a real process group.
	stopForceKillPIDFn = func(int) error {
		return errors.New("force-kill PID sealed by TestMain default-stub")
	}
	// CleanupLogWatchers shells out to wmic/ps through snapshotProcessesFn, then
	// kills matched PIDs via killOnePID (not the taskkill seams) — seal to an empty
	// snapshot so the default test path scans nothing and kills nothing.
	snapshotProcessesFn = func() ([]processRow, error) { return nil, nil }

	// ── Client-config sandbox audit ─────────────────────────────────────────
	// Fails any test whose admitted adapters resolve to a config path outside
	// the test sandbox — including adapters constructed by the production code
	// under test (adopt's DefaultScanConfigPaths fan-out, cleanup's unfiltered
	// AllStdioEntries walk, install's per-binding BackupKeep+AddEntry). The
	// MIMOCODE_DISABLE_CLAUDE_CODE_MCP default above closes exactly ONE channel
	// of that class; this closes the class. Contract and the report-mode knob:
	// internal/clients/config_path_sandbox_audit.go.
	auditRestore := clients.EnforceSandboxedConfigPaths(tmp)

	code := m.Run()

	if escapes := auditRestore(); escapes > 0 && code == 0 {
		code = 1
	}

	daemonStateRootOverride = prevOverride
	lookupProcess = prevLookupProcess
	lookupProcessBatch = prevLookupProcessBatch
	registerSupervisorReconcileFn = prevRegisterReconcile
	supervisorReconcileApplyFn = prevReconcileApply
	supervisorRestartRespawnFn = prevRestartRespawn
	statusInternalDialFn = prevStatusInternalDial
	supervisorIPCStatusFn = prevSupervisorIPCStatus
	serenaWakeReconcileFn = prevSerenaWakeReconcile
	autoRegisterReconcileFn = prevAutoRegisterReconcile
	loopbackPortOwnerFn = prevLoopbackPortOwner
	installAutostartOwnerStartFn = prevAutostartOwnerStart
	installAutostartBackendFactoryFn = prevAutostartBackendFactory
	proxyReadinessFn = prevProxyReadiness
	autoRegisterReadinessFn = prevAutoRegisterReadiness
	taskkillProcessTreeByPIDFn = prevTaskkillTree
	stopForceKillPIDFn = prevStopForceKillPID
	snapshotProcessesFn = prevSnapshotProcesses
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
