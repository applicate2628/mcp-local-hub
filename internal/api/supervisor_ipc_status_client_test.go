package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api/apitest"
)

func TestDialSupervisorIPCStatus_HappyPath(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir, owner, func(req IPCRequest) IPCResponse {
		if req.Cmd != "status" {
			t.Fatalf("cmd = %q, want status", req.Cmd)
		}
		return IPCResponse{
			ID: req.ID,
			OK: true,
			Result: map[string]any{
				"state": "running",
				"daemons": []map[string]any{
					{
						"task_name":      `\mcp-local-hub-memory-default`,
						"server":         "memory",
						"daemon":         "default",
						"workspace":      `D:\work\default`,
						"port":           9101,
						"state":          "Running",
						"current_pid":    4321,
						"is_maintenance": false,
					},
				},
			},
		}
	})
	defer stop()

	rows, err := DialSupervisorIPCStatus(context.Background())
	if err != nil {
		t.Fatalf("DialSupervisorIPCStatus: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.TaskName != `\mcp-local-hub-memory-default` || got.Server != "memory" || got.Daemon != "default" {
		t.Fatalf("row identity = %+v, want memory/default task", got)
	}
	if got.Workspace != `D:\work\default` || got.Port != 9101 || got.State != "Running" || got.PID != 4321 {
		t.Fatalf("row fields = %+v, want workspace/port/state/pid from IPC payload", got)
	}
}

func TestDialSupervisorIPCStatus_HandshakeMismatch(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)
	owner := SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:00Z"}
	writeSupervisorOwnerForTest(t, stateDir, owner)

	stop := startFakeSupervisorIPCStatusServer(t, stateDir,
		SupervisorLockOwner{PID: 4242, StartedAt: "2026-05-18T10:00:01Z"},
		func(req IPCRequest) IPCResponse {
			t.Fatalf("client sent %q after mismatched hello; want close before request", req.Cmd)
			return IPCResponse{}
		})
	defer stop()

	_, err := DialSupervisorIPCStatus(context.Background())
	if err == nil {
		t.Fatal("DialSupervisorIPCStatus returned nil error on handshake mismatch")
	}
	if !strings.Contains(err.Error(), "hello mismatch") {
		t.Fatalf("err = %v, want hello mismatch", err)
	}
}

func TestDialSupervisorIPCStatus_NoSupervisor(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	withDaemonStateRootOverride(t, stateDir)

	_, err := DialSupervisorIPCStatus(context.Background())
	if err == nil {
		t.Fatal("DialSupervisorIPCStatus returned nil error with no supervisor lock owner")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("err = %v, want errors.Is(os.ErrNotExist)", err)
	}
	if !strings.Contains(err.Error(), "supervisor.lock.owner.json") {
		t.Fatalf("err = %v, want supervisor.lock.owner.json path context", err)
	}
}

func TestReadSupervisorIPCLine_ProcessesBytesReturnedWithEOF(t *testing.T) {
	conn := &singleReadEOFConn{data: []byte(`{"id":7}` + "\n")}

	line, err := readSupervisorIPCLine(conn, 4096)
	if err != nil {
		t.Fatalf("readSupervisorIPCLine: %v", err)
	}
	if string(line) != `{"id":7}` {
		t.Fatalf("line = %q, want JSON payload without newline", string(line))
	}
}

func TestReadSupervisorIPCResponse_AllowsLargeStatusFrame(t *testing.T) {
	largeArg := strings.Repeat("x", 20*1024)
	raw, err := json.Marshal(map[string]any{
		"id": int64(7),
		"ok": true,
		"result": map[string]any{
			"state": "running",
			"daemons": []map[string]any{
				{
					"task_name": `\mcp-local-hub-memory-default`,
					"server":    "memory",
					"daemon":    "default",
					"args":      []string{largeArg},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal large response: %v", err)
	}
	conn := &singleReadEOFConn{data: append(raw, '\n')}

	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		t.Fatalf("readSupervisorIPCResponse: %v", err)
	}
	if resp.ID != 7 || !resp.OK {
		t.Fatalf("response = %+v, want id=7 ok=true", resp)
	}
}

func withDaemonStateRootOverride(t *testing.T, stateDir string) {
	t.Helper()
	prev := daemonStateRootOverride
	daemonStateRootOverride = stateDir
	t.Cleanup(func() { daemonStateRootOverride = prev })
}

func writeSupervisorOwnerForTest(t *testing.T, stateDir string, owner SupervisorLockOwner) {
	t.Helper()
	if err := WriteStateFileAtomic(filepath.Join(stateDir, "supervisor.lock.owner.json"), owner); err != nil {
		t.Fatalf("seed supervisor lock owner: %v", err)
	}
}

func serveFakeSupervisorIPCStatusConn(t *testing.T, conn net.Conn, hello SupervisorLockOwner, handler func(IPCRequest) IPCResponse) {
	t.Helper()
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	helloRaw, err := json.Marshal(map[string]any{
		"hello": IPCHello{Version: 1, PID: hello.PID, StartedAt: hello.StartedAt},
	})
	if err != nil {
		t.Errorf("marshal hello: %v", err)
		return
	}
	if _, err := conn.Write(append(helloRaw, '\n')); err != nil {
		t.Errorf("write hello: %v", err)
		return
	}

	line, err := readTestIPCLine(conn, 4096)
	if err != nil {
		return
	}
	var req IPCRequest
	if err := json.Unmarshal(line, &req); err != nil {
		t.Errorf("decode request %q: %v", line, err)
		return
	}
	resp := handler(req)
	if resp.ID == 0 {
		resp.ID = req.ID
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Errorf("marshal response: %v", err)
		return
	}
	_, _ = conn.Write(append(raw, '\n'))
}

func readTestIPCLine(conn net.Conn, max int) ([]byte, error) {
	buf := make([]byte, 0, max)
	tmp := make([]byte, 1)
	for len(buf) < max {
		n, err := conn.Read(tmp)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		if tmp[0] == '\n' {
			return buf, nil
		}
		buf = append(buf, tmp[0])
	}
	return nil, errors.New("line too long")
}

type singleReadEOFConn struct {
	net.Conn
	data []byte
	done bool
}

func (c *singleReadEOFConn) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	n := copy(p, c.data)
	c.data = c.data[n:]
	if len(c.data) == 0 {
		c.done = true
		return n, io.EOF
	}
	return n, nil
}
