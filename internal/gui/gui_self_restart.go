// internal/gui/gui_self_restart.go
//
// POST /api/gui/restart — make the GUI HTTP listener re-exec ITSELF with
// a clean single-instance-lock HANDOFF.
//
// Why this is distinct from POST /api/supervisor/restart:
//
//   - /api/supervisor/restart kills + respawns the SUPERVISOR (and its
//     daemon children). It does NOT touch the GUI HTTP listener.
//   - A GUI-port change (gui_server.port) or a GUI-binary swap is a
//     GUI-LISTENER concern: the running listener is bound to the old
//     port / runs the old binary, and only relaunching the `mcphub gui`
//     PROCESS itself picks up the change. Before this endpoint the only
//     way to do that was to close the tray/window and reopen the app by
//     hand (see SectionGuiServer.tsx restart-required badges).
//
// The handoff problem (why this is delicate — it touches the
// single-instance lock):
//
//   The GUI owns the single-instance flock on <pidport>.lock. A freshly
//   spawned `mcphub gui` child acquires that lock with a SINGLE,
//   non-retrying TryLock() (see acquireSingleInstanceAt). If the parent
//   still holds the lock when the child boots, the child gets
//   ErrSingleInstanceBusy ONCE, handshakes (activate-window) with the
//   dying parent, and EXITS without ever starting a server — leaving NO
//   GUI running once the parent exits. There is no second attempt.
//
//   The safe handoff therefore needs BOTH halves:
//     1. (this file) the parent spawns a detached child carrying the
//        MCPHUB_GUI_SELF_RESTART_HANDOFF=1 env signal, then exits via
//        os.Exit so the OS releases the flock (the same OS-release
//        mechanism `mcphub gui --force --kill` already relies on).
//     2. (internal/cli/gui.go) the child, seeing the handoff env, runs a
//        BOUNDED retry loop around AcquireSingleInstanceAt so it tolerates
//        the brief window in which the parent has not yet exited. Without
//        this the single-shot TryLock would BUSY-out and strand the
//        handoff with no GUI.
//
//   os.Exit (rather than triggering the cobra stop() context) is
//   deliberate: the GUI's normal return path runs a deferred
//   manager.Stop() that tears down the supervisor + its ~14 daemon
//   children. A self-RESTART must NOT kill the fleet — the child adopts
//   the still-live supervisor. os.Exit skips that defer, so the
//   supervisor survives and the flock is released by process death.
//
// Best-effort + seam-driven: the spawn and the process-exit are behind
// package-level function seams (selfRestartSpawnFn / selfRestartExitFn)
// so the handler test never spawns a real process nor exits the test
// binary.

package gui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
)

// SelfRestartHandoffEnv is the env var the parent sets on the spawned
// child to request the bounded lock-acquire retry. The child
// (internal/cli/gui.go) reads it; an absent/empty value means "normal
// single-shot acquire" (the default for every other launch path).
const SelfRestartHandoffEnv = "MCPHUB_GUI_SELF_RESTART_HANDOFF"

// selfRestartExitDelay is the grace window between writing the 200
// response and exiting the process, so the HTTP response is flushed to
// the browser before the listener dies. Kept short — the child's
// bounded acquire retry absorbs the remaining handoff window.
const selfRestartExitDelay = 250 * time.Millisecond

// guiSelfRestartResponse reports the spawn outcome. `spawned` false with
// a non-empty `spawn_error` means the child was NOT launched and the
// parent will NOT exit (so the operator is never left with no GUI).
type guiSelfRestartResponse struct {
	// Spawned reports whether the replacement `mcphub gui` child started.
	Spawned bool `json:"spawned"`
	// SpawnedPID is the replacement child PID, or 0 when spawn failed.
	SpawnedPID int `json:"spawned_pid"`
	// SpawnError carries the spawn failure diagnostic, empty on success.
	SpawnError string `json:"spawn_error,omitempty"`
	// Restarting reports whether the parent is about to exit to hand off
	// the lock. True only when Spawned is true.
	Restarting bool `json:"restarting"`
}

// registerGUISelfRestartRoutes wires POST /api/gui/restart. Same-origin
// guarded exactly like /api/supervisor/restart.
func registerGUISelfRestartRoutes(s *Server) {
	s.mux.HandleFunc("/api/gui/restart", s.requireSameOrigin(s.guiSelfRestartHandler))
}

func (s *Server) guiSelfRestartHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := guiSelfRestartResponse{}

	pid, err := selfRestartSpawnFn()
	if err != nil {
		// Spawn failed — do NOT exit; the operator keeps the running GUI
		// and sees the error, rather than being stranded with no GUI.
		resp.SpawnError = err.Error()
		_ = api.LogHubMcpEvent("error", "gui-self-restart-spawn-failed", map[string]any{
			"error": err.Error(),
		})
		w.Header().Set("Content-Type", "application/json")
		// 200 with spawned:false — the frontend inspects the body
		// (mirrors the /api/supervisor/restart always-200 contract so a
		// partial outcome is never lost behind a bare HTTP status).
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp.Spawned = true
	resp.SpawnedPID = pid
	resp.Restarting = true

	_ = api.LogHubMcpEvent("info", "gui-self-restart-via-gui", map[string]any{
		"spawned_pid": pid,
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)

	// Flush the response, then exit so the OS releases the
	// single-instance flock for the freshly-spawned child to acquire.
	// os.Exit (NOT the cobra stop() context) is intentional: it skips the
	// deferred supervisor manager.Stop() so the daemon fleet survives the
	// GUI re-exec — the child adopts the live supervisor.
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	go func() {
		time.Sleep(selfRestartExitDelay)
		selfRestartExitFn()
	}()
}

// selfRestartSpawnFn / selfRestartExitFn are the production-default
// seams. Tests swap them so the handler never spawns a real process nor
// terminates the test binary.
var (
	selfRestartSpawnFn = spawnSelfRestartGUI
	selfRestartExitFn  = func() { os.Exit(0) }
)

// spawnSelfRestartGUI launches a detached replacement `mcphub gui`
// process that re-runs the CURRENT invocation's arguments (os.Args[1:],
// so the same --port / --no-tray / --strict-mode flags are preserved),
// with the handoff env var added so the child uses the bounded
// lock-acquire retry. Returns the child PID on success.
//
// It reuses the same detach machinery as spawnDetachedSupervisor
// (configureDetached + startDetachedSupervisorTolerant) so the child
// gains the same DETACHED|NEW_GROUP|BREAKAWAY orphan-escape and the
// corp-host ERROR_ACCESS_DENIED flag-stripping fallback.
func spawnSelfRestartGUI() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("os.Executable: %w", err)
	}
	if resolved, lerr := filepath.EvalSymlinks(exe); lerr == nil {
		exe = resolved
	}

	// Re-run the current process's own argv tail so the replacement GUI
	// binds the same port / flags. os.Args[0] is the binary path (we use
	// the symlink-resolved `exe` instead); os.Args[1:] is the cobra
	// command + flags ("gui", "--port", "9125", ...).
	childArgs := append([]string{}, os.Args[1:]...)

	// Carry the parent env forward (LOCALAPPDATA relax-lane vars, E2E
	// seams, etc.) and append the handoff signal so the child retries the
	// lock acquire instead of single-shot TryLock-ing.
	childEnv := append(os.Environ(), SelfRestartHandoffEnv+"=1")

	build := func() *exec.Cmd {
		c := exec.Command(exe, childArgs...)
		c.Env = childEnv
		c.Stdin = nil
		c.Stdout = nil
		c.Stderr = nil
		configureDetached(c) // platform-specific (_windows.go / _other.go)
		return c
	}
	cmd, err := startDetachedSupervisorTolerant(build)
	if err != nil {
		return 0, fmt.Errorf("start replacement gui: %w", err)
	}
	// Reap the OS-side handle so the child outlives this process cleanly.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}
