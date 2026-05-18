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

func TestIPCStatusReadsRuntimeTracker(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)

	intent := &api.SupervisorIntentFile{
		Version: 1,
		Daemons: []api.SupervisorDaemon{
			{
				TaskName:  `\mcp-local-hub-memory-default`,
				Command:   `C:\tools\mcphub.exe`,
				Args:      []string{"daemon", "memory"},
				Workspace: `D:\work\default`,
				Port:      9101,
			},
			{
				TaskName:  `\mcp-local-hub-serena-codex`,
				Command:   `C:\tools\mcphub.exe`,
				Args:      []string{"daemon", "serena", "--daemon", "codex"},
				Workspace: `D:\work\codex`,
				Port:      9122,
			},
			{
				TaskName:  `\mcp-local-hub-filesystem-default`,
				Command:   `C:\tools\mcphub.exe`,
				Args:      []string{"daemon", "filesystem"},
				Workspace: `D:\work\fs`,
				Port:      9123,
			},
		},
	}
	if err := api.WriteSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"), intent); err != nil {
		t.Fatalf("seed supervisor-intent.json: %v", err)
	}
	tracker := NewDaemonRuntimeTracker()
	tracker.MarkSpawned(`\mcp-local-hub-memory-default`, 4321, time.Date(2026, 5, 18, 10, 0, 0, 0, time.UTC))
	tracker.MarkTerminated(`\mcp-local-hub-serena-codex`)

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	var ready atomic.Bool
	var loaded atomic.Bool
	ready.Store(true)
	loaded.Store(true)
	var graceful gracefulCounter
	deps := ipcDispatchDeps{
		stateDir:            stateDir,
		runtimeTracker:      tracker,
		reconcileReady:      &ready,
		intentFilesLoaded:   &loaded,
		gracefulInProgress:  &graceful,
		triggerGracefulExit: func() {},
	}

	done := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		done <- dispatchIPCRequest(serverConn, api.IPCRequest{ID: 101, Cmd: "status"}, deps)
	}()

	_ = clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read status response: %v", err)
	}
	var resp api.IPCResponse
	if err := json.Unmarshal([]byte(line), &resp); err != nil {
		t.Fatalf("decode response %q: %v", line, err)
	}
	if !resp.OK || resp.Error != nil {
		t.Fatalf("status response not OK: %+v", resp)
	}
	result, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want object", resp.Result)
	}
	rawDaemons, ok := result["daemons"].([]any)
	if !ok {
		t.Fatalf("daemons type = %T, want array in result %+v", result["daemons"], result)
	}
	if len(rawDaemons) != 3 {
		t.Fatalf("daemons len = %d, want 3: %+v", len(rawDaemons), rawDaemons)
	}

	byTask := map[string]map[string]any{}
	for _, raw := range rawDaemons {
		row, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("daemon row type = %T, want object", raw)
		}
		task, _ := row["task_name"].(string)
		byTask[task] = row
	}
	mem := byTask[`\mcp-local-hub-memory-default`]
	if mem == nil {
		t.Fatalf("memory daemon missing from response: %+v", byTask)
	}
	if mem["server"] != "memory" || mem["daemon"] != "default" {
		t.Fatalf("memory server/daemon = %v/%v, want memory/default", mem["server"], mem["daemon"])
	}
	if mem["state"] != "Running" || mem["current_pid"] != float64(4321) {
		t.Fatalf("memory state/pid = %v/%v, want Running/4321", mem["state"], mem["current_pid"])
	}
	if mem["workspace"] != `D:\work\default` || mem["port"] != float64(9101) {
		t.Fatalf("memory workspace/port = %v/%v, want seeded descriptor", mem["workspace"], mem["port"])
	}

	codex := byTask[`\mcp-local-hub-serena-codex`]
	if codex == nil {
		t.Fatalf("serena codex daemon missing from response: %+v", byTask)
	}
	if codex["server"] != "serena" || codex["daemon"] != "codex" {
		t.Fatalf("serena codex server/daemon = %v/%v, want serena/codex", codex["server"], codex["daemon"])
	}
	if codex["state"] != "Stopped" || codex["current_pid"] != float64(0) {
		t.Fatalf("codex state/pid = %v/%v, want Stopped/0", codex["state"], codex["current_pid"])
	}
	fs := byTask[`\mcp-local-hub-filesystem-default`]
	if fs == nil {
		t.Fatalf("filesystem daemon missing from response: %+v", byTask)
	}
	if fs["state"] != "Idle" || fs["current_pid"] != float64(0) {
		t.Fatalf("filesystem state/pid = %v/%v, want Idle/0 for absent tracker row", fs["state"], fs["current_pid"])
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dispatch returned write error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dispatch did not return after writing status response")
	}
}
