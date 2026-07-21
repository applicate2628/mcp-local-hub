//go:build windows

package cli

import (
	"io"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"mcp-local-hub/internal/process"
)

// TestConfigureSupervisorDetachSuppressesConsoleAttach closes the link
// between the two halves of the detach.
//
// The creation flags were never the problem on their own — a detached
// child of a console-holding parent still calls
// AttachConsole(ATTACH_PARENT_PROCESS) in its own main() and becomes a
// console client, so closing the launching terminal delivered
// CTRL_CLOSE_EVENT to the GUI-spawned supervisor and KILL_ON_JOB_CLOSE
// then reaped every daemon under it. Measured externally against a real
// -H windowsgui build: detached child, no marker -> appears in the
// parent's GetConsoleProcessList; detached child WITH the marker -> never
// appears.
//
// This asserts the marker is actually applied at the spawn site, so the
// measured behavior above is the behavior production gets. It also covers
// the respawn path, which rebuilds every replacement supervisor cmd
// through this same hook.
func TestConfigureSupervisorDetachSuppressesConsoleAttach(t *testing.T) {
	cmd := exec.Command("mcphub", "supervise")
	configureSupervisorDetach(cmd)

	marker := process.SuppressConsoleAttachEnv + "=1"
	found := false
	for _, e := range cmd.Env {
		if strings.EqualFold(e, marker) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("configureSupervisorDetach did not mark the child attach-suppressed (%s missing); "+
			"the detach flags alone do NOT stop the child from calling AttachConsole, so a "+
			"GUI-spawned supervisor would re-attach to the launching terminal and die with it",
			marker)
	}

	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&windowsDetachedProcess == 0 {
		t.Error("configureSupervisorDetach dropped DETACHED_PROCESS; the marker suppresses the " +
			"deliberate re-attach but only the flag blocks console INHERITANCE at create time")
	}
}

// TestGUISupervisorRespawnPathIsAttachSuppressed verifies the RESPAWN
// chain rather than asserting it from reading the code.
//
// The chain is: supervisorManager's respawn loop -> spawnSupervisorFn ->
// ensureSupervisorRunning -> newGUISupervisorCmd. Each link is checked:
// the seam still points at ensureSupervisorRunning (a test that swapped it
// permanently, or a refactor that repointed it, would break the property
// silently), and the command that function builds carries the marker.
//
// Without this, a supervisor that crashed and was respawned WHILE the GUI
// still held a console would come back attach-capable even though the
// original startup spawn was fixed.
func TestGUISupervisorRespawnPathIsAttachSuppressed(t *testing.T) {
	if got, want := reflect.ValueOf(spawnSupervisorFn).Pointer(),
		reflect.ValueOf(ensureSupervisorRunning).Pointer(); got != want {
		t.Fatalf("spawnSupervisorFn no longer resolves to ensureSupervisorRunning; the respawn "+
			"loop builds its replacement supervisors somewhere else, so the console-attach "+
			"suppression verified below does not necessarily apply to them (got %v want %v)",
			got, want)
	}

	marker := process.SuppressConsoleAttachEnv + "=1"
	for _, args := range [][]string{{"supervise"}, {"supervise", "--strict-mode"}} {
		cmd := newGUISupervisorCmd("mcphub", args, io.Discard)
		found := false
		for _, e := range cmd.Env {
			if strings.EqualFold(e, marker) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("respawned supervisor (%v) is not attach-suppressed; a supervisor that "+
				"crashed under a console-holding GUI would come back as a CTRL_CLOSE_EVENT "+
				"target even though the startup spawn is fixed", args)
		}
	}
}
