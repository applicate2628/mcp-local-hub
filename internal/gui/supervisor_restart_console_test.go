package gui

import (
	"os/exec"
	"reflect"
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
		t.Error("configureDetachedSupervisor did not run: the marker suppresses the deliberate re-attach, " +
			"but only the creation flags block console INHERITANCE at create time")
	}
}

// TestNewRestartV3GUICmdDoesNotSuppressConsoleAttach pins the deliberate
// ASYMMETRY, so a later consistency sweep does not "helpfully" suppress the
// attach here and silently break --foreground.
//
// This child is a replacement GUI, not a supervisor, and it re-parses the
// SAME argv. Under --foreground / --no-tray the operator explicitly asked
// for a console-attached GUI; suppressing the attach would remove the
// restarted GUI's terminal output. In the default background mode the
// parent already released its console, so there is nothing to attach to and
// suppression would verify nothing. Neither case wants it.
//
// The justification is terminal OUTPUT only — Ctrl-C is already severed by
// CREATE_NEW_PROCESS_GROUP either way, so it is not an argument here.
//
// Since the two configurators were split, this is really a guard on the
// CHOICE: newRestartV3GUICmd must call configureDetachedGUI, and picking
// configureDetachedSupervisor by mistake is what this catches.
func TestNewRestartV3GUICmdDoesNotSuppressConsoleAttach(t *testing.T) {
	cmd := newRestartV3GUICmd("mcphub", []string{"gui", "--foreground"}, []string{"FOO=bar"})

	if hasSuppressionMarker(cmd.Env) {
		t.Fatalf("RestartV3 replacement GUI was marked attach-suppressed (%s present) — it was "+
			"built with configureDetachedSupervisor instead of configureDetachedGUI. That is a "+
			"supervisor-spawn control applied to a GUI spawn: under --foreground the restarted "+
			"GUI would lose the operator's terminal output. If this was intentional, the "+
			"--foreground contract has to change first",
			process.SuppressConsoleAttachEnv)
	}
	if cmd.SysProcAttr == nil {
		t.Error("detach flags missing: the GUI spawn still needs its own process group and " +
			"non-inheritance, it just must not suppress the attach")
	}
}

// TestDetachedConfiguratorsDifferOnlyInConsoleSuppression pins the split
// itself. The two configurators must apply the SAME detach flags and differ
// on exactly one axis — console-attach suppression. A future edit that
// diverges their flags (or re-merges them) breaks the property that picking
// a constructor is purely the console decision.
func TestDetachedConfiguratorsDifferOnlyInConsoleSuppression(t *testing.T) {
	sup := exec.Command("mcphub", "supervise")
	configureDetachedSupervisor(sup)
	gui := exec.Command("mcphub", "gui")
	configureDetachedGUI(gui)

	if sup.SysProcAttr == nil || gui.SysProcAttr == nil {
		t.Fatal("a configurator left SysProcAttr nil; both must apply the detach flags")
	}
	if !reflect.DeepEqual(sup.SysProcAttr, gui.SysProcAttr) {
		t.Errorf("the two configurators no longer apply identical detach flags "+
			"(supervisor=%+v gui=%+v); they must differ ONLY in console-attach suppression, "+
			"otherwise choosing between them silently changes process lifecycle too",
			sup.SysProcAttr, gui.SysProcAttr)
	}
	if !hasSuppressionMarker(sup.Env) {
		t.Error("configureDetachedSupervisor does not suppress the console attach")
	}
	if hasSuppressionMarker(gui.Env) {
		t.Error("configureDetachedGUI suppresses the console attach; that is the whole " +
			"difference the split exists to express, inverted")
	}
}
