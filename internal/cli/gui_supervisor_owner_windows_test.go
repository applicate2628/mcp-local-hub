//go:build windows

package cli

import (
	"io"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func assertWindowsBackgroundCommand(t *testing.T, cmdArgs []string, flags uint32, hide bool) {
	t.Helper()
	if !hide || flags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("background child flags=%#x HideWindow=%v, want CREATE_NO_WINDOW and HideWindow", flags, hide)
	}
	for _, arg := range cmdArgs {
		if arg == WindowsDebugConsolePrefix {
			t.Fatalf("background child argv propagated %q: %q", WindowsDebugConsolePrefix, cmdArgs)
		}
	}
}

func TestWindowsSupervisorSpawnNoConsole(t *testing.T) {
	cmd := newGUISupervisorCmd("mcphub", []string{"supervise"}, io.Discard)
	if cmd.SysProcAttr == nil {
		t.Fatal("supervisor SysProcAttr is nil")
	}
	assertWindowsBackgroundCommand(t, cmd.Args, cmd.SysProcAttr.CreationFlags, cmd.SysProcAttr.HideWindow)
	if len(cmd.Env) != 0 {
		t.Fatalf("supervisor command unexpectedly rewrote environment: %q", cmd.Env)
	}
}

func TestWindowsSupervisorRetryNoConsole(t *testing.T) {
	if got, want := reflect.ValueOf(spawnSupervisorFn).Pointer(), reflect.ValueOf(ensureSupervisorRunning).Pointer(); got != want {
		t.Fatalf("respawn seam moved: got %v want %v", got, want)
	}
	for _, args := range [][]string{{"supervise"}, {"supervise", "--strict-mode"}} {
		for attempt := 0; attempt < 3; attempt++ {
			cmd := newGUISupervisorCmd("mcphub", args, io.Discard)
			assertWindowsBackgroundCommand(t, cmd.Args, cmd.SysProcAttr.CreationFlags, cmd.SysProcAttr.HideWindow)
			if len(cmd.Env) != 0 {
				t.Fatalf("retry %d unexpectedly rewrote environment: %q", attempt, cmd.Env)
			}
		}
	}
}
