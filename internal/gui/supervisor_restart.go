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
	// supervisor PID + the start_time it was written for. Sidecar
	// absent = no live supervisor; skip kill. The start_time is the
	// anchor for the kill-target identity gate's PID-recycle defense
	// (Step 2).
	pid, startedAt, lockErr := readSupervisorLockOwner(stateDir)
	if lockErr != nil {
		resp.PerStepError["read_lock"] = lockErr.Error()
		// Not fatal — we still try to spawn a fresh supervisor.
	}
	resp.KilledPID = pid

	// Step 2: Kill if a PID was found, but ONLY after the three-part
	// identity gate proves the recorded PID is still the live mcphub
	// supervisor the sidecar names (image basename + argv 'supervise'
	// + start-time precedes the sidecar's started_at). The owner
	// sidecar is best-effort and SURVIVES a supervisor crash, so its
	// PID can be REUSED by an unrelated OS process; killSupervisorProcess
	// refuses (with a clear error) when the gate fails so a recycled PID
	// is never killed. Best-effort otherwise: if the kill itself fails
	// the new spawn step might still recover (the supervisor's own
	// lock-acquire path detects stale lock owners by PID liveness).
	if pid > 0 {
		if killErr := killSupervisorProcess(pid, startedAt); killErr != nil {
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
	// gui-events.log audit row (deep-review P3 finding): emit ONLY when the
	// mutation actually committed (a fresh supervisor was spawned) so a
	// failed restart attempt never leaves a misleading success record. The
	// per-step kill outcome rides along as non-sensitive identifiers
	// (PIDs); no secret material is ever in scope for this handler.
	if resp.Spawned {
		s.events.PublishOperatorAction("supervisor-restart", api.CurrentOSUser(), map[string]any{
			"killed_pid":  resp.KilledPID,
			"killed":      resp.Killed,
			"spawned_pid": resp.SpawnedPID,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	// Always return 200 — per-step status lives in the body. Surfacing
	// non-2xx on partial success would lose the diagnostic that says
	// "kill succeeded but spawn failed because binary not on PATH" or
	// vice-versa.
	_ = json.NewEncoder(w).Encode(resp)
}

// readSupervisorLockOwner reads <state-dir>/supervisor.lock.owner.json
// and returns the recorded PID + start_time. Empty/absent sidecar →
// (0, "", nil) so the caller can skip the kill step without erroring.
// The start_time (RFC3339Nano UTC, the same value AcquireSupervisorLock
// writes) anchors the kill-target identity gate's PID-recycle defense.
func readSupervisorLockOwner(stateDir string) (int, string, error) {
	path := filepath.Join(stateDir, "supervisor.lock.owner.json")
	raw, err := api.ReadStateFileInodeAnchored(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, "", nil
		}
		return 0, "", fmt.Errorf("read %s: %w", path, err)
	}
	var sidecar struct {
		PID       int    `json:"pid"`
		StartedAt string `json:"started_at"`
	}
	if jErr := json.Unmarshal(raw, &sidecar); jErr != nil {
		return 0, "", fmt.Errorf("parse %s: %w", path, jErr)
	}
	return sidecar.PID, sidecar.StartedAt, nil
}

// killSupervisorProcess terminates the supervisor process by PID, but
// ONLY after the three-part kill-target identity gate proves the
// recorded PID is still the live mcphub supervisor the sidecar names.
// Windows: taskkill /PID <n> /F (force; no graceful path because the
// supervisor's IPC may already be wedged — the post-mortem case).
// POSIX: SIGKILL via os.Process.Kill().
//
// Why the gate: supervisor.lock.owner.json is best-effort and SURVIVES
// a supervisor crash (an OS-killed supervisor never tidies it). If that
// crashed supervisor's PID is later REUSED by an unrelated OS process,
// killing the sidecar PID blindly would `taskkill /F` (or SIGKILL) that
// unrelated process. The reaper ForceKillSupervisor in the upgrade flow
// (internal/cli/install_migration_wiring_windows.go) gates exactly this
// way; this path is the GUI-side equivalent and must not be weaker.
// If the gate refuses, killSupervisorProcess returns a clear error and
// kills NOTHING, so a recycled PID is never terminated.
func killSupervisorProcess(pid int, sidecarStartedAt string) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}
	if eligible, err := supervisorKillTargetEligible(pid, sidecarStartedAt); err != nil {
		// UNPROVABLE: a transient identity-probe error. We cannot prove
		// the recorded PID is the supervisor, so we must NOT kill it.
		return fmt.Errorf("kill-target identity gate could not verify PID %d: %w", pid, err)
	} else if !eligible {
		// PROVEN not the supervisor (recycled / different subcommand /
		// foreign image / gone). Refuse with a clear error so the
		// operator sees why nothing was killed; the per-step body
		// surfaces it. A recycled PID is never killed.
		return fmt.Errorf("kill-target identity gate refused PID %d: recorded supervisor.lock PID is not a live mcphub supervisor (PID recycled, exited, or owned by a different process); refusing to kill", pid)
	}
	return killSupervisorPIDFn(pid)
}

// killSupervisorPIDFn is the seam over the actual force-kill so the
// identity-gate unit test can observe WHICH PID — if any — would be
// killed WITHOUT ever shelling taskkill or signalling a real process
// (the developer runs ~21 live production daemons under their
// supervisor; CLAUDE.md). Production wires it to killSupervisorPID; the
// test swaps it to record the call. It is reached ONLY after
// supervisorKillTargetEligible has proven the target is the supervisor.
var killSupervisorPIDFn = killSupervisorPID

// killSupervisorPID force-kills the (already gate-verified) supervisor
// PID. Windows: taskkill /PID <n> /F. POSIX: SIGKILL via
// os.Process.Kill().
func killSupervisorPID(pid int) error {
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

// supervisorKillTargetEligible is the three-part kill-target identity
// gate. It mirrors the upgrade reaper's supervisorPIDIsLiveMcphubSupervisor
// (internal/cli/install_migration_wiring_windows.go) using the gui
// package's cross-platform processID probe so the SAME defense runs on
// the GUI POST /api/supervisor/restart path.
//
// The three axes:
//
//	Gate 1 (image basename) — mcphub(.exe), via matchBasename (the same
//	  per-OS helper the gui --force --kill gate uses).
//	Gate 2 (argv token)     — argv[1] == "supervise" EXACTLY (token, not
//	  substring). A recycled PID running any other subcommand is refused.
//	Gate 3 (start-time)     — the process start time must PRECEDE (within
//	  a 1s tolerance) the started_at the sidecar recorded. A PID whose
//	  process began AFTER the sidecar write cannot be the supervisor the
//	  sidecar was written for, so it is a reuse — refuse.
//
// Tri-state return:
//
//	(true,  nil) — proven live supervisor → kill.
//	(false, nil) — PROVEN not the supervisor (dead PID, different image,
//	  different subcommand, recycled-after-write, or a missing/unparseable
//	  started_at that cannot anchor Gate 3) → benign refusal, kill nothing.
//	(false, err) — UNPROVABLE: a transient probe error means the gate
//	  cannot prove identity, so it propagates and the caller refuses the
//	  kill rather than guessing.
func supervisorKillTargetEligible(pid int, sidecarStartedAt string) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	id, err := processID(pid)
	if err != nil {
		// Transient / unsupported probe error. Cannot prove identity.
		return false, err
	}
	defer id.Close()
	// A dead PID (Alive=false) is PROVEN gone — nothing to reap.
	if !id.Alive {
		return false, nil
	}
	// Denied=true means the probe could not enumerate image/argv/start
	// (privilege rejected). We cannot prove this is the supervisor →
	// fail closed (refuse), matching the gui --force --kill posture of
	// refusing take-over of a process it cannot identify.
	if id.Denied {
		return false, nil
	}
	// Gate 1: image basename is the mcphub binary.
	if !matchBasename(id.ImagePath) {
		return false, nil
	}
	// Gate 2: argv[1] is EXACTLY "supervise" (token, not substring).
	if !cmdlineIsSupervise(id.Cmdline) {
		return false, nil
	}
	// Gate 3: the process start time precedes the started_at the sidecar
	// recorded. An empty/unparseable started_at cannot anchor this
	// defense → fail closed (refuse).
	recorded, perr := time.Parse(time.RFC3339Nano, strings.TrimSpace(sidecarStartedAt))
	if perr != nil {
		return false, nil
	}
	if !startTimeBeforeMtime(id.StartTime, recorded.UTC(), time.Second) {
		return false, nil
	}
	return true, nil
}

// cmdlineIsSupervise reports whether argv[1] is exactly "supervise".
// The supervisor analog of cmdlineIsGui — keyed on the precise cobra
// subcommand token, not a substring, so a recycled PID running another
// mcphub subcommand (or a foreign process whose argv coincidentally
// contains "supervise") is refused.
func cmdlineIsSupervise(argv []string) bool {
	return len(argv) >= 2 && argv[1] == "supervise"
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
	build := func() *exec.Cmd {
		c := exec.Command(exe, "supervise")
		c.Stdin = nil
		c.Stdout = nil
		c.Stderr = nil
		configureDetached(c) // platform-specific (see _windows.go / _other.go)
		return c
	}
	// §5-follow-up: route the manual-restart spawn through the breakaway-tolerant
	// helper so it gains the same CREATE_BREAKAWAY_FROM_JOB orphan-escape +
	// ERROR_ACCESS_DENIED flagless-retry the automatic (cli) spawn paths got — it
	// is no longer the less-robust path on a locked-down host.
	cmd, err := startDetachedSupervisorTolerant(build)
	if err != nil {
		return 0, fmt.Errorf("start supervisor: %w", err)
	}
	// Release the OS-side handle so the child outlives this process.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
