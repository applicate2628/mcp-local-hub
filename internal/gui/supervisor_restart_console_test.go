//go:build windows

package gui

import (
	"os/exec"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func assertRestartCommandNoConsole(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("restart command attributes=%+v, want HideWindow and CREATE_NO_WINDOW", cmd.SysProcAttr)
	}
}

func TestWindowsRestartNoConsole(t *testing.T) {
	supervisor := newDetachedSupervisorCmd("mcphub")
	assertRestartCommandNoConsole(t, supervisor)
	if !reflect.DeepEqual(supervisor.Args, []string{"mcphub", "supervise"}) {
		t.Fatalf("supervisor argv=%q, want exact process-local restart argv", supervisor.Args)
	}
	if len(supervisor.Env) != 0 {
		t.Fatalf("supervisor environment unexpectedly rewritten: %q", supervisor.Env)
	}

	replacementGUI := newRestartV3GUICmd("mcphub", []string{"gui", "--foreground"}, []string{"FOO=bar"})
	assertRestartCommandNoConsole(t, replacementGUI)
	if !reflect.DeepEqual(replacementGUI.Args, []string{"mcphub", "gui", "--foreground"}) {
		t.Fatalf("replacement GUI argv=%q, want exact supplied argv", replacementGUI.Args)
	}
	if !reflect.DeepEqual(replacementGUI.Env, []string{"FOO=bar"}) {
		t.Fatalf("replacement GUI environment=%q, want caller environment preserved", replacementGUI.Env)
	}

	taskkill := newSupervisorTaskkillCmd(42)
	assertRestartCommandNoConsole(t, taskkill)
}
