//go:build windows

package cli

import "testing"

func TestWindowsInstallSupervisorNoConsole(t *testing.T) {
	for _, strictMode := range []bool{false, true} {
		build := installSupervisorCmdBuilder("mcphub", strictMode)
		for attempt := 0; attempt < 3; attempt++ {
			cmd := build()
			if cmd.SysProcAttr == nil {
				t.Fatalf("strict=%v attempt=%d: nil SysProcAttr", strictMode, attempt)
			}
			assertWindowsBackgroundCommand(t, cmd.Args, cmd.SysProcAttr.CreationFlags, cmd.SysProcAttr.HideWindow)
			if len(cmd.Env) != 0 {
				t.Fatalf("strict=%v attempt=%d unexpectedly rewrote environment: %q", strictMode, attempt, cmd.Env)
			}
		}
	}
}
