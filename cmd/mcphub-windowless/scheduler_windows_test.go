//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	schedulerHelperModeEnv   = "MCPHUB_WINDOWLESS_SCHEDULER_HELPER"
	schedulerHelperReportEnv = "MCPHUB_WINDOWLESS_SCHEDULER_REPORT"
)

func TestAdapterStartsChildWithoutStandardHandles(t *testing.T) {
	dir := t.TempDir()
	adapter := buildGUIAdapter(t, dir)
	installTestHelperSibling(t, dir)
	reportPath := filepath.Join(dir, "scheduler-child-started")
	t.Setenv(schedulerHelperModeEnv, "1")
	t.Setenv(schedulerHelperReportEnv, reportPath)

	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{
		adapter,
		"-test.run=^TestWindowlessSchedulerHelper$",
	}))
	if err != nil {
		t.Fatal(err)
	}
	workingDirectory, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	startup := windows.StartupInfo{
		Cb:    uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Flags: windows.STARTF_USESTDHANDLES,
	}
	var processInfo windows.ProcessInformation
	if err := windows.CreateProcess(
		nil,
		commandLine,
		nil,
		nil,
		false,
		uint32(windows.CREATE_NO_WINDOW|windows.CREATE_UNICODE_ENVIRONMENT),
		nil,
		workingDirectory,
		&startup,
		&processInfo,
	); err != nil {
		t.Fatalf("start adapter without standard handles: %v", err)
	}
	_ = windows.CloseHandle(processInfo.Thread)
	defer windows.CloseHandle(processInfo.Process)
	waited := false
	defer func() {
		if !waited {
			_ = windows.TerminateProcess(processInfo.Process, 1)
			_, _ = windows.WaitForSingleObject(processInfo.Process, 5000)
		}
	}()

	status, err := windows.WaitForSingleObject(processInfo.Process, 10000)
	if err != nil {
		t.Fatalf("wait for adapter: %v", err)
	}
	if status != uint32(windows.WAIT_OBJECT_0) {
		t.Fatalf("adapter wait status=%#x, want WAIT_OBJECT_0", status)
	}
	waited = true
	var exitCode uint32
	if err := windows.GetExitCodeProcess(processInfo.Process, &exitCode); err != nil {
		t.Fatalf("read adapter exit code: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("adapter exit code=%d, want child exit code 0", exitCode)
	}
	if body, err := os.ReadFile(reportPath); err != nil {
		t.Fatalf("read scheduler child report: %v", err)
	} else if string(body) != "started" {
		t.Fatalf("scheduler child report=%q, want started", body)
	}
}

func buildGUIAdapter(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(dir, "mcphub-windowless.exe")
	cmd := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", target, ".")
	cmd.Dir = packageDir(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build GUI adapter: %v\n%s", err, output)
	}
	return target
}

func TestWindowlessSchedulerHelper(t *testing.T) {
	if os.Getenv(schedulerHelperModeEnv) != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv(schedulerHelperReportEnv), []byte("started"), 0o600); err != nil {
		t.Fatal(err)
	}
}
