package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
)

// fakeIPCConn is a minimal net.Conn that captures all bytes written to
// it so tests can decode the IPCResponse the handler emitted. Read /
// Close / deadlines are no-ops; the handler only calls Write.
type fakeIPCConn struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func newFakeIPCConn() *fakeIPCConn                { return &fakeIPCConn{} }
func (c *fakeIPCConn) Read(b []byte) (int, error) { return 0, errors.New("no read in test") }
func (c *fakeIPCConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(b)
}
func (c *fakeIPCConn) Close() error                       { return nil }
func (c *fakeIPCConn) LocalAddr() net.Addr                { return fakeAddr{} }
func (c *fakeIPCConn) RemoteAddr() net.Addr               { return fakeAddr{} }
func (c *fakeIPCConn) SetDeadline(t time.Time) error      { return nil }
func (c *fakeIPCConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *fakeIPCConn) SetWriteDeadline(t time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// lastResponse decodes the last (or only) IPC response frame from the
// fake-conn buffer. Frames are newline-delimited JSON; the helper takes
// the LAST non-empty line so single-frame responses return their final
// frame even when a multi-frame handler accumulates earlier acks.
func (c *fakeIPCConn) lastResponse(t *testing.T) api.IPCResponse {
	t.Helper()
	c.mu.Lock()
	raw := c.buf.Bytes()
	c.mu.Unlock()
	lines := bytes.Split(raw, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		var resp api.IPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("decode IPC response frame %q: %v", line, err)
		}
		return resp
	}
	t.Fatalf("no IPC response frames written; buffer=%q", raw)
	return api.IPCResponse{}
}

func readEventLogForTest(t *testing.T, stateDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(stateDir, "supervisor-events.log"))
	if err != nil {
		return ""
	}
	return string(b)
}

// hydrateTrackerState seeds a single daemon's state into the tracker.
// Avoids the lack of a public MarkX setter for "quarantine" by going
// through HydrateFromState, which is the supported API for restoring
// arbitrary tracker state during startup hydration.
func hydrateTrackerState(t *testing.T, tracker *DaemonRuntimeTracker, taskName, state string) {
	t.Helper()
	tracker.HydrateFromState(&api.SupervisorStateFile{
		Version: 1,
		Daemons: map[string]api.SupervisorDaemonState{
			taskName: {State: state, CurrentPID: 1234},
		},
	})
}

// newRespawnTestDeps constructs an ipcDispatchDeps suitable for
// unit-testing handleRespawn. spawn/terminate are atomic-counter fakes
// so the test can assert call counts.
func newRespawnTestDeps(t *testing.T, intent *api.SupervisorIntentFile) (ipcDispatchDeps, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	tmpHome := t.TempDir()
	events, err := api.OpenSupervisorEventLog(filepath.Join(tmpHome, "supervisor-events.log"))
	if err != nil {
		t.Fatalf("OpenSupervisorEventLog: %v", err)
	}
	t.Cleanup(func() { events.Close() })

	var (
		spawnCalls     atomic.Int32
		terminateCalls atomic.Int32
		reconcileReady atomic.Bool
	)
	reconcileReady.Store(true)

	respawnLate := &respawnLateBindings{}
	respawnLate.Set(
		func(d api.SupervisorDaemon) error { spawnCalls.Add(1); return nil },
		func(d api.SupervisorDaemon) error { terminateCalls.Add(1); return nil },
	)

	deps := ipcDispatchDeps{
		stateDir:       tmpHome,
		events:         events,
		runtimeTracker: NewDaemonRuntimeTracker(),
		reconcileReady: &reconcileReady,
		intent:         intent,
		respawnLate:    respawnLate,
	}
	return deps, &spawnCalls, &terminateCalls
}

func TestHandleRespawn_UnknownTaskReturnsUnknownTaskError(t *testing.T) {
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-foo-default`, Server: "foo", Daemon: "default"},
		},
	}
	deps, spawnCalls, terminateCalls := newRespawnTestDeps(t, intent)

	req := api.IPCRequest{
		ID:  1,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": "nonexistent-daemon",
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != ipcErrorUnknownTask {
		t.Fatalf("expected UNKNOWN_TASK; got %+v", resp.Error)
	}
	if !resp.Final {
		t.Fatal("response must be final")
	}
	if spawnCalls.Load() != 0 || terminateCalls.Load() != 0 {
		t.Fatalf("spawn/terminate must NOT fire on unknown task; got spawn=%d term=%d",
			spawnCalls.Load(), terminateCalls.Load())
	}
}

// TestHandleRespawn_LegacyNilSpecSerenaProxyRefused is the bot PR #246 r2 P2-1
// guard. A legacy serena-proxy descriptor (daemon serena-proxy argv) with a nil
// RuntimeSpec — a pre-redesign row — must be REFUSED on the IPC respawn path,
// mirroring the reconcile desired-set exclusion. Spawning it would cmd.Start a
// proxy that fails loud on the nil spec and churns restart backoff/quarantine.
func TestHandleRespawn_LegacyNilSpecSerenaProxyRefused(t *testing.T) {
	taskName := `\mcp-local-hub-serena-deadbeef`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{{
			TaskName:    taskName,
			Server:      "serena",
			Daemon:      "deadbeef",
			Args:        []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\alpha`, "--port", "9121", "--task-name", taskName},
			RuntimeSpec: nil, // pre-redesign / stale row
		}},
	}
	deps, spawnCalls, terminateCalls := newRespawnTestDeps(t, intent)

	req := api.IPCRequest{ID: 90, Cmd: "respawn", Args: map[string]any{"task_name": taskName, "force": false}}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != ipcErrorRespawnFailed {
		t.Fatalf("expected RESPAWN_FAILED for legacy nil-spec serena-proxy; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "runtime_spec") {
		t.Fatalf("refuse message must name the missing runtime_spec; got %q", resp.Error.Message)
	}
	if spawnCalls.Load() != 0 || terminateCalls.Load() != 0 {
		t.Fatalf("legacy nil-spec serena-proxy must NOT spawn/terminate; got spawn=%d term=%d",
			spawnCalls.Load(), terminateCalls.Load())
	}
}

// TestHandleRespawn_SpecBearingSerenaProxyNotRefused is the positive control for
// P2-1: a serena-proxy WITH a RuntimeSpec is NOT caught by the legacy-row guard
// and respawns normally.
func TestHandleRespawn_SpecBearingSerenaProxyNotRefused(t *testing.T) {
	taskName := `\mcp-local-hub-serena-cafef00d`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{{
			TaskName: taskName,
			Server:   "serena",
			Daemon:   "cafef00d",
			Args:     []string{"daemon", "serena-proxy", "--server", "serena", "--workspace", `C:\work\beta`, "--port", "9122", "--task-name", taskName},
			RuntimeSpec: &api.DaemonRuntimeSpec{
				SpecVersion:   api.DaemonRuntimeSpecVersion,
				ChildCommand:  "uvx",
				UpstreamPort:  19122,
				ExternalPort:  9122,
				WorkspacePath: `C:\work\beta`,
			},
		}},
	}
	deps, spawnCalls, _ := newRespawnTestDeps(t, intent)
	hydrateTrackerState(t, deps.runtimeTracker, taskName, "running")

	req := api.IPCRequest{ID: 91, Cmd: "respawn", Args: map[string]any{"task_name": taskName, "force": false}}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}
	resp := conn.lastResponse(t)
	if resp.Error != nil && resp.Error.Code == ipcErrorRespawnFailed && strings.Contains(resp.Error.Message, "runtime_spec") {
		t.Fatalf("spec-bearing serena-proxy must NOT be refused by the legacy guard; got %+v", resp.Error)
	}
	if spawnCalls.Load() != 1 {
		t.Fatalf("spec-bearing serena-proxy must respawn (spawn once); got spawn=%d", spawnCalls.Load())
	}
}

func TestHandleRespawn_QuarantinedRefusedWithoutForce(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	deps, spawnCalls, terminateCalls := newRespawnTestDeps(t, intent)
	hydrateTrackerState(t, deps.runtimeTracker, taskName, "quarantine")

	req := api.IPCRequest{
		ID:  2,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": taskName,
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != ipcErrorRespawnQuarantined {
		t.Fatalf("expected QUARANTINED; got %+v", resp.Error)
	}
	if spawnCalls.Load() != 0 || terminateCalls.Load() != 0 {
		t.Fatalf("spawn/terminate must NOT fire on quarantine refusal; got spawn=%d term=%d",
			spawnCalls.Load(), terminateCalls.Load())
	}

	logBytes := readEventLogForTest(t, deps.stateDir)
	if !strings.Contains(logBytes, "supervisor-respawn-refused-quarantined") {
		t.Fatalf("expected supervisor-respawn-refused-quarantined event; log: %s", logBytes)
	}
}

func TestHandleRespawn_QuarantinedForceProceeds(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	deps, spawnCalls, terminateCalls := newRespawnTestDeps(t, intent)
	hydrateTrackerState(t, deps.runtimeTracker, taskName, "quarantine")

	req := api.IPCRequest{
		ID:  3,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": taskName,
			"force":     true,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if !resp.OK {
		t.Fatalf("expected OK; got %+v", resp)
	}
	if terminateCalls.Load() != 1 {
		t.Fatalf("expected 1 terminate call; got %d", terminateCalls.Load())
	}
	if spawnCalls.Load() != 1 {
		t.Fatalf("expected 1 spawn call; got %d", spawnCalls.Load())
	}

	logBytes := readEventLogForTest(t, deps.stateDir)
	if !strings.Contains(logBytes, "supervisor-respawn-via-gui") {
		t.Fatalf("expected supervisor-respawn-via-gui event; log: %s", logBytes)
	}
}

func TestHandleRespawn_RunningDaemonRespawnsSuccessfully(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	deps, spawnCalls, terminateCalls := newRespawnTestDeps(t, intent)
	hydrateTrackerState(t, deps.runtimeTracker, taskName, "running")

	req := api.IPCRequest{
		ID:  4,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": taskName,
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if !resp.OK || !resp.Final {
		t.Fatalf("expected OK+Final; got %+v", resp)
	}
	if terminateCalls.Load() != 1 || spawnCalls.Load() != 1 {
		t.Fatalf("expected 1 terminate + 1 spawn; got term=%d spawn=%d",
			terminateCalls.Load(), spawnCalls.Load())
	}
}

func TestHandleRespawn_BareFormTaskNameMatchesCanonical(t *testing.T) {
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-foo-default`, Server: "foo", Daemon: "default"},
		},
	}
	deps, spawnCalls, _ := newRespawnTestDeps(t, intent)

	req := api.IPCRequest{
		ID:  5,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": "mcp-local-hub-foo-default",
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if !resp.OK {
		t.Fatalf("bare-form task_name should match canonical intent; got %+v", resp)
	}
	if spawnCalls.Load() != 1 {
		t.Fatalf("expected spawn to fire; got %d", spawnCalls.Load())
	}
}

func TestHandleRespawn_NotReadyReturnsRetryable(t *testing.T) {
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: `\mcp-local-hub-foo-default`, Server: "foo", Daemon: "default"},
		},
	}
	deps, _, _ := newRespawnTestDeps(t, intent)
	deps.respawnLate = &respawnLateBindings{}

	req := api.IPCRequest{
		ID:  6,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": `\mcp-local-hub-foo-default`,
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != ipcErrorRespawnNotReady {
		t.Fatalf("expected RESPAWN_NOT_READY; got %+v", resp.Error)
	}
	if !resp.Error.Retryable {
		t.Fatal("RESPAWN_NOT_READY must be Retryable so clients re-poll after startup")
	}
}

func TestHandleRespawn_MissingTaskNameReturnsInvalidArgs(t *testing.T) {
	deps, _, _ := newRespawnTestDeps(t, &api.SupervisorIntentFile{})

	req := api.IPCRequest{
		ID:   7,
		Cmd:  "respawn",
		Args: map[string]any{},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != "INVALID_ARGS" {
		t.Fatalf("expected INVALID_ARGS; got %+v", resp.Error)
	}
}

func TestHandleRespawn_SpawnFailurePropagates(t *testing.T) {
	taskName := `\mcp-local-hub-foo-default`
	intent := &api.SupervisorIntentFile{
		Daemons: []api.SupervisorDaemon{
			{TaskName: taskName, Server: "foo", Daemon: "default"},
		},
	}
	deps, _, _ := newRespawnTestDeps(t, intent)
	deps.respawnLate.Set(
		func(d api.SupervisorDaemon) error { return errors.New("simulated spawn fail") },
		func(d api.SupervisorDaemon) error { return nil },
	)

	req := api.IPCRequest{
		ID:  8,
		Cmd: "respawn",
		Args: map[string]any{
			"task_name": taskName,
			"force":     false,
		},
	}
	conn := newFakeIPCConn()
	if err := handleRespawn(conn, req, deps); err != nil {
		t.Fatalf("handleRespawn: %v", err)
	}

	resp := conn.lastResponse(t)
	if resp.Error == nil || resp.Error.Code != ipcErrorRespawnFailed {
		t.Fatalf("expected RESPAWN_FAILED; got %+v", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "simulated spawn fail") {
		t.Fatalf("error message should mention root cause; got %q", resp.Error.Message)
	}
}
