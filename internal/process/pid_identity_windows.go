//go:build windows

package process

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const terminateWaitMilliseconds = 5000

type processHandleIdentity struct {
	Basename       string
	ExecutablePath string
	CreationTime   time.Time
}

func PIDExecutableMatches(pid int, expectedPath string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ident, err := identityFromHandle(pid, h)
	if err != nil {
		return false
	}
	return executablePathMatches(ident.ExecutablePath, expectedPath)
}

func VerifyPIDIdentity(proof PIDIdentityProof) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(proof.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return fmt.Errorf("process: PID %d already exited before identity open: %w", proof.PID, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: open PID %d for identity: %w", proof.PID, err)
	}
	defer windows.CloseHandle(h)
	return verifyHandleIdentity(proof, h)
}

func TerminatePIDWithIdentity(proof PIDIdentityProof) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	access := uint32(windows.PROCESS_TERMINATE | windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION)
	h, err := windows.OpenProcess(access, false, uint32(proof.PID))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return fmt.Errorf("process: PID %d already exited before terminate open: %w", proof.PID, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: open PID %d for terminate: %w", proof.PID, err)
	}
	defer windows.CloseHandle(h)
	if err := verifyHandleIdentity(proof, h); err != nil {
		return err
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			if ev, waitErr := windows.WaitForSingleObject(h, 0); waitErr == nil && ev == windows.WAIT_OBJECT_0 {
				return fmt.Errorf("process: PID %d already exited before terminate: %w", proof.PID, ErrProcessAlreadyExited)
			}
		}
		return fmt.Errorf("process: terminate PID %d: %w", proof.PID, err)
	}
	ev, waitErr := windows.WaitForSingleObject(h, terminateWaitMilliseconds)
	if waitErr != nil {
		return fmt.Errorf("process: wait for PID %d after terminate: %w", proof.PID, waitErr)
	}
	if ev == uint32(windows.WAIT_TIMEOUT) {
		return fmt.Errorf("process: timeout waiting for PID %d to exit after terminate", proof.PID)
	}
	if ev != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("process: wait for PID %d after terminate returned unexpected code %d", proof.PID, ev)
	}
	return nil
}

func verifyHandleIdentity(proof PIDIdentityProof, h windows.Handle) error {
	ident, err := identityFromHandle(proof.PID, h)
	if err != nil {
		return err
	}
	if !executablePathMatches(ident.ExecutablePath, proof.ExecutablePath) {
		return fmt.Errorf("%w: PID %d executable path mismatch", ErrProcessIdentityMismatch, proof.PID)
	}
	if !strings.EqualFold(ident.Basename, filepath.Base(proof.ExecutablePath)) {
		return fmt.Errorf("%w: PID %d executable basename mismatch", ErrProcessIdentityMismatch, proof.PID)
	}
	recorded, err := parseExpectedStartedAt(proof.StartedAt)
	if err != nil {
		return err
	}
	observed := ident.CreationTime.UTC()
	if !startTimesMatchWithin(recorded, observed, pidIdentityStartToleranceFor(proof)) {
		return fmt.Errorf("%w: PID %d started_at mismatch recorded=%s observed=%s", ErrProcessIdentityMismatch, proof.PID, recorded.Format(time.RFC3339Nano), observed.Format(time.RFC3339Nano))
	}
	return nil
}

func identityFromHandle(pid int, h windows.Handle) (processHandleIdentity, error) {
	exe, err := queryProcessImagePath(h)
	if err != nil {
		return processHandleIdentity{}, fmt.Errorf("process: query PID %d image path: %w", pid, err)
	}
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return processHandleIdentity{}, fmt.Errorf("process: query PID %d process times: %w", pid, err)
	}
	return processHandleIdentity{
		Basename:       filepath.Base(exe),
		ExecutablePath: exe,
		CreationTime:   time.Unix(0, creation.Nanoseconds()).UTC(),
	}, nil
}

func queryProcessImagePath(h windows.Handle) (string, error) {
	size := uint32(windows.MAX_PATH)
	for {
		buf := make([]uint16, size)
		n := size
		err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n)
		if err == nil {
			return windows.UTF16ToString(buf[:n]), nil
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return "", err
		}
		size *= 2
		if size > 32768 {
			return "", fmt.Errorf("image path exceeds 32768 UTF-16 code units")
		}
	}
}

func executablePathMatches(got, expected string) bool {
	got = normalizeWindowsExecutablePath(got)
	expected = normalizeWindowsExecutablePath(expected)
	return got != "" && expected != "" && strings.EqualFold(got, expected)
}

func normalizeWindowsExecutablePath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}
