package api

import (
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/scheduler"
)

// TestPortForTask_WorkspaceScopedBeatsManifest guards the port-lookup
// priority for Stop/Restart: workspace-scoped tasks (lazy-proxy names)
// must resolve their port from the registry, NOT the manifest map.
// The manifest has no entry for lsp-<key>-<lang> tasks, so a naive
// `ports[srv][dmn]` lookup would return 0 and kill-by-port would
// no-op, leaving the orphan daemon running.
func TestPortForTask_WorkspaceScopedBeatsManifest(t *testing.T) {
	wsByTask := map[string]WorkspaceEntry{
		"mcp-local-hub-lsp-deadbeef-python": {Port: 9217, Language: "python", Backend: "mcp-language-server"},
	}
	ports := map[string]map[string]int{} // empty manifest map — workspace-scoped must still resolve
	got := portForTask("mcp-local-hub-lsp-deadbeef-python", ports, wsByTask)
	if got != 9217 {
		t.Errorf("workspace-scoped port = %d, want 9217", got)
	}
}

// TestPortForTask_GlobalFallback confirms global daemons still resolve
// from the manifest map when not in the workspace-scoped registry.
func TestPortForTask_GlobalFallback(t *testing.T) {
	ports := map[string]map[string]int{
		"serena": {"claude": 9121, "default": 9121},
	}
	got := portForTask("mcp-local-hub-serena-claude", ports, nil)
	if got != 9121 {
		t.Errorf("global port = %d, want 9121", got)
	}
}

// TestProbeDaemonHealth_TagsLazyProxyBySourceEvenWithoutLanguage guards
// the Source-tagging contract: lazy-proxy rows must be marked
// "proxy-synthetic" based on task-name structure, not registry-populated
// Language. Language can be empty when registry enrichment fails
// (missing/corrupt file), but TaskName still identifies the proxy.
// Without this fix the CLI falls through to global formatting "OK (N)"
// as if a real backend validated, misleading operators in incidents.
func TestProbeDaemonHealth_TagsLazyProxyBySourceEvenWithoutLanguage(t *testing.T) {
	origProbe := singleHealthProbeFn
	defer func() { singleHealthProbeFn = origProbe }()
	singleHealthProbeFn = func(port int) *HealthProbe {
		return &HealthProbe{OK: true, ToolCount: 6}
	}
	// Lazy-proxy task with EMPTY Language (simulates registry-miss).
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-deadbeef-python", State: "Running", Port: 9217},
	}
	probeDaemonHealth(rows)
	if rows[0].Health == nil {
		t.Fatal("Health not populated")
	}
	if rows[0].Health.Source != "proxy-synthetic" {
		t.Errorf("Source = %q, want proxy-synthetic (should be tagged by task-name structure, not Language)", rows[0].Health.Source)
	}
}

// TestEnrichStatusWithRegistry_OrphanWorkspaceTaskPreservesRawState guards
// the "missing registry entry" edge case for a workspace-scoped scheduler
// task. Without this guard, deriveState saw Port=0 → alive=false → the
// raw "Running" would flip to "Starting", misreporting a healthy orphan
// proxy as still starting. Keep the raw scheduler state when no matching
// registry row exists so the operator sees the truth and can investigate.
func TestEnrichStatusWithRegistry_OrphanWorkspaceTaskPreservesRawState(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	// Registry is EMPTY — the task exists in scheduler but has no matching
	// registry entry (corruption / stale scheduler task).
	rows := []DaemonStatus{
		{
			TaskName: "mcp-local-hub-lsp-deadbeef-python",
			State:    "Running",
			NextRun:  "",
		},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].State != "Running" {
		t.Errorf("orphan workspace-scoped task: State = %q, want %q (raw scheduler state must be preserved when registry has no entry)",
			rows[0].State, "Running")
	}
	if rows[0].Port != 0 {
		t.Errorf("expected Port=0 (no registry entry to resolve from); got %d", rows[0].Port)
	}
}

// TestEnrichStatusWithRegistry_WorkspaceScoped seeds a registry entry for a
// lazy-proxy task name and asserts enrichStatusWithRegistry populates every
// workspace-scoped field (Workspace, Language, Backend, Lifecycle,
// LastMaterializedAt, LastToolsCallAt, LastError, Port).
func TestEnrichStatusWithRegistry_WorkspaceScoped(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	now := time.Now().UTC().Truncate(time.Second)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:       "abcd1234",
		WorkspacePath:      "/home/u/project",
		Language:           "python",
		Backend:            "mcp-language-server",
		Port:               9217,
		TaskName:           "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:          LifecycleActive,
		LastMaterializedAt: now.Add(-30 * time.Minute),
		LastToolsCallAt:    now.Add(-5 * time.Minute),
		LastError:          "",
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-lsp-abcd1234-python`},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	r := rows[0]
	if r.Workspace != "/home/u/project" {
		t.Errorf("Workspace = %q, want /home/u/project", r.Workspace)
	}
	if r.Language != "python" {
		t.Errorf("Language = %q, want python", r.Language)
	}
	if r.Backend != "mcp-language-server" {
		t.Errorf("Backend = %q, want mcp-language-server", r.Backend)
	}
	if r.Lifecycle != LifecycleActive {
		t.Errorf("Lifecycle = %q, want %q", r.Lifecycle, LifecycleActive)
	}
	if !r.LastMaterializedAt.Equal(now.Add(-30 * time.Minute)) {
		t.Errorf("LastMaterializedAt = %v, want %v", r.LastMaterializedAt, now.Add(-30*time.Minute))
	}
	if !r.LastToolsCallAt.Equal(now.Add(-5 * time.Minute)) {
		t.Errorf("LastToolsCallAt = %v, want %v", r.LastToolsCallAt, now.Add(-5*time.Minute))
	}
	if r.Port != 9217 {
		t.Errorf("Port = %d, want 9217", r.Port)
	}
	if !r.IsWorkspaceScoped {
		t.Errorf("IsWorkspaceScoped = false, want true for lazy-proxy task name")
	}
}

func TestStatusWithOpts_MergesRegistryOnlyWorkspaceRows(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")

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
	registryOnlyStatusPortLiveFn = func(port int) bool { return false }
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "/home/u/project",
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9217,
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     LifecycleActive,
	})
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "serena1234",
		WorkspacePath: "/home/u/serena",
		Language:      SerenaLanguageSentinel,
		Backend:       "serena",
		Port:          9401,
		TaskName:      "mcp-local-hub-serena-default",
		Lifecycle:     LifecycleActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1 registry-only workspace row: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.TaskName != "mcp-local-hub-lsp-abcd1234-python" {
		t.Fatalf("TaskName = %q, want registry task", row.TaskName)
	}
	if !row.IsWorkspaceScoped {
		t.Fatalf("IsWorkspaceScoped = false, want true")
	}
	if row.Port != 9217 {
		t.Fatalf("Port = %d, want 9217", row.Port)
	}
	if row.Language != "python" {
		t.Fatalf("Language = %q, want python", row.Language)
	}
	if row.Lifecycle != LifecycleActive {
		t.Fatalf("Lifecycle = %q, want %q", row.Lifecycle, LifecycleActive)
	}
	if row.State != "Stopped" {
		t.Fatalf("State = %q, want Stopped for registry-only LSP row with no live port", row.State)
	}
	for _, row := range rows {
		if row.TaskName == "mcp-local-hub-serena-default" || row.Language == SerenaLanguageSentinel {
			t.Fatalf("StatusWithOpts merged non-LSP registry row: %+v", row)
		}
	}
}

func TestStatusWithOpts_RegistryOnlyWorkspaceRowsUsePortLivenessWithoutHealth(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")

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

	const livePort = 39217
	const deadPort = 39218
	origPortLive := registryOnlyStatusPortLiveFn
	registryOnlyStatusPortLiveFn = func(port int) bool { return port == livePort }
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	reg := NewRegistry(regPath)
	for _, e := range []WorkspaceEntry{
		{
			WorkspaceKey:  "abcd1234",
			WorkspacePath: "/home/u/live",
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          livePort,
			TaskName:      "mcp-local-hub-lsp-abcd1234-python",
			Lifecycle:     LifecycleActive,
		},
		{
			WorkspaceKey:  "deadbeef",
			WorkspacePath: "/home/u/dead",
			Language:      "go",
			Backend:       "gopls-mcp",
			Port:          deadPort,
			TaskName:      "mcp-local-hub-lsp-deadbeef-go",
			Lifecycle:     LifecycleActive,
		},
	} {
		reg.Put(e)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	byTask := map[string]DaemonStatus{}
	for _, row := range rows {
		byTask[row.TaskName] = row
	}
	if got := byTask["mcp-local-hub-lsp-abcd1234-python"].State; got != "Running" {
		t.Fatalf("live registry-only row State = %q, want Running", got)
	}
	if got := byTask["mcp-local-hub-lsp-deadbeef-go"].State; got != "Stopped" {
		t.Fatalf("dead registry-only row State = %q, want Stopped", got)
	}
	if byTask["mcp-local-hub-lsp-abcd1234-python"].Health != nil {
		t.Fatalf("non-health status unexpectedly probed health: %+v", byTask["mcp-local-hub-lsp-abcd1234-python"].Health)
	}
}

func TestStatusWithOpts_HealthProbesLiveRegistryOnlyWorkspaceRows(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")

	live, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen live proxy port: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })
	livePort := live.Addr().(*net.TCPAddr).Port

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve dead proxy port: %v", err)
	}
	deadPort := dead.Addr().(*net.TCPAddr).Port
	_ = dead.Close()

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
	registryOnlyStatusPortLiveFn = portInUse
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	var probed []int
	origProbe := singleHealthProbeFn
	singleHealthProbeFn = func(port int) *HealthProbe {
		probed = append(probed, port)
		return &HealthProbe{OK: true, ToolCount: 6}
	}
	t.Cleanup(func() { singleHealthProbeFn = origProbe })

	reg := NewRegistry(regPath)
	for _, e := range []WorkspaceEntry{
		{
			WorkspaceKey:  "abcd1234",
			WorkspacePath: "/home/u/live",
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          livePort,
			TaskName:      "mcp-local-hub-lsp-abcd1234-python",
			Lifecycle:     LifecycleActive,
		},
		{
			WorkspaceKey:  "deadbeef",
			WorkspacePath: "/home/u/dead",
			Language:      "go",
			Backend:       "gopls-mcp",
			Port:          deadPort,
			TaskName:      "mcp-local-hub-lsp-deadbeef-go",
			Lifecycle:     LifecycleActive,
		},
	} {
		reg.Put(e)
	}
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{ProbeHealth: true})
	if err != nil {
		t.Fatalf("StatusWithOpts: %v", err)
	}
	if len(probed) != 1 || probed[0] != livePort {
		t.Fatalf("health probed ports = %v, want only live port %d", probed, livePort)
	}
	byTask := map[string]DaemonStatus{}
	for _, row := range rows {
		byTask[row.TaskName] = row
	}
	liveRow := byTask["mcp-local-hub-lsp-abcd1234-python"]
	if liveRow.Health == nil || !liveRow.Health.OK {
		t.Fatalf("live registry-only row health = %+v, want OK probe", liveRow.Health)
	}
	deadRow := byTask["mcp-local-hub-lsp-deadbeef-go"]
	if deadRow.Health != nil {
		t.Fatalf("dead registry-only row health = %+v, want nil", deadRow.Health)
	}
}

func TestStatusWithOpts_MergesRegistryRowsWhenSchedulerUnavailable(t *testing.T) {
	origScheduler := statusSchedulerFactory
	statusSchedulerFactory = func() (scheduler.Scheduler, error) {
		return nil, errors.New("linux scheduler not yet implemented (Phase 0-1 is Windows-first)")
	}
	t.Cleanup(func() { statusSchedulerFactory = origScheduler })

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
	registryOnlyStatusPortLiveFn = func(port int) bool { return false }
	t.Cleanup(func() { registryOnlyStatusPortLiveFn = origPortLive })

	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: "/home/u/project",
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9217,
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     LifecycleActive,
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}

	rows, err := NewAPI().StatusWithOpts(StatusOpts{})
	if err != nil {
		t.Fatalf("StatusWithOpts with unavailable scheduler: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1 registry-only workspace row: %+v", len(rows), rows)
	}
	if rows[0].TaskName != "mcp-local-hub-lsp-abcd1234-python" || rows[0].Language != "python" {
		t.Fatalf("row = %+v, want registry-backed python LSP row", rows[0])
	}
}

// TestEnrichStatusWithRegistry_FailedEntryCarriesLastError asserts a
// missing-or-failed entry's LastError round-trips through enrichment.
func TestEnrichStatusWithRegistry_FailedEntryCarriesLastError(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "deadbeef",
		Language:     "go",
		Backend:      "gopls-mcp",
		Port:         9220,
		TaskName:     "mcp-local-hub-lsp-deadbeef-go",
		Lifecycle:    LifecycleMissing,
		LastError:    "gopls not on PATH",
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-deadbeef-go"},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != LifecycleMissing {
		t.Errorf("Lifecycle = %q, want %q", rows[0].Lifecycle, LifecycleMissing)
	}
	if rows[0].LastError != "gopls not on PATH" {
		t.Errorf("LastError = %q, want %q", rows[0].LastError, "gopls not on PATH")
	}
}

// TestEnrichStatusWithRegistry_GlobalRowUntouched asserts a non-lazy-proxy
// task name (e.g. `mcp-local-hub-serena-claude`) leaves the workspace-scoped
// fields empty, preserving the stable global-daemon output contract.
func TestEnrichStatusWithRegistry_GlobalRowUntouched(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	reg.Put(WorkspaceEntry{
		WorkspaceKey: "abcd1234",
		Language:     "python",
		TaskName:     "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:    LifecycleActive,
	})
	if err := reg.Save(); err != nil {
		t.Fatal(err)
	}
	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-serena-claude`},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != "" {
		t.Errorf("global row got lifecycle = %q; must stay empty", rows[0].Lifecycle)
	}
	if rows[0].Workspace != "" || rows[0].Language != "" || rows[0].Backend != "" {
		t.Errorf("global row got workspace fields populated: %+v", rows[0])
	}
	// parseTaskName still runs for the global row.
	if rows[0].Server != "serena" || rows[0].Daemon != "claude" {
		t.Errorf("global row parse broke: Server=%q Daemon=%q", rows[0].Server, rows[0].Daemon)
	}
	if rows[0].IsWorkspaceScoped {
		t.Errorf("IsWorkspaceScoped = true on global row; must stay false")
	}
}

// TestEnrichStatusWithRegistry_NoRegistryFileIsSilentNoop asserts a missing
// registry file does not break enrichment — workspace-scoped rows get their
// task-name split done but fields stay empty.
func TestEnrichStatusWithRegistry_NoRegistryFileIsSilentNoop(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/nonexistent.yaml"
	rows := []DaemonStatus{
		{TaskName: "mcp-local-hub-lsp-abcd1234-python"},
	}
	enrichStatusWithRegistry(rows, "", regPath)
	if rows[0].Lifecycle != "" {
		t.Errorf("missing registry should not populate lifecycle; got %q", rows[0].Lifecycle)
	}
	// Server / Daemon still get parsed.
	if rows[0].Server != "mcp-language-server" {
		t.Errorf("Server = %q, want mcp-language-server", rows[0].Server)
	}
	// IsWorkspaceScoped is the structural flag; it must survive the
	// registry-missing scenario (it is set BEFORE the overlay). This is
	// the exact failure mode the GUI Logs picker depends on — without
	// this guarantee, workspace-proxy rows would leak into the global
	// log dropdown whenever registry loading fails.
	if !rows[0].IsWorkspaceScoped {
		t.Errorf("IsWorkspaceScoped = false when registry is missing; must still be true (derived from TaskName structure, not registry)")
	}
}

// TestEnrichStatusWithRegistry_MaintenanceFlag covers the structural
// IsMaintenance flag populated for weekly-refresh scheduler tasks. The
// GUI (Logs picker, Dashboard) uses this flag to filter maintenance
// rows out of daemon-only surfaces — without it, selecting the hub-wide
// weekly-refresh row in the Logs picker issues GET /api/logs/?... with
// an empty server and 404s, and the Dashboard renders a blank-name
// card whose Restart button hits /api/servers//restart.
//
// Covers all three weekly-refresh naming conventions (hub-wide global,
// hub-wide workspace, legacy per-server) and confirms a normal daemon
// row stays IsMaintenance=false.
func TestEnrichStatusWithRegistry_MaintenanceFlag(t *testing.T) {
	dir := t.TempDir()
	regPath := dir + "/ws.yaml"
	rows := []DaemonStatus{
		{TaskName: `\mcp-local-hub-workspace-weekly-refresh`}, // hub-wide workspace (WeeklyRefreshTaskName)
		{TaskName: `\mcp-local-hub-weekly-refresh`},           // hub-wide global (WeeklyRefreshSet)
		{TaskName: `\mcp-local-hub-serena-weekly-refresh`},    // legacy per-server
		{TaskName: `\mcp-local-hub-serena-claude`},            // normal daemon row
	}
	enrichStatusWithRegistry(rows, "", regPath)

	for i, want := range []bool{true, true, true, false} {
		if rows[i].IsMaintenance != want {
			t.Errorf("rows[%d] TaskName=%q IsMaintenance = %v, want %v",
				i, rows[i].TaskName, rows[i].IsMaintenance, want)
		}
	}
	// Also confirm the task-name parse produced the expected daemon label
	// — if parseTaskName ever regresses, IsMaintenance would silently stop
	// firing for the hub-wide workspace task.
	if rows[0].Daemon != "weekly-refresh" {
		t.Errorf("hub-wide workspace task: Daemon = %q, want weekly-refresh", rows[0].Daemon)
	}
	if rows[3].Daemon != "claude" {
		t.Errorf("normal daemon row: Daemon = %q, want claude", rows[3].Daemon)
	}
}

// TestParseLazyProxyTaskName exercises the pattern classifier.
func TestParseLazyProxyTaskName(t *testing.T) {
	cases := []struct {
		in       string
		wantKey  string
		wantLang string
		wantOK   bool
	}{
		{`mcp-local-hub-lsp-abcd1234-python`, "abcd1234", "python", true},
		{`\mcp-local-hub-lsp-abcd1234-python`, "abcd1234", "python", true},
		{`mcp-local-hub-lsp-deadbeef-vscode-css`, "deadbeef", "vscode-css", true},
		// wrong prefix
		{`mcp-local-hub-serena-claude`, "", "", false},
		{`mcp-local-hub-weekly-refresh`, "", "", false},
		// too-short key (must be exactly 8 hex)
		{`mcp-local-hub-lsp-abc-python`, "", "", false},
		// non-hex key
		{`mcp-local-hub-lsp-ZZZZZZZZ-python`, "", "", false},
		// missing language
		{`mcp-local-hub-lsp-abcd1234-`, "", "", false},
		{`mcp-local-hub-lsp-abcd1234`, "", "", false},
	}
	for _, tc := range cases {
		gotKey, gotLang, gotOK := parseLazyProxyTaskName(tc.in)
		if gotKey != tc.wantKey || gotLang != tc.wantLang || gotOK != tc.wantOK {
			t.Errorf("parseLazyProxyTaskName(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.in, gotKey, gotLang, gotOK, tc.wantKey, tc.wantLang, tc.wantOK)
		}
	}
}
