package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/apitest"
)

func TestCommitSerenaActivity_ValidatesGenerationAndIsIdempotent(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	registeredAt := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	activityAt := registeredAt.Add(time.Minute)
	entry := api.WorkspaceEntry{
		WorkspaceKey: "serena-key", WorkspacePath: "/work/serena", Language: api.SerenaLanguageSentinel,
		Backend: api.SerenaServerName, TaskName: `\mcp-local-hub-serena-serena-key`, Port: 9321, RegisteredAt: registeredAt,
	}
	registry := api.NewRegistry(filepath.Join(stateDir, "workspaces.yaml"))
	registry.Put(entry)
	if err := registry.Save(); err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{TaskName: entry.TaskName, Workspace: entry.WorkspacePath, Port: entry.Port}}}); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	request := api.SerenaActivityCommitRequestV1{ProtocolVersion: 1, WorkspaceKey: entry.WorkspaceKey, WorkspacePath: entry.WorkspacePath, TaskName: entry.TaskName, ExpectedPort: entry.Port, RegisteredAt: registeredAt, ActivityAt: activityAt}
	first := dispatchSerenaActivityRequest(t, stateDir, request)
	if !first.OK || first.Error != nil {
		t.Fatalf("first response = %+v", first)
	}
	var receipt api.SerenaActivityCommitReceiptV1
	if err := json.Unmarshal(first.Result.(json.RawMessage), &receipt); err != nil {
		t.Fatalf("decode first receipt: %v", err)
	}
	if receipt.State != "committed" || !receipt.ActivityAt.Equal(activityAt) {
		t.Fatalf("first receipt = %+v", receipt)
	}
	second := dispatchSerenaActivityRequest(t, stateDir, request)
	if !second.OK || second.Error != nil {
		t.Fatalf("second response = %+v", second)
	}
	if err := json.Unmarshal(second.Result.(json.RawMessage), &receipt); err != nil {
		t.Fatalf("decode second receipt: %v", err)
	}
	if receipt.State != "already_committed" {
		t.Fatalf("second receipt state = %q, want already_committed", receipt.State)
	}
	reloaded := api.NewRegistry(filepath.Join(stateDir, "workspaces.yaml"))
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload registry: %v", err)
	}
	got, ok := reloaded.Get(entry.WorkspaceKey, api.SerenaLanguageSentinel)
	if !ok || !got.LastToolsCallAt.Equal(activityAt) {
		t.Fatalf("registry activity = %+v, want %v", got, activityAt)
	}

	stale := request
	stale.RegisteredAt = registeredAt.Add(time.Second)
	staleResponse := dispatchSerenaActivityRequest(t, stateDir, stale)
	if staleResponse.Error == nil || staleResponse.Error.Code != "STALE_ACTIVITY_TARGET" {
		t.Fatalf("stale response = %+v, want STALE_ACTIVITY_TARGET", staleResponse)
	}
	reloaded = api.NewRegistry(filepath.Join(stateDir, "workspaces.yaml"))
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload after stale request: %v", err)
	}
	got, _ = reloaded.Get(entry.WorkspaceKey, api.SerenaLanguageSentinel)
	if !got.LastToolsCallAt.Equal(activityAt) {
		t.Fatalf("stale request changed activity = %v, want %v", got.LastToolsCallAt, activityAt)
	}
}

func TestCommitSerenaActivity_LegacyGenerationIsSupervisorRepaired(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	entry := api.WorkspaceEntry{
		WorkspaceKey: "legacy-ipc", WorkspacePath: "/work/legacy-ipc", Language: api.SerenaLanguageSentinel,
		Backend: api.SerenaServerName, TaskName: `\mcp-local-hub-serena-legacy-ipc`, Port: 9322,
	}
	registry := api.NewRegistry(filepath.Join(stateDir, "workspaces.yaml"))
	registry.Put(entry)
	if err := registry.Save(); err != nil {
		t.Fatalf("seed legacy registry: %v", err)
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), &api.SupervisorIntentFile{Version: 1, Daemons: []api.SupervisorDaemon{{TaskName: entry.TaskName, Workspace: entry.WorkspacePath, Port: entry.Port}}}); err != nil {
		t.Fatalf("seed legacy intent: %v", err)
	}
	request := api.SerenaActivityCommitRequestV1{
		ProtocolVersion: 1, WorkspaceKey: entry.WorkspaceKey, WorkspacePath: entry.WorkspacePath,
		TaskName: entry.TaskName, ExpectedPort: entry.Port, LegacyGenerationUnspecified: true, ActivityAt: time.Now().UTC(),
	}
	response := dispatchSerenaActivityRequest(t, stateDir, request)
	if !response.OK || response.Error != nil {
		t.Fatalf("legacy response = %+v", response)
	}
	var receipt api.SerenaActivityCommitReceiptV1
	if err := json.Unmarshal(response.Result.(json.RawMessage), &receipt); err != nil {
		t.Fatalf("decode legacy receipt: %v", err)
	}
	if !receipt.LegacyGenerationRepaired || receipt.RegisteredAt.IsZero() {
		t.Fatalf("legacy receipt = %+v, want supervisor-owned generation", receipt)
	}
	reloaded := api.NewRegistry(filepath.Join(stateDir, "workspaces.yaml"))
	if err := reloaded.Load(); err != nil {
		t.Fatalf("reload legacy registry: %v", err)
	}
	got, _ := reloaded.Get(entry.WorkspaceKey, api.SerenaLanguageSentinel)
	if !got.RegisteredAt.Equal(receipt.RegisteredAt) || !got.LastToolsCallAt.Equal(request.ActivityAt) {
		t.Fatalf("legacy persisted row = %+v, receipt=%+v", got, receipt)
	}
}

func TestCommitLazyProxyReadiness_StampsCurrentWrapperGeneration(t *testing.T) {
	tracker := NewDaemonRuntimeTracker()
	taskName := `\mcp-local-hub-lsp-abcdef12-go`
	startedAt := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	tracker.MarkSpawned(taskName, 4812, startedAt)
	var ready atomic.Bool
	ready.Store(true)
	starting := api.IPCRequest{
		ID: 78, Cmd: "commit_lazy_proxy_readiness", Args: map[string]any{"event": map[string]any{
			"protocol_version": 1, "task_name": taskName, "wrapper_pid": 4812,
			"wrapper_started_at": startedAt.Format(time.RFC3339Nano), "backend_epoch": 1,
			"transition": "starting", "observed_at": startedAt.Add(time.Second).Format(time.RFC3339Nano),
		}},
	}
	response := dispatchLazyReadinessRequest(t, starting, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if !response.OK || response.Error != nil {
		t.Fatalf("lazy readiness starting response = %+v", response)
	}
	readyRequest := starting
	readyRequest.ID = 79
	readyRequest.Args["event"].(map[string]any)["transition"] = "ready"
	readyRequest.Args["event"].(map[string]any)["observed_at"] = startedAt.Add(2 * time.Second).Format(time.RFC3339Nano)
	response = dispatchLazyReadinessRequest(t, readyRequest, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if !response.OK || response.Error != nil {
		t.Fatalf("lazy readiness ready response = %+v", response)
	}
	result, ok := response.Result.(json.RawMessage)
	if !ok {
		t.Fatalf("lazy readiness result type = %T", response.Result)
	}
	var receipt map[string]any
	if err := json.Unmarshal(result, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt["disposition"] != "accepted" || receipt["pid_generation"] != float64(1) || receipt["backend_epoch"] != float64(1) || receipt["transition"] != "ready" {
		t.Fatalf("lazy readiness receipt = %v", receipt)
	}
	readyRequest.ID = 80
	response = dispatchLazyReadinessRequest(t, readyRequest, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if !response.OK || response.Error != nil {
		t.Fatalf("lazy readiness duplicate response = %+v", response)
	}
	if err := json.Unmarshal(response.Result.(json.RawMessage), &receipt); err != nil || receipt["disposition"] != "duplicate" {
		t.Fatalf("lazy readiness duplicate receipt = %v err=%v", receipt, err)
	}
	staleStarting := api.IPCRequest{ID: 81, Cmd: "commit_lazy_proxy_readiness", Args: map[string]any{"event": map[string]any{
		"protocol_version": 1, "task_name": taskName, "wrapper_pid": 4812,
		"wrapper_started_at": startedAt.Format(time.RFC3339Nano), "backend_epoch": 1,
		"transition": "starting", "observed_at": startedAt.Add(3 * time.Second).Format(time.RFC3339Nano),
	}}}
	response = dispatchLazyReadinessRequest(t, staleStarting, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if !response.OK || response.Error != nil {
		t.Fatalf("lazy readiness superseded response = %+v", response)
	}
	if err := json.Unmarshal(response.Result.(json.RawMessage), &receipt); err != nil || receipt["disposition"] != "superseded" || receipt["transition"] != "ready" {
		t.Fatalf("lazy readiness superseded receipt = %v err=%v", receipt, err)
	}
	staleIncarnation := readyRequest
	staleIncarnation.ID = 82
	staleIncarnation.Args = map[string]any{"event": map[string]any{
		"protocol_version": 1, "task_name": taskName, "wrapper_pid": 4812,
		"wrapper_started_at": startedAt.Add(time.Second).Format(time.RFC3339Nano), "backend_epoch": 2,
		"transition": "starting", "observed_at": startedAt.Add(4 * time.Second).Format(time.RFC3339Nano),
	}}
	response = dispatchLazyReadinessRequest(t, staleIncarnation, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if response.Error == nil || response.Error.Code != "LAZY_READINESS_INCARNATION_MISMATCH" {
		t.Fatalf("lazy readiness stale incarnation response = %+v", response)
	}
	invalid := readyRequest
	invalid.ID = 83
	invalid.Args = map[string]any{"event": map[string]any{"protocol_version": 1, "task_name": taskName, "wrapper_pid": 4812, "wrapper_started_at": startedAt.Format(time.RFC3339Nano), "backend_epoch": 1, "transition": "unknown", "observed_at": startedAt.Add(5 * time.Second).Format(time.RFC3339Nano)}}
	response = dispatchLazyReadinessRequest(t, invalid, ipcDispatchDeps{runtimeTracker: tracker, reconcileReady: &ready})
	if response.Error == nil || response.Error.Code != "LAZY_READINESS_INVALID" {
		t.Fatalf("lazy readiness invalid transition response = %+v", response)
	}
	entry, found := tracker.Get(taskName)
	if !found || entry.ReadinessObservation == nil || entry.ReadinessObservation.CurrentPIDGeneration != 1 || !entry.ReadinessObservation.MCPToolsListReady {
		t.Fatalf("tracker readiness entry = %+v found=%t", entry, found)
	}
}

func TestDaemonRuntimeTracker_LazyReadinessStateClearsOnLifecycleTransitions(t *testing.T) {
	taskName := `\mcp-local-hub-lsp-abcdef12-go`
	startedAt := time.Date(2026, 9, 12, 16, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		mark func(*DaemonRuntimeTracker)
	}{
		{"spawn failed", func(t *DaemonRuntimeTracker) { t.MarkSpawnFailed(taskName, errors.New("failed")) }},
		{"exit", func(t *DaemonRuntimeTracker) { t.MarkExited(taskName) }},
		{"backoff", func(t *DaemonRuntimeTracker) { t.MarkBackoff(taskName) }},
		{"quarantine", func(t *DaemonRuntimeTracker) { t.MarkQuarantined(taskName) }},
		{"remove", func(t *DaemonRuntimeTracker) { t.Remove(taskName) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewDaemonRuntimeTracker()
			tracker.MarkSpawned(taskName, 4812, startedAt)
			_, _, err := tracker.CommitLazyProxyReadinessEvent(api.LazyProxyReadinessEventV1{ProtocolVersion: 1, TaskName: taskName, WrapperPID: 4812, WrapperStartedAt: startedAt.Format(time.RFC3339Nano), BackendEpoch: 1, Transition: "starting", ObservedAt: startedAt.Add(time.Second).Format(time.RFC3339Nano)})
			if err != nil {
				t.Fatal(err)
			}
			tc.mark(tracker)
			if _, ok := tracker.lazyReadiness[canonicalSupervisorTaskName(taskName)]; ok {
				t.Fatal("lifecycle transition retained lazy readiness epoch")
			}
		})
	}
}

func dispatchSerenaActivityRequest(t *testing.T, stateDir string, request api.SerenaActivityCommitRequestV1) api.IPCResponse {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	var ready atomic.Bool
	ready.Store(true)
	go func() {
		defer server.Close()
		done <- dispatchIPCRequest(server, api.IPCRequest{ID: 77, Cmd: "commit_serena_activity", Args: map[string]any{"request": request}}, ipcDispatchDeps{stateDir: stateDir, reconcileReady: &ready})
	}()
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var raw struct {
		ID     int64           `json:"id"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *api.IPCErr     `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return api.IPCResponse{ID: raw.ID, OK: raw.OK, Result: raw.Result, Error: raw.Error}
}

func dispatchLazyReadinessRequest(t *testing.T, request api.IPCRequest, deps ipcDispatchDeps) api.IPCResponse {
	t.Helper()
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		done <- dispatchIPCRequest(server, request, deps)
	}()
	line, err := bufio.NewReader(client).ReadString('\n')
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var raw struct {
		ID     int64           `json:"id"`
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  *api.IPCErr     `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	return api.IPCResponse{ID: raw.ID, OK: raw.OK, Result: raw.Result, Error: raw.Error}
}
