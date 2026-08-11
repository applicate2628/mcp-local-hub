//go:build windows

package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsConsolePrefixGrammar(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantPolicy WindowsConsolePolicy
		wantArgs   []string
	}{
		{"bare", []string{"mcphub"}, WindowsConsoleDisabled, []string{"mcphub"}},
		{"exact prefix only", []string{"mcphub", WindowsDebugConsolePrefix}, WindowsConsoleDebugExplicit, []string{"mcphub"}},
		{"exact prefix status help", []string{"mcphub", WindowsDebugConsolePrefix, "status", "--help"}, WindowsConsoleDebugExplicit, []string{"mcphub", "status", "--help"}},
		{"known option value", []string{"mcphub", "adopt", "placeholder", "--name", WindowsDebugConsolePrefix, "--help"}, WindowsConsoleDisabled, []string{"mcphub", "adopt", "placeholder", "--name", WindowsDebugConsolePrefix, "--help"}},
		{"string array option value", []string{"mcphub", "cleanup", "aggressive", "--include-class", WindowsDebugConsolePrefix, "--help"}, WindowsConsoleDisabled, []string{"mcphub", "cleanup", "aggressive", "--include-class", WindowsDebugConsolePrefix, "--help"}},
		{"after terminator", []string{"mcphub", "--", WindowsDebugConsolePrefix}, WindowsConsoleDisabled, []string{"mcphub", "--", WindowsDebugConsolePrefix}},
		{"equals false", []string{"mcphub", WindowsDebugConsolePrefix + "=false"}, WindowsConsoleDisabled, []string{"mcphub", WindowsDebugConsolePrefix + "=false"}},
		{"equals true", []string{"mcphub", WindowsDebugConsolePrefix + "=true"}, WindowsConsoleDisabled, []string{"mcphub", WindowsDebugConsolePrefix + "=true"}},
		{"equals one", []string{"mcphub", WindowsDebugConsolePrefix + "=1"}, WindowsConsoleDisabled, []string{"mcphub", WindowsDebugConsolePrefix + "=1"}},
		{"exact prefix takes no value", []string{"mcphub", WindowsDebugConsolePrefix, "false"}, WindowsConsoleDebugExplicit, []string{"mcphub", "false"}},
		{"post subcommand", []string{"mcphub", "status", WindowsDebugConsolePrefix}, WindowsConsoleDisabled, []string{"mcphub", "status", WindowsDebugConsolePrefix}},
		{"subcommand option position", []string{"mcphub", "adopt", "placeholder", WindowsDebugConsolePrefix, "--help"}, WindowsConsoleDisabled, []string{"mcphub", "adopt", "placeholder", WindowsDebugConsolePrefix, "--help"}},
		{"unknown root option", []string{"mcphub", "--definitely-unknown", WindowsDebugConsolePrefix, "--help"}, WindowsConsoleDisabled, []string{"mcphub", "--definitely-unknown", WindowsDebugConsolePrefix, "--help"}},
		{"known then unknown option", []string{"mcphub", "adopt", "placeholder", "--name", "value", "--definitely-unknown", WindowsDebugConsolePrefix, "--help"}, WindowsConsoleDisabled, []string{"mcphub", "adopt", "placeholder", "--name", "value", "--definitely-unknown", WindowsDebugConsolePrefix, "--help"}},
		{"prefix consumes one occurrence only", []string{"mcphub", WindowsDebugConsolePrefix, WindowsDebugConsolePrefix}, WindowsConsoleDebugExplicit, []string{"mcphub", WindowsDebugConsolePrefix}},
		{"single hyphen near spelling", []string{"mcphub", "-debug-console"}, WindowsConsoleDisabled, []string{"mcphub", "-debug-console"}},
		{"underscore near spelling", []string{"mcphub", "--debug_console"}, WindowsConsoleDisabled, []string{"mcphub", "--debug_console"}},
		{"case variant", []string{"mcphub", "--DEBUG-CONSOLE"}, WindowsConsoleDisabled, []string{"mcphub", "--DEBUG-CONSOLE"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string(nil), tc.args...)
			gotPolicy, gotArgs := ResolveWindowsConsolePolicy(tc.args)
			if gotPolicy != tc.wantPolicy {
				t.Fatalf("policy = %v, want %v", gotPolicy, tc.wantPolicy)
			}
			if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
				t.Fatalf("argv = %#v, want %#v", gotArgs, tc.wantArgs)
			}
			if !reflect.DeepEqual(tc.args, before) {
				t.Fatalf("input argv mutated: got %#v, before %#v", tc.args, before)
			}
		})
	}
}

func TestWindowsRootHelpDebugConsolePrefix(t *testing.T) {
	root := NewRootCmd()
	if root.PersistentFlags().Lookup("debug-console") != nil || root.Flags().Lookup("debug-console") != nil {
		t.Fatal("debug-console must not be registered with Cobra/pflag")
	}
	if !strings.Contains(root.Long, "mcphub "+WindowsDebugConsolePrefix+" [command ...]") {
		t.Fatalf("root help does not advertise exact startup prefix %q: %q", WindowsDebugConsolePrefix, root.Long)
	}
}

func TestWindowsGUIConsoleState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		acquired   bool
		foreground bool
		noTray     bool
		want       bool
	}{
		{"ordinary default", false, false, false, false},
		{"ordinary foreground", false, true, false, false},
		{"ordinary no tray", false, false, true, false},
		{"explicit default", true, false, false, true},
		{"explicit foreground", true, true, false, false},
		{"explicit no tray", true, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveReleaseConsole(tc.acquired, tc.foreground, tc.noTray); got != tc.want {
				t.Fatalf("resolveReleaseConsole(%v,%v,%v) = %v, want %v", tc.acquired, tc.foreground, tc.noTray, got, tc.want)
			}
		})
	}
}

func TestWindowsReleaseDebugConsole(t *testing.T) {
	if !resolveReleaseConsole(true, false, false) {
		t.Fatal("an explicitly acquired background debug console was not released")
	}
	if resolveReleaseConsole(false, false, false) {
		t.Fatal("ordinary launch attempted to release an unowned console")
	}
}

func TestGUIBareRouting(t *testing.T) {
	_, normalized := ResolveWindowsConsolePolicy([]string{"mcphub", WindowsDebugConsolePrefix})
	got := RouteInvocationArgs(normalized)
	want := []string{"mcphub", "gui"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("prefix-only bare routing=%q, want %q", got, want)
	}
}

func TestGUILifetime(t *testing.T) {
	if shouldRunTray(true) {
		t.Fatal("--no-tray unexpectedly enables tray")
	}
	if !shouldRunTray(false) {
		t.Fatal("default GUI unexpectedly disables tray")
	}
	if resolveReleaseConsole(false, true, false) || resolveReleaseConsole(false, false, true) {
		t.Fatal("foreground/no-tray implied a console without explicit debug acquisition")
	}
	root := NewRootCmd()
	gui, _, err := root.Find([]string{"gui"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"foreground", "no-tray"} {
		usage := gui.Flags().Lookup(name).Usage
		if strings.Contains(usage, "attached to the launching terminal") || strings.Contains(usage, "keep the launching terminal's console") {
			t.Fatalf("--%s help retains obsolete console implication: %q", name, usage)
		}
		if !strings.Contains(usage, "does not enable a console") {
			t.Fatalf("--%s help does not state console-neutral contract: %q", name, usage)
		}
	}
}
