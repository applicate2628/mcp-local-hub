package gui

import (
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

func hasSuppressionMarker(env []string) bool {
	marker := process.SuppressConsoleAttachEnv + "=1"
	for _, e := range env {
		if strings.EqualFold(e, marker) {
			return true
		}
	}
	return false
}

// TestNewDetachedSupervisorCmdSuppressesConsoleAttach covers the
// manual-restart supervisor spawn (POST /api/supervisor/restart).
//
// The detach flags alone never delivered "the child is not a console
// client": DETACHED_PROCESS blocks INHERITANCE, but the child is the same
// mcphub binary and calls AttachConsole(ATTACH_PARENT_PROCESS) in its own
// main(). Measured externally against a real -H windowsgui build with this
// site's exact flag set (DETACHED|NEW_GROUP|BREAKAWAY = 0x01000208):
//
//	no marker -> child appears in the parent's GetConsoleProcessList
//	marker    -> never appears
//
// Reachable whenever the GUI still holds a console (--foreground /
// --no-tray) and the operator hits the Dashboard restart affordance.
func TestNewDetachedSupervisorCmdSuppressesConsoleAttach(t *testing.T) {
	cmd := newDetachedSupervisorCmd("mcphub")

	if !hasSuppressionMarker(cmd.Env) {
		t.Fatalf("manual-restart supervisor spawn is not attach-suppressed (%s missing); "+
			"the replacement supervisor would re-attach to the GUI's console and die with "+
			"that terminal, taking every daemon under its Job Object with it",
			process.SuppressConsoleAttachEnv)
	}
	if cmd.SysProcAttr == nil {
		t.Error("configureDetached did not run: the marker suppresses the deliberate re-attach, " +
			"but only the creation flags block console INHERITANCE at create time")
	}
}

// TestNewRestartV3GUICmdDoesNotSuppressConsoleAttach pins the deliberate
// ASYMMETRY, so a later sweep does not "consistently" apply the marker
// here and silently break --foreground.
//
// This child is a replacement GUI, not a supervisor, and it re-parses the
// SAME argv. Under --foreground / --no-tray the operator explicitly asked
// for a console-attached GUI; suppressing the attach would kill their
// terminal output and Ctrl-C across a restart. In the default background
// mode the parent already released its console, so there is nothing to
// attach to and the marker would verify nothing. Neither case wants it.
func TestNewRestartV3GUICmdDoesNotSuppressConsoleAttach(t *testing.T) {
	cmd := newRestartV3GUICmd("mcphub", []string{"gui", "--foreground"}, []string{"FOO=bar"})

	if hasSuppressionMarker(cmd.Env) {
		t.Fatalf("RestartV3 replacement GUI was marked attach-suppressed (%s present). "+
			"That is a supervisor-spawn control applied to a GUI spawn: under --foreground "+
			"the restarted GUI would lose the operator's console, output and Ctrl-C. If this "+
			"was intentional, the --foreground contract has to change first",
			process.SuppressConsoleAttachEnv)
	}
}
