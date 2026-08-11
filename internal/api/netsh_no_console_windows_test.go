//go:build windows

package api

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsNetshNoConsole(t *testing.T) {
	cmd := newExcludedPortNetshCommand("netsh", "int", "ipv4", "show", "excludedportrange", "protocol=tcp")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
		t.Fatalf("excluded-port netsh attributes=%+v, want HideWindow and CREATE_NO_WINDOW", cmd.SysProcAttr)
	}
}
