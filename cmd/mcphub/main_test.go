package main

import "testing"

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
