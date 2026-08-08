package main

import (
	"os"
	"reflect"
	"testing"

	"mcp-local-hub/internal/cli"
)

func TestRouteBareInvocationProducesTrayEnabledGUIArgs(t *testing.T) {
	routed := routeInvocationArgs([]string{"mcphub"})
	if want := []string{"mcphub", "gui"}; !reflect.DeepEqual(routed, want) {
		t.Fatalf("routeInvocationArgs(bare) = %q, want %q", routed, want)
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
