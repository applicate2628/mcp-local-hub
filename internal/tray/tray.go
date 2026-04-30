// Package tray runs the system tray icon as a SEPARATE OS PROCESS
// to guarantee the tray menu remains responsive even when the main
// GUI process is busy, blocked, or its message pump is starved.
//
// Architecture (PR #25):
//
//	┌──────────────────┐  child stdin (state JSON lines)   ┌───────────────────┐
//	│  mcphub gui      │ ────────────────────────────────► │  mcphub tray      │
//	│  (main process)  │                                   │  (child process)  │
//	│                  │  child stdout (event JSON lines)  │  - direct Win32   │
//	│  - HTTP server   │ ◄──────────────────────────────── │  - own message    │
//	│  - status poller │                                   │    pump, no       │
//	│  - this Run()    │                                   │    network I/O    │
//	│    spawns child  │                                   │                   │
//	└──────────────────┘                                   └───────────────────┘
//
// Why a subprocess: previous in-process designs (getlantern/systray
// then fyne.io/systray) suffered from "right-click menu doesn't
// appear after external foreground change" symptoms because (a) the
// click handler made an HTTP self-call to /api/activate-window
// (latency on the menu thread), and (b) Win32 TrackPopupMenu
// requires the calling thread to be the foreground thread — any
// foreground-grab from another process (e.g. Chrome tab tear-off)
// could silently disable the popup. Moving the tray to its own
// process puts the menu's message pump on a fresh thread that owns
// no shared state and never makes HTTP calls.
//
// The child implements the tray via direct Win32 syscalls (user32
// + shell32) — no CGo and no third-party tray library. The public
// API is platform-agnostic; on non-Windows hosts the child is a
// no-op (tray is Windows-only per spec §2.2).
package tray

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"mcp-local-hub/internal/process"
)

// Config is what Run needs to produce the menu and route actions.
// Identical to the previous in-process API so callers don't change.
type Config struct {
	// ActivateWindow is called when the user picks "Open dashboard"
	// from the right-click menu (or left-clicks the icon).
	ActivateWindow func()
	// Quit is called when the user picks "Quit (keep daemons)".
	Quit func()
	// QuitAndStopAll is called when the user picks "Quit and stop all
	// daemons" from the tray menu. Implementer should stop every
	// running daemon (api.StopAll) and then trigger the same GUI
	// shutdown path as Quit. Optional: if nil, the menu falls back
	// to plain Quit semantics so the menu item never silently no-ops.
	QuitAndStopAll func()
	// RunAllDaemons is called when the user picks "Run all daemons"
	// from the tray menu. Implementer should call api.RestartAll —
	// restart of a stopped daemon is functionally a start, so this
	// serves as "Run all" for the user. Fire-and-forget: GUI stays
	// open. Optional: silently no-op if nil.
	RunAllDaemons func()
	// StopAllDaemons is called when the user picks "Stop all daemons"
	// from the tray menu. Implementer should call api.StopAll.
	// Fire-and-forget: GUI stays open. Optional: silently no-op if nil.
	StopAllDaemons func()
	// StateCh delivers TrayState transitions. The parent forwards
	// each value to the child as a JSON state line.
	StateCh <-chan TrayState
}

// Run spawns the tray subprocess and routes events between it and
// the in-process Config callbacks. Returns when ctx is canceled or
// the subprocess exits. On non-Windows hosts (or when subprocess
// spawn fails) it falls back to blocking on ctx.Done() so the
// caller's goroutine still exits cleanly.
//
// The subprocess is the running mcphub binary invoked with the
// hidden `tray` subcommand: `<self> tray`. The child reads JSON
// state lines from its stdin and writes JSON event lines to its
// stdout; see RunChild for the wire format.
func Run(ctx context.Context, cfg Config) error {
	// Tray is Windows-only in MVP — the non-Windows runChildImpl is a
	// no-op (returns immediately on stdin EOF). Spawning a child just
	// to have it exit instantly would (a) violate Run's "block until
	// ctx.Done()" contract for any caller relying on it for lifecycle
	// coordination, and (b) waste a process spawn on every Linux/macOS
	// GUI start. Block on ctx here directly so the goroutine that
	// `mcphub gui` launches stays alive for the GUI's lifetime.
	// Codex bot review on PR #24 P2 (bypass non-Windows spawn).
	if runtime.GOOS != "windows" {
		<-ctx.Done()
		return nil
	}

	selfPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: cannot resolve self path (%v); tray disabled\n", err)
		<-ctx.Done()
		return nil
	}

	// Plain exec.Command (NOT exec.CommandContext): the child needs a
	// graceful shutdown sequence — close stdin → child reads EOF →
	// child's WM_CLOSE handler runs NIM_DELETE before destroying the
	// window — to avoid leaving a ghost tray icon. exec.CommandContext
	// would Process.Kill() on ctx cancel, skipping that path.
	// Codex bot review on PR #24 P2.
	c := exec.Command(selfPath, "tray")
	process.NoConsole(c) // child mcphub.exe is windowsgui too; belt-and-braces
	stdin, err := c.StdinPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: stdin pipe: %v; tray disabled\n", err)
		<-ctx.Done()
		return nil
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: stdout pipe: %v; tray disabled\n", err)
		<-ctx.Done()
		return nil
	}
	// Forward child diagnostics ONLY when the parent's stderr is a
	// valid handle. Windows GUI apps launched without a console (the
	// normal Explorer-launch path) have invalid std handles; passing
	// an invalid *os.File to exec.Cmd would make Start() fail with
	// invalid-handle, disabling the tray entirely. Codex bot review
	// on PR #24 P1 (avoid inheriting invalid stderr).
	if stderrIsValid() {
		c.Stderr = os.Stderr
	}

	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tray: spawn: %v; tray disabled\n", err)
		<-ctx.Done()
		return nil
	}

	// childDone closes when the child exits — used by the watchdog to
	// stop waiting for ctx cancellation if the child died on its own.
	// runCtx is a parent-derived context that we explicitly cancel as
	// soon as scanning completes, so the stdin writer exits immediately
	// instead of one extra encode-to-dead-pipe iteration. Codex bot
	// review on PR #24 P2 (tray.go: stdin-writer race on early child
	// exit).
	childDone := make(chan struct{})
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// wg waits on the stdin-writer goroutine so Run does not return
	// while it is still alive. The watchdog is bounded by ctx anyway.
	var wg sync.WaitGroup
	wg.Add(1)

	// Stdin writer: drain cfg.StateCh, write JSON state lines.
	// Exits when runCtx cancels (parent ctx OR cancelRun on child exit)
	// OR cfg.StateCh closes. Closes stdin on exit so the child reads
	// EOF and runs its graceful shutdown path.
	go func() {
		defer wg.Done()
		enc := json.NewEncoder(stdin)
		defer stdin.Close()
		for {
			select {
			case <-runCtx.Done():
				return
			case state, ok := <-cfg.StateCh:
				if !ok {
					return
				}
				_ = enc.Encode(stateMessage{State: state.String()})
			}
		}
	}()

	// Cancellation watchdog: when parent ctx cancels, close stdin so
	// the child receives EOF and exits gracefully (running NIM_DELETE).
	// If the child doesn't exit within a bounded fallback window,
	// kill it to avoid a hung GUI shutdown. If the child exits on its
	// own first (childDone), the watchdog has nothing to do — exit
	// instead of leaking until parent cancels.
	go func() {
		select {
		case <-ctx.Done():
			_ = stdin.Close() // signal child to exit gracefully
			select {
			case <-childDone:
				// graceful exit — done
			case <-time.After(2 * time.Second):
				if c.Process != nil {
					_ = c.Process.Kill()
				}
			}
		case <-childDone:
			// Child exited on its own; nothing to do.
			return
		}
	}()

	// Read JSON event lines from child's stdout, dispatch via callbacks.
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		var ev eventMessage
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			fmt.Fprintf(os.Stderr, "tray: bad event line %q: %v\n", scanner.Bytes(), err)
			continue
		}
		switch ev.Event {
		case "open-dashboard":
			if cfg.ActivateWindow != nil {
				cfg.ActivateWindow()
			}
		case "quit":
			if cfg.Quit != nil {
				cfg.Quit()
			}
		case "quit-and-stop-all":
			// Fall back to plain Quit if the GUI didn't wire the
			// stronger callback — never silently swallow a user
			// click. The fallback at least closes the GUI; daemons
			// keep running, which mirrors the existing "Quit (keep
			// daemons)" item rather than failing closed.
			switch {
			case cfg.QuitAndStopAll != nil:
				cfg.QuitAndStopAll()
			case cfg.Quit != nil:
				cfg.Quit()
			}
		case "run-all":
			if cfg.RunAllDaemons != nil {
				cfg.RunAllDaemons()
			}
		case "stop-all":
			if cfg.StopAllDaemons != nil {
				cfg.StopAllDaemons()
			}
		default:
			fmt.Fprintf(os.Stderr, "tray: unknown event %q\n", ev.Event)
		}
	}
	// scanner.Err() may be context.Canceled on parent shutdown — not
	// worth logging. Cancel runCtx FIRST so the writer exits without
	// any further encode attempts on the now-dying pipe, then reap and
	// signal childDone for the watchdog.
	cancelRun()
	waitErr := c.Wait()
	close(childDone)
	wg.Wait()
	// If the child exited with a non-zero status AND the parent ctx
	// is still alive, surface the failure so the GUI doesn't silently
	// lose tray functionality. Common failure modes: spawn went OK but
	// the child crashed mid-pump (Win32 shell init error,
	// CreateWindowExW failure, panic). Without surfacing, the GUI runs
	// without a tray and the user gets no signal — Windows desktop
	// builds typically have no visible stderr console to read the
	// child's diagnostics. Codex bot review on PR #24 P2.
	if waitErr != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "tray: subprocess exited unexpectedly: %v\n", waitErr)
		return fmt.Errorf("tray subprocess exited: %w", waitErr)
	}
	return nil
}

// RunChild is the entry point for the tray subprocess. It reads
// JSON state lines from r (stdin) and writes JSON event lines to w
// (stdout). On stdin EOF (parent closed the pipe / parent exited),
// it tears down the systray cleanly and returns.
//
// Wire format:
//
//	parent → child (stdin):  {"state":"healthy"}\n
//	                         {"state":"partial"}\n  ...
//	child → parent (stdout): {"event":"open-dashboard"}\n
//	                         {"event":"quit"}\n
//
// Lines are newline-delimited JSON; any encoding/parsing error on
// either end is logged to stderr but does not kill the connection.
func RunChild(r io.Reader, w io.Writer) error {
	return runChildImpl(r, w)
}

// stateMessage is the wire-format payload parent sends to child on stdin.
type stateMessage struct {
	State string `json:"state"`
}

// eventMessage is the wire-format payload child sends to parent on stdout.
type eventMessage struct {
	Event string `json:"event"`
}

// parseStateLabel maps a String() label back to its TrayState.
// Used by the child to decode state lines from the parent.
func parseStateLabel(label string) (TrayState, bool) {
	switch label {
	case "healthy":
		return StateHealthy, true
	case "partial":
		return StatePartial, true
	case "down":
		return StateDown, true
	case "error":
		return StateError, true
	}
	return StateHealthy, false
}
