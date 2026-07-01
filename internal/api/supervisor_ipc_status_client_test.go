package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// A hello-mismatch is a TRANSPORT failure (supervisor up but broken), NOT a
	// local setup failure — it must NOT match ErrStatusSetupFailure, so the GUI
	// startup probe keeps the "existing supervisor IPC broken" wording rather
	// than mis-surfacing the DACL-repair remediation (bot PR #477 P3 status twin).
	if errors.Is(err, ErrStatusSetupFailure) {
		t.Fatalf("hello-mismatch err = %v matched ErrStatusSetupFailure; a transport failure must stay 'supervisor up but broken', not a local setup fault", err)
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

// TestStatusSetupErrorSentinel_ClassifiesAndPreservesCause pins the
// ErrStatusSetupFailure multi-%w wrapping contract (bot PR #477 P3, status
// twin): a wrapped setup error classifies via errors.Is AND still exposes its
// underlying cause, while a plain transport error does NOT match the sentinel.
// This is the invariant the GUI startup probe's setup-fault-vs-transport split
// (internal/cli/gui_supervisor_owner.go) relies on.
func TestStatusSetupErrorSentinel_ClassifiesAndPreservesCause(t *testing.T) {
	cause := errors.New("read supervisor.lock.owner.json: DACL refused")
	setup := fmt.Errorf("supervisor IPC status: read x.owner.json: %w: %w", ErrStatusSetupFailure, cause)
	if !errors.Is(setup, ErrStatusSetupFailure) {
		t.Fatalf("errors.Is(setup, ErrStatusSetupFailure) = false; a setup failure must classify so the startup probe surfaces the DACL-repair remediation")
	}
	if !errors.Is(setup, cause) {
		t.Fatalf("multi-%%w must preserve the underlying cause for diagnosis")
	}
	transport := fmt.Errorf("supervisor IPC status: %w", errors.New("hello mismatch"))
	if errors.Is(transport, ErrStatusSetupFailure) {
		t.Fatalf("a transport failure must NOT match ErrStatusSetupFailure (it stays 'supervisor up but broken', not a local setup fault)")
	}
}

// TestStatusDialCorruptOwnerIsSetupError exercises the REAL owner-read setup
// path (bot PR #477 P3, status twin): a present-but-corrupt supervisor owner
// sidecar makes ReadSupervisorLockOwner return a non-IsNotExist error
// (readStateFileInodeAnchored reads the bytes, then json.Unmarshal fails) — the
// same failure class a DACL/mode refusal produces on a sandbox-broadened
// %LOCALAPPDATA%. dialSupervisorIPCStatusFromStateDir must wrap it with
// ErrStatusSetupFailure so the GUI startup probe surfaces the repair-state-dacl
// remediation instead of mis-diagnosing "existing supervisor IPC broken". An
// ABSENT owner file, by contrast, returns ErrSupervisorIPCUnavailable (covered
// by TestDialSupervisorIPCStatus_NoSupervisor) and is NOT this path.
func TestStatusDialCorruptOwnerIsSetupError(t *testing.T) {
	stateDir := apitest.HardenedTempDir(t)
	// A present-but-unparseable sidecar. Mirrors the corrupt-sidecar seed in
	// supervisor_ipc_respawn_client_test.go / supervisor_lock_test.go.
	ownerPath := filepath.Join(stateDir, "supervisor.lock.owner.json")
	if err := os.WriteFile(ownerPath, []byte(`{"pid":`), 0o600); err != nil {
		t.Fatalf("seed corrupt owner sidecar: %v", err)
	}
	_, err := dialSupervisorIPCStatusFromStateDir(context.Background(), stateDir)
	if err == nil {
		t.Fatalf("expected a setup error on a corrupt owner sidecar, got nil")
	}
	if !errors.Is(err, ErrStatusSetupFailure) {
		t.Fatalf("err = %v, want errors.Is(ErrStatusSetupFailure) — a present-but-unreadable owner sidecar is a local setup failure (repair-state-dacl), not a live-but-broken supervisor", err)
	}
	// It must NOT collapse into the SUPERVISOR_UNAVAILABLE (absent-sidecar)
	// classification — that path is reserved for os.IsNotExist and is what the
	// startup probe treats as "just spawn one".
	if errors.Is(err, ErrSupervisorIPCUnavailable) {
		t.Fatalf("err = %v matched ErrSupervisorIPCUnavailable; a present-but-corrupt sidecar must classify as a setup fault, not absence", err)
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

func TestDecodeSupervisorIPCStatusResult_PreservesEmptyWorkspace(t *testing.T) {
	raw := json.RawMessage(`{"state":"running","daemons":[{"task_name":"\\mcp-local-hub-memory-default","server":"memory","daemon":"default","workspace":"","state":"running","current_pid":4321}]}`)

	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil {
		t.Fatalf("decodeSupervisorIPCStatusResult: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].Workspace != "" {
		t.Fatalf("Workspace = %q, want empty string preserved", rows[0].Workspace)
	}
}

func TestDecodeSupervisorIPCStatusResult_PreservesStalePID(t *testing.T) {
	// deep-sec #268 Reg-F1: the supervisor emits stale_pid for a port-stale
	// running daemon (state "Restarting", current_pid 0). Without the
	// supervisorIPCStatusDaemon.StalePID field, json.Unmarshal silently
	// drops it before it reaches DaemonStatus.
	raw := json.RawMessage(`{"state":"running","daemons":[{"task_name":"\\mcp-local-hub-memory-default","server":"memory","daemon":"default","state":"Restarting","current_pid":0,"stale_pid":22036}]}`)

	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil {
		t.Fatalf("decodeSupervisorIPCStatusResult: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	if rows[0].StalePID != 22036 {
		t.Fatalf("StalePID = %d, want 22036 round-tripped", rows[0].StalePID)
	}
}

// FIX-1 (wire-shape regression guard): the supervisor IPC payload carries
// `started_at` (RFC3339Nano), NOT a precomputed `uptime_sec`. The decoder must
// DERIVE UptimeSec from started_at; the never-called idle-sweeper fallback
// depends on a non-zero UptimeSec for a daemon that has never recorded a
// /serena/mcp tool-call. This fixture deliberately omits uptime_sec entirely so
// a future refactor that drops the started_at derivation (the original bug —
// started_at was decoded but discarded, UptimeSec stayed 0 in production while
// tests injected it) fails here loud.
func TestDecodeSupervisorIPCStatusResult_DerivesUptimeFromStartedAt(t *testing.T) {
	startedAt := time.Now().Add(-90 * time.Minute).UTC().Format(time.RFC3339Nano)
	// NOTE: NO "uptime_sec" key — only started_at, exactly the supervisor wire shape.
	raw := json.RawMessage(`{"state":"running","daemons":[{"task_name":"\\mcp-local-hub-serena-alpha","server":"serena","daemon":"default","workspace":"D:\\proj\\alpha","state":"Running","current_pid":7000,"started_at":"` + startedAt + `"}]}`)

	rows, err := decodeSupervisorIPCStatusResult(raw)
	if err != nil {
		t.Fatalf("decodeSupervisorIPCStatusResult: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows len = %d, want 1", len(rows))
	}
	// ~90 minutes uptime derived from started_at. Allow a generous band for
	// the time.Now() taken inside the decoder vs the one taken here.
	if rows[0].UptimeSec < 89*60 || rows[0].UptimeSec > 91*60 {
		t.Fatalf("UptimeSec = %d, want ~5400 (90m) derived from started_at; the IPC path must populate uptime so the idle sweeper's never-called fallback works in production", rows[0].UptimeSec)
	}
}

// FIX-1: a degenerate/missing/future-dated started_at must NOT inflate uptime
// (which would trick the idle sweeper into killing a fresh daemon). Each maps
// to UptimeSec 0 (downstream "unknown / just-spawned").
func TestSupervisorIPCUptimeSec_DegenerateInputs(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		started string
	}{
		{"empty", ""},
		{"unparseable", "not-a-timestamp"},
		{"future-dated", now.Add(2 * time.Hour).Format(time.RFC3339Nano)},
		{"exactly-now", now.Format(time.RFC3339Nano)},
	}
	for _, c := range cases {
		if got := supervisorIPCUptimeSec(c.started, now); got != 0 {
			t.Errorf("supervisorIPCUptimeSec(%q) = %d, want 0 (degenerate must not inflate uptime)", c.name, got)
		}
	}
	// A valid past start yields a positive second count.
	if got := supervisorIPCUptimeSec(now.Add(-10*time.Minute).Format(time.RFC3339Nano), now); got != 600 {
		t.Fatalf("supervisorIPCUptimeSec(10m ago) = %d, want 600", got)
	}
	// RFC3339 second-granularity form (no nanos) also parses.
	if got := supervisorIPCUptimeSec(now.Add(-5*time.Minute).Format(time.RFC3339), now); got != 300 {
		t.Fatalf("supervisorIPCUptimeSec(RFC3339 5m ago) = %d, want 300", got)
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
