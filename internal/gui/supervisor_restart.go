// internal/gui/supervisor_restart.go
//
// POST /api/supervisor/restart — Dashboard-accessible recovery
// affordance when the supervisor IPC has gone silent. Reads the
// supervisor.lock owner sidecar to find the current supervisor PID,
// terminates it (graceful taskkill → SIGKILL fallback), and spawns a
// new detached `mcphub.exe supervise` process.
//
// Why this lives in the GUI server (vs. only in the CLI):
//
//   - When IPC dies but the supervisor process stays alive (the
//     2026-05-19 22:52:17 post-mortem: accept-loop early-exit on a
//     transient hello-write race), the GUI's Dashboard rendered
//     "Failed to load: i/o timeout" with no recovery affordance.
//     The operator had to open PowerShell and Stop-Process the
//     dead supervisor manually before re-running `mcphub supervise`.
//
//   - With this handler wired to a Dashboard banner button, a single
//     click does both: kill the stuck supervisor + spawn fresh one.
//     The frontend polls /api/status after the POST returns to
//     surface daemon recovery in real time.
//
// Best-effort throughout: every step is non-fatal, the response
// reports outcome per step so the operator can fall back to manual
// shell commands when automation refuses. Mirrors the
// gui/force_kill.go non-destructive defaults — the operator must
// explicitly hit Restart Supervisor, never the auto-recovery path.

package gui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorRestartResponse reports each step's outcome so the
// frontend can render a per-step status list. Empty `error` means
// the step succeeded (or was skipped because the prior step already
// produced the desired state).
type supervisorRestartResponse struct {
	// KilledPID is the supervisor PID that was terminated, or 0 if
	// no PID was found / nothing was killed.
	KilledPID int `json:"killed_pid"`
	// Killed reports whether the kill step actually fired.
	Killed bool `json:"killed"`
	// SpawnedPID is the PID of the freshly spawned supervisor child,
	// or 0 if spawn failed.
	SpawnedPID int `json:"spawned_pid"`
	// Spawned reports whether the new supervisor process started OK.
	Spawned bool `json:"spawned"`
	// PerStepError carries per-step diagnostic strings. Keys:
	// "read_lock", "kill", "spawn". Absent keys = step succeeded.
	PerStepError map[string]string `json:"per_step_error,omitempty"`
}

func registerSupervisorRestartRoutes(s *Server) {
	s.mux.HandleFunc("/api/supervisor/restart", s.requireSameOrigin(s.supervisorRestartHandler))
}

func (s *Server) supervisorRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := supervisorRestartResponse{
		PerStepError: map[string]string{},
	}

	stateDir, err := api.DaemonStateDir()
	if err != nil {
		writeAPIError(w, fmt.Errorf("resolve state dir: %w", err), http.StatusInternalServerError, "STATE_DIR_FAILED")
		return
	}

	// Step 1: Read supervisor.lock.owner.json to find the current
	// supervisor PID. Sidecar absent = no live supervisor; skip kill.
	pid, lockErr := readSupervisorLockOwnerPID(stateDir)
	if lockErr != nil {
		resp.PerStepError["read_lock"] = lockErr.Error()
		// Not fatal — we still try to spawn a fresh supervisor.
	}
	resp.KilledPID = pid

	// Step 2: Kill if a PID was found. taskkill /PID <n> /F on
	// Windows; SIGKILL on POSIX. Best-effort: if the kill fails the
	// new spawn step might still recover (the supervisor's own
	// lock-acquire path detects stale lock owners by PID liveness).
	if pid > 0 {
		if killErr := killSupervisorProcess(pid); killErr != nil {
			resp.PerStepError["kill"] = killErr.Error()
		} else {
			resp.Killed = true
			// Allow a brief window for the lock file to be released
			// and the OS to reap the process handle. Without this,
			// the new spawn may race the dying process's lock and
			// fail with "supervisor.lock held but owner metadata
			// stale or unobservable".
			waitForLockRelease(stateDir, 3*time.Second)
		}
	}

	// Step 3: Spawn a fresh supervisor process. Detached so it
	// outlives the GUI handler that started it (and outlives the
	// GUI itself if the operator restarts the GUI separately).
	spawned, spawnErr := spawnDetachedSupervisor()
	if spawnErr != nil {
		resp.PerStepError["spawn"] = spawnErr.Error()
	} else {
		resp.SpawnedPID = spawned
		resp.Spawned = true
	}

	_ = api.LogHubMcpEvent("info", "supervisor-restart-via-gui", map[string]any{
		"killed_pid":  resp.KilledPID,
		"killed":      resp.Killed,
		"spawned_pid": resp.SpawnedPID,
		"spawned":     resp.Spawned,
		"step_errors": resp.PerStepError,
	})

	w.Header().Set("Content-Type", "application/json")
	// Always return 200 — per-step status lives in the body. Surfacing
	// non-2xx on partial success would lose the diagnostic that says
	// "kill succeeded but spawn failed because binary not on PATH" or
	// vice-versa.
	_ = json.NewEncoder(w).Encode(resp)
}

// readSupervisorLockOwnerPID reads <state-dir>/supervisor.lock.owner.json
// and returns the recorded PID. Empty/absent sidecar → (0, nil) so
// the caller can skip the kill step without erroring.
func readSupervisorLockOwnerPID(stateDir string) (int, error) {
	path := filepath.Join(stateDir, "supervisor.lock.owner.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	var sidecar struct {
		PID int `json:"pid"`
	}
	if jErr := json.Unmarshal(raw, &sidecar); jErr != nil {
		return 0, fmt.Errorf("parse %s: %w", path, jErr)
	}
	return sidecar.PID, nil
}

// killSupervisorProcess terminates the supervisor process by PID.
// Windows: taskkill /PID <n> /F (force; no graceful path because the
// supervisor's IPC may already be wedged — the post-mortem case).
// POSIX: SIGKILL via os.Process.Kill().
func killSupervisorProcess(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	if runtime.GOOS == "windows" {
		cmd := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/F")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("taskkill: %w (output: %s)", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find proc %d: %w", pid, err)
	}
	return proc.Kill()
}

// waitForLockRelease polls supervisor.lock.owner.json until it
// disappears OR the deadline expires. Used after a kill to let the
// OS finish reaping the supervisor process and releasing its file
// handles before the new spawn tries to grab the lock.
func waitForLockRelease(stateDir string, deadline time.Duration) {
	path := filepath.Join(stateDir, "supervisor.lock.owner.json")
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// spawnDetachedSupervisor starts `<this-binary> supervise` as a
// detached child that outlives the current GUI process. Returns the
// child PID on success.
//
// The new supervisor inherits MCPHUB_ALLOW_UNHARDENED_STATE_READ +
// other env vars from the current process — important for hosts
// whose %LOCALAPPDATA% has an AD-pushed Domain Users ACE and need
// the relax-lane to start at all.
func spawnDetachedSupervisor() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("os.Executable: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(exe); lerr == nil {
		exe = resolved
	}
	cmd := exec.Command(exe, "supervise")
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	configureDetached(cmd) // platform-specific (see _windows.go / _other.go)
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("start supervisor: %w", err)
	}
	// Release the OS-side handle so the child outlives this process.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
