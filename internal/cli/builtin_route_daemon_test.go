// internal/cli/builtin_route_daemon_test.go
//
// Guard tests for Increment 1b (work-items/decisions/2026-07-25-supervisor-
// builtin-singleton-daemon.md): the supervisor auto-spawns and manages the
// already-built `mcphub route` front daemon as an ordinary SupervisorDaemon
// descriptor. These tests guard the two invariants specific to the internal/cli
// side of the seam: durability across a simulated re-read, and that the
// production spawn closure's env composition needs zero new code to give the
// route daemon the same console-attach-suppression inheritance every other
// supervised daemon gets for free.
package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
	"mcp-local-hub/internal/process"
)

// TestEnsureBuiltinRouteDaemonAtStartup_PersistsAndSurvivesReread is S4 test 1.
//
// The whole reason ensureBuiltinRouteDaemonAtStartup exists (rather than just
// appending the row to the in-memory `intent` this function already has) is
// that an in-memory-only row is dropped by the next IntentWatcher re-read:
// supervisor_controller.go's 60s intentCache swap only ever keeps what
// refreshSupervisorIntent reads back OFF DISK. This test proves the row
// survives exactly that re-read, not just the caller's own copy.
//
// Mutation proof: temporarily commenting out this function's
// `api.MutateSupervisorIntentIfChanged(...)` call (persisting nothing, only
// mutating the in-memory `intent` argument) makes assertion (b) below fail
// with "re-read of supervisor-intent.json ... is missing" while assertion (a)
// still passes — isolating exactly the orphan-drop bug this test exists to
// catch. Reverted after confirming the failure; see the implementation
// report for the transcript.
func TestEnsureBuiltinRouteDaemonAtStartup_PersistsAndSurvivesReread(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	// Fresh cold start: no supervisor-intent.json on disk yet, matching a
	// clean host (loadIntentFiles returns supervisorIntent==nil on
	// os.ErrNotExist, but the production call site always passes a non-nil
	// *SupervisorIntentFile downstream — mirror that shape here).
	intent := &api.SupervisorIntentFile{Version: 1}

	ensureBuiltinRouteDaemonAtStartup(stateDir, intent, events)

	// Assertion (a): THIS cold start's own in-memory intent (the one about
	// to feed the initial reconcile plan) already carries the row.
	if !supervisorIntentHasBuiltinRouteRow(intent) {
		t.Fatalf("in-memory intent has no %s row after ensureBuiltinRouteDaemonAtStartup", api.BuiltinRouteTaskName)
	}

	// Assertion (b): a SEPARATE, independent read of supervisor-intent.json
	// off disk — standing in for the 60s IntentWatcher poll — still finds
	// the row. This is the assertion that actually distinguishes "persisted"
	// from "only mutated the caller's in-memory copy".
	supervisorIntentPath := filepath.Join(stateDir, "supervisor-intent.json")
	reread, err := api.ReadSupervisorIntent(supervisorIntentPath)
	if err != nil {
		t.Fatalf("re-read supervisor-intent.json: %v", err)
	}
	if !supervisorIntentHasBuiltinRouteRow(reread) {
		t.Fatalf("re-read of supervisor-intent.json (simulating the IntentWatcher's periodic re-read) is missing %s row — the row was not durably persisted, only mutated in-memory", api.BuiltinRouteTaskName)
	}
}

func supervisorIntentHasBuiltinRouteRow(f *api.SupervisorIntentFile) bool {
	if f == nil {
		return false
	}
	for _, d := range f.Daemons {
		if d.TaskName == api.BuiltinRouteTaskName {
			return true
		}
	}
	return false
}

// TestProductionSpawnFn_RouteDescriptorInheritsConsoleAttachSuppression is S4
// test 2. It mirrors internal/daemon's
// TestComposeChildEnvPropagatesConsoleAttachSuppression, but drives the REAL
// production spawn closure (makeProductionSpawnFnWithStatePath) — the same
// closure `mcphub supervise` uses to spawn every daemon — with a descriptor
// carrying the built-in route daemon's reserved identity
// (TaskName/Server/Daemon), swapping only Command/Args to the package's
// existing env-dump helper (TestSpawnEnvDumpHelper, defined in
// supervise_overlay_marker_spawn_test.go) so the test can observe the
// composed cmd.Env without actually needing a `route` binary.
//
// This proves the claim in builtin_route_daemon.go's file header: the route
// daemon needs ZERO new console-attach wiring. The supervisor's own
// os.Environ() (carrying MCPHUB_NO_CONSOLE_ATTACH=1 whenever the supervisor
// itself was launched detached — process.SuppressConsoleAttach) flows into
// EVERY spawned daemon's cmd.Env through the existing
// mergeDaemonEnv(os.Environ(), d.Env, overlayEnv) composition
// (supervise.go), with no per-descriptor special-casing. The route
// descriptor is not special-cased anywhere in that composition, so it
// inherits the marker for free.
//
// Mutation proof: temporarily adding `if d.TaskName ==
// api.BuiltinRouteTaskName { delete env var }`-style special-casing into the
// production spawn closure (or, more simply, temporarily NOT setting
// MCPHUB_NO_CONSOLE_ATTACH in the parent test env via t.Setenv) makes this
// test fail with "missing MCPHUB_NO_CONSOLE_ATTACH" — confirmed during
// development, then reverted; see the implementation report for the
// transcript.
func TestProductionSpawnFn_RouteDescriptorInheritsConsoleAttachSuppression(t *testing.T) {
	tmpHome := apitest.HardenedTempDir(t)
	t.Setenv("MCPHUB_STATE_DIR_OVERRIDE", tmpHome)
	t.Setenv(daemonOverlayAppliedEnvVar, "")
	t.Setenv(daemonOverlayKeysEnvVar, "")

	// The supervisor's own environment carries the console-attach
	// suppression marker, exactly as it would after
	// process.SuppressConsoleAttach launched it detached.
	t.Setenv(process.SuppressConsoleAttachEnv, "1")

	dumpPath := filepath.Join(tmpHome, "route-child-env-dump.txt")
	t.Setenv(overlayMarkerHelperSentinelEnv, "1")
	t.Setenv(overlayMarkerHelperDumpPathEnv, dumpPath)

	eventsPath := filepath.Join(tmpHome, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	crashCh := make(chan crashEvent, 8)
	shutdown := make(chan struct{})
	spawnFn := makeProductionSpawnFnWithStatePath(
		events, NewDaemonRuntimeTracker(), "", nil, "", crashCh, shutdown, nil, false,
	)

	// Route-shaped identity (TaskName/Server/Daemon match
	// api.BuildBuiltinRouteDaemon's output exactly), Command/Args swapped to
	// the env-dump helper for testability — the production closure does not
	// branch on identity, so this is a faithful stand-in for the real
	// descriptor's env composition.
	descriptor := api.SupervisorDaemon{
		TaskName: api.BuiltinRouteTaskName,
		Server:   api.BuiltinRouteServer,
		Daemon:   api.BuiltinRouteDaemonName,
		Command:  os.Args[0],
		Args:     []string{"-test.run=^TestSpawnEnvDumpHelper$"},
	}
	if err := spawnFn(descriptor); err != nil {
		t.Fatalf("spawn fn failed on env-dump helper for route descriptor: %v", err)
	}

	childEnv := waitForEnvDump(t, dumpPath)

	marker, ok := envValueFromDump(childEnv, process.SuppressConsoleAttachEnv)
	if !ok || marker != "1" {
		t.Fatalf("route-descriptor child env %s = %q (present=%v), want \"1\" — "+
			"the production spawn closure must propagate the supervisor's own "+
			"console-attach suppression to every daemon it spawns, including the "+
			"built-in route daemon, with no new per-descriptor wiring",
			process.SuppressConsoleAttachEnv, marker, ok)
	}
}

// TestBuildBuiltinRouteDaemon_PortMatchesArgsPortFlag is S4 test 5. It guards
// the descriptor-shape invariant BuildBuiltinRouteDaemon must hold for the
// reconcile/spawn path to make sense at all: the SupervisorDaemon.Port field
// (used for liveness/port-owner checks) and the `--port` value actually
// passed to the spawned `mcphub route` process (used at bind time) must
// never drift apart. Both are derived from DefaultRouteDaemonPort, the
// single-owner port constant (internal/cli/route.go).
//
// Mutation proof: temporarily hardcoding a different literal into either
// BuildBuiltinRouteDaemon's Port field or its Args `--port` value (breaking
// the single-source-of-truth) makes this test fail with a Port/Args
// mismatch. Confirmed during development, then reverted; see the
// implementation report for the transcript.
func TestBuildBuiltinRouteDaemon_PortMatchesArgsPortFlag(t *testing.T) {
	const command = "/fake/mcphub"
	d := api.BuildBuiltinRouteDaemon(command, DefaultRouteDaemonPort)

	if d.Command != command {
		t.Errorf("Command = %q, want %q", d.Command, command)
	}
	if d.Port != DefaultRouteDaemonPort {
		t.Fatalf("descriptor Port = %d, want DefaultRouteDaemonPort (%d)", d.Port, DefaultRouteDaemonPort)
	}

	portFlagValue := ""
	for i := 0; i+1 < len(d.Args); i++ {
		if d.Args[i] == "--port" {
			portFlagValue = d.Args[i+1]
		}
	}
	if portFlagValue == "" {
		t.Fatalf("Args %v has no --port flag value", d.Args)
	}
	wantPortFlagValue := DefaultRouteDaemonPort
	gotPortFlagValue, err := strconv.Atoi(portFlagValue)
	if err != nil {
		t.Fatalf("parse --port value %q as int: %v", portFlagValue, err)
	}
	if gotPortFlagValue != wantPortFlagValue {
		t.Fatalf("Args --port value = %q (%d), want %d — Port field and the "+
			"spawned process's own --port flag must derive from the same "+
			"DefaultRouteDaemonPort constant, not drift apart",
			portFlagValue, gotPortFlagValue, wantPortFlagValue)
	}
	if d.Args[0] != "route" {
		t.Errorf(`Args[0] = %q, want "route" (the reconcile spawn-exclusion `+
			`predicates IsSerenaProxyDescriptor/IsWorkspaceLSPProxyDescriptor `+
			`both require Args[0]=="daemon"; "route" must stay distinct)`, d.Args[0])
	}
}
