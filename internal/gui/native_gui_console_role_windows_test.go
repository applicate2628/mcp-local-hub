//go:build windows

package gui

import "testing"

func TestNativeGUIArgvMatchesWindowsStartupConsolePolicy(t *testing.T) {
	const executable = `D:\Program Files\MCPHub\mcphub.exe`
	for _, c := range []struct {
		name string
		tail []string
		gui  bool
	}{
		{"plain_gui", []string{"gui", "--foreground"}, true},
		{"exact_startup_prefix_then_gui", []string{"--debug-console", "gui", "--foreground"}, true},
		{"exact_startup_prefix_then_implicit_gui", []string{"--debug-console"}, true},
		{"exact_startup_prefix_then_status", []string{"--debug-console", "status"}, false},
		{"duplicate_prefix", []string{"--debug-console", "--debug-console", "gui"}, false},
		{"unknown_leading_option", []string{"--unknown", "--debug-console", "gui"}, false},
		{"near_prefix", []string{"--debug-console=true", "gui"}, false},
		{"terminator_then_prefix", []string{"--", "--debug-console", "gui"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := cmdlineIsGui(append([]string{executable}, c.tail...)); got != c.gui {
				t.Fatalf("native GUI role = %t, want %t", got, c.gui)
			}
		})
	}
}
