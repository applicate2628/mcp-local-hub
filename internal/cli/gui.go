// internal/cli/gui.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/gui"
	"mcp-local-hub/internal/tray"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newGuiCmdReal() *cobra.Command {
	var (
		port      int
		noBrowser bool
		noTray    bool
		force     bool
		kill      bool
		yes       bool
	)
	c := &cobra.Command{
		Use:   "gui",
		Short: "Launch the local GUI (browser window + tray icon served by mcphub itself)",
		Long: `mcphub gui starts an HTTP server on 127.0.0.1 that serves a local-only
browser GUI for managing MCP servers. A Windows tray icon and auto-launched
Chrome/Edge app-mode window accompany it by default.

The server binds 127.0.0.1 only — no remote access, no auth, no TLS.
A Windows named mutex guards against a second instance: a second invocation
activates the first window and exits 0.`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			// --kill and --yes are stuck-instance-recovery modifiers
			// for --force; both are silently inert without it. Reject
			// the misuse with a usage error so cobra prints help and
			// exits 1, instead of silently dropping the destructive
			// intent. (Exit-code map for stuck-instance recovery is
			// reserved for runtime outcomes — operator misuse uses
			// cobra's default 1.)
			if kill && !force {
				return fmt.Errorf("--kill requires --force; pass `--force --kill` for stuck-instance kill recovery")
			}
			if yes && !force {
				return fmt.Errorf("--yes requires --force; pass `--force --kill --yes` to confirm non-interactive kill")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			// Resolve pidport location (test seam: override via env for subprocess tests).
			pidportPath, err := gui.PidportPath()
			if err != nil {
				return err
			}
			if d := os.Getenv("MCPHUB_GUI_TEST_PIDPORT_DIR"); d != "" {
				pidportPath = filepath.Join(d, "gui.pidport")
			}

			// Phase A: acquire the single-instance lock BEFORE binding any
			// port. If we bind first and the requested --port is already in
			// use (e.g. because the incumbent GUI owns it), ListenTCP fails
			// with "address already in use" and we never reach the
			// handshake path that would activate the incumbent. The
			// pidport file initially records the requested port (which may
			// be 0 = auto); once the server actually binds, we rewrite it
			// with the resolved port so second-instance handshake probes
			// reach the right place.
			lock, err := gui.AcquireSingleInstanceAt(pidportPath, port)
			if err != nil {
				if !errors.Is(err, gui.ErrSingleInstanceBusy) {
					return err
				}
				// PR #23 C1 stuck-instance recovery. Three flows:
				//   - default ErrSingleInstanceBusy without --force →
				//     try handshake; on failure, exit 1 with concise
				//     "rerun with --force" message (legacy).
				//   - bare --force → run Probe, print structured
				//     diagnostic, open lock folder, exit 2.
				//   - --force --kill → KillRecordedHolder (with
				//     three-part identity gate); on success continue
				//     normal startup; on failure map Verdict to the
				//     appropriate exit code.
				if force {
					if kill {
						newLock, exitCode := runForceKill(cmd, pidportPath, yes)
						if newLock != nil {
							// Take-over succeeded: continue into Phase B
							// with the freshly-acquired lock. Helper
							// extraction (no goto) keeps repo style intact.
							return startGuiServer(cmd, ctx, stop, newLock, port, noBrowser, noTray, pidportPath)
						}
						return forceExit(exitCode)
					}
					exitCode := runForceDiagnostic(cmd, pidportPath)
					return forceExit(exitCode)
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					return fmt.Errorf(
						"another mcphub gui is running but unreachable (%v); "+
							"rerun with --force for diagnostic, or --force --kill to recover",
						err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
				return nil
			}
			return startGuiServer(cmd, ctx, stop, lock, port, noBrowser, noTray, pidportPath)
		},
	}
	c.Flags().IntVar(&port, "port", 0, "TCP port on 127.0.0.1 (0 = auto-pick from ephemeral)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "do not auto-launch a browser window")
	c.Flags().BoolVar(&noTray, "no-tray", false, "do not show the system-tray icon")
	c.Flags().BoolVar(&force, "force", false, "stuck-instance recovery: print diagnostic + open lock folder. Add --kill to terminate the recorded PID after a three-part identity gate.")
	c.Flags().BoolVar(&kill, "kill", false, "with --force: kill the recorded PID (image/argv/start-time gate); SIGKILL/TerminateProcess. The kernel releases the flock as a side effect.")
	c.Flags().BoolVar(&yes, "yes", false, "with --force --kill: skip the confirmation prompt (required in non-interactive shells).")
	_ = c.Flags().MarkHidden("force")
	_ = c.Flags().MarkHidden("kill")
	_ = c.Flags().MarkHidden("yes")
	return c
}

// startGuiServer runs Phase B: server start, status poller, optional
// browser launch, optional tray icon. Extracted from the RunE body so
// both the normal-acquire path AND the `--force --kill` recovery path
// share one implementation. Caller MUST own a non-nil lock; this
// helper takes ownership of the lock's release.
//
// Helper-extraction approach (preferred over goto + label) keeps the
// repo's existing control-flow style. See plan task 4 §"alternative".
func startGuiServer(cmd *cobra.Command, ctx context.Context, stop context.CancelFunc,
	lock *gui.SingleInstanceLock, port int, noBrowser, noTray bool, pidportPath string) error {
	defer lock.Release()

	// Phase B: start the HTTP server. Server.Start binds 127.0.0.1
	// on the configured port (0 = OS-assigned) and signals ready
	// once the listener is live.
	s := gui.NewServer(gui.Config{Port: port, Version: versionString()})
	s.OnActivateWindow(func() {
		// Phase 3B-II C2: bring the Chrome app-mode window to
		// foreground via Win32 SetForegroundWindow. Match by
		// page <title> ("mcp-local-hub"), which is stable
		// across Chrome versions in app-mode (chromeless
		// window keeps page title as window title). On
		// non-Windows, FocusBrowserWindow returns an error
		// (logged below); the tray "Open dashboard" action
		// shares the same surface and the same limitation.
		//
		// Fallback: if no matching window exists (user closed
		// the Chrome dashboard earlier, or GUI was spawned
		// with --no-browser), open a fresh window. Without
		// this fallback the tray "Open dashboard" action
		// silently no-ops when there's nothing to focus.
		// "Local Dashboard" is the unique suffix in the page
		// <title>; it disambiguates from other apps that
		// happen to have "mcp-local-hub" in their window
		// title (Cursor IDE has "mcp-local-hub - Cursor",
		// terminals running in the repo dir, file explorer,
		// etc.). Without the unique suffix the focus call
		// silently steals foreground for the wrong window.
		err := gui.FocusBrowserWindow("Local Dashboard")
		if err == nil {
			return
		}
		// Codex PR #22 r3 P2: only fall back to LaunchBrowser
		// when enumeration completed without a matching
		// window (gui.ErrFocusNoWindow sentinel). Other
		// failures — Win32 transient SetForegroundWindow
		// rejection on Windows 10+ when our thread isn't
		// foreground, syscall plumbing regressions, etc. —
		// must NOT spawn a duplicate dashboard. The
		// non-Windows stub also wraps ErrFocusNoWindow so
		// "Open dashboard" on Linux/macOS still launches a
		// fresh browser (no tray to focus there anyway).
		if !errors.Is(err, gui.ErrFocusNoWindow) {
			fmt.Fprintf(cmd.OutOrStderr(),
				"activate-window: focus failed (no fallback for non-no-window error): %v\n", err)
			return
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
		if launchErr := gui.LaunchBrowser(url); launchErr != nil {
			fmt.Fprintf(cmd.OutOrStderr(),
				"activate-window: focus failed (%v); browser launch also failed: %v\n",
				err, launchErr)
		}
	})

	ready := make(chan struct{})
	errCh := make(chan error, 1)
	go func() { errCh <- s.Start(ctx, ready) }()

	// Poll daemon status every 5s and push daemon-state events onto /api/events.
	poller := gui.NewStatusPoller(gui.RealStatusProvider{}, s.Broadcaster(), 5*time.Second)
	// Tray state plumbing (C3): wire a snapshot channel between
	// poller and tray. Aggregator goroutine reads each snapshot,
	// computes a TrayState, and pushes onto trayStateCh ONLY when
	// the aggregate changes — avoids redundant SetIcon calls when
	// individual daemons flap but the overall state is steady.
	//
	// Both channels are size-1 buffered with non-blocking sends
	// at every send site so a stalled tray cannot back up the
	// poller, and a stalled poller cannot back up status reads.
	snapshotCh := make(chan []api.DaemonStatus, 1)
	trayStateCh := make(chan tray.TrayState, 1)
	poller.SetSnapshotChannel(snapshotCh)
	go aggregateTrayState(ctx, snapshotCh, trayStateCh)
	go poller.Run(ctx)

	select {
	case <-ready:
		// Now we know the actual bound port. Unconditionally rewrite
		// the pidport with our PID + the bound port. The flock on
		// *.lock still gates ownership; the pidport file is
		// ownership metadata the lock holder freely updates.
		//
		// Codex PR #23 P2 #2: previously this branch only ran when
		// actualPort != port (the requested port). After a
		// successful --force --kill takeover the pidport still held
		// the killed incumbent's PID + port; if the user requested
		// an explicit port that we then bound, the conditional
		// short-circuited and the stale PID/port persisted forever.
		// The new unconditional write is idempotent on the normal-
		// acquire path (pidport already has our PID + the requested
		// port from AcquireSingleInstanceAt) and corrective on the
		// takeover path.
		actualPort := s.Port()
		if err := gui.WritePidport(pidportPath, os.Getpid(), actualPort); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "warning: pidport rewrite: %v\n", err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "GUI listening on http://127.0.0.1:%d\n", actualPort)
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}

	if !noBrowser {
		url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
		if err := gui.LaunchBrowser(url); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "warning: could not auto-launch browser: %v\n", err)
		}
	}
	if !noTray {
		go func() {
			_ = tray.Run(ctx, tray.Config{
				ActivateWindow: func() {
					// In-process handshake: hit our own activate handler to
					// trigger whatever OnActivateWindow callback is registered
					// (Phase 3B-II: focus browser window).
					_ = gui.TryActivateIncumbent(pidportPath, 500*time.Millisecond)
				},
				StateCh: trayStateCh,
				Quit:    stop, // signal.NotifyContext's cancel function
			})
		}()
	}
	return <-errCh
}

// runForceDiagnostic implements the bare `--force` flow: Probe,
// print structured block, open lock folder, return exit code 2 (or
// 0 on Healthy fall-through to handshake).
func runForceDiagnostic(cmd *cobra.Command, pidportPath string) int {
	v := gui.Probe(cmd.Context(), pidportPath)
	if v.Class == gui.VerdictHealthy {
		// Healthy → fall through to TryActivateIncumbent (legacy
		// handshake). Returning 0 signals the caller to handshake.
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "incumbent reported healthy but activate-window failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(cmd.OutOrStdout(), "activated existing mcphub gui")
		return 0
	}
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
	_ = gui.OpenFolderAt(pidportPath)
	return 2
}

// runForceKill implements `--force --kill`. Returns
// (acquiredLock, exitCode). On success acquiredLock is non-nil and
// exitCode==0; the caller continues into Phase B.
func runForceKill(cmd *cobra.Command, pidportPath string, yes bool) (*gui.SingleInstanceLock, int) {
	// Gate 0 (Claude r2 #3): non-TTY without --yes → exit 6.
	if !yes && !term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprintln(cmd.OutOrStderr(), "non-interactive shell — pass --yes to confirm --kill")
		return nil, 6
	}

	v := gui.Probe(cmd.Context(), pidportPath)

	// Gate 1: Healthy early-exit (Codex r5 #7b): never kill a healthy gui.
	if v.Class == gui.VerdictHealthy {
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent is healthy (PID %d); activating instead of killing\n", v.PID)
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
	}

	// Print diagnostic so the operator sees what we're about to kill.
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))

	// Gate 2: confirmation prompt unless --yes.
	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(), "Kill PID %d (mcphub gui)? [y/N]: ", v.PID)
		var resp string
		_, _ = fmt.Fscanln(os.Stdin, &resp)
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		}
	}

	lock, killVerdict, err := gui.KillRecordedHolder(cmd.Context(), pidportPath, gui.KillOpts{})
	if killVerdict.Class == gui.VerdictKilledRecovered {
		fmt.Fprintln(cmd.OutOrStdout(), killVerdict.Diagnose)
		return lock, 0
	}
	// Map class to exit code.
	fmt.Fprintln(cmd.OutOrStderr(), killVerdict.Diagnose)
	if killVerdict.Hint != "" {
		fmt.Fprintln(cmd.OutOrStderr(), "hint:", killVerdict.Hint)
	}
	switch killVerdict.Class {
	case gui.VerdictKillRefused:
		return nil, 7
	case gui.VerdictKillFailed:
		return nil, 4
	case gui.VerdictRaceLost:
		return nil, 3
	case gui.VerdictMalformed:
		return nil, 4
	case gui.VerdictDeadPID:
		// Probe said dead but acquire failed afterward — treat as
		// race-lost because the OS should have released flock when
		// the dead process exited; if we can't acquire, someone
		// else holds it now.
		return nil, 3
	default:
		// Forward-compat safety net: Verdict will grow in the A4-b
		// HTTP API path. If a future class lands without a switch
		// arm, surface the class + err to stderr instead of silently
		// exiting 1 with no diagnostic.
		fmt.Fprintf(cmd.OutOrStderr(), "internal: unrecognized verdict class %q (err=%v)\n", killVerdict.Class.String(), err)
		return nil, 1
	}
}

// formatDiagnostic builds the human-readable diagnostic block from
// a Verdict. Output format matches memo §"Diagnostic format".
func formatDiagnostic(v gui.Verdict, pidportPath string) string {
	var b strings.Builder
	b.WriteString("Cannot acquire mcphub gui single-instance lock.\n\n")
	fmt.Fprintf(&b, "Lock file:  %s.lock\n", pidportPath)
	fmt.Fprintf(&b, "Pidport:    %s\n", pidportPath)
	fmt.Fprintf(&b, "  recorded PID:  %d\n", v.PID)
	fmt.Fprintf(&b, "  recorded port: %d\n", v.Port)
	if !v.Mtime.IsZero() {
		fmt.Fprintf(&b, "  pidport mtime: %s\n", v.Mtime.UTC().Format(time.RFC3339))
	}
	b.WriteString("\nLive-holder probe:\n")
	if v.PIDAlive {
		fmt.Fprintf(&b, "  PID %d status:    alive\n", v.PID)
		if v.PIDImage != "" {
			fmt.Fprintf(&b, "  PID %d image:     %s\n", v.PID, v.PIDImage)
		}
	} else {
		fmt.Fprintf(&b, "  PID %d status:    not alive\n", v.PID)
	}
	if v.PingMatch {
		fmt.Fprintf(&b, "  /api/ping on %d:  ok (PID matches)\n\n", v.Port)
	} else {
		fmt.Fprintf(&b, "  /api/ping on %d:  failed or PID mismatch\n\n", v.Port)
	}
	if v.Diagnose != "" {
		b.WriteString("Verdict: " + v.Class.String() + "\n")
		b.WriteString("  " + v.Diagnose + "\n")
	}
	if v.Hint != "" {
		b.WriteString("Hint: " + v.Hint + "\n")
	}
	return b.String()
}

// forceExitError is a typed error that carries an exit code. cmd/mcphub/main.go
// uses errors.As(err, &fe) where fe is `interface{ ExitCode() int }` to map
// these errors onto os.Exit(code) — without that branch cobra defaults to
// exit 1 on error and the distinct exit codes (2/3/4/6/7) are lost.
type forceExitError struct{ code int }

func (e *forceExitError) Error() string { return fmt.Sprintf("force exit %d", e.code) }
func (e *forceExitError) ExitCode() int { return e.code }

func forceExit(code int) error {
	return &forceExitError{code: code}
}

// versionString returns the linker-baked version. Ephemeral placeholder
// for MVP; Phase 3B-II wires build-time ldflags through here.
func versionString() string {
	if v := os.Getenv("MCPHUB_VERSION"); v != "" {
		return v
	}
	return "dev"
}
