package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	if isSupervisorReachable(ctx, 2*time.Second) {
		return &supervisorOwner{spawned: false, stateDir: stateDir}, nil
	}

	// Spawn a fresh supervisor. context.Background here is intentional:
	// CommandContext would kill the supervisor when ctx is canceled,
	// but our ownership contract is that Stop() drives shutdown via
	// IPC, not parent context. The caller-side ctx is only used to
	// bound the readiness wait below.
	//
	// strictMode threads through verbatim — the GUI's autostart entry
	// may have been installed with --strict-mode (corp-managed Windows
	// hosts), and the spawned supervisor MUST inherit it so the
	// hardened state-file write posture is preserved end-to-end.
	args := []string{"supervise"}
	if strictMode {
		args = append(args, "--strict-mode")
	}
	cmd := exec.Command(mcphubBin, args...)
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	configureSupervisorDetach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("supervisor owner: spawn %q: %w", mcphubBin, err)
	}
	proc := cmd.Process

	deadline := time.Now().Add(waitFor)
	for {
		// Probe IPC reachability. A bare "no supervisor IPC" error
		// (ErrSupervisorIPCUnavailable) is the expected pre-bind state —
		// keep polling. Any other error (handshake mismatch, decode
		// failure) is propagated immediately because it means the
		// supervisor IS running but in a broken state.
		if isSupervisorReachable(ctx, 500*time.Millisecond) {
			return &supervisorOwner{spawned: true, proc: proc, stateDir: stateDir}, nil
		}
		if time.Now().After(deadline) {
			// Timeout: kill the spawned process so we don't leak
			// a half-started supervisor.
			_ = proc.Kill()
			_, _ = proc.Wait()
			return nil, fmt.Errorf("supervisor owner: spawned PID %d but IPC not reachable within %s", proc.Pid, waitFor)
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
	// Send the graceful exit. If the supervisor doesn't acknowledge
	// (process already dead, IPC unreachable, etc), proceed to the
	// force-kill fallback rather than blocking on a dead pipe.
	ipcErr := api.DialSupervisorIPCExit(ctx, graceTimeoutMs)

	// Wait for the process to exit. Wait() in a goroutine so we can
	// time out independently and force-kill if needed.
	exited := make(chan error, 1)
	go func() {
		_, err := s.proc.Wait()
		exited <- err
	}()

	total := time.Duration(graceTimeoutMs)*time.Millisecond + supervisorForceKillFallbackWindow
	select {
	case <-exited:
		// Process exited within the grace window. If the IPC layer
		// itself failed (e.g. handshake mismatch) but the process is
		// gone anyway, surface the IPC failure for diagnosability;
		// otherwise return nil.
		if ipcErr != nil && !errors.Is(ipcErr, api.ErrSupervisorIPCUnavailable) {
			return fmt.Errorf("supervisor owner: graceful exit IPC failed but process exited: %w", ipcErr)
		}
		return nil
	case <-time.After(total):
		// Force-kill — supervisor didn't exit in time.
		_ = s.proc.Kill()
		<-exited
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

// isSupervisorReachable performs a single IPC handshake + status query
// against the running supervisor. Returns true iff the supervisor is
// alive AND responds. False on any failure — including the expected
// pre-bind "supervisor IPC unavailable" state.
func isSupervisorReachable(ctx context.Context, probeTimeout time.Duration) bool {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	_, err := api.DialSupervisorIPCStatus(probeCtx)
	return err == nil
}

// resolveMCPHubBinary returns the absolute path to the currently-
// running mcphub binary. Used by the GUI to spawn its supervisor
// child with the same code as the running process — critical for dev
// builds where the binary path is not on PATH and for guaranteeing
// version consistency between GUI and supervisor.
func resolveMCPHubBinary() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve mcphub binary: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return exe, nil // best-effort; caller will spawn with non-abs
	}
	return abs, nil
}
