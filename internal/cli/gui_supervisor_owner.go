package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorOwner manages a supervisor process from the GUI side.
//
// Two modes:
//
//   - spawned=true: the GUI process started `mcphub supervise` as a
//     detached child during ensureSupervisorRunning(). GUI owns the
//     shutdown — Stop() will send IPC exit{graceful:true} and wait,
//     then force-kill on grace-window expiry.
//
//   - spawned=false: a supervisor was already running when the GUI
//     started (lock owner sidecar + live IPC handshake). GUI adopts
//     the existing process for status reads only; Stop() is a no-op
//     so the original owner's shutdown contract is preserved.
//
// The owner is thread-safe — Stop() is idempotent and safe to call
// concurrently with passive status reads.
type supervisorOwner struct {
	spawned     bool
	proc        *os.Process
	stateDir    string
	stoppedOnce sync.Once
	stoppedErr  error
}

// supervisorReadyPollInterval bounds how often ensureSupervisorRunning
// re-probes the supervisor IPC pipe while waiting for a freshly-spawned
// supervisor to bind. 200ms is the same cadence sendIPCWithResponse
// uses in the migration install path for similar reachability waits.
const supervisorReadyPollInterval = 200 * time.Millisecond

// ensureSupervisorRunning verifies a supervisor is reachable via IPC.
// If not, it spawns `mcphub supervise` as a detached child (using the
// caller-provided binary path so dev / installed builds work) and
// waits up to `waitFor` for the new process to bind its IPC pipe.
//
// Cross-platform spawn detach lives in
// gui_supervisor_owner_{windows,other}.go via the
// configureSupervisorDetach hook.
//
// On error the spawned process (if any) is killed and reaped so this
// function never leaks zombies. The caller is responsible for calling
// Stop on the returned owner during shutdown.
func ensureSupervisorRunning(ctx context.Context, mcphubBin string, strictMode bool, waitFor time.Duration) (*supervisorOwner, error) {
	stateDir, err := api.DaemonStateDir()
	if err != nil {
		return nil, fmt.Errorf("supervisor owner: resolve state dir: %w", err)
	}
	if ok, probeErr := probeSupervisor(ctx, 2*time.Second); ok {
		return &supervisorOwner{spawned: false, stateDir: stateDir}, nil
	} else if probeErr != nil {
		return nil, fmt.Errorf("supervisor owner: existing supervisor IPC broken (refusing to spawn duplicate): %w", probeErr)
	}

	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	cmd := exec.Command(mcphubBin, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	// Capture stderr to a bounded buffer so a startup crash (corrupt
	// supervisor-intent.json, state-path sanity rejection, internal
	// panic before any audit log row lands) is visible in the
	// readiness-timeout error rather than dropped silently. 4 KiB is
	// generous enough for any Go-style fatal message plus stack-
	// trace prefix, while bounding worst-case memory pressure if the
	// supervisor floods stderr in a panic loop. PR #212 r5 finding 2.
	stderrBuf := newBoundedBuffer(4096)
	cmd.Stderr = stderrBuf
	configureSupervisorDetach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor owner: spawn %q: %w", mcphubBin, err)
	}
	proc := cmd.Process

	deadline := time.Now().Add(waitFor)
	for {
		ok, probeErr := probeSupervisor(ctx, 500*time.Millisecond)
		if ok {
			return &supervisorOwner{spawned: true, proc: proc, stateDir: stateDir}, nil
		}
		if probeErr != nil {
			_ = proc.Kill()
			_, _ = proc.Wait()
			return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC handshake broken: %w", proc.Pid, probeErr)
		}
		if time.Now().After(deadline) {
			_ = proc.Kill()
			_, _ = proc.Wait()
			stderrTail := strings.TrimSpace(stderrBuf.String())
			if stderrTail != "" {
				return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC not reachable within %s; check supervisor-events.log; supervisor stderr tail: %s", proc.Pid, waitFor, stderrTail)
			}
			return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC not reachable within %s; check supervisor-events.log", proc.Pid, waitFor)
		}
		select {
		case <-ctx.Done():
			_ = proc.Kill()
			_, _ = proc.Wait()
			return nil, ctx.Err()
		case <-time.After(supervisorReadyPollInterval):
		}
	}
}

// boundedBuffer is a bytes.Buffer-like sink that drops writes after
// a fixed byte cap. Used to capture supervisor stderr during the
// readiness window without unbounded memory growth. Preserves the
// FIRST `cap` bytes (what an operator actually needs for diagnosis
// — Go-style fatal messages put the actionable content at the
// start; later panic-loop output is redundant).
type boundedBuffer struct {
	cap int
	buf []byte
}

func newBoundedBuffer(cap int) *boundedBuffer { return &boundedBuffer{cap: cap} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(b.buf) >= b.cap {
		return len(p), nil
	}
	room := b.cap - len(b.buf)
	if room > len(p) {
		room = len(p)
	}
	b.buf = append(b.buf, p[:room]...)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return string(b.buf) }

// Stop sends graceful exit IPC to a spawned supervisor and waits for
// it to terminate. For adopted supervisors (spawned=false) this is a
// no-op.
//
// Idempotent — sync.Once guards repeated calls; the first call's
// error (if any) is remembered and returned for subsequent calls.
//
// graceTimeoutMs is the IPC-layer drain budget that gets forwarded to
// the supervisor's quiesce path. The total wall-time spent in Stop is
// at most graceTimeoutMs + supervisorForceKillFallbackWindow.
func (s *supervisorOwner) Stop(ctx context.Context, graceTimeoutMs int) error {
	if s == nil {
		return nil
	}
	s.stoppedOnce.Do(func() {
		s.stoppedErr = s.stop(ctx, graceTimeoutMs)
	})
	return s.stoppedErr
}

// supervisorForceKillFallbackWindow is the additional time we wait
// for the supervisor process to exit after the graceful-exit IPC has
// been acknowledged. Picked to match the install_upgrade.go force-
// kill grace (5s) but conservatively bumped for slow-host systems
// where daemon teardown can stretch the supervisor's own exit path.
const supervisorForceKillFallbackWindow = 7 * time.Second

func (s *supervisorOwner) stop(ctx context.Context, graceTimeoutMs int) error {
	if !s.spawned {
		return nil
	}
	if s.proc == nil {
		return errors.New("supervisor owner: spawned=true but no Process handle recorded")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	// Start the process-exit waiter FIRST so the total wall-time budget
	// begins ticking from the same instant the IPC request starts. The
	// previous serial pattern (IPC first, then time.After) let the IPC
	// call eat up to the parent ctx deadline (typically 30s) BEFORE the
	// graceful-exit timer began — pushing total Stop wall-time from the
	// documented graceTimeoutMs+supervisorForceKillFallbackWindow (~12s
	// at default settings) to ~42s on a hung supervisor.
	exited := make(chan error, 1)
	go func() {
		_, err := s.proc.Wait()
		exited <- err
	}()

	// Issue the IPC exit concurrently under a sub-deadline derived
	// from graceTimeoutMs only — never the full parent ctx deadline.
	// This bounds total Stop wall-time to graceTimeoutMs + fallback
	// even when the supervisor's IPC pipe is hung or the supervisor
	// process is unresponsive.
	ipcDone := make(chan error, 1)
	go func() {
		ipcCtx, cancel := context.WithTimeout(ctx, time.Duration(graceTimeoutMs)*time.Millisecond)
		defer cancel()
		ipcDone <- api.DialSupervisorIPCExit(ipcCtx, graceTimeoutMs)
	}()

	total := time.Duration(graceTimeoutMs)*time.Millisecond + supervisorForceKillFallbackWindow
	select {
	case <-exited:
		// Process exited within the grace window. Drain the IPC
		// result for diagnosability: if the IPC layer failed (e.g.
		// handshake mismatch) but the process is gone anyway, surface
		// the IPC failure. A pending IPC call against an already-dead
		// supervisor returns within the sub-deadline established
		// above, so this drain is bounded.
		ipcErr := <-ipcDone
		if ipcErr != nil && !errors.Is(ipcErr, api.ErrSupervisorIPCUnavailable) {
			return fmt.Errorf("supervisor owner: graceful exit IPC failed but process exited: %w", ipcErr)
		}
		return nil
	case <-time.After(total):
		// Force-kill — supervisor didn't exit in time.
		killErr := s.proc.Kill()
		// Wait for the process exit with a backstop timer. If Kill
		// failed (Windows ACCESS_DENIED on protected process, POSIX
		// EPERM, kernel-level immortal-process bug), the supervisor
		// is still alive and proc.Wait blocks forever. The 2s
		// backstop unblocks Stop() so shutdown stays responsive and
		// the operator sees an actionable error rather than a hung
		// tray-close. PR #212 r5 silent-failure-hunt finding 3.
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
			if killErr != nil {
				return fmt.Errorf("supervisor owner: graceful exit timeout after %s; force-kill failed for PID %d: %v (process may still be running)",
					total, s.proc.Pid, killErr)
			}
			return fmt.Errorf("supervisor owner: graceful exit timeout after %s; force-kill issued for PID %d but process did not exit within 2s (may be unkillable)",
				total, s.proc.Pid)
		}
		// Best-effort drain of the IPC result. The IPC goroutine's
		// ipcCtx times out at graceTimeoutMs so it doesn't leak.
		var ipcErr error
		select {
		case ipcErr = <-ipcDone:
		case <-time.After(100 * time.Millisecond):
		}
		if ipcErr != nil {
			return fmt.Errorf("supervisor owner: graceful exit timeout after %s (IPC error: %v), force-killed PID %d",
				total, ipcErr, s.proc.Pid)
		}
		return fmt.Errorf("supervisor owner: graceful exit timeout after %s, force-killed PID %d",
			total, s.proc.Pid)
	}
}

// Spawned reports whether GUI owns the supervisor lifecycle. The tray
// + diagnostics paths surface this to operators so an adopted-mode
// session is visibly distinct from a GUI-owned session.
func (s *supervisorOwner) Spawned() bool {
	if s == nil {
		return false
	}
	return s.spawned
}

// Pid returns the supervisor process's PID. Returns 0 for adopted
// mode (we don't track the PID in that case — caller can read it
// from supervisor.lock.owner.json if needed).
func (s *supervisorOwner) Pid() int {
	if s == nil || s.proc == nil {
		return 0
	}
	return s.proc.Pid
}

// probeSupervisor performs a single IPC handshake + status query
// against the running supervisor. Returns (true, nil) when reachable.
// Returns (false, nil) ONLY for the expected pre-bind state
// (ErrSupervisorIPCUnavailable — no lock owner sidecar, no pipe, or
// connect-refused). Any other failure (hello-version mismatch, JSON
// decode error, ID mismatch) returns (false, error) so the caller
// can distinguish "supervisor not yet up" from "supervisor up but
// broken" and surface the actionable error to the operator.
//
// PR #212 r5 silent-failure-hunt finding 1: the previous bool-only
// signature collapsed handshake failures into "not reachable", which
// made the readiness-wait loop spin for the full 15s timeout and then
// emit a generic "IPC not reachable" message instead of the actual
// hello-mismatch text. Worse, the pre-spawn probe at the top of
// ensureSupervisorRunning collapsed the same way, causing the GUI to
// spawn a SECOND supervisor on top of a hello-broken first one.
func probeSupervisor(ctx context.Context, probeTimeout time.Duration) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if _, err := api.DialSupervisorIPCStatus(probeCtx); err != nil {
		if errors.Is(err, api.ErrSupervisorIPCUnavailable) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// resolveMCPHubBinary returns the absolute, symlink-resolved path to
// the currently-running mcphub binary. Used by the GUI to spawn its
// supervisor child with the same code as the running process —
// critical for dev builds where the binary path is not on PATH and
// for guaranteeing version consistency between GUI and supervisor.
//
// EvalSymlinks closes a TOCTOU window on macOS (preview tier) where
// os.Executable can return a symlink path; an attacker with write
// access to the symlink's parent directory could swap the target
// between resolve and cmd.Start, redirecting the supervisor spawn to
// an attacker-controlled binary. EvalSymlinks resolves to the inode
// path so subsequent spawn cannot be redirected via symlink swap.
// On Linux, /proc/self/exe already returns the resolved path so
// EvalSymlinks is a no-op there; on Windows, the path is canonicalized
// at kernel-load time and symlinks aren't part of the exe-resolution
// chain. PR #212 r3 security finding 2.
func resolveMCPHubBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve mcphub binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe, nil // best-effort; caller will spawn with non-abs
	}
	return abs, nil
}
