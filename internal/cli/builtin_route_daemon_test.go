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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
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
// Assertion (a) reads the RETURN VALUE, not the original `intent` variable
// passed in (finding A3 fix, architecture-adversarial-reverify.md +
// qa-adversarial-falsifiers.md): the caller's own pre-lock copy is no longer
// mutated in place — the function instead returns the exact generation
// observed/committed under the supervisor-intent flock, which the caller
// (internal/cli/supervise.go's runSupervise) is documented to reassign its
// own `intent` variable to. Checking the original pointer's identity would
// test a since-removed implementation detail, not the documented contract.
//
// Mutation proof: temporarily commenting out this function's
// `api.MutateSupervisorIntentIfChangedReturning(...)` call (persisting
// nothing, returning the original intent untouched) makes assertion (b)
// below fail with "re-read of supervisor-intent.json ... is missing" while
// assertion (a) also fails (the returned value would carry no row either,
// since nothing would have been persisted or reapplied) — isolating exactly
// the orphan-drop bug this test exists to catch. Reverted after confirming
// the failure; see the implementation report for the transcript.
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

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, intent, events)

	// Assertion (a): THIS cold start's own returned intent (the one the
	// caller reassigns its `intent` variable to, and about to feed the
	// initial reconcile plan) already carries the row.
	if !supervisorIntentHasBuiltinRouteRow(got) {
		t.Fatalf("returned intent has no %s row after ensureBuiltinRouteDaemonAtStartup", api.BuiltinRouteTaskName)
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

func TestEnsureBuiltinRouteDaemonAtStartup_StableFrontKeepsAdmittedPortAcrossRequestedChange(t *testing.T) {
	const (
		admittedPort  = 19137
		requestedPort = 19138
		generation    = 7
	)
	t.Setenv("LOCALAPPDATA", apitest.HardenedTempDir(t))
	seedBuiltinRouteRoutingSettings(t, api.MCPFrontRoutingTargetFront, generation, admittedPort, requestedPort)
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, &api.SupervisorIntentFile{Version: 1}, events)
	assertBuiltinRouteSeederProjection(t, got, stateDir, eventsPath, api.MCPFrontRoutingTargetFront, generation, admittedPort, requestedPort)
}

func TestEnsureBuiltinRouteDaemonAtStartup_PreparingRebaseUsesAdmittedPort(t *testing.T) {
	const (
		admittedPort  = 20137
		requestedPort = 20138
		generation    = 8
	)
	t.Setenv("LOCALAPPDATA", apitest.HardenedTempDir(t))
	seedBuiltinRouteRoutingSettings(t, api.MCPFrontRoutingTargetFrontPreparing, generation, admittedPort, requestedPort)
	stateDir := apitest.HardenedTempDir(t)
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}
	defer events.Close()

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, &api.SupervisorIntentFile{Version: 1}, events)
	assertBuiltinRouteSeederProjection(t, got, stateDir, eventsPath, api.MCPFrontRoutingTargetFrontPreparing, generation, admittedPort, requestedPort)
}

func seedBuiltinRouteRoutingSettings(t *testing.T, state api.MCPFrontRoutingTarget, generation, admittedPort, requestedPort int) {
	t.Helper()
	path := api.SettingsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create settings parent: %v", err)
	}
	body := strings.Join([]string{
		"mcp_front.routing_target: " + string(state),
		"mcp_front.routing_generation: \"" + strconv.Itoa(generation) + "\"",
		"mcp_front.routing_admitted_port: \"" + strconv.Itoa(admittedPort) + "\"",
		"mcp_front.port: \"" + strconv.Itoa(requestedPort) + "\"",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write routing settings: %v", err)
	}
}

func assertBuiltinRouteSeederProjection(t *testing.T, got *api.SupervisorIntentFile, stateDir, eventsPath string, state api.MCPFrontRoutingTarget, generation, admittedPort, requestedPort int) {
	t.Helper()
	wantRow := api.BuildBuiltinRouteDaemon(canonicalMcphubPath(), admittedPort)
	assertSingleCanonicalBuiltinRouteRow(t, got, wantRow, "returned intent")

	disk, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil {
		t.Fatalf("re-read supervisor intent: %v", err)
	}
	assertSingleCanonicalBuiltinRouteRow(t, disk, wantRow, "disk intent")

	settingsAPI := api.NewAPI()
	requested, err := settingsAPI.SettingsGet(api.MCPFrontPortSettingKey)
	if err != nil || requested != strconv.Itoa(requestedPort) {
		t.Fatalf("requested port after seeding=%q err=%v, want %d", requested, err, requestedPort)
	}
	snapshot, err := settingsAPI.MCPFrontRoutingTargetSnapshot()
	wantSnapshot := api.MCPFrontRoutingTargetSnapshot{State: state, Generation: generation, Port: admittedPort}
	if err != nil || snapshot != wantSnapshot {
		t.Fatalf("routing epoch after seeding=%+v err=%v, want unchanged %+v", snapshot, err, wantSnapshot)
	}

	event := readLastBuiltinRouteEnsuredEvent(t, eventsPath)
	if event.Body["port"] != float64(admittedPort) || event.Body["routing_state"] != string(state) || event.Body["routing_generation"] != float64(generation) {
		t.Fatalf("builtin-route-ensured body=%v, want port=%d state=%s generation=%d", event.Body, admittedPort, state, generation)
	}
}

func assertSingleCanonicalBuiltinRouteRow(t *testing.T, intent *api.SupervisorIntentFile, want api.SupervisorDaemon, source string) {
	t.Helper()
	var rows []api.SupervisorDaemon
	if intent != nil {
		for _, daemon := range intent.Daemons {
			if daemon.TaskName == api.BuiltinRouteTaskName {
				rows = append(rows, daemon)
			}
		}
	}
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], want) {
		t.Fatalf("%s route rows=%+v, want exactly canonical %+v", source, rows, want)
	}
}

func readLastBuiltinRouteEnsuredEvent(t *testing.T, path string) api.SupervisorEvent {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	var found api.SupervisorEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event api.SupervisorEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("decode event %q: %v", line, err)
		}
		if event.Event == "builtin-route-ensured" {
			found = event
		}
	}
	if found.Event == "" {
		t.Fatalf("builtin-route-ensured missing from %s", path)
	}
	return found
}

// TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow is
// the P1-2 falsifying test (adversarial cross-family review): a persist
// failure must NEVER leave the in-memory `intent` return value AHEAD of what
// is durably on disk. This test seeds intent with an EXISTING route row (an
// older command/port, simulating a prior successful boot), then forces the
// persist write to fail deterministically by pointing stateDir's
// supervisor-intent.json at a path whose parent cannot be created (stateDir
// itself is a FILE, not a directory, so
// MutateSupervisorIntentIfChanged's os.MkdirAll(filepath.Dir(path)) —
// i.e. os.MkdirAll(stateDir) — fails).
//
// Mutation-proven: reverting to the pre-fix ordering (mutate `intent` BEFORE
// attempting the persist, return it unconditionally) makes this test fail —
// the returned intent's route row would carry the NEW command/port even
// though the disk write never happened, which is exactly the "reconcile
// plan runs ahead of disk" bug the next 60s IntentWatcher poll would then
// have to un-do by killing the freshly-spawned daemon as an orphan.
func TestEnsureBuiltinRouteDaemonAtStartup_PersistFailurePreservesExistingRow(t *testing.T) {
	parent := t.TempDir()
	stateDir := filepath.Join(parent, "not-a-directory")
	if err := os.WriteFile(stateDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("seed non-directory stateDir: %v", err)
	}

	eventsPath := filepath.Join(parent, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	oldRow := api.BuildBuiltinRouteDaemon("/old/mcphub", 19999)
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{oldRow}}

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, intent, events)
	_ = events.Close()

	if got == nil {
		t.Fatalf("ensureBuiltinRouteDaemonAtStartup returned nil intent")
	}
	if len(got.Daemons) != 1 {
		t.Fatalf("Daemons count = %d, want 1 (the old row preserved, no duplicate/no drop): %+v", len(got.Daemons), got.Daemons)
	}
	if got.Daemons[0].Command != oldRow.Command || got.Daemons[0].Port != oldRow.Port {
		t.Fatalf("route row changed despite a persist failure: got %+v, want unchanged %+v", got.Daemons[0], oldRow)
	}

	raw, rerr := os.ReadFile(eventsPath)
	if rerr != nil {
		t.Fatalf("read events log: %v", rerr)
	}
	if !strings.Contains(string(raw), `"event":"builtin-route-ensure-failed"`) {
		t.Fatalf("builtin-route-ensure-failed event missing from audit log:\n%s", string(raw))
	}
}

// TestEnsureBuiltinRouteDaemonAtStartup_PortResolutionFailurePreservesExistingRow
// is the P2-5 falsifying test: a corrupt gui-preferences.yaml must not make
// the supervisor silently canonicalize the reserved route-daemon row back
// onto DefaultMCPFrontPort. This test seeds an existing row on a
// DIFFERENT, non-default port (simulating an operator who already ran
// `mcphub install --reconcile-mcp-front` onto a custom port), corrupts the
// settings file, and asserts the row's port is left completely untouched.
//
// Mutation-proven: reverting resolveMCPFrontPortStrictFn's call site back to
// resolveMCPFrontPortFn (the graceful MCPFrontPortOrDefault fallback) makes
// this test fail — the row's port would be silently rewritten to
// api.DefaultMCPFrontPort (9137) despite the corrupt settings file.
func TestEnsureBuiltinRouteDaemonAtStartup_PortResolutionFailurePreservesExistingRow(t *testing.T) {
	// api.SettingsPath (unlike api.DaemonStateDir) reads LOCALAPPDATA
	// directly, not the in-memory daemonStateRootOverride — mirrors
	// api.DefaultRegistryPath. t.Setenv overrides internal/cli's TestMain
	// global default for just this test and auto-restores.
	settingsStateDir := t.TempDir()
	t.Setenv("LOCALAPPDATA", settingsStateDir)

	corruptPath := filepath.Join(settingsStateDir, "mcp-local-hub", "gui-preferences.yaml")
	if err := os.MkdirAll(filepath.Dir(corruptPath), 0o700); err != nil {
		t.Fatalf("mkdir settings dir: %v", err)
	}
	if err := os.WriteFile(corruptPath, []byte("mcp_front: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write corrupt settings file: %v", err)
	}

	stateDir := t.TempDir()
	eventsPath := filepath.Join(stateDir, "supervisor-events.log")
	events, err := api.OpenSupervisorEventLog(eventsPath)
	if err != nil {
		t.Fatalf("open event log: %v", err)
	}

	const configuredPort = 40017
	oldRow := api.BuildBuiltinRouteDaemon("/some/mcphub", configuredPort)
	intent := &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{oldRow}}

	got := ensureBuiltinRouteDaemonAtStartup(stateDir, intent, events)
	_ = events.Close()

	if got == nil || len(got.Daemons) != 1 {
		t.Fatalf("ensureBuiltinRouteDaemonAtStartup: got %+v, want the single pre-existing row untouched", got)
	}
	if got.Daemons[0].Port != configuredPort {
		t.Fatalf("route row port = %d, want %d (unchanged) — a corrupt settings file must never silently canonicalize this row back to the default port", got.Daemons[0].Port, configuredPort)
	}
	if got.Daemons[0].Command != oldRow.Command {
		t.Fatalf("route row command = %q, want %q (unchanged)", got.Daemons[0].Command, oldRow.Command)
	}

	// Disk must ALSO stay untouched — no supervisor-intent.json should have
	// been created at all (the ensure was skipped entirely on the
	// resolution failure, before any persist attempt).
	if _, statErr := os.Stat(filepath.Join(stateDir, "supervisor-intent.json")); statErr == nil {
		t.Fatalf("supervisor-intent.json was created despite a port-resolution failure; the ensure must be skipped entirely, not persist a fabricated port")
	}

	raw, rerr := os.ReadFile(eventsPath)
	if rerr != nil {
		t.Fatalf("read events log: %v", rerr)
	}
	if !strings.Contains(string(raw), `"event":"builtin-route-ensure-failed"`) {
		t.Fatalf("builtin-route-ensure-failed event missing from audit log:\n%s", string(raw))
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
