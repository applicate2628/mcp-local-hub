package api

import (
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// writeTestIntent writes a minimal supervisor-intent.json under dir and returns
// its path.
func writeTestIntent(t *testing.T, dir string, daemons []SupervisorDaemon) string {
	t.Helper()
	path := joinStateFilePath(dir, supervisorIntentFileLeaf)
	f := &SupervisorIntentFile{Version: 1, Daemons: daemons}
	if err := WriteSupervisorIntent(path, f); err != nil {
		t.Fatalf("write test intent: %v", err)
	}
	return path
}

// readTestIntentPorts re-reads the intent from disk and returns a task→port map.
func readTestIntentPorts(t *testing.T, path string) map[string]int {
	t.Helper()
	intent, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	out := map[string]int{}
	for _, d := range intent.Daemons {
		out[d.TaskName] = d.Port
	}
	return out
}

// stubPortResolver swaps resolveManifestPortAndDeadlineFn for the test and
// restores it afterward. The map is keyed "server/daemon".
func stubPortResolver(t *testing.T, table map[string][2]int) {
	t.Helper()
	prev := resolveManifestPortAndDeadlineFn
	t.Cleanup(func() { resolveManifestPortAndDeadlineFn = prev })
	resolveManifestPortAndDeadlineFn = func(server, daemon string) (int, int, bool) {
		v, ok := table[server+"/"+daemon]
		if !ok {
			return 0, 0, false
		}
		return v[0], v[1], true
	}
}

func TestBackfillIntentDaemonPorts_BackfillsLegacyPortZero(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{
		"memory/default": {9123, 0},
		"gdb/default":    {9129, 45}, // manifest that also declares a bind deadline
	})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0},
		{TaskName: `\mcp-local-hub-gdb-default`, Server: "gdb", Daemon: "default", Port: 0},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 2 {
		t.Fatalf("want 2 applied, got %d (%+v)", len(res.Applied), res.Applied)
	}
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("want 0 unresolved, got %v", res.UnresolvedPortZero)
	}

	ports := readTestIntentPorts(t, path)
	if ports[`\mcp-local-hub-memory-default`] != 9123 {
		t.Errorf("memory port not persisted: got %d", ports[`\mcp-local-hub-memory-default`])
	}
	if ports[`\mcp-local-hub-gdb-default`] != 9129 {
		t.Errorf("gdb port not persisted: got %d", ports[`\mcp-local-hub-gdb-default`])
	}

	// The gdb row must also carry the manifest bind deadline.
	intent, _ := ReadSupervisorIntent(path)
	for _, d := range intent.Daemons {
		if d.TaskName == `\mcp-local-hub-gdb-default` && d.StartupBindDeadlineSeconds != 45 {
			t.Errorf("gdb bind deadline not backfilled: got %d, want 45", d.StartupBindDeadlineSeconds)
		}
	}
}

func TestBackfillIntentDaemonPorts_LeavesNonZeroPortUntouched(t *testing.T) {
	dir := t.TempDir()
	// Resolver would return a DIFFERENT port; the backfill must NOT overwrite a
	// descriptor that already has a non-zero port.
	stubPortResolver(t, map[string][2]int{"time/default": {9999, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-time-default`, Server: "time", Daemon: "default", Port: 9128},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("want 0 applied for an already-ported row, got %+v", res.Applied)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-time-default`]; got != 9128 {
		t.Errorf("non-zero port was overwritten: got %d, want 9128", got)
	}
}

func TestBackfillIntentDaemonPorts_SkipsRuntimeSpecDaemon(t *testing.T) {
	dir := t.TempDir()
	// A runtime_spec (serena) descriptor with Port=0 must be skipped even though
	// a resolver entry exists — its port comes from the spec, not the manifest.
	stubPortResolver(t, map[string][2]int{"serena/6935d24c": {9150, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-serena-6935d24c`, Server: "serena", Daemon: "6935d24c", Port: 0, RuntimeSpec: &DaemonRuntimeSpec{}},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("runtime_spec daemon must be skipped, got applied %+v", res.Applied)
	}
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("runtime_spec daemon must not be reported unresolved, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-serena-6935d24c`]; got != 0 {
		t.Errorf("runtime_spec descriptor port was mutated: got %d, want 0", got)
	}
}

// TestBackfillIntentDaemonPorts_SkipsLegacyUnifiedSerena is the regression
// guard for the F5 serena-clobber defect: a LEGACY-unified serena descriptor
// (kind=global, RuntimeSpec==nil, Port=0, args `daemon --server serena --daemon
// unified`) slips past the RuntimeSpec!=nil guard, and the serena manifest DOES
// resolve a real port (9121). Before the by-server skip, F5 stamped Port=9121 —
// turning on the liveness port-check with the 60s DEFAULT deadline (the descriptor
// args are not the `daemon serena-proxy` shape supervisorStartupBindDeadline keys
// on for 120s), which restart-cycles serena on its slow cold start. F5 must leave
// EVERY serena descriptor untouched: Port stays 0 (port-check disabled, pre-F5
// behavior), and it is NOT reported unresolved (skipped before the resolve).
func TestBackfillIntentDaemonPorts_SkipsLegacyUnifiedSerena(t *testing.T) {
	dir := t.TempDir()
	// The serena manifest WOULD resolve a real port; the skip must fire before
	// the resolver is consulted, so even a present entry must not be stamped.
	stubPortResolver(t, map[string][2]int{"serena/unified": {9121, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{
			TaskName: `\mcp-local-hub-serena-unified`,
			Server:   "serena",
			Daemon:   "unified",
			Port:     0,
			// Legacy-unified: NO runtime_spec, global daemon arg shape.
			Args: []string{"daemon", "--server", "serena", "--daemon", "unified"},
		},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("legacy-unified serena must be skipped, got applied %+v", res.Applied)
	}
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("serena skip must fire BEFORE the resolve, so it must not be reported unresolved, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-serena-unified`]; got != 0 {
		t.Errorf("legacy-unified serena port was mutated: got %d, want 0 (port-check must stay disabled, as pre-F5)", got)
	}
}

// TestBackfillIntentDaemonPorts_SkipsLegacyUnifiedSerenaBlankFields is the same
// guard for the blank-identity vintage: even when Server/Daemon are empty and the
// identity is recovered from args, the by-server skip must still catch serena.
func TestBackfillIntentDaemonPorts_SkipsLegacyUnifiedSerenaBlankFields(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"serena/unified": {9121, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{
			TaskName: `\mcp-local-hub-serena-unified`,
			// Server/Daemon blank — recovered from args by descriptorServerDaemon.
			Port: 0,
			Args: []string{"daemon", "--server", "serena", "--daemon", "unified"},
		},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("blank-field legacy serena must be skipped, got applied %+v", res.Applied)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-serena-unified`]; got != 0 {
		t.Errorf("blank-field legacy serena port was mutated: got %d, want 0", got)
	}
	// And the blank identity fields must NOT have been healed either — F5 does not
	// touch serena at all.
	intent, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	for _, d := range intent.Daemons {
		if d.TaskName == `\mcp-local-hub-serena-unified` && (d.Server != "" || d.Daemon != "") {
			t.Errorf("serena identity fields were healed by F5: server=%q daemon=%q, want both blank (F5 must not touch serena)", d.Server, d.Daemon)
		}
	}
}

// TestBackfillIntentDaemonPorts_SkipsLegacyWorkspaceHashSerena pins the
// daemon-name-agnostic behavior the by-SERVER (not by-daemon) guard gives: the
// OLDER legacy serena shape is a workspace-hash-named nil-RuntimeSpec row (e.g.
// `\mcp-local-hub-serena-6935d24c`, daemon "6935d24c") predating the runtime_spec
// redesign (serena_intent_repair.go). It too must be skipped — the guard keys on
// server=="serena", so the workspace-hash daemon name is irrelevant.
func TestBackfillIntentDaemonPorts_SkipsLegacyWorkspaceHashSerena(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"serena/6935d24c": {9121, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{
			TaskName: `\mcp-local-hub-serena-6935d24c`,
			Server:   "serena",
			Daemon:   "6935d24c",
			Port:     0,
			// Legacy workspace-hash serena: NO runtime_spec.
			Args: []string{"daemon", "--server", "serena", "--daemon", "6935d24c"},
		},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("legacy workspace-hash serena must be skipped, got applied %+v", res.Applied)
	}
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("serena skip must fire before the resolve, got unresolved %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-serena-6935d24c`]; got != 0 {
		t.Errorf("legacy workspace-hash serena port was mutated: got %d, want 0", got)
	}
}

func TestBackfillIntentDaemonPorts_UnresolvedPortZeroLeftUntouched(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{}) // resolver returns !ok for everything
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-mystery-default`, Server: "mystery", Daemon: "default", Port: 0},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("want 0 applied, got %+v", res.Applied)
	}
	if len(res.UnresolvedPortZero) != 1 || res.UnresolvedPortZero[0] != `\mcp-local-hub-mystery-default` {
		t.Fatalf("want the unresolved row reported, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-mystery-default`]; got != 0 {
		t.Errorf("unresolved row port was mutated: got %d, want 0", got)
	}
}

func TestBackfillIntentDaemonPorts_ContendedFlockSkips(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"memory/default": {9123, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0},
	})

	// Hold the intent flock so the backfill sees contention and skips.
	lk := flock.New(path + supervisorIntentLockSuffix)
	locked, err := lk.TryLock()
	if err != nil || !locked {
		t.Fatalf("pre-acquire intent flock: locked=%v err=%v", locked, err)
	}
	defer func() { _ = lk.Unlock() }()

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill under contention must be non-fatal: %v", err)
	}
	if !res.Contended {
		t.Fatalf("want Contended=true when the flock is held")
	}
	if len(res.Applied) != 0 {
		t.Fatalf("want 0 applied under contention, got %+v", res.Applied)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-memory-default`]; got != 0 {
		t.Errorf("intent was mutated under contention: got %d, want 0", got)
	}
}

func TestBackfillIntentDaemonPorts_MissingIntentIsNoop(t *testing.T) {
	dir := t.TempDir()
	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("missing intent must be a no-op, got err: %v", err)
	}
	if len(res.Applied) != 0 || len(res.UnresolvedPortZero) != 0 || res.Contended {
		t.Fatalf("missing intent must yield an empty result, got %+v", res)
	}
}

func TestBackfillIntentDaemonPorts_Idempotent(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"memory/default": {9123, 0}})
	writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0},
	})

	if _, err := BackfillIntentDaemonPorts(dir); err != nil {
		t.Fatalf("first backfill: %v", err)
	}
	res2, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if len(res2.Applied) != 0 {
		t.Fatalf("second run must find nothing to backfill, got %+v", res2.Applied)
	}
}

func TestBackfillIntentDaemonPorts_SkipsEmptyServerDaemonTimerRow(t *testing.T) {
	dir := t.TempDir()
	// The resolver must NEVER be consulted for a timer row — skip happens first.
	stubPortResolver(t, map[string][2]int{})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-workspace-weekly-refresh`, Server: "", Daemon: "", Port: 0, Args: []string{"workspace-weekly-refresh"}},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("timer row must not be applied, got %+v", res.Applied)
	}
	// The whole point of SC1: a portless timer must be skipped SILENTLY, not
	// reported as an unresolved port (which would warn on every startup forever).
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("empty Server/Daemon timer row must be skipped silently, NOT reported unresolved, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-workspace-weekly-refresh`]; got != 0 {
		t.Errorf("timer row port mutated: got %d, want 0", got)
	}
}

// TestBackfillIntentDaemonPorts_BlankFieldsRecoveredFromArgs is the bot PR #504
// regression: a REAL daemon row whose Server/Daemon struct fields are blank (an
// older intent shape) but whose args carry the canonical --server/--daemon must
// be backfilled via args-derivation, NOT skipped as if it were a portless timer.
func TestBackfillIntentDaemonPorts_BlankFieldsRecoveredFromArgs(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"memory/default": {9123, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-memory-default`, Server: "", Daemon: "", Port: 0,
			Args: []string{"daemon", "--server", "memory", "--daemon", "default"}},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Port != 9123 {
		t.Fatalf("blank-field daemon row must be backfilled via args-derivation, got %+v", res.Applied)
	}
	if res.Applied[0].Server != "memory" || res.Applied[0].Daemon != "default" {
		t.Errorf("Applied must carry the derived server/daemon, got server=%q daemon=%q", res.Applied[0].Server, res.Applied[0].Daemon)
	}
	if len(res.UnresolvedPortZero) != 0 {
		t.Fatalf("a real daemon row must not be reported unresolved, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-memory-default`]; got != 9123 {
		t.Errorf("blank-field daemon port not persisted: got %d, want 9123", got)
	}
	// The identity fields must be HEALED on the persisted descriptor, not left
	// blank — otherwise the squatter classifier's argv gate (supervise_squatter.go)
	// and `mcphub daemon recover` misclassify a genuine own-child as foreign and
	// never reap it, defeating the protection this backfill restores (bot PR #504
	// + opus delta review).
	intent, rerr := ReadSupervisorIntent(path)
	if rerr != nil {
		t.Fatalf("re-read: %v", rerr)
	}
	var found bool
	for _, d := range intent.Daemons {
		if d.TaskName == `\mcp-local-hub-memory-default` {
			found = true
			if d.Server != "memory" || d.Daemon != "default" {
				t.Errorf("identity not healed on persisted row: server=%q daemon=%q, want memory/default", d.Server, d.Daemon)
			}
		}
	}
	if !found {
		t.Fatal("memory row disappeared from persisted intent")
	}
}

func TestBackfillIntentDaemonPorts_ManifestPortZeroIsUnresolved(t *testing.T) {
	dir := t.TempDir()
	// Resolver MATCHES the daemon (ok=true) but the manifest declares port 0 —
	// the `port <= 0 && ok` branch. Must land in UnresolvedPortZero, not backfill 0.
	stubPortResolver(t, map[string][2]int{"x/default": {0, 0}})
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-x-default`, Server: "x", Daemon: "default", Port: 0},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 0 {
		t.Fatalf("a port-0 manifest must not backfill, got %+v", res.Applied)
	}
	if len(res.UnresolvedPortZero) != 1 || res.UnresolvedPortZero[0] != `\mcp-local-hub-x-default` {
		t.Fatalf("a matched-but-port-0 row must be reported unresolved, got %v", res.UnresolvedPortZero)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-x-default`]; got != 0 {
		t.Errorf("row mutated: got %d, want 0", got)
	}
}

// TestBackfillIntentDaemonPorts_PreservesNonDaemonFields is the clobber-safety
// regression: a mixed intent (StrictMode + UpdatedAt + MaintenanceTimers + Stops
// + a runtime_spec serena row) must survive the whole-file backfill write
// byte-intact while one legacy row is raised. Pins the preservation that is
// otherwise only guaranteed by struct round-trip.
func TestBackfillIntentDaemonPorts_PreservesNonDaemonFields(t *testing.T) {
	dir := t.TempDir()
	stubPortResolver(t, map[string][2]int{"memory/default": {9123, 0}})
	path := joinStateFilePath(dir, supervisorIntentFileLeaf)
	enabled := true
	orig := &SupervisorIntentFile{
		Version:    1,
		UpdatedAt:  "2026-07-04T12:00:00Z",
		StrictMode: true,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0}, // backfill target
			{TaskName: `\mcp-local-hub-serena-abc`, Server: "serena", Daemon: "abc", Port: 9150,
				RuntimeSpec: &DaemonRuntimeSpec{SpecVersion: 1, ChildCommand: "uvx", ExternalPort: 9150, UpstreamPort: 9350}},
		},
		MaintenanceTimers: []MaintenanceTimer{
			{Name: `\mcp-local-hub-workspace-weekly-refresh`, Kind: "workspace-weekly-refresh", Command: "mcphub", Args: []string{"workspace-weekly-refresh"}, Enabled: &enabled},
		},
		Stops: map[string]DaemonIntent{
			`\mcp-local-hub-gdb-default`: {Desired: "stopped", Reason: "user-stop", UpdatedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)},
		},
	}
	if err := WriteSupervisorIntent(path, orig); err != nil {
		t.Fatalf("write mixed intent: %v", err)
	}

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Port != 9123 {
		t.Fatalf("want the memory row backfilled to 9123, got %+v", res.Applied)
	}

	got, err := ReadSupervisorIntent(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if !got.StrictMode {
		t.Error("StrictMode not preserved through the backfill write")
	}
	if got.UpdatedAt != "2026-07-04T12:00:00Z" {
		t.Errorf("UpdatedAt mutated: %q", got.UpdatedAt)
	}
	if len(got.MaintenanceTimers) != 1 || got.MaintenanceTimers[0].Kind != "workspace-weekly-refresh" {
		t.Errorf("MaintenanceTimers not preserved: %+v", got.MaintenanceTimers)
	}
	if s, ok := got.Stops[`\mcp-local-hub-gdb-default`]; !ok || s.Desired != "stopped" || s.Reason != "user-stop" {
		t.Errorf("Stops entry not preserved: %+v", got.Stops)
	}
	var serenaSeen, memorySeen bool
	for _, d := range got.Daemons {
		switch d.TaskName {
		case `\mcp-local-hub-serena-abc`:
			serenaSeen = true
			if d.Port != 9150 {
				t.Errorf("serena runtime_spec row port mutated: got %d, want 9150", d.Port)
			}
			if d.RuntimeSpec == nil {
				t.Error("serena RuntimeSpec dropped by the backfill write")
			}
		case `\mcp-local-hub-memory-default`:
			memorySeen = true
			if d.Port != 9123 {
				t.Errorf("memory row not backfilled: got %d, want 9123", d.Port)
			}
		}
	}
	if !serenaSeen || !memorySeen {
		t.Errorf("a daemon row disappeared: serenaSeen=%v memorySeen=%v", serenaSeen, memorySeen)
	}
}

// TestBackfillResolverMatchesInstallFanout is the F-2 drift guard: F5's resolver
// and the install fan-out (supervisorDaemonsFromPlan) must agree on the
// port-protection fields (Port + StartupBindDeadlineSeconds) so a backfilled
// legacy row is byte-identical to what a fresh install writes. If install starts
// deriving either field differently, this trips instead of silently drifting.
func TestBackfillResolverMatchesInstallFanout(t *testing.T) {
	m, err := loadManifestForServer("", "memory")
	if err != nil || m == nil {
		t.Fatalf("load memory manifest: %v", err)
	}
	built := supervisorDaemonsFromPlan(m, "default")
	if len(built) != 1 {
		t.Fatalf("want 1 built descriptor for memory/default, got %d", len(built))
	}
	port, deadline, ok := resolveManifestPortAndDeadline("memory", "default")
	if !ok {
		t.Fatal("resolver failed for memory/default")
	}
	if port != built[0].Port {
		t.Errorf("port drift: F5 resolver=%d, install fan-out=%d", port, built[0].Port)
	}
	if deadline != built[0].StartupBindDeadlineSeconds {
		t.Errorf("bind-deadline drift: F5 resolver=%d, install fan-out=%d", deadline, built[0].StartupBindDeadlineSeconds)
	}
}

// TestBackfillIntentDaemonPorts_RealEmbeddedManifest exercises the DEFAULT
// resolver against the real embedded manifest store — proving the end-to-end
// path (loadManifestForServer → daemon-name match → port) resolves memory@9123
// exactly as the running fleet binds it. No resolver stub.
func TestBackfillIntentDaemonPorts_RealEmbeddedManifest(t *testing.T) {
	dir := t.TempDir()
	path := writeTestIntent(t, dir, []SupervisorDaemon{
		{TaskName: `\mcp-local-hub-memory-default`, Server: "memory", Daemon: "default", Port: 0},
	})

	res, err := BackfillIntentDaemonPorts(dir)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if len(res.Applied) != 1 || res.Applied[0].Port != 9123 {
		t.Fatalf("real embedded memory manifest must resolve port 9123, got %+v", res.Applied)
	}
	if got := readTestIntentPorts(t, path)[`\mcp-local-hub-memory-default`]; got != 9123 {
		t.Errorf("embedded backfill not persisted: got %d, want 9123", got)
	}
}
