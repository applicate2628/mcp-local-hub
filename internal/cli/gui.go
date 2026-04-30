// internal/cli/gui.go
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// inputIsTerminal reports whether r is a terminal-backed *os.File. The
// non-TTY guard for --force --kill must check the SAME stream the
// confirmation prompt reads from (cmd.InOrStdin) so test / embedded
// callers that override input via cmd.SetIn(...) get consistent
// behavior. term.IsTerminal needs an int fd, so non-*os.File readers
// (bytes.Buffer, strings.Reader) return false unconditionally —
// matching the documented "scripted input ⇒ non-interactive" intent.
// Codex bot review on PR #23 P2 (round 3).
func inputIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return term.IsTerminal(int(f.Fd()))
}

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
			// `--yes` is the confirmation bypass for `--force --kill`,
			// not for bare `--force`. Reject `--force --yes` (without
			// --kill) too — otherwise `mcphub gui --force --yes` runs
			// the bare-diagnostic path silently and a typo-skipped
			// `--kill` looks like a handled force flow in automation.
			// `kill && !force` is enforced above, so `yes && !kill`
			// also covers the lone `--yes` case (no --force, no --kill).
			if yes && !kill {
				return fmt.Errorf("--yes requires --force --kill; pass `--force --kill --yes` to confirm non-interactive kill")
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
						// Codex iter-10 P2 #1: pass signal-aware ctx
						// (from signal.NotifyContext above) so Ctrl+C
						// during the kill path actually cancels the
						// destructive operation. cmd.Context() is the
						// cobra parent context and ignores SIGINT.
						newLock, exitCode := runForceKill(ctx, cmd, pidportPath, yes)
						if newLock != nil {
							// Take-over succeeded: continue into Phase B
							// with the freshly-acquired lock. Helper
							// extraction (no goto) keeps repo style intact.
							return startGuiServer(cmd, ctx, stop, newLock, port, noBrowser, noTray, pidportPath)
						}
						return forceExit(exitCode)
					}
					exitCode := runForceDiagnostic(ctx, cmd, pidportPath)
					return forceExit(exitCode)
				}
				if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
					var ina *gui.IncumbentNoActivationTargetError
					if errors.As(err, &ina) {
						// Incumbent reachable but headless — print
						// SSH-tunnel guidance using the port the
						// handshake already verified via /api/ping.
						// We do NOT re-read the pidport: that races a
						// successor's pre-bind port=0 write and turns
						// usable guidance into a spurious error.
						// Codex CLI xhigh review on PR #26 P3.
						fmt.Fprintf(cmd.OutOrStdout(),
							"mcphub gui is already running headless on port %d. SSH-tunnel and visit http://127.0.0.1:%d/\n",
							ina.Port, ina.Port)
						return nil
					}
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
	s.OnActivateWindow(func() error {
		// Phase 3B-II C2: focus the existing Chrome app-mode dashboard
		// via Win32 SetForegroundWindow. Title-substring "Local
		// Dashboard" disambiguates from other "mcp-local-hub" windows
		// (Cursor IDE, terminals, file explorer).
		err := gui.FocusBrowserWindow("Local Dashboard")
		if err == nil {
			return nil
		}
		if !errors.Is(err, gui.ErrFocusNoWindow) {
			// Win32 transient failure — log + best-effort 204 so the
			// second invocation prints "activated" (incumbent IS
			// reachable, just focus jitter happened).
			fmt.Fprintf(cmd.OutOrStderr(),
				"activate-window: focus failed (no fallback for non-no-window error): %v\n", err)
			return nil
		}
		// SECURITY: --no-browser MUST disable every launch path.
		// Without this guard, ANY local actor POSTing
		// /api/activate-window on a leftover --no-browser orphan
		// could spawn a Chrome window the operator never asked for
		// (real bug observed when test orphans piled up). The
		// /api/activate-window handler maps ErrActivationNoTarget to
		// 503; the second-instance handshake reads the typed
		// sentinel and prints diagnostic instead of falsely claiming
		// activation.
		if noBrowser {
			fmt.Fprintln(cmd.OutOrStderr(),
				"activate-window: focus failed and --no-browser set — refusing to launch")
			return gui.ErrActivationNoTarget
		}
		// Headless Linux: no display server, browser launch would
		// xdg-open-fail noisily. Surface ErrActivationNoTarget so
		// the second instance prints SSH-tunnel guidance (PR #26 F4).
		if gui.HeadlessSession() {
			fmt.Fprintln(cmd.OutOrStderr(),
				"activate-window: focus failed and headless session — no browser to open")
			return gui.ErrActivationNoTarget
		}
		url := fmt.Sprintf("http://127.0.0.1:%d/", s.Port())
		if launchErr := gui.LaunchBrowser(url); launchErr != nil {
			fmt.Fprintf(cmd.OutOrStderr(),
				"activate-window: focus failed (%v); browser launch also failed: %v\n",
				err, launchErr)
			return nil
		}
		return nil
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
			// PR #24 added child-failure propagation: tray.Run
			// returns non-nil when the tray subprocess exits
			// unexpectedly while ctx is alive. Surface the error
			// so the GUI doesn't silently lose tray functionality.
			// Tray callbacks dispatch through the SAME HTTP endpoints as the
			// Dashboard buttons. Going through HTTP (rather than calling
			// api.NewAPI() directly) means the SSE Broadcaster fires
			// bulk-action lifecycle events that any open Dashboard tab
			// observes — buttons flash "Starting…" / "Stopping…" exactly
			// as if the user had clicked them in the browser. Without
			// this round-trip the tray would mutate daemon state silently
			// and the Dashboard would only catch up via the per-daemon
			// SSE updates, with no overall progress indicator. One
			// pipeline, one source of truth.
			port := int(s.Port())
			postBulk := func(action string) error {
				url := fmt.Sprintf("http://127.0.0.1:%d/api/%s-all", port, action)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
				if err != nil {
					return err
				}
				// requireSameOrigin gate accepts requests with no Origin
				// header (CSRF middleware allows non-browser clients);
				// adding the loopback Origin makes the request indistinguishable
				// from a Dashboard fetch on the wire.
				req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode >= 500 {
					return fmt.Errorf("HTTP %d", resp.StatusCode)
				}
				return nil
			}
			if err := tray.Run(ctx, tray.Config{
				ActivateWindow: func() {
					_ = gui.TryActivateIncumbent(pidportPath, 500*time.Millisecond)
				},
				StateCh: trayStateCh,
				Quit:    stop,
				QuitAndStopAll: func() {
					// Stop all via HTTP (so the Dashboard sees the SSE
					// lifecycle), then trigger the GUI shutdown. Errors
					// don't block the shutdown — partial cleanup beats
					// a hung GUI.
					if err := postBulk("stop"); err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "tray: POST /api/stop-all: %v\n", err)
					}
					stop()
				},
				RunAllDaemons: func() {
					if err := postBulk("restart"); err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "tray: POST /api/restart-all: %v\n", err)
					}
				},
				StopAllDaemons: func() {
					if err := postBulk("stop"); err != nil {
						fmt.Fprintf(cmd.OutOrStderr(), "tray: POST /api/stop-all: %v\n", err)
					}
				},
			}); err != nil {
				fmt.Fprintf(cmd.OutOrStderr(), "tray: %v (GUI continues without tray)\n", err)
			}
		}()
	}
	return <-errCh
}

// runForceDiagnostic implements the bare `--force` flow: Probe,
// print structured block, open lock folder, return exit code 2 (or
// 0 on Healthy fall-through to handshake).
//
// ctx is the signal-aware context from RunE so Ctrl+C/SIGTERM
// during Probe (which makes a network call) cancels promptly.
// (Codex iter-10 P2 #1.)
func runForceDiagnostic(ctx context.Context, cmd *cobra.Command, pidportPath string) int {
	v := gui.Probe(ctx, pidportPath)
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
//
// ctx is the signal-aware context from RunE (from signal.NotifyContext)
// so Ctrl+C/SIGTERM during the kill path is honored — including the
// post-kill wait-for-exit loop and the acquire-poll loop inside
// KillRecordedHolder. cmd.Context() would NOT receive SIGINT.
// (Codex iter-10 P2 #1.)
func runForceKill(ctx context.Context, cmd *cobra.Command, pidportPath string, yes bool) (*gui.SingleInstanceLock, int) {
	// Probe FIRST so the healthy-incumbent early-exit can run without
	// requiring --yes in non-TTY contexts. The original ordering put
	// Gate 0 (non-TTY ⇒ require --yes) before the probe, which broke
	// CI/cron usage of `mcphub gui --force --kill` as a defensive
	// idempotent activate: a healthy incumbent should always route to
	// activate-only (no kill, no destructive consent needed). Codex
	// bot review on PR #23 P1.
	v := gui.Probe(ctx, pidportPath)

	// Gate 1: Healthy early-exit (Codex r5 #7b): never kill a healthy gui.
	if v.Class == gui.VerdictHealthy {
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent is healthy (PID %d); activating instead of killing\n", v.PID)
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
	}

	// Gate 2 (Codex iter-6 P2 #1): only LiveUnreachable is a kill
	// target. Malformed and DeadPID skip the destructive prompt and
	// exit with the documented unrecoverable / already-recovered
	// codes. Without this gate, a corrupt pidport (PID 0) or a dead
	// recorded PID would still ask "Kill PID 0?" before any kill,
	// and "Enter to cancel" would silently exit 0 even though
	// nothing could have been killed.
	//
	// Codex bot review on PR #23 P2 (round 2): this verdict-classification
	// MUST run BEFORE the non-TTY/--yes guard. Otherwise CI/cron
	// callers hit exit 6 even for VerdictMalformed (4) or DeadPID (3)
	// where no kill is attempted; that masks the proper exit codes
	// and forces automation to add --yes for non-destructive paths.
	switch v.Class {
	case gui.VerdictMalformed:
		// Codex iter-8 P2 #2: kill-mode malformed maps to exit 4
		// (pidport unrecoverable) per memo §"Exit codes". Bare
		// --force diagnostic uses exit 2; --force --kill is a
		// distinct contract and CI scripts must distinguish them.
		fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
		return nil, 4
	case gui.VerdictDeadPID:
		// Probe says the recorded PID is already gone — the OS
		// should have released the flock as a side effect. Map to
		// exit 3 (race-lost / already-recovered semantic per memo
		// §Exit codes).
		fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))
		return nil, 3
	case gui.VerdictLiveUnreachable:
		// fall through to identity gate + prompt + KillRecordedHolder
	default:
		fmt.Fprintf(cmd.OutOrStderr(),
			"internal: unexpected verdict class %q from Probe; refusing kill\n",
			v.Class.String())
		return nil, 1
	}

	// Gate 0 (Claude r2 #3): non-TTY without --yes → exit 6.
	// Reached only when verdict == LiveUnreachable — the path that
	// actually attempts a kill. Non-TTY callers that truly want the
	// kill must pass --yes. Healthy / Malformed / DeadPID short-circuit
	// above without consent (no kill happens).
	//
	// Codex bot review on PR #23 P2 (round 3): probe TTY-ness on the
	// SAME stream the prompt reads from (cmd.InOrStdin), not os.Stdin.
	// Otherwise tests / embedded callers that override input via
	// cmd.SetIn(...) get inconsistent behavior — guard skips --yes
	// even though scripted input is non-interactive, then the prompt
	// EOFs and silently exits 0 without performing the recovery.
	if !yes && !inputIsTerminal(cmd.InOrStdin()) {
		fmt.Fprintln(cmd.OutOrStderr(), "non-interactive shell — pass --yes to confirm --kill")
		return nil, 6
	}

	// Print diagnostic so the operator sees what we're about to kill.
	fmt.Fprintln(cmd.OutOrStdout(), formatDiagnostic(v, pidportPath))

	// Codex iter-9 P2 #1: run the identity gate BEFORE the prompt.
	// Without this, the operator could be asked "Kill PID X
	// (mcphub gui)?" for a PID that the gate later refuses (e.g.
	// the recorded PID is `mcphub daemon`, a recycled PID belonging
	// to another process, or macOS-unsupported) — they consent to a
	// kill that never happens, then see the refusal afterward.
	// KillRecordedHolder still re-runs the same gate internally for
	// defense in depth; this pre-prompt invocation guards UX, not
	// safety.
	if refused, reason := gui.CheckIdentityGate(v); refused {
		fmt.Fprintln(cmd.OutOrStderr(), "kill refused:", reason)
		return nil, 7
	}

	// Gate 3: confirmation prompt unless --yes.
	if !yes {
		fmt.Fprintf(cmd.OutOrStdout(), "Kill PID %d (mcphub gui)? [y/N]: ", v.PID)
		// Read in a goroutine + select on ctx.Done so Ctrl+C / SIGTERM
		// during the prompt actually unblocks the wait. cmd.InOrStdin()
		// is the prompt's input stream so callers using cmd.SetIn(...)
		// for scripted input get honored.
		//
		// Sonnet review on PR #23 F2: pipe the read through an
		// io.Pipe whose reader we close on ctx.Done. Closing pr
		// unblocks Fscanln with io.ErrClosedPipe so the consumer
		// goroutine exits cleanly when ctx fires.
		//
		// Codex CLI xhigh round-4 follow-up: the source-side io.Copy
		// goroutine still blocks on the original cmd.InOrStdin (an
		// os.File or buffer with no cancellation primitive that we
		// own). This is a documented residual leak bounded by process
		// lifetime: the CLI is single-shot, so on ctx-cancel the
		// process is exiting anyway and the goroutine is reaped by
		// the OS. The Fscanln consumer side (the actual blocker the
		// prior bug worried about — operator hits Ctrl+C and stays
		// stuck) IS unblocked. A future embedding context (planned
		// A4-b HTTP /api/force-kill) needs a longer-lived solution
		// (close the source-side fd via an *os.File assertion + ctx
		// goroutine) — out of scope for the CLI surface.
		pr, pw := io.Pipe()
		go func() {
			_, _ = io.Copy(pw, cmd.InOrStdin())
			_ = pw.Close()
		}()
		respCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			var resp string
			_, err := fmt.Fscanln(pr, &resp)
			if err != nil {
				errCh <- err
				return
			}
			respCh <- resp
		}()
		var resp string
		select {
		case resp = <-respCh:
		case <-errCh:
			// Fscanln error (EOF, bad input). Treat as cancel:
			// the prompt was implicitly declined.
			_ = pr.Close()
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		case <-ctx.Done():
			// Unblock the Fscanln goroutine via pipe close.
			_ = pr.Close()
			fmt.Fprintln(cmd.OutOrStderr(), "interrupted")
			return nil, 1
		}
		// Drain pipe reader so the io.Copy goroutine can finish on
		// stdin EOF without blocking on a write to a still-open pipe.
		_ = pr.Close()
		resp = strings.TrimSpace(strings.ToLower(resp))
		if resp != "y" && resp != "yes" {
			fmt.Fprintln(cmd.OutOrStdout(), "cancelled")
			return nil, 0
		}
	}

	// Codex iter-5 P1: pass the identity tuple the cli already saw
	// (and printed/confirmed with the user) into KillRecordedHolder
	// so its internal re-probe refuses with VerdictRaceLost (exit 3)
	// if a competitor rewrote pidport during the prompt window. The
	// guard runs even on --yes because the prompt-skip path still
	// has a sub-second TOCTOU window between this Probe and the one
	// inside KillRecordedHolder.
	lock, killVerdict, err := gui.KillRecordedHolder(ctx, pidportPath, gui.KillOpts{
		Expected: gui.ExpectedIdentity{PID: v.PID, Port: v.Port, Mtime: v.Mtime},
	})
	if killVerdict.Class == gui.VerdictKilledRecovered {
		fmt.Fprintln(cmd.OutOrStdout(), killVerdict.Diagnose)
		return lock, 0
	}
	if killVerdict.Class == gui.VerdictHealthy {
		// Codex PR #23 P2 #2 (iter-2): KillRecordedHolder's internal
		// re-probe found the incumbent healthy after this cli's first
		// Probe (e.g., Server.Start finally bound during the user
		// confirmation prompt above). Honor "never kill healthy"
		// exactly as the early-exit at the top of runForceKill:
		// route to TryActivateIncumbent and exit 0. Handled before
		// the stderr-diagnose preamble below so the success path
		// stays on stdout.
		fmt.Fprintf(cmd.OutOrStdout(), "incumbent recovered to healthy (PID %d); activating instead of killing\n", killVerdict.PID)
		if err := gui.TryActivateIncumbent(pidportPath, 2*time.Second); err != nil {
			fmt.Fprintf(cmd.OutOrStderr(), "activate-window failed: %v\n", err)
			return nil, 1
		}
		return nil, 0
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
// uses errors.As(err, &fe) where fe is the combined
// `interface{ ExitCode() int; IsMcphubForceExit() bool }` to map these
// errors onto os.Exit(code) — without that branch cobra defaults to
// exit 1 on error and the distinct exit codes (2/3/4/6/7) are lost.
type forceExitError struct{ code int }

func (e *forceExitError) Error() string { return fmt.Sprintf("force exit %d", e.code) }
func (e *forceExitError) ExitCode() int { return e.code }

// IsMcphubForceExit is the marker that distinguishes this CLI sentinel
// from os/exec.ExitError (which also satisfies `interface{ ExitCode() int }`).
// cmd/mcphub/main.go must match against this method to avoid silently
// suppressing diagnostic context from wrapped subprocess failures
// (editor in `mcphub manifest edit` / `mcphub secrets edit`, taskkill,
// etc. — see fmt.Errorf("...: %w", err) wrappings in those files).
// Codex iter-5 P2.
func (e *forceExitError) IsMcphubForceExit() bool { return true }

func forceExit(code int) error {
	// Code 0 is a successful outcome (e.g. healthy-incumbent activate
	// short-circuit, normal acquire after take-over). Wrapping it in
	// forceExitError would make cmd.Execute() report a non-nil error,
	// breaking the standard Cobra success contract for in-process
	// callers and emitting spurious usage output. Codex bot review on
	// PR #23 P2 (round 4).
	if code == 0 {
		return nil
	}
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
