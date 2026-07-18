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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	// HandoffID and Generation identify a gate-ON v3 handoff. They are
	// omitted from the retained v1 response.
	HandoffID  string       `json:"handoff_id,omitempty"`
	Generation string       `json:"generation,omitempty"`
	Phase      HandoffPhase `json:"phase,omitempty"`
	// Spawned reports whether the replacement `mcphub gui` child started.
	Spawned bool `json:"spawned"`
	// SpawnedPID is the replacement child PID, or 0 when spawn failed.
	SpawnedPID int `json:"spawned_pid"`
	// SpawnError carries the spawn failure diagnostic, empty on success.
	SpawnError string `json:"spawn_error,omitempty"`
	// Restarting reports whether the parent is about to exit to hand off
	// the lock. True only when Spawned is true.
	Restarting bool `json:"restarting"`
	OldPort    int  `json:"old_port,omitempty"`
	TargetPort int  `json:"target_port,omitempty"`
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
	if s.cfg.RestartV3Enabled {
		s.guiRestartV3Handler(w)
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

func (s *Server) guiRestartV3Handler(w http.ResponseWriter) {
	resp := guiSelfRestartResponse{}
	if s.restartCoordinator == nil {
		resp.SpawnError = "restart v3 parent coordinator is not configured"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	started, err := s.restartCoordinator.Start()
	alreadyInProgress := errors.Is(err, ErrRestartAlreadyInProgress) && started.HandoffID != ""
	if err != nil && !alreadyInProgress {
		resp.SpawnError = err.Error()
		_ = api.LogHubMcpEvent("error", "gui-self-restart-v3-spawn-failed", map[string]any{
			"error": err.Error(),
		})
		w.Header().Set("Content-Type", "application/json")
		// The current frontend reads spawn_error only after fetch has accepted
		// a 2xx response. Keep pre-accept failures on the established 200 body.
		_ = json.NewEncoder(w).Encode(resp)
		return
	}

	resp.HandoffID = started.HandoffID
	resp.Generation = started.Generation
	resp.Phase = started.Phase
	resp.Spawned = true
	resp.SpawnedPID = started.SpawnedPID
	resp.Restarting = true
	resp.OldPort = started.OldPort
	if started.TargetPort != started.OldPort {
		resp.TargetPort = started.TargetPort
	}
	eventType := "gui-self-restart-v3-in-progress"
	if alreadyInProgress {
		eventType = "gui-self-restart-v3-already-in-progress"
	}
	_ = api.LogHubMcpEvent("info", eventType, map[string]any{
		"handoff_id": started.HandoffID, "generation": started.Generation,
		"spawned_pid": started.SpawnedPID, "old_port": started.OldPort, "target_port": started.TargetPort,
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	if !alreadyInProgress {
		// Encode has completed, and Flush was attempted when the writer exposes
		// it. A wrapper without http.Flusher must still open the coordinator's
		// response barrier or same-port handoff can remain quiesced forever.
		started.AcknowledgeResponseFlushed()
	}
}

// selfRestartSpawnFn / selfRestartExitFn are the production-default
// seams. Tests swap them so the handler never spawns a real process nor
// terminates the test binary.
var (
	selfRestartSpawnFn = spawnSelfRestartGUI
	selfRestartExitFn  = func() { os.Exit(0) }
)

// RequestSelfRestartExit crosses the self-restart-specific process boundary.
// The production seam is os.Exit, intentionally bypassing normal-return
// defers such as CLI supervisor manager.Stop.
func RequestSelfRestartExit() { selfRestartExitFn() }

type retainedRestartGUIProcess struct {
	mu          sync.Mutex
	cmd         *exec.Cmd
	waitStarted bool
	detached    bool
	waitDone    chan struct{}
}

func (p *retainedRestartGUIProcess) PID() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

func (p *retainedRestartGUIProcess) TerminateBeforeRelease(ctx context.Context) error {
	if ctx == nil {
		return errors.New("terminate restart child: nil context")
	}
	p.mu.Lock()
	if p.detached {
		p.mu.Unlock()
		return errors.New("terminate restart child: retained handle already detached")
	}
	if !p.waitStarted {
		if p.cmd == nil || p.cmd.Process == nil {
			p.mu.Unlock()
			return errors.New("terminate restart child: no retained process")
		}
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			p.mu.Unlock()
			return fmt.Errorf("terminate restart child: %w", err)
		}
		p.waitStarted = true
		p.waitDone = make(chan struct{})
		cmd := p.cmd
		done := p.waitDone
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
	}
	done := p.waitDone
	p.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *retainedRestartGUIProcess) DetachAtRelease() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.detached || p.waitStarted {
		return nil
	}
	if p.cmd == nil || p.cmd.Process == nil {
		return errors.New("detach restart child: no retained process")
	}
	if err := p.cmd.Process.Release(); err != nil {
		return fmt.Errorf("detach restart child: %w", err)
	}
	p.detached = true
	return nil
}

// SpawnRestartV3GUI starts the parser-rebuilt CLI argv with a structured,
// non-secret handoff descriptor and RETAINS the OS process handle. Unlike the
// v1 helper it does not start cmd.Wait: the coordinator alone may terminate
// and reap the exact authenticated child before lease release, or detach the
// handle without terminating at the release boundary.
func SpawnRestartV3GUI(argv []string, handoff SelfRestartHandoff) (RestartParentChild, error) {
	rawHandoff, err := EncodeSelfRestartHandoff(handoff)
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("os.Executable: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(exe); resolveErr == nil {
		exe = resolved
	}
	childArgs := append([]string(nil), argv...)
	childEnv := replaceEnvironmentValue(os.Environ(), SelfRestartHandoffEnv, rawHandoff)
	build := func() *exec.Cmd {
		cmd := exec.Command(exe, childArgs...)
		cmd.Env = childEnv
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		configureDetached(cmd)
		return cmd
	}
	cmd, err := startDetachedSupervisorTolerant(build)
	if err != nil {
		return nil, fmt.Errorf("start retained replacement gui: %w", err)
	}
	return &retainedRestartGUIProcess{cmd: cmd}, nil
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := strings.ToUpper(key) + "="
	replaced := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), prefix) {
			continue
		}
		replaced = append(replaced, entry)
	}
	return append(replaced, key+"="+value)
}

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
