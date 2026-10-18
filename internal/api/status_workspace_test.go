package api

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}
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
	workspacePath := filepath.Join(dir, "workspace-a")
	regPath := dir + "/ws.yaml"
	reg := NewRegistry(regPath)
	now := time.Now().UTC().Truncate(time.Second)
	reg.Put(WorkspaceEntry{
		WorkspaceKey:       "abcd1234",
		WorkspacePath:      workspacePath,
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}
	r := rows[0]
	if r.Workspace != workspacePath {
		t.Errorf("Workspace = %q, want %q", r.Workspace, workspacePath)
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
	// Redirect the supervisor-intent read (the Phase-F supervisor-only seed in
	// StatusWithOpts) to a fresh empty temp state dir so it never reads the
	// host's live supervisor-intent.json. Absent file → no-op merge.
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	workspaceRoot := filepath.Dir(regPath)
	lazyWorkspace := filepath.Join(workspaceRoot, "workspace-a")
	serenaWorkspace := filepath.Join(workspaceRoot, "workspace-b")
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
		WorkspacePath: lazyWorkspace,
		Language:      "python",
		Backend:       "mcp-language-server",
		Port:          9217,
		TaskName:      "mcp-local-hub-lsp-abcd1234-python",
		Lifecycle:     LifecycleActive,
	})
	if err := reg.PutSerena(WorkspaceEntry{
		WorkspaceKey:  "serena1234",
		WorkspacePath: serenaWorkspace,
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

func TestStatusWithOptsRejectsWorkspaceRegistryPortMismatch(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
	stateDir := t.TempDir()
	t.Cleanup(SetDaemonStateRootForTest(stateDir))

	const taskName = `\mcp-local-hub-lsp-abcd1234-go`
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSupervisorIntent(intentPath, &SupervisorIntentFile{
		Version: 1,
		Daemons: []SupervisorDaemon{{
			TaskName:  taskName,
			Server:    "mcp-language-server",
			Daemon:    "lsp-abcd1234-go",
			Workspace: `/workspace/project`,
			Port:      9421,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	registryPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	previousRegistryPath := defaultRegistryPathFn
	defaultRegistryPathFn = func() (string, error) { return registryPath, nil }
	t.Cleanup(func() { defaultRegistryPathFn = previousRegistryPath })
	registry := NewRegistry(registryPath)
	// A sentinel row is excluded from the registry-only LSP seed, so the public
	// rich-status path reaches the supervisor-intent row with its existing port.
	// The exact task match still presents the same cross-snapshot contradiction
	// to the metadata join that a default IPC row can carry.
	registry.Workspaces = []WorkspaceEntry{{
		WorkspaceKey:  "abcd1234",
		WorkspacePath: `/workspace/project`,
		Language:      SerenaLanguageSentinel,
		Backend:       "gopls-mcp",
		Port:          9422,
		TaskName:      strings.TrimPrefix(taskName, `\`),
		Lifecycle:     LifecycleActive,
	}}
	if err := registry.Save(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{
			name: "detailed",
			run: func() error {
				_, err := NewAPI().StatusWithOptsDetailed(StatusOpts{})
				return err
			},
		},
		{
			name: "legacy wrapper",
			run: func() error {
				_, err := NewAPI().StatusWithOpts(StatusOpts{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("status returned nil error for nonzero status/registry port mismatch")
			}
			if !strings.Contains(err.Error(), "WORKSPACE_STATUS_METADATA_MISMATCH") {
				t.Fatalf("status error = %T (%v), want stable mismatch identifier", err, err)
			}
		})
	}
}

func TestApplyWorkspaceRegistryMetadataValidatesAllRowsBeforeMutation(t *testing.T) {
	firstMaterialized := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	rows := []DaemonStatus{
		{
			TaskName:       `\mcp-local-hub-lsp-abcd1234-go`,
			Port:           0,
			State:          "Running",
			PID:            111,
			ServiceState:   ServiceStateStarting,
			ReadinessStage: ReadinessStageUpstreamListener,
		},
		{
			TaskName: `\mcp-local-hub-lsp-deadbeef-python`,
			Port:     9500,
			State:    "Running",
			PID:      222,
		},
	}
	before := append([]DaemonStatus(nil), rows...)
	entries := []WorkspaceEntry{
		{
			TaskName:           "mcp-local-hub-lsp-abcd1234-go",
			WorkspacePath:      "/workspace/first",
			Language:           "go",
			Backend:            "gopls-mcp",
			Port:               9421,
			Lifecycle:          LifecycleActive,
			LastMaterializedAt: firstMaterialized,
		},
		{
			TaskName:      "mcp-local-hub-lsp-deadbeef-python",
			WorkspacePath: "/workspace/second",
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          9501,
			Lifecycle:     LifecycleActive,
		},
	}

	err := applyWorkspaceRegistryMetadata(rows, entries)
	var mismatch *WorkspaceStatusMetadataMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("metadata error = %T (%v), want *WorkspaceStatusMetadataMismatchError", err, err)
	}
	if mismatch.TaskName != rows[1].TaskName || mismatch.StatusPort != 9500 || mismatch.RegistryPort != 9501 {
		t.Fatalf("mismatch = %+v, want task and both conflicting ports", mismatch)
	}
	if !strings.HasPrefix(err.Error(), WorkspaceStatusMetadataMismatchID) {
		t.Fatalf("error = %q, want stable prefix %q", err, WorkspaceStatusMetadataMismatchID)
	}
	if !reflect.DeepEqual(rows, before) {
		t.Fatalf("late mismatch partially mutated rows:\n got  %#v\n want %#v", rows, before)
	}
}

func TestApplyWorkspaceRegistryMetadataCompatibleIdentityPreservesRuntimeFields(t *testing.T) {
	materialized := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	toolsCall := time.Date(2026, 9, 12, 12, 5, 0, 0, time.UTC)
	observation := &DaemonReadinessObservationV1{Policy: ReadinessPolicySyntheticProxy}
	rows := []DaemonStatus{
		{
			TaskName:             `\mcp-local-hub-lsp-abcd1234-go`,
			Server:               "mcp-language-server",
			Daemon:               "lsp-abcd1234-go",
			IsWorkspaceScoped:    true,
			State:                "Running",
			PID:                  111,
			ReadinessObservation: observation,
			ServiceState:         ServiceStateStarting,
			ReadinessStage:       ReadinessStageUpstreamListener,
			ReadinessSettled:     false,
		},
		{
			TaskName:          "mcp-local-hub-lsp-deadbeef-python",
			Port:              9500,
			State:             "Ready",
			PID:               222,
			ServiceState:      ServiceStateRunning,
			ReadinessSettled:  true,
			IsWorkspaceScoped: true,
		},
		{TaskName: "mcp-local-hub-lsp-feedface-rust", Port: 9600, State: "Stopped"},
		{TaskName: "mcp-local-hub-lsp-c0ffee00-typescript", Port: 9700, State: "Stopped"},
		{TaskName: `\mcp-local-hub-memory-default`, Port: 9304, State: "Running", PID: 333},
	}
	entries := []WorkspaceEntry{
		{
			TaskName:           "mcp-local-hub-lsp-abcd1234-go",
			WorkspacePath:      "/workspace/go",
			Language:           "go",
			Backend:            "gopls-mcp",
			Port:               9421,
			Lifecycle:          LifecycleActive,
			LastMaterializedAt: materialized,
			LastToolsCallAt:    toolsCall,
			LastError:          "historical",
		},
		{
			TaskName:      `\mcp-local-hub-lsp-deadbeef-python`,
			WorkspacePath: "/workspace/python",
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          9500,
			Lifecycle:     LifecycleActive,
		},
		{
			TaskName:      "mcp-local-hub-lsp-feedface-rust",
			WorkspacePath: "/workspace/rust",
			Language:      "rust",
			Backend:       "rust-analyzer",
			Port:          0,
			Lifecycle:     LifecycleConfigured,
		},
		{
			TaskName:      `\mcp-local-hub-memory-default`,
			WorkspacePath: "/must/not/apply",
			Port:          9305,
			Lifecycle:     LifecycleActive,
		},
	}

	if err := applyWorkspaceRegistryMetadata(rows, entries); err != nil {
		t.Fatal(err)
	}
	first := rows[0]
	if first.Workspace != "/workspace/go" || first.Language != "go" || first.Backend != "gopls-mcp" || first.Lifecycle != LifecycleActive || first.LastError != "historical" {
		t.Fatalf("first metadata = %+v, want all registry fields", first)
	}
	if first.Port != 9421 || !first.LastMaterializedAt.Equal(materialized) || !first.LastToolsCallAt.Equal(toolsCall) {
		t.Fatalf("first port/timestamps = %d/%v/%v", first.Port, first.LastMaterializedAt, first.LastToolsCallAt)
	}
	if first.Server != "mcp-language-server" || first.Daemon != "lsp-abcd1234-go" || !first.IsWorkspaceScoped || first.State != "Running" || first.PID != 111 || first.ReadinessObservation != observation || first.ServiceState != ServiceStateStarting || first.ReadinessStage != ReadinessStageUpstreamListener || first.ReadinessSettled {
		t.Fatalf("first runtime/structural fields changed: %+v", first)
	}
	second := rows[1]
	if second.Port != 9500 || second.Workspace != "/workspace/python" || second.State != "Ready" || second.PID != 222 || second.ServiceState != ServiceStateRunning || !second.ReadinessSettled {
		t.Fatalf("equal-port row = %+v, want metadata with runtime fields preserved", second)
	}
	if rows[2].Workspace != "/workspace/rust" || rows[2].Port != 9600 || rows[2].State != "Stopped" || rows[2].Lifecycle != LifecycleConfigured {
		t.Fatalf("zero registry port row = %+v, want metadata without port overwrite", rows[2])
	}
	if rows[3].Workspace != "" || rows[3].Port != 9700 || rows[3].State != "Stopped" {
		t.Fatalf("missing registry row changed: %+v", rows[3])
	}
	if rows[4].Workspace != "" || rows[4].Port != 9304 || rows[4].State != "Running" || rows[4].PID != 333 {
		t.Fatalf("non-LSP row changed: %+v", rows[4])
	}
}

func TestStatusWithOpts_RegistryOnlyWorkspaceRowsUsePortLivenessWithoutHealth(t *testing.T) {
	t.Setenv("MCPHUB_E2E_SCHEDULER", "none")
	// Hermetic supervisor-intent read (see sibling test) — empty temp state dir.
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	workspaceRoot := filepath.Dir(regPath)
	liveWorkspace := filepath.Join(workspaceRoot, "workspace-live")
	deadWorkspace := filepath.Join(workspaceRoot, "workspace-dead")
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
			WorkspacePath: liveWorkspace,
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          livePort,
			TaskName:      "mcp-local-hub-lsp-abcd1234-python",
			Lifecycle:     LifecycleActive,
		},
		{
			WorkspaceKey:  "deadbeef",
			WorkspacePath: deadWorkspace,
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
	// Hermetic supervisor-intent read (see sibling test) — empty temp state dir.
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

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
	workspaceRoot := filepath.Dir(regPath)
	liveWorkspace := filepath.Join(workspaceRoot, "workspace-live")
	deadWorkspace := filepath.Join(workspaceRoot, "workspace-dead")
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
			WorkspacePath: liveWorkspace,
			Language:      "python",
			Backend:       "mcp-language-server",
			Port:          livePort,
			TaskName:      "mcp-local-hub-lsp-abcd1234-python",
			Lifecycle:     LifecycleActive,
		},
		{
			WorkspaceKey:  "deadbeef",
			WorkspacePath: deadWorkspace,
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
	// Hermetic supervisor-intent read (the Phase-F supervisor-only seed in
	// StatusWithOpts) — empty temp state dir so it never reads the host's live
	// supervisor-intent.json. Absent file → no-op merge.
	t.Cleanup(SetDaemonStateRootForTest(t.TempDir()))

	origScheduler := statusSchedulerFactory
	statusSchedulerFactory = func() (scheduler.Scheduler, error) {
		return nil, fmt.Errorf("linux scheduler not yet implemented (Phase 0-1 is Windows-first): %w", scheduler.ErrNotImplemented)
	}
	t.Cleanup(func() { statusSchedulerFactory = origScheduler })

	regPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	workspacePath := filepath.Join(filepath.Dir(regPath), "workspace-a")
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
		WorkspacePath: workspacePath,
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}
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

func TestEnrichStatusWithRegistry_InvalidRegistryIsSilentNoop(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "workspaces.yaml")
	if err := os.WriteFile(registryPath, []byte("version: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rows := []DaemonStatus{{
		TaskName: `\mcp-local-hub-lsp-abcd1234-python`,
		State:    "Running",
	}}

	if err := enrichStatusWithRegistry(rows, "", registryPath); err != nil {
		t.Fatalf("invalid registry must preserve best-effort rich status: %v", err)
	}
	if rows[0].Workspace != "" || rows[0].Language != "" || rows[0].Lifecycle != "" || rows[0].Port != 0 {
		t.Fatalf("invalid registry populated metadata: %+v", rows[0])
	}
	if !rows[0].IsWorkspaceScoped || rows[0].Server != "mcp-language-server" || rows[0].Daemon != "lsp-abcd1234-python" || rows[0].State != "Running" {
		t.Fatalf("invalid registry lost structural/raw status fields: %+v", rows[0])
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
	if err := enrichStatusWithRegistry(rows, "", regPath); err != nil {
		t.Fatal(err)
	}

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
