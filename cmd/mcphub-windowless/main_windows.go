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

	"golang.org/x/sys/windows"
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
	standardFiles, err := prepareWindowlessStandardFiles(os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		writeLaunchFailure(diagnostics, err)
		return windowlessLaunchExitCode
	}
	defer standardFiles.closeOwned()
	if _, err := processowner.StartWithJobFiles(job, child, standardFiles.stdin, standardFiles.stdout, standardFiles.stderr); err != nil {
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

type windowlessStandardFiles struct {
	stdin  *os.File
	stdout *os.File
	stderr *os.File
	owned  []*os.File
}

func prepareWindowlessStandardFiles(stdin, stdout, stderr *os.File) (*windowlessStandardFiles, error) {
	files := &windowlessStandardFiles{}
	for _, candidate := range []struct {
		name     string
		original *os.File
		flags    int
		target   **os.File
	}{
		{name: "stdin", original: stdin, flags: os.O_RDONLY, target: &files.stdin},
		{name: "stdout", original: stdout, flags: os.O_WRONLY, target: &files.stdout},
		{name: "stderr", original: stderr, flags: os.O_WRONLY, target: &files.stderr},
	} {
		file, owned, err := usableWindowlessStandardFile(candidate.name, candidate.original, candidate.flags)
		if err != nil {
			files.closeOwned()
			return nil, err
		}
		*candidate.target = file
		if owned {
			files.owned = append(files.owned, file)
		}
	}
	return files, nil
}

func usableWindowlessStandardFile(name string, original *os.File, flags int) (*os.File, bool, error) {
	if original != nil {
		handle := original.Fd()
		if handle != 0 && handle != ^uintptr(0) {
			if _, err := windows.GetFileType(windows.Handle(handle)); err == nil {
				return original, false, nil
			} else if !errors.Is(err, windows.ERROR_INVALID_HANDLE) {
				return nil, false, fmt.Errorf("probe inherited %s handle: %w", name, err)
			}
		}
	}

	replacement, err := os.OpenFile(os.DevNull, flags, 0)
	if err != nil {
		return nil, false, fmt.Errorf("open NUL for missing %s handle: %w", name, err)
	}
	return replacement, true, nil
}

func (files *windowlessStandardFiles) closeOwned() {
	if files == nil {
		return
	}
	for _, file := range files.owned {
		_ = file.Close()
	}
	files.owned = nil
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
