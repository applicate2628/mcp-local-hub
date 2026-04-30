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
)

// Config is what Run needs to produce the menu and route actions.
// Identical to the previous in-process API so callers don't change.
type Config struct {
	// ActivateWindow is called when the user picks "Open dashboard"
	// from the right-click menu (or left-clicks the icon).
	ActivateWindow func()
	// Quit is called when the user picks "Quit (keep daemons)".
	Quit func()
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
	selfPath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tray: cannot resolve self path (%v); tray disabled\n", err)
		<-ctx.Done()
		return nil
	}

	c := exec.CommandContext(ctx, selfPath, "tray")
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
	c.Stderr = os.Stderr // forward child diagnostics

	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tray: spawn: %v; tray disabled\n", err)
		<-ctx.Done()
		return nil
	}

	// Goroutine: drain cfg.StateCh, write JSON state lines to child's stdin.
	go func() {
		enc := json.NewEncoder(stdin)
		defer stdin.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case state, ok := <-cfg.StateCh:
				if !ok {
					return
				}
				_ = enc.Encode(stateMessage{State: state.String()})
			}
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
		default:
			fmt.Fprintf(os.Stderr, "tray: unknown event %q\n", ev.Event)
		}
	}
	// scanner.Err() may be context.Canceled on parent shutdown — not
	// worth logging. Child process cleanup is managed by exec.CommandContext.
	_ = c.Wait()
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
