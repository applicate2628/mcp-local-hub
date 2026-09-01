//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	processowner "mcp-local-hub/internal/process"
)

const (
	windowlessLaunchFailureID = "E_WINDOWS_WINDOWLESS_LAUNCH"
	windowlessLaunchExitCode  = 1
)

func main() {
	os.Exit(runWindowless(os.Args[1:], os.Stderr))
}

func runWindowless(args []string, diagnostics io.Writer) int {
	target, err := siblingCLIPath()
	if err != nil {
		writeLaunchFailure(diagnostics, err)
		return windowlessLaunchExitCode
	}

	job, err := processowner.NewJob(processowner.JobOptions{
		KillOnClose: true,
		BreakawayOK: true,
	})
	if err != nil {
		writeLaunchFailure(diagnostics, err)
		return windowlessLaunchExitCode
	}
	jobOpen := true
	defer func() {
		if jobOpen {
			_ = job.Close()
		}
	}()

	child := exec.Command(target, args...)
	child.Env = os.Environ()
	if _, err := processowner.StartWithJobFiles(job, child, os.Stdin, os.Stdout, os.Stderr); err != nil {
		writeLaunchFailure(diagnostics, err)
		return windowlessLaunchExitCode
	}

	waitErr := child.Wait()
	closeErr := job.Close()
	jobOpen = false
	if closeErr != nil {
		writeLaunchFailure(diagnostics, fmt.Errorf("close child Job Object: %w", closeErr))
		return windowlessLaunchExitCode
	}
	if waitErr == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode()
	}
	writeLaunchFailure(diagnostics, fmt.Errorf("wait for sibling mcphub.exe: %w", waitErr))
	return windowlessLaunchExitCode
}

func siblingCLIPath() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve adapter executable: %w", err)
	}
	absolute, err := filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("resolve adapter absolute path: %w", err)
	}
	return filepath.Join(filepath.Dir(absolute), "mcphub.exe"), nil
}

func writeLaunchFailure(writer io.Writer, err error) {
	if writer == nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "%s: %v", windowlessLaunchFailureID, err)
	var errno syscall.Errno
	if errors.As(err, &errno) {
		_, _ = fmt.Fprintf(writer, " (win32=%d)", uint32(errno))
	}
	_, _ = fmt.Fprintln(writer)
}
