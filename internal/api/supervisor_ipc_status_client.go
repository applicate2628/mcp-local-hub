package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DialSupervisorIPCStatus is the production IPC client used by the GUI via
// SupervisorIPCStatusFn. It reads supervisor.lock.owner.json, validates the
// supervisor hello frame against that owner, sends a status request, and
// converts the supervisor daemon payload into []DaemonStatus. Any failure is
// returned as a non-nil error so /api/status keeps the fail-loud
// STATUS_FAILED contract.
func DialSupervisorIPCStatus(ctx context.Context) ([]DaemonStatus, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	stateDir, err := DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("supervisor IPC status: resolve state dir: %w", err)
	}
	return dialSupervisorIPCStatusFromStateDir(ctx, stateDir)
}

func dialSupervisorIPCStatusFromStateDir(ctx context.Context, stateDir string) ([]DaemonStatus, error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("supervisor IPC status: supervisor.lock.owner.json not found at %s.owner.json: %w", lockPath, err)
		}
		return nil, fmt.Errorf("supervisor IPC status: read %s.owner.json: %w", lockPath, err)
	}
	address := SupervisorIPCAddress(stateDir)
	conn, err := dialSupervisorIPC(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("supervisor IPC status: dial %s: %w", address, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if err := validateSupervisorIPCHello(conn, owner); err != nil {
		return nil, fmt.Errorf("supervisor IPC status: %w", err)
	}
	req := IPCRequest{
		Version: 1,
		ID:      time.Now().UnixNano(),
		Cmd:     "status",
	}
	if err := writeSupervisorIPCRequest(conn, req); err != nil {
		return nil, fmt.Errorf("supervisor IPC status: send status: %w", err)
	}
	resp, err := readSupervisorIPCResponse(conn)
	if err != nil {
		return nil, fmt.Errorf("supervisor IPC status: read status response: %w", err)
	}
	if resp.ID != req.ID {
		return nil, fmt.Errorf("supervisor IPC status: response id mismatch: got %d want %d", resp.ID, req.ID)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("supervisor IPC status: %s: %s", resp.Error.Code, resp.Error.Message)
	}
	if !resp.OK {
		return nil, fmt.Errorf("supervisor IPC status: non-OK response without error")
	}
	rows, err := decodeSupervisorIPCStatusResult(resp.Result)
	if err != nil {
		return nil, fmt.Errorf("supervisor IPC status: decode status result: %w", err)
	}
	return rows, nil
}

type supervisorIPCRawResponse struct {
	ID     int64           `json:"id"`
	OK     bool            `json:"ok,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *IPCErr         `json:"error,omitempty"`
	Final  bool            `json:"final,omitempty"`
}

type supervisorIPCStatusResult struct {
	State   string                      `json:"state"`
	Daemons []supervisorIPCStatusDaemon `json:"daemons"`
}

type supervisorIPCStatusDaemon struct {
	TaskName      string   `json:"task_name"`
	Server        string   `json:"server"`
	Daemon        string   `json:"daemon"`
	Command       string   `json:"command"`
	Args          []string `json:"args"`
	Workspace     string   `json:"workspace"`
	Port          int      `json:"port"`
	State         string   `json:"state"`
	CurrentPID    int      `json:"current_pid"`
	StartedAt     string   `json:"started_at"`
	IsMaintenance bool     `json:"is_maintenance"`
}

func validateSupervisorIPCHello(conn net.Conn, expected SupervisorLockOwner) error {
	line, err := readSupervisorIPCLine(conn, 4096)
	if err != nil {
		return fmt.Errorf("read hello: %w", err)
	}
	var env struct {
		Hello IPCHello `json:"hello"`
	}
	if err := json.Unmarshal(line, &env); err != nil {
		return fmt.Errorf("decode hello: %w (raw=%q)", err, string(line))
	}
	if env.Hello.Version != 1 {
		return fmt.Errorf("unsupported hello version %d", env.Hello.Version)
	}
	if !ValidateHandshake(env.Hello, expected) {
		return fmt.Errorf("hello mismatch: got pid=%d started_at=%s expected pid=%d started_at=%s",
			env.Hello.PID, env.Hello.StartedAt, expected.PID, expected.StartedAt)
	}
	return nil
}

func writeSupervisorIPCRequest(conn net.Conn, req IPCRequest) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	_, err = conn.Write(append(raw, '\n'))
	return err
}

func readSupervisorIPCResponse(conn net.Conn) (supervisorIPCRawResponse, error) {
	line, err := readSupervisorIPCLine(conn, 16384)
	if err != nil {
		return supervisorIPCRawResponse{}, err
	}
	var resp supervisorIPCRawResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		return supervisorIPCRawResponse{}, fmt.Errorf("decode response: %w (raw=%q)", err, string(line))
	}
	return resp, nil
}

func readSupervisorIPCLine(conn net.Conn, max int) ([]byte, error) {
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
	return nil, fmt.Errorf("frame exceeded %d bytes", max)
}

func decodeSupervisorIPCStatusResult(raw json.RawMessage) ([]DaemonStatus, error) {
	var result supervisorIPCStatusResult
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing result")
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	rows := make([]DaemonStatus, 0, len(result.Daemons))
	for _, d := range result.Daemons {
		workspace := d.Workspace
		if workspace == "" {
			workspace = d.Daemon
		}
		rows = append(rows, DaemonStatus{
			Server:        d.Server,
			Daemon:        d.Daemon,
			TaskName:      d.TaskName,
			State:         normalizeSupervisorIPCStatusState(d.State),
			Port:          d.Port,
			PID:           d.CurrentPID,
			Workspace:     workspace,
			IsMaintenance: d.IsMaintenance,
		})
	}
	return rows, nil
}

func normalizeSupervisorIPCStatusState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running":
		return "Running"
	case "idle", "stopped":
		return "Stopped"
	case "backoff", "backoff-waiting", "restarting":
		return "Restarting"
	default:
		return state
	}
}
