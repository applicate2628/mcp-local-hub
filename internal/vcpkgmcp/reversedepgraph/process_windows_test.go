//go:build windows

package reversedepgraph

import (
	"errors"
	"os/exec"
	"testing"

	"golang.org/x/sys/windows"
)

func TestStartPlatformProcessSuppressesConsole(t *testing.T) {
	startErr := errors.New("synthetic start refusal")
	// Cmd.Start returns Err before it can create a native process.
	cmd := &exec.Cmd{Err: startErr}
	started, err := startPlatformProcess(cmd)
	if err != startErr || started != nil || cmd.Process != nil {
		t.Fatalf("start error = %v, process returned = %t, child created = %t; want original error and no process", err, started != nil, cmd.Process != nil)
	}

	attr := cmd.SysProcAttr
	const want = windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP
	if attr == nil || !attr.HideWindow || attr.CreationFlags&want != want {
		t.Fatalf("start attributes = %+v; want HideWindow, CREATE_NO_WINDOW and CREATE_NEW_PROCESS_GROUP", attr)
	}
}
