package cli

import (
	"bufio"
	"encoding/json"
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
