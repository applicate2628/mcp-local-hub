package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/api/serena_routing"
	"mcp-local-hub/internal/gui"
)

func TestActivateDashboardFromTrayDecisionMatrix(t *testing.T) {
	const (
		pidportPath = "test-gui.pidport"
		port        = 19123
	)
	unexpected := errors.New("tray transport failed")
	tests := []struct {
		name          string
		requestedPort int // what startGuiServer passes in (the --port FLAG value)
		activationErr error
		wantLaunches  int
		wantURL       string
		wantLog       string
	}{
		{
			name:          "no target",
			requestedPort: port,
			activationErr: &gui.IncumbentNoActivationTargetError{
				Port: port, Reason: gui.ReasonNoBrowserWindow,
			},
			wantLaunches: 1,
			wantURL:      "http://127.0.0.1:19123/",
			wantLog:      "no dashboard window to focus",
		},
		{
			// The default `--port 0` (an ephemeral bind) is the production
			// posture: the REQUESTED port stays 0 while the server binds a real
			// one. The URL must come from the handshake-verified port on the
			// typed error, or the tray opens http://127.0.0.1:0/.
			name:          "no target under default --port 0 uses the bound port",
			requestedPort: 0,
			activationErr: &gui.IncumbentNoActivationTargetError{
				Port: port, Reason: gui.ReasonNoBrowserWindow,
			},
			wantLaunches: 1,
			wantURL:      "http://127.0.0.1:19123/",
			wantLog:      "no dashboard window to focus",
		},
		{name: "nil", requestedPort: port, wantLaunches: 0},
		{name: "unexpected error", requestedPort: port, activationErr: unexpected, wantLaunches: 0, wantLog: "tray: activate-window failed: tray transport failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			activateCalls := 0
			launchCalls := 0
			launchedURL := ""
			activateDashboardFromTray(
				pidportPath,
				tt.requestedPort,
				&log,
				func(gotPath string, gotTimeout time.Duration) error {
					activateCalls++
					if gotPath != pidportPath {
						t.Errorf("pidport path = %q, want %q", gotPath, pidportPath)
					}
					if gotTimeout != 500*time.Millisecond {
						t.Errorf("activation timeout = %v, want 500ms", gotTimeout)
					}
					return tt.activationErr
				},
				func(url string) error {
					launchCalls++
					launchedURL = url
					return nil
				},
			)

			if activateCalls != 1 {
				t.Fatalf("activation calls = %d, want 1", activateCalls)
			}
			if launchCalls != tt.wantLaunches {
				t.Fatalf("browser launch calls = %d, want %d", launchCalls, tt.wantLaunches)
			}
			if tt.wantLaunches == 1 && launchedURL != tt.wantURL {
				t.Errorf("launched URL = %q, want %q", launchedURL, tt.wantURL)
			}
			if tt.wantLog != "" && !strings.Contains(log.String(), tt.wantLog) {
				t.Errorf("log = %q, want substring %q", log.String(), tt.wantLog)
			}
		})
	}
}

func TestActivateDashboardWindowDecisionMatrix(t *testing.T) {
	unexpected := errors.New("focus failed")
	tests := []struct {
		name         string
		focusErr     error
		headless     bool
		noBrowser    bool
		wantReason   gui.ActivationNoTargetReason
		wantLaunches int
	}{
		{name: "focus success", headless: true, noBrowser: true},
		{name: "non-no-window focus error", focusErr: unexpected, headless: true, noBrowser: true},
		{name: "headless", focusErr: gui.ErrFocusNoWindow, headless: true, wantReason: gui.ReasonHeadless},
		{name: "no browser and no window", focusErr: gui.ErrFocusNoWindow, noBrowser: true, wantReason: gui.ReasonNoBrowserWindow},
		{name: "browser fallback", focusErr: gui.ErrFocusNoWindow, wantLaunches: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log bytes.Buffer
			focusCalls := 0
			launchCalls := 0
			launchedURL := ""
			err := activateDashboardWindow(
				tt.noBrowser,
				19123,
				&log,
				func(title string) error {
					focusCalls++
					if title != "Local Dashboard" {
						t.Errorf("focus title = %q, want Local Dashboard", title)
					}
					return tt.focusErr
				},
				func(url string) error {
					launchCalls++
					launchedURL = url
					return nil
				},
				func() bool { return tt.headless },
			)

			if tt.wantReason == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
			} else {
				if !errors.Is(err, gui.ErrActivationNoTarget) {
					t.Fatalf("error = %v, want ErrActivationNoTarget", err)
				}
				var noTarget *gui.ActivationNoTargetError
				if !errors.As(err, &noTarget) {
					t.Fatalf("error type = %T, want *gui.ActivationNoTargetError", err)
				}
				if noTarget.Reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", noTarget.Reason, tt.wantReason)
				}
			}
			if focusCalls != 1 {
				t.Errorf("focus calls = %d, want 1", focusCalls)
			}
			if launchCalls != tt.wantLaunches {
				t.Errorf("browser launch calls = %d, want %d", launchCalls, tt.wantLaunches)
			}
			if tt.wantLaunches == 1 && launchedURL != "http://127.0.0.1:19123/" {
				t.Errorf("launched URL = %q, want http://127.0.0.1:19123/", launchedURL)
			}
		})
	}
}

func TestHandleIncumbentActivationResultGuidance(t *testing.T) {
	unexpected := errors.New("activate failed")
	tests := []struct {
		name        string
		err         error
		wantHandled bool
		wantErr     error
		wantOutput  string
	}{
		{name: "nil"},
		{
			name: "headless",
			err: &gui.IncumbentNoActivationTargetError{
				Port: 19123, Reason: gui.ReasonHeadless,
			},
			wantHandled: true,
			wantOutput:  "SSH-tunnel and visit http://127.0.0.1:19123/",
		},
		{
			name: "no browser window",
			err: &gui.IncumbentNoActivationTargetError{
				Port: 19123, Reason: gui.ReasonNoBrowserWindow,
			},
			wantHandled: true,
			wantOutput:  "no dashboard window to focus. Open http://127.0.0.1:19123/",
		},
		{name: "unexpected", err: unexpected, wantErr: unexpected},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			handled, err := handleIncumbentActivationResult(&out, tt.err)
			if handled != tt.wantHandled {
				t.Errorf("handled = %v, want %v", handled, tt.wantHandled)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.wantOutput != "" && !strings.Contains(out.String(), tt.wantOutput) {
				t.Errorf("output = %q, want substring %q", out.String(), tt.wantOutput)
			}
		})
	}
}

func TestGuiCmd_HelpIncludesFlags(t *testing.T) {
	cmd := newGuiCmdReal()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// PR #214 closes QA-r6-Gap-4 by asserting --strict-mode appears in
	// the user-visible help. The flag was added by PR #212 r1
	// (gui_supervisor_owner.go) but the help-text test never updated;
	// a future regression that drops the cobra registration would
	// have gone unnoticed without this assertion.
	for _, want := range []string{"--port", "--no-browser", "--no-tray", "--strict-mode"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help missing %q; got %q", want, buf.String())
		}
	}
	// --force is intentionally hidden (Phase 3B-II placeholder); --help must NOT advertise it.
	if strings.Contains(buf.String(), "--force") {
		t.Errorf("--help unexpectedly advertises --force; should be hidden until take-over is implemented")
	}
}

// TestGuiCmd_TrayShownByDefaultOnBareLaunch pins operator requirement #2: a
// bare `mcphub` (no flags) must show the system-tray icon on a normal
// launch.
//
// cmd/mcphub/main.go's shouldAutoLaunchGUIForArgs routes a bare invocation to
// EXACTLY `gui` with no other argv appended (see TestShouldAutoLaunchGUIForArgs
// in cmd/mcphub/main_test.go), so the tray is shown on that path if and only
// if the `--no-tray` flag defaults to false and stays false when no flags are
// parsed at all -- which is exactly the argv shape a bare launch produces.
//
// Two assertions, deliberately not one: DefValue pins the flag DECLARATION
// (BoolVar(&noTray, "no-tray", false, ...)); the parse-then-GetBool pins that
// nothing between declaration and the noTray variable that
// startGuiServerWithStartup actually reads (guarding `if !noTray { spawn tray }`,
// internal/cli/gui.go) silently flips the resolved value for an empty argv.
func TestGuiCmd_TrayShownByDefaultOnBareLaunch(t *testing.T) {
	cmd := newGuiCmdReal()

	noTrayFlag := cmd.Flags().Lookup("no-tray")
	if noTrayFlag == nil {
		t.Fatal("--no-tray flag not registered on the gui command")
	}
	if noTrayFlag.DefValue != "false" {
		t.Fatalf("--no-tray default is %q, want \"false\": a bare `mcphub` (which cmd/mcphub/main.go "+
			"routes to exactly `gui`, no other flags) must show the tray on a normal launch "+
			"(operator requirement #2)", noTrayFlag.DefValue)
	}

	// Exercise the exact argv shape a bare invocation produces: zero flags.
	if err := cmd.Flags().Parse(nil); err != nil {
		t.Fatalf("parse empty argv: %v", err)
	}
	noTray, err := cmd.Flags().GetBool("no-tray")
	if err != nil {
		t.Fatalf("GetBool(no-tray): %v", err)
	}
	if noTray {
		t.Fatal("--no-tray resolved true with no flags passed; a bare `mcphub` would silently " +
			"suppress the tray, contradicting operator requirement #2")
	}
}

func TestSerenaBackendLossReconcileTicker_NilTriggerWaitsForInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSerenaBackendLossReconcileTicker(ctx, &gui.Server{}, time.Hour)
	}()

	select {
	case <-done:
		t.Fatal("reconcile ticker exited before its interval fired or ctx was canceled")
	case <-time.After(50 * time.Millisecond):
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("reconcile ticker did not exit after ctx cancel")
	}
}

// TestGuiCmd_ForceFlagStillParseable confirms `--force` is hidden but
// remains a valid flag (parseable, not removed). Hiding via MarkHidden
// keeps the wiring in place for Phase 3B-II without breaking any
// scripted callers that already pass --force.
func TestGuiCmd_ForceFlagStillParseable(t *testing.T) {
	cmd := newGuiCmdReal()
	if cmd.Flags().Lookup("force") == nil {
		t.Fatal("--force flag should still be defined (just hidden)")
	}
	if !cmd.Flags().Lookup("force").Hidden {
		t.Error("--force should be marked hidden")
	}
}

// TestResolveGuiPort pins bug-bash A5 (#18/#19/#20) closure: effective
// port follows --port flag when explicitly passed, otherwise reads
// `gui_server.port` settings, otherwise 0 (auto-pick). Pre-fix, the
// persisted setting was cosmetic — startup ignored it.
func TestResolveGuiPort(t *testing.T) {
	cases := []struct {
		name         string
		flagChanged  bool
		flagValue    int
		settingValue string
		want         int
	}{
		// 1. Flag explicitly set wins.
		{"explicit --port 9200 wins over setting 9125", true, 9200, "9125", 9200},
		{"explicit --port 0 wins (operator wants ephemeral)", true, 0, "9125", 0},
		// 2. Flag not set + valid setting → use setting.
		{"no flag, valid setting 9125", false, 0, "9125", 9125},
		{"no flag, setting 1024 (min)", false, 0, "1024", 1024},
		{"no flag, setting 65535 (max)", false, 0, "65535", 65535},
		{"no flag, setting with whitespace", false, 0, " 9125 ", 9125},
		// 3. Flag not set + invalid/empty setting → 0 (auto-pick).
		{"no flag, empty setting", false, 0, "", 0},
		{"no flag, non-numeric setting", false, 0, "abc", 0},
		{"no flag, below privileged-port boundary", false, 0, "80", 0},
		{"no flag, above 65535", false, 0, "70000", 0},
		{"no flag, negative setting", false, 0, "-1", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveGuiPort(tc.flagChanged, tc.flagValue, tc.settingValue)
			if got != tc.want {
				t.Errorf("resolveGuiPort(flagChanged=%v, flagValue=%d, setting=%q) = %d, want %d",
					tc.flagChanged, tc.flagValue, tc.settingValue, got, tc.want)
			}
		})
	}
}

func TestRunSessionCleanupTicker_ExpiresOldSessions(t *testing.T) {
	oldNow := time.Now().Add(-2 * time.Hour)
	sessions := serena_routing.NewSessionRouterWithClock(func() time.Time { return oldNow })
	sessions.BindSession("s1", &api.WorkspaceEntry{WorkspacePath: "alpha"})
	sessions.BindSession("s2", &api.WorkspaceEntry{WorkspacePath: "beta"})
	if got := sessions.Len(); got != 2 {
		t.Fatalf("Len before cleanup = %d, want 2", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// nil Server: this test exercises only the sticky-router sweep;
		// SweepSerenaSessions is covered by the gui-package unit test.
		runSessionCleanupTicker(ctx, nil, sessions, 10*time.Millisecond, time.Hour)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("runSessionCleanupTicker did not exit after ctx cancel")
		}
	})

	deadline := time.After(time.Second)
	for sessions.Len() != 0 {
		select {
		case <-deadline:
			t.Fatalf("Len after cleanup ticker = %d, want 0", sessions.Len())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// runSerenaIdleShutdownTicker (v0.6 #6) exits promptly on ctx cancel. A nil
// Server is the safe no-op path (the tick is skipped) — this asserts the
// goroutine lifecycle, not the sweep itself (covered by the gui-package tests).
func TestRunSerenaIdleShutdownTicker_ExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runSerenaIdleShutdownTicker(ctx, nil, 10*time.Millisecond)
		close(done)
	}()
	// Let a couple of ticks fire against the nil server (no-op), then cancel.
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runSerenaIdleShutdownTicker did not exit after ctx cancel")
	}
}
