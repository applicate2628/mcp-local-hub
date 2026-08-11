//go:build windows

package cli

import (
	"context"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsNetshNoConsole(t *testing.T) {
	for _, args := range [][]string{
		{"int", "ipv4", "show", "dynamicport", "tcp"},
		{"int", "ipv4", "set", "dynamicport", "tcp", "start=50000", "num=15536"},
	} {
		cmd := newNetshCommandContext(context.Background(), "netsh", args...)
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 {
			t.Fatalf("netsh args=%q attributes=%+v, want HideWindow and CREATE_NO_WINDOW", args, cmd.SysProcAttr)
		}
	}
}
