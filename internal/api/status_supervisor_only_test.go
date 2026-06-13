package api

// Phase-F supervisor-only daemon visibility in the StatusWithOpts health /
// force-materialize path (bot PR #288 r38-1 P2).
//
// In v0.6 every GLOBAL install uses SkipSchedulerTasks=true
// (install_parsed_manifest.go), so a newly installed/migrated global daemon
// (e.g. `fetch`) exists ONLY in supervisor-intent.json — it is NOT a scheduler
// task. StatusWithOpts used to seed its row set ONLY from statusSchedulerTasks
// (plus the registry-only LSP merge, which covers workspace-scoped lazy proxies
// only), so the Phase-F supervisor-only daemon DISAPPEARED from
// `mcphub status --health` / `--force-materialize` / ProbeHealth even though
// bare `mcphub status` (supervisor IPC seam) lists it.
//
// These tests are hermetic: SetDaemonStateRootForTest redirects the
// supervisor-intent read to a fresh temp dir, defaultRegistryPathFn routes the
// registry to a temp path, the netstat/process-lookup seams are nil'd, and the
// scheduler factory is faked — nothing touches the live host
// %LOCALAPPDATA%\mcp-local-hub\, the real scheduler, the real registry, or any
// real port.

import (
	"path/filepath"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

// statusSupervisorOnlyHermeticEnv installs the shared hermetic seams every
// test below needs: a temp state dir (for the supervisor-intent read), a temp
// registry path, nil'd process-lookup + port-live seams, and a scheduler
// factory returning the supplied fake. It returns the resolved intent path.
func statusSupervisorOnlyHermeticEnv(t *testing.T, sch *fakeScheduler) string {
	t.Helper()

	stateDir := t.TempDir()
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	origRegPath := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return regPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = origRegPath })

	origBatch := lookupProcessBatch
	origLookup := lookupProcess
	lookupProcessBatch = nil
	lookupProcess = nil
	t.Cleanup(func() {
		lookupProcessBatch = origBatch
		lookupProcess = origLookup
	})

	origPortLive := registryOnlyStatusPortLiveFn
	registryOnlyStatusPortLiveFn = func(int) bool { return false }
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	origScheduler := statusSchedulerFactory
	statusSchedulerFactory = func() (scheduler.Scheduler, error) { return sch, nil }
	t.Cleanup(func() { statusSchedulerFactory = origScheduler })

	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatalf("DefaultSupervisorIntentPath: %v", err)
	}
	return intentPath
}

func statusRowByTask(rows []DaemonStatus) map[string]DaemonStatus {
	byTask := make(map[string]DaemonStatus, len(rows))
	for _, r := range rows {
		byTask[r.TaskName] = r
	}
	return byTask
}

// TestStatusWithOpts_SeedsPhaseFSupervisorOnlyDaemon is the core positive
// assertion: a global daemon descriptor present ONLY in supervisor-intent.json
// (no scheduler task) must surface as a StatusWithOpts row so the health /
// force-materialize probe can reach it.
//
// Pre-fix the fetch row is ABSENT (StatusWithOpts seeded only from
// statusSchedulerTasks + the registry LSP merge), so this assertion FAILS
// against the pre-fix code.
func TestStatusWithOpts_SeedsPhaseFSupervisorOnlyDaemon(t *testing.T) {
	// Scheduler has NO tasks — mirrors a v0.6 global install (SkipSchedulerTasks).
	sch := &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
	intentPath := statusSupervisorOnlyHermeticEnv(t, sch)

	// `fetch` is the prompt's named example AND is in the embedded manifest
	// set, so enrichStatusWithRegistry legitimately resolves its port from the
	// embed (9133). `noembedsrv` is NOT in the embed, so its descriptor port
	// survives enrichment (the embed-miss carry-through the merge promises).
	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-fetch-default`, Server: "fetch", Daemon: "default", Command: "mcphub.exe", Port: 9133},
			{TaskName: `\mcp-local-hub-noembedsrv-default`, Server: "noembedsrv", Daemon: "default", Command: "mcphub.exe", Port: 9233},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: false})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	byTask := statusRowByTask(rows)

	row, ok := byTask[`\mcp-local-hub-fetch-default`]
	if !ok {
		t.Fatalf("Phase-F supervisor-only daemon `fetch` is ABSENT from StatusWithOpts rows; the health/force-materialize path can never probe it. rows=%+v", rows)
	}
	if row.Server != "fetch" {
		t.Errorf("fetch row Server = %q, want %q", row.Server, "fetch")
	}
	if row.Port == 0 {
		t.Errorf("fetch row Port = 0, want a non-zero probeable port (descriptor or embed-resolved)")
	}

	// Embed-miss row: the descriptor's authoritative Port must survive the
	// enrichStatusWithRegistry manifest lookup (no embed entry to overwrite it).
	noembed, ok := byTask[`\mcp-local-hub-noembedsrv-default`]
	if !ok {
		t.Fatalf("Phase-F supervisor-only daemon `noembedsrv` is ABSENT from StatusWithOpts rows; rows=%+v", rows)
	}
	if noembed.Port != 9233 {
		t.Errorf("noembedsrv row Port = %d, want 9233 (authoritative descriptor port, embed-miss carry-through)", noembed.Port)
	}
}

// TestStatusWithOpts_ExcludesMaintenanceSupervisorIntentRows asserts the
// negative control: a maintenance descriptor (e.g. weekly-refresh) in
// supervisor-intent.json is NOT surfaced as a probeable daemon row.
func TestStatusWithOpts_ExcludesMaintenanceSupervisorIntentRows(t *testing.T) {
	sch := &fakeScheduler{tasks: map[string]bool{}, xml: map[string][]byte{}}
	intentPath := statusSupervisorOnlyHermeticEnv(t, sch)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			{TaskName: `\mcp-local-hub-fetch-default`, Server: "fetch", Daemon: "default", Command: "mcphub.exe", Port: 9233},
			// Maintenance descriptors must be excluded — not probeable daemons.
			{TaskName: `\mcp-local-hub-weekly-refresh`, Command: "mcphub.exe"},
			{TaskName: `\mcp-local-hub-liveness`, Command: "mcphub.exe"},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: false})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	byTask := statusRowByTask(rows)
	if _, ok := byTask[`\mcp-local-hub-fetch-default`]; !ok {
		t.Fatalf("fetch daemon row missing; rows=%+v", rows)
	}
	if _, ok := byTask[`\mcp-local-hub-weekly-refresh`]; ok {
		t.Errorf("weekly-refresh maintenance descriptor must NOT be a probeable status row; rows=%+v", rows)
	}
	if _, ok := byTask[`\mcp-local-hub-liveness`]; ok {
		t.Errorf("liveness maintenance descriptor must NOT be a probeable status row; rows=%+v", rows)
	}
}

// TestStatusWithOpts_DedupsSchedulerAndSupervisorIntentRow asserts a daemon
// present in BOTH the scheduler task list AND supervisor-intent.json appears
// exactly once (the scheduler seed wins; the intent merge dedups against it).
func TestStatusWithOpts_DedupsSchedulerAndSupervisorIntentRow(t *testing.T) {
	sch := &fakeScheduler{
		tasks: map[string]bool{},
		xml:   map[string][]byte{},
		listSeed: []scheduler.TaskStatus{
			{Name: `\mcp-local-hub-fetch-default`, State: "Ready"},
		},
	}
	intentPath := statusSupervisorOnlyHermeticEnv(t, sch)

	seed := &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{
			// Same canonical task name as the scheduler row above (the
			// intent stores the leading-backslash form). Must dedup.
			{TaskName: `\mcp-local-hub-fetch-default`, Server: "fetch", Daemon: "default", Command: "mcphub.exe", Port: 9233},
		},
	}
	if err := WriteSupervisorIntent(intentPath, seed); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: false})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	count := 0
	for _, r := range rows {
		if r.TaskName == `\mcp-local-hub-fetch-default` {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("fetch daemon appears %d times, want exactly 1 (scheduler+intent must dedup); rows=%+v", count, rows)
	}
}

// TestStatusWithOpts_IntentAbsentLeavesSchedulerRowsUnchanged asserts the
// best-effort fail-open posture: with NO supervisor-intent.json on disk, the
// merge is a no-op and the scheduler-derived row set is unchanged.
func TestStatusWithOpts_IntentAbsentLeavesSchedulerRowsUnchanged(t *testing.T) {
	sch := &fakeScheduler{
		tasks: map[string]bool{},
		xml:   map[string][]byte{},
		listSeed: []scheduler.TaskStatus{
			{Name: `\mcp-local-hub-serena-claude`, State: "Ready"},
		},
	}
	// NO WriteSupervisorIntent — the intent file is absent on disk.
	statusSupervisorOnlyHermeticEnv(t, sch)

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: false})
	if err != nil {
		t.Fatalf("StatusWithOpts with absent intent: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1 scheduler-only row (intent-absent must be a no-op); rows=%+v", len(rows), rows)
	}
	if rows[0].TaskName != `\mcp-local-hub-serena-claude` {
		t.Fatalf("row = %+v, want the lone scheduler-derived serena row", rows[0])
	}
}
