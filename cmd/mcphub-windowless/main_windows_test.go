//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

const (
	adapterHelperModeEnv   = "MCPHUB_WINDOWLESS_TEST_HELPER_MODE"
	adapterHelperReportEnv = "MCPHUB_WINDOWLESS_TEST_REPORT"
	adapterHelperPIDEnv    = "MCPHUB_WINDOWLESS_TEST_PID"
	adapterForwardedEnv    = "MCPHUB_WINDOWLESS_TEST_FORWARDED"
)

type adapterHelperReport struct {
	Args  []string `json:"args"`
	Env   string   `json:"env"`
	CWD   string   `json:"cwd"`
	Stdin string   `json:"stdin"`
}

func TestAdapterForwardsProcessContractAndMirrorsExit(t *testing.T) {
	dir := t.TempDir()
	adapter := buildAdapter(t, dir)
	installTestHelperSibling(t, dir)
	childCWD := filepath.Join(dir, "child cwd")
	if err := os.Mkdir(childCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(dir, "report.json")
	wantArgs := []string{
		"-test.run=^TestWindowlessAdapterHelper$",
		"--",
		"alpha",
		"space value",
		`quote"inside`,
		"",
	}
	cmd := exec.Command(adapter, wantArgs...)
	cmd.Dir = childCWD
	cmd.Env = append(os.Environ(),
		adapterHelperModeEnv+"=contract",
		adapterHelperReportEnv+"="+reportPath,
		adapterForwardedEnv+"=exact-value",
	)
	cmd.Stdin = strings.NewReader("stdin-through-adapter\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("adapter exit error=%v, code=%v; want exact child code 23", err, exitCode(exitErr))
	}
	if got := stdout.String(); got != "adapter-helper-stdout" {
		t.Fatalf("stdout=%q, want inherited child stdout", got)
	}
	if got := stderr.String(); got != "adapter-helper-stderr" {
		t.Fatalf("stderr=%q, want inherited child stderr", got)
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read helper report: %v", err)
	}
	var report adapterHelperReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatalf("decode helper report: %v", err)
	}
	if strings.Join(report.Args, "\x00") != strings.Join(wantArgs, "\x00") {
		t.Fatalf("child args=%q, want exact %q", report.Args, wantArgs)
	}
	if report.Env != "exact-value" {
		t.Fatalf("child env=%q, want exact-value", report.Env)
	}
	if report.CWD != childCWD {
		t.Fatalf("child cwd=%q, want %q", report.CWD, childCWD)
	}
	if report.Stdin != "stdin-through-adapter\n" {
		t.Fatalf("child stdin=%q, want forwarded bytes", report.Stdin)
	}
}

func TestAdapterMissingSiblingFailsLoudWithWin32Cause(t *testing.T) {
	dir := t.TempDir()
	adapter := buildAdapter(t, dir)
	cmd := exec.Command(adapter)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatal("adapter without sibling exited successfully")
	}
	got := stderr.String()
	if !strings.Contains(got, "E_WINDOWS_WINDOWLESS_LAUNCH") {
		t.Fatalf("stderr=%q, missing stable failure ID", got)
	}
	if !strings.Contains(got, "win32=2") {
		t.Fatalf("stderr=%q, missing ERROR_FILE_NOT_FOUND Win32 cause", got)
	}
}

func TestTerminatingAdapterKillsDirectChild(t *testing.T) {
	dir := t.TempDir()
	adapter := buildAdapter(t, dir)
	installTestHelperSibling(t, dir)
	pidPath := filepath.Join(dir, "child.pid")
	cmd := exec.Command(adapter, "-test.run=^TestWindowlessAdapterHelper$")
	cmd.Env = append(os.Environ(),
		adapterHelperModeEnv+"=hold",
		adapterHelperPIDEnv+"="+pidPath,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start adapter: %v", err)
	}
	adapterWaited := false
	defer func() {
		if !adapterWaited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	childPID := waitForAdapterPIDFile(t, pidPath, 10*time.Second)
	childHandle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(childPID))
	if err != nil {
		t.Fatalf("open direct child pid=%d: %v", childPID, err)
	}
	defer windows.CloseHandle(childHandle)
	defer func() {
		_ = windows.TerminateProcess(childHandle, 1)
		_, _ = windows.WaitForSingleObject(childHandle, 5000)
	}()

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("terminate adapter: %v", err)
	}
	_ = cmd.Wait()
	adapterWaited = true
	if status, err := windows.WaitForSingleObject(childHandle, 5000); err != nil {
		t.Fatalf("wait for Job-reaped child: %v", err)
	} else if status != windows.WAIT_OBJECT_0 {
		t.Fatalf("child wait status=%#x, want WAIT_OBJECT_0 after adapter termination", status)
	}
}

func TestWindowlessAdapterHelper(t *testing.T) {
	switch os.Getenv(adapterHelperModeEnv) {
	case "contract":
		stdin, err := io.ReadAll(os.Stdin)
		if err != nil {
			t.Fatal(err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		report := adapterHelperReport{
			Args:  os.Args[1:],
			Env:   os.Getenv(adapterForwardedEnv),
			CWD:   cwd,
			Stdin: string(stdin),
		}
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(os.Getenv(adapterHelperReportEnv), body, 0o600); err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprint(os.Stdout, "adapter-helper-stdout")
		_, _ = fmt.Fprint(os.Stderr, "adapter-helper-stderr")
		os.Exit(23)
	case "hold":
		if err := os.WriteFile(os.Getenv(adapterHelperPIDEnv), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Minute)
	}
}

func buildAdapter(t *testing.T, dir string) string {
	t.Helper()
	target := filepath.Join(dir, "mcphub-windowless.exe")
	cmd := exec.Command("go", "build", "-o", target, ".")
	cmd.Dir = packageDir(t)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build adapter: %v\n%s", err, output)
	}
	return target
}

func installTestHelperSibling(t *testing.T, dir string) {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(filepath.Join(dir, "mcphub.exe"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func waitForAdapterPIDFile(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(body)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("invalid PID file %q: %v", body, parseErr)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("PID file %s was not written within %v", path, timeout)
	return 0
}

func exitCode(err *exec.ExitError) int {
	if err == nil {
		return 0
	}
	return err.ExitCode()
}
