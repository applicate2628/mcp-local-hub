package gui

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

func startReadySupervisorFixture(t *testing.T, stateDir string, initial []api.SupervisorDaemon) {
	t.Helper()
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	if err := api.WriteSupervisorIntent(intentPath, &api.SupervisorIntentFile{Version: 1, Daemons: append([]api.SupervisorDaemon(nil), initial...)}); err != nil {
		t.Fatalf("seed supervisor intent: %v", err)
	}
	owner := api.SupervisorLockOwner{PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	ownerRaw, err := json.Marshal(owner)
	if err != nil {
		t.Fatalf("marshal supervisor lock owner: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "supervisor.lock.owner.json"), ownerRaw, 0o600); err != nil {
		t.Fatalf("seed supervisor lock owner: %v", err)
	}
	t.Cleanup(startReadySupervisorStatusServer(t, stateDir, owner, func(conn net.Conn) {
		serveReadySupervisorStatus(t, conn, intentPath, owner)
	}))
}

func serveReadySupervisorStatus(t *testing.T, conn net.Conn, intentPath string, owner api.SupervisorLockOwner) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	hello, err := json.Marshal(map[string]any{"hello": api.IPCHello{Version: 1, PID: owner.PID, StartedAt: owner.StartedAt}})
	if err != nil {
		t.Errorf("marshal supervisor IPC hello: %v", err)
		return
	}
	if _, err := conn.Write(append(hello, '\n')); err != nil {
		return
	}
	line, err := readSupervisorFixtureLine(conn)
	if err != nil {
		return
	}
	var req api.IPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		t.Errorf("decode supervisor IPC request: %v", err)
		return
	}
	if req.Cmd == "reconcile" {
		raw, err := json.Marshal(api.IPCResponse{ID: req.ID, OK: true, Result: api.ReconcileResponse{}, Final: true})
		if err != nil {
			t.Errorf("marshal supervisor IPC reconcile: %v", err)
			return
		}
		_, _ = conn.Write(append(raw, '\n'))
		return
	}
	if req.Cmd != "status" {
		t.Errorf("supervisor IPC command = %q, want status or reconcile", req.Cmd)
		return
	}
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		t.Errorf("read supervisor intent for status: %v", err)
		return
	}
	daemons := make([]map[string]any, 0, len(intent.Daemons))
	for _, daemon := range intent.Daemons {
		observation := api.DaemonReadinessObservationV1{TaskName: daemon.TaskName, Server: daemon.Server, Daemon: daemon.Daemon, Port: daemon.Port, PID: owner.PID, ProcessState: "Running", CurrentPIDGeneration: 1, ObservedPIDGeneration: 1, IntentPresent: true, IntentRunnable: true, WrapperStarted: true, ListenerReady: true, MCPInitializeReady: true, MCPToolsListReady: true, Policy: api.ReadinessPolicyMCPUpstream}
		daemons = append(daemons, map[string]any{"task_name": daemon.TaskName, "server": daemon.Server, "daemon": daemon.Daemon, "port": daemon.Port, "state": "Running", "current_pid": owner.PID, "started_at": owner.StartedAt, "readiness_observation": api.EncodeReadinessObservationV1(&observation)})
	}
	raw, err := json.Marshal(api.IPCResponse{ID: req.ID, OK: true, Result: map[string]any{"state": "running", "daemons": daemons}})
	if err != nil {
		t.Errorf("marshal supervisor IPC status: %v", err)
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

func readSupervisorFixtureLine(conn net.Conn) ([]byte, error) {
	buf, tmp := make([]byte, 0, 4096), make([]byte, 1)
	for len(buf) < 4096 {
		n, err := conn.Read(tmp)
		if n > 0 {
			if tmp[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, tmp[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, os.ErrInvalid
}
