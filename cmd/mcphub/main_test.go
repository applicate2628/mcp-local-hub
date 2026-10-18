package main

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/cli"
)

func TestPrepareInvocationWindowsBareLaunchReturnsWithoutInlineCobra(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	calls := 0
	launchBareGUIFn = func() (bool, error) {
		calls++
		return true, nil
	}

	args, handled, err := prepareInvocation([]string{"mcphub"}, cli.WindowsConsoleDisabled)
	if err != nil || !handled || calls != 1 || !reflect.DeepEqual(args, []string{"mcphub"}) {
		t.Fatalf("prepareInvocation=(%q,%t,%v) calls=%d, want original args/handled/nil/1", args, handled, err, calls)
	}
}

func TestClassifyWindowsEntrypointMode_CBuildIsStdioProtocol(t *testing.T) {
	if got := classifyWindowsEntrypointMode([]string{"mcphub", "cbuild"}); got != windowsEntrypointStdioProtocol {
		t.Fatalf("cbuild entrypoint mode = %d, want windowsEntrypointStdioProtocol", got)
	}
}

func TestPrepareInvocationPreservesNonBareAndOptOutRoutes(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	launchBareGUIFn = func() (bool, error) {
		t.Fatal("bare coordinator called for preserved route")
		return false, nil
	}

	for _, tc := range []struct {
		name string
		args []string
		env  string
	}{
		{name: "explicit gui", args: []string{"mcphub", "gui"}},
		{name: "version", args: []string{"mcphub", "version"}},
		{name: "debug console", args: []string{"mcphub", cli.WindowsDebugConsolePrefix}},
		{name: "bare opt out", args: []string{"mcphub"}, env: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(NoAutoGUIEnv, tc.env)
			args, handled, err := prepareInvocation(tc.args, cli.WindowsConsoleDisabled)
			if err != nil || handled || !reflect.DeepEqual(args, tc.args) {
				t.Fatalf("prepareInvocation=(%q,%t,%v), want unchanged/unhandled/nil", args, handled, err)
			}
		})
	}
}

func TestPrepareInvocationBareLaunchFailureRemainsVisible(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	want := errors.New("bare launch failed")
	launchBareGUIFn = func() (bool, error) { return false, want }

	_, handled, err := prepareInvocation([]string{"mcphub"}, cli.WindowsConsoleDisabled)
	if handled || !errors.Is(err, want) {
		t.Fatalf("handled=%t error=%v, want false and launch cause", handled, err)
	}
}

func TestPrepareInvocationNormalizedDebugBareStaysInline(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	calls := 0
	launchBareGUIFn = func() (bool, error) {
		calls++
		return true, nil
	}

	original := []string{"mcphub", cli.WindowsDebugConsolePrefix}
	policy, normalized := cli.ResolveWindowsConsolePolicy(original)
	if policy != cli.WindowsConsoleDebugExplicit || !reflect.DeepEqual(normalized, []string{"mcphub"}) {
		t.Fatalf("normalization premise=(%v,%q), want explicit debug and bare argv", policy, normalized)
	}
	args, handled, err := prepareInvocation(normalized, policy)
	if err != nil || handled || calls != 0 || !reflect.DeepEqual(args, []string{"mcphub", "gui"}) {
		t.Fatalf("prepare normalized debug=(%q,%t,%v) calls=%d, want routed gui/unhandled/nil/0", args, handled, err, calls)
	}
}

func TestPrepareInvocationNormalizedPlainBareCallsDetachedCoordinator(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	calls := 0
	launchBareGUIFn = func() (bool, error) {
		calls++
		return true, nil
	}

	policy, normalized := cli.ResolveWindowsConsolePolicy([]string{"mcphub"})
	args, handled, err := prepareInvocation(normalized, policy)
	if err != nil || !handled || calls != 1 || !reflect.DeepEqual(args, []string{"mcphub"}) {
		t.Fatalf("prepare normalized plain bare=(%q,%t,%v) calls=%d, want original/handled/nil/1", args, handled, err, calls)
	}
}

func TestRouteBareInvocationProducesTrayEnabledGUIArgs(t *testing.T) {
	routed := cli.RouteInvocationArgs([]string{"mcphub"})
	if want := []string{"mcphub", "gui"}; !reflect.DeepEqual(routed, want) {
		t.Fatalf("cli.RouteInvocationArgs(bare) = %q, want %q", routed, want)
	}

	root := cli.NewRootCmd()
	guiCmd, remaining, err := root.Find(routed[1:])
	if err != nil {
		t.Fatalf("find routed command: %v", err)
	}
	if guiCmd.Name() != "gui" {
		t.Fatalf("routed command = %q, want gui", guiCmd.Name())
	}
	if err := guiCmd.Flags().Parse(remaining); err != nil {
		t.Fatalf("parse routed GUI args %q: %v", remaining, err)
	}
	noTray, err := guiCmd.Flags().GetBool("no-tray")
	if err != nil {
		t.Fatalf("GetBool(no-tray): %v", err)
	}
	if noTray {
		t.Fatal("bare invocation routed to GUI with --no-tray enabled")
	}
}

// TestShouldAutoLaunchGUIForArgs pins the bare-run routing contract:
// `mcphub` with no subcommand routes to `gui`; anything carrying an
// argument keeps cobra's normal dispatch (including `--help` / `help`,
// which must still print the command list).
//
// This is the CONSOLE-INDEPENDENT contract introduced 2026-07-20. The
// previous implementation additionally required "no console attached",
// so a terminal `mcphub` printed the 40-command help instead of starting
// the hub. The table below fails against that implementation on the
// bare-invocation case, which is the mutation this test guards.
func TestShouldAutoLaunchGUIForArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"bare invocation routes to gui", []string{"mcphub"}, true},
		{"bare invocation with absolute path", []string{`C:\Users\x\.local\bin\mcphub.exe`}, true},
		{"defensive: empty argv treated as bare", nil, true},

		{"--help still prints the command list", []string{"mcphub", "--help"}, false},
		{"-h still prints the command list", []string{"mcphub", "-h"}, false},
		{"help subcommand untouched", []string{"mcphub", "help"}, false},
		{"gui subcommand untouched", []string{"mcphub", "gui"}, false},
		{"status subcommand untouched", []string{"mcphub", "status"}, false},
		{"subcommand with flags untouched", []string{"mcphub", "gui", "--no-tray"}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldAutoLaunchGUIForArgs(tc.args); got != tc.want {
				t.Errorf("shouldAutoLaunchGUIForArgs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestShouldAutoLaunchGUIIsConsoleIndependent states the regression
// explicitly rather than leaving it implicit in the table: the decision
// must depend ONLY on argv. Any reintroduction of a stdout/console probe
// makes the bare case environment-dependent — it would return false under
// a test binary (whose stdout is a real handle) and the assertion below
// would fail.
func TestShouldAutoLaunchGUIIsConsoleIndependent(t *testing.T) {
	if !shouldAutoLaunchGUIForArgs([]string{"mcphub"}) {
		t.Fatal("bare `mcphub` must route to gui regardless of console attachment; " +
			"a console/stdout probe has been reintroduced into the routing decision")
	}
}

// TestShouldAutoLaunchGUI_EnvOptOut pins the escape hatch for the
// bare-`mcphub` CONTRACT CHANGE. Bare `mcphub` went from "print help, exit
// 0" to "bind a port, spawn a supervisor, block forever", which breaks any
// script, CI step or healthcheck that ran it as a cheap liveness probe.
// MCPHUB_NO_AUTO_GUI=1 restores the old behavior for those callers.
//
// It exercises shouldAutoLaunchGUI (the impure wrapper) rather than
// shouldAutoLaunchGUIForArgs, because keeping the env read OUT of the pure
// seam is the design being pinned: the seam stays table-testable and the
// ambient read stays in one place.
func TestShouldAutoLaunchGUI_EnvOptOut(t *testing.T) {
	origArgs := os.Args
	t.Cleanup(func() { os.Args = origArgs })
	os.Args = []string{"mcphub"}

	tests := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{"unset: bare mcphub starts the hub", "", false, true},
		{"empty value is not an opt-out", "", true, true},
		{"0 is not an opt-out", "0", true, true},
		{"arbitrary value is not an opt-out", "yes-please", true, true},

		{"1 opts out", "1", true, false},
		{"true opts out", "true", true, false},
		{"TRUE opts out (case-insensitive)", "TRUE", true, false},
		{"padded value opts out (trimmed)", "  1  ", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(NoAutoGUIEnv, tc.val)
			} else {
				t.Setenv(NoAutoGUIEnv, "")
				os.Unsetenv(NoAutoGUIEnv)
			}
			if got := shouldAutoLaunchGUI(); got != tc.want {
				t.Errorf("shouldAutoLaunchGUI() with %s=%q (set=%v) = %v, want %v",
					NoAutoGUIEnv, tc.val, tc.set, got, tc.want)
			}
		})
	}
}

// TestShouldAutoLaunchGUI_OptOutDoesNotLeakIntoThePureSeam guards the
// separation itself: the env var must change the wrapper's answer and
// leave the pure seam untouched, so the table test above stays
// environment-independent.
func TestShouldAutoLaunchGUI_OptOutDoesNotLeakIntoThePureSeam(t *testing.T) {
	t.Setenv(NoAutoGUIEnv, "1")
	if !shouldAutoLaunchGUIForArgs([]string{"mcphub"}) {
		t.Fatal("MCPHUB_NO_AUTO_GUI leaked into shouldAutoLaunchGUIForArgs; " +
			"the pure seam must stay a function of argv alone")
	}
}

// Catches a new root command acquiring a console by accident, or an existing
// GUI/background/protocol command drifting into ordinary CLI handle repair.
func TestClassifyWindowsEntrypointModeExhaustiveRootCommands(t *testing.T) {
	wantSpecial := map[string]windowsEntrypointMode{
		"gui":                                   windowsEntrypointGUI,
		"supervise":                             windowsEntrypointBackground,
		"daemon":                                windowsEntrypointBackground,
		"route":                                 windowsEntrypointBackground,
		"tray":                                  windowsEntrypointBackground,
		"workspace-weekly-refresh":              windowsEntrypointBackground,
		"audit-lock-terminal-worker":            windowsEntrypointBackground,
		"daemon-recovery-audit-handoff-worker":  windowsEntrypointBackground,
		"gui-owner-unknown-confirmation-worker": windowsEntrypointBackground,
		"relay":                                 windowsEntrypointStdioProtocol,
		"cbuild":                                windowsEntrypointStdioProtocol,
	}
	for _, command := range cli.NewRootCmd().Commands() {
		want := windowsEntrypointCLI
		if special, ok := wantSpecial[command.Name()]; ok {
			want = special
			delete(wantSpecial, command.Name())
		}
		if got := classifyWindowsEntrypointMode([]string{"mcphub", command.Name()}); got != want {
			t.Errorf("command %q mode = %v, want %v", command.Name(), got, want)
		}
	}
	if len(wantSpecial) != 0 {
		t.Fatalf("special entry commands are not registered at the root: %v", wantSpecial)
	}
	for _, args := range [][]string{nil, {"mcphub"}} {
		if got := classifyWindowsEntrypointMode(args); got != windowsEntrypointGUI {
			t.Errorf("bare args %q mode = %v, want GUI", args, got)
		}
	}
	for _, args := range [][]string{{"mcphub", "unknown-future-command"}, {"mcphub", "gui-owner-unknown-confirmation-marker-worker"}} {
		if got := classifyWindowsEntrypointMode(args); got != windowsEntrypointCLI {
			t.Errorf("safe-default args %q mode = %v, want CLI", args, got)
		}
	}
}

// Catches the existing bare-GUI opt-out being classified as GUI after the
// product executable becomes a GUI-subsystem image, which would skip ordinary
// CLI output repair before Cobra prints the command list.
func TestClassifyWindowsEntrypointModeBareOptOutKeepsCobraHelp(t *testing.T) {
	previous := launchBareGUIFn
	t.Cleanup(func() { launchBareGUIFn = previous })
	for _, optOut := range []string{"1", "true", " TRUE "} {
		t.Run(optOut, func(t *testing.T) {
			t.Setenv(NoAutoGUIEnv, optOut)
			if got := classifyWindowsEntrypointMode([]string{"mcphub"}); got != windowsEntrypointCLI {
				t.Errorf("bare opt-out mode = %v, want CLI", got)
			}
			launchCalls := 0
			launchBareGUIFn = func() (bool, error) {
				launchCalls++
				return true, nil
			}
			args, handled, err := prepareInvocation([]string{"mcphub"}, cli.WindowsConsoleDisabled)
			if err != nil || handled || launchCalls != 0 || !reflect.DeepEqual(args, []string{"mcphub"}) {
				t.Fatalf("prepare opt-out = (%q,%t,%v) launch calls=%d, want unchanged/unhandled/nil/0", args, handled, err, launchCalls)
			}
			root := cli.NewRootCmd()
			var stdout, stderr bytes.Buffer
			root.SetOut(&stdout)
			root.SetErr(&stderr)
			root.SetArgs([]string{})
			if err := root.Execute(); err != nil {
				t.Fatalf("execute opted-out bare root: %v", err)
			}
			if !strings.Contains(stdout.String(), "Usage:") || stderr.Len() != 0 {
				t.Fatalf("opt-out help stdout/stderr = %q/%q, want visible command help/empty", stdout.String(), stderr.String())
			}
		})
	}
}
