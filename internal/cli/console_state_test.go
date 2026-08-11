package cli

import "testing"

// TestResolveReleaseConsole pins the console-lifetime policy that replaced
// the `!noTray` gate.
//
// The defect being guarded is a silent flag overload: releasing the console
// was gated on "the tray is off", so a cosmetic flag decided process
// lifetime. An operator asking for "hub + GUI, no tray icon" also lost
// Ctrl-C-less background behavior they never asked about, and "tray on,
// keep my console" could not be expressed at all.
//
// Two properties matter and both are load-bearing:
//
//   - consoleAttached is the real discriminator. With no console there is
//     nothing to release, so the autostart shim (which launches
//     `gui --no-browser` WITHOUT --no-tray, and therefore used to take the
//     release branch) must no longer call FreeConsole at all — it was only
//     harmless there by accident.
//   - --no-tray must KEEP implying foreground. It is the documented dev
//     workflow and the E2E fixtures; silently taking their console away
//     would be a regression dressed up as a cleanup.
func TestResolveReleaseConsole(t *testing.T) {
	tests := []struct {
		name            string
		consoleAttached bool
		foreground      bool
		noTray          bool
		want            bool
	}{
		{"terminal launch, defaults: release and survive the terminal", true, false, false, true},

		{"no console (Explorer double-click): nothing to release", false, false, false, false},
		{"no console (autostart shim, tray ON): nothing to release", false, false, false, false},
		{"no console with --no-tray: still nothing to release", false, false, true, false},
		{"no console with --foreground: still nothing to release", false, true, false, false},

		{"--foreground keeps the console with the tray ON", true, true, false, false},
		{"--no-tray keeps the console (documented dev workflow)", true, false, true, false},
		{"both flags keep the console", true, true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveReleaseConsole(tc.consoleAttached, tc.foreground, tc.noTray)
			if got != tc.want {
				t.Errorf("resolveReleaseConsole(consoleAttached=%v, foreground=%v, noTray=%v) = %v, want %v",
					tc.consoleAttached, tc.foreground, tc.noTray, got, tc.want)
			}
		})
	}
}

// TestSetConsoleAttachedRoundTrips covers the injection seam itself: the
// value main() observes is the value the gui command later reads. A
// default of false is the safe one — no console claimed, nothing released.
func TestSetDebugConsoleAcquiredRoundTrips(t *testing.T) {
	orig := DebugConsoleAcquired()
	t.Cleanup(func() { SetDebugConsoleAcquired(orig) })

	SetDebugConsoleAcquired(false)
	if DebugConsoleAcquired() {
		t.Fatal("DebugConsoleAcquired() reported true after SetDebugConsoleAcquired(false)")
	}
	SetDebugConsoleAcquired(true)
	if !DebugConsoleAcquired() {
		t.Fatal("DebugConsoleAcquired() reported false after SetDebugConsoleAcquired(true); " +
			"the injected console state is not reaching the gui command, so a " +
			"terminal-launched GUI would never release its console")
	}
}
