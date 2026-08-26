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
	"time"
)

const (
	supervisorIPCHelloMaxBytes  = 4096
	supervisorIPCStatusMaxBytes = 1 << 20
)

// ErrSupervisorIPCUnavailable marks the rollout-transition case where the
// supervisor IPC endpoint is not present, so callers may use the legacy
// scheduler-backed status path instead of treating this as a supervisor fault.
var ErrSupervisorIPCUnavailable = errors.New("supervisor IPC unavailable")

// ErrStatusSetupFailure marks a LOCAL setup/state-file failure inside the
// status dial path — the state directory cannot be resolved, or the supervisor
// owner sidecar (supervisor.lock.owner.json) is PRESENT but unreadable (a
// DACL/mode refusal from readStateFileInodeAnchored on a sandbox-broadened
// %LOCALAPPDATA% or under MCPHUB_REQUIRE_SINGLE_USER_HOME=1, or corrupt bytes) —
// as distinct from a transport-level unavailability (dial / hello handshake /
// send / read). It is the status twin of ErrRespawnSetupFailure
// (supervisor_ipc_respawn_client.go, bot PR #477 P3): a setup failure means NO
// live supervisor is proven, just a broken local state file, so a caller such
// as the GUI startup probe (internal/cli/gui_supervisor_owner.go) must NOT
// classify it as "supervisor up but broken" and refuse to spawn — it must
// surface the local-fault remediation (mcphub repair-state-dacl) instead. It is
// wrapped ALONGSIDE the underlying cause via multi-%w, so errors.Is(err,
// ErrStatusSetupFailure) classifies the failure while the cause stays
// inspectable in the error string. Distinct from ErrSupervisorIPCUnavailable:
// an ABSENT owner sidecar (os.IsNotExist) stays SUPERVISOR_UNAVAILABLE — a
// present-but-unreadable one is this setup fault.
var ErrStatusSetupFailure = errors.New("supervisor IPC status: local setup failure")

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
		// Local setup failure (state dir unresolvable) — wrap the sentinel
		// alongside the cause so the startup probe treats it as a local fault to
		// remediate, not a live-but-broken supervisor (mirrors the respawn twin,
		// bot PR #477 P3).
		return nil, fmt.Errorf("supervisor IPC status: resolve state dir: %w: %w", ErrStatusSetupFailure, err)
	}
	return dialSupervisorIPCStatusFromStateDir(ctx, stateDir)
}

func dialSupervisorIPCStatusFromStateDir(ctx context.Context, stateDir string) ([]DaemonStatus, error) {
	lockPath := filepath.Join(stateDir, "supervisor.lock")
	owner, err := ReadSupervisorLockOwner(lockPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("supervisor IPC status: supervisor.lock.owner.json not found at %s.owner.json: %w: %w",
				lockPath, ErrSupervisorIPCUnavailable, err)
		}
		// The owner sidecar exists (not IsNotExist — that returns the
		// ErrSupervisorIPCUnavailable branch above) but is unreadable/corrupt: a
		// DACL/mode refusal or corrupt bytes. This is a LOCAL setup failure, not
		// a transport outage and not a live-but-broken supervisor — wrap the
		// sentinel so the startup probe surfaces the repair-state-dacl
		// remediation instead of "refusing to spawn duplicate" (bot PR #477 P3
		// respawn-twin shape).
		return nil, fmt.Errorf("supervisor IPC status: read %s.owner.json: %w: %w", lockPath, ErrStatusSetupFailure, err)
	}
	address := SupervisorIPCAddress(stateDir)
	conn, err := dialSupervisorIPC(ctx, address)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("supervisor IPC status: dial %s: %w: %w", address, ErrSupervisorIPCUnavailable, err)
		}
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
	// RAMBytes is the daemon's resident set size (working-set bytes),
	// looked up by the supervisor from the live current_pid for Running
	// daemons (internal/cli/supervise_status.go). Zero/omitted when RAM
	// could not be determined (non-Windows host, port-stale/Idle daemon,
	// PID recycled). Without this field json.Unmarshal would silently
	// drop the ram_bytes the producer emits before it reached the
	// DaemonStatus consumer — mirroring the OrphanPID/StalePID precedent.
	RAMBytes uint64 `json:"ram_bytes,omitempty"`
	// OrphanPID surfaces a Windows post-create orphan PID when the
	// supervisor's best-effort kill failed; operator-visible via
	// `mcphub status --json` and the GUI Dashboard for manual cleanup
	// (`taskkill /F /T /PID <orphan_pid>` on Windows). Zero (omitted
	// in JSON) on the happy path. Closes bot finding on PR #238
	// 044489a (P2 surface-orphan-PID-through-status-clients): without
	// this field, json.Unmarshal would silently drop the orphan_pid
	// emitted by supervisorStatusDaemons before it reached the
	// DaemonStatus consumer.
	OrphanPID int `json:"orphan_pid,omitempty"`
	// StalePID surfaces the wedged PID of a port-stale running daemon the
	// supervisor is terminate-restarting (state "Restarting"); diagnostic
	// via `mcphub status --json`. Zero (omitted) on the happy path. Without
	// this field json.Unmarshal would silently drop the stale_pid emitted
	// by supervisorStatusDaemons before it reached DaemonStatus (deep-sec
	// #268 Reg-F1, mirroring the OrphanPID precedent above).
	StalePID int `json:"stale_pid,omitempty"`
	// JobProtection mirrors SupervisorDaemonState.JobProtection across
	// the IPC boundary. Tri-state via *bool: nil = unknown/legacy/not-
	// yet-probed (no badge), &true = per-spawn Job allocated (orphan-
	// protection invariant holds), &false = NewKillOnCloseJob failed
	// and the supervisor fell through to plain cmd.Start (orphan-
	// protection lost). Operator-visible via `mcphub status --json`
	// and the GUI Dashboard. Closes consultant strategic concern #1
	// on PR #241 (silent-degradation gap when fallback fires).
	JobProtection *bool `json:"job_protection,omitempty"`
	// SpawnHoldReason / SpawnHoldPath mirror SupervisorDaemonState's pre-spawn
	// existence-gate hold across the IPC boundary. REQUIRED, not optional:
	// without these fields json.Unmarshal would silently drop the
	// spawn_hold_reason / spawn_hold_path the producer emits before they reached
	// the DaemonStatus consumer — the same trap the OrphanPID / StalePID /
	// RAMBytes comments above record. That silent drop would break exactly the
	// surface this feature exists for (the GUI telling a non-technical operator
	// the mcphub binary is missing), and it would fail silently rather than
	// loudly. Empty on the happy path.
	SpawnHoldReason      string                      `json:"spawn_hold_reason,omitempty"`
	SpawnHoldPath        string                      `json:"spawn_hold_path,omitempty"`
	ReadinessObservation *ReadinessObservationWireV1 `json:"readiness_observation,omitempty"`
}

func validateSupervisorIPCHello(conn net.Conn, expected SupervisorLockOwner) error {
	line, err := readSupervisorIPCLine(conn, supervisorIPCHelloMaxBytes)
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
	line, err := readSupervisorIPCLine(conn, supervisorIPCStatusMaxBytes)
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
		if n > 0 {
			if tmp[0] == '\n' {
				return buf, nil
			}
			buf = append(buf, tmp[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				return nil, err
			}
			return nil, err
		}
		if n == 0 {
			continue
		}
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
		rows = append(rows, DaemonStatus{
			Server:   d.Server,
			Daemon:   d.Daemon,
			TaskName: d.TaskName,
			// DisplayName: human-readable label via the single owner
			// (ComputeDaemonDisplayName). The supervisor descriptor already
			// carries the workspace ROOT path (d.Workspace) for serena/LSP
			// rows, so the 8-hex hash never needs reversing. Empty for global
			// daemons → CLI/GUI fall back to the plain task/server name.
			DisplayName:   ComputeDaemonDisplayName(d.TaskName, d.Server, d.Daemon, d.Workspace),
			State:         normalizeSupervisorIPCStatusState(d.State),
			Port:          d.Port,
			PID:           d.CurrentPID,
			OrphanPID:     d.OrphanPID,
			StalePID:      d.StalePID,
			JobProtection: d.JobProtection,
			// Pre-spawn existence-gate hold (P1.1) — carried through so the GUI
			// Dashboard and `mcphub status --json` can name the absent path and
			// the remedy instead of showing an unexplained non-running daemon.
			SpawnHoldReason: d.SpawnHoldReason,
			SpawnHoldPath:   d.SpawnHoldPath,
			Workspace:       d.Workspace,
			// UptimeSec is derived HERE from the supervisor's started_at
			// (the wire carries started_at, not a precomputed uptime_sec).
			// The v0.6 idle sweeper (internal/gui/serena_idle_sweeper.go)
			// uses this as the never-called fallback baseline; without it a
			// never-called serena daemon would carry UptimeSec==0 and never
			// be idle-stopped (FIX-1 — the production IPC path used to drop
			// started_at on the floor, leaving UptimeSec zero in production
			// while tests injected it). Uses time.Now() as the evaluation
			// clock — the same wall clock the sweeper's last-activity uses.
			UptimeSec: supervisorIPCUptimeSec(d.StartedAt, time.Now()),
			// RAMBytes is carried straight through from the supervisor's
			// live per-pid lookup (the wire field). Zero means "unknown" —
			// the GUI Dashboard omits the RAM row in that case.
			RAMBytes:             d.RAMBytes,
			IsMaintenance:        d.IsMaintenance,
			ReadinessObservation: d.ReadinessObservation.Decode(),
		})
	}
	return rows, nil
}

// supervisorIPCUptimeSec converts the supervisor's RFC3339(Nano) started_at
// wire field into an uptime-in-seconds relative to `now`. It returns 0 (treated
// downstream as "unknown / just-spawned") for an empty/unparseable started_at or
// a future-dated start (clock skew) so a degenerate value never inflates uptime
// and tricks the idle sweeper into killing a fresh daemon. The supervisor emits
// started_at via daemonRuntimeStartedAt (internal/cli/supervise_status.go) in
// time.RFC3339Nano; RFC3339Nano parses both the nano and the second-granularity
// form, so either producer formatting round-trips.
func supervisorIPCUptimeSec(startedAt string, now time.Time) int64 {
	if startedAt == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return 0
	}
	d := now.Sub(t)
	if d <= 0 {
		// Future-dated start (clock skew) or exactly-now: not yet idle by any
		// positive measure. Treat as just-spawned (0) rather than negative.
		return 0
	}
	return int64(d / time.Second)
}

// normalizeSupervisorIPCStatusState is a thin delegate over the canonical
// daemon-state classifier (daemon_state.go::ProjectIPCStatusState). The
// vocabulary — including the now-enumerated Quarantined case that closes the
// latent fail-quiet trap — lives in one place; this function name is kept so
// its caller (decodeSupervisorIPCStatusResult) is unchanged.
func normalizeSupervisorIPCStatusState(state string) string {
	return ProjectIPCStatusState(state)
}
