//go:build linux

package process

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const linuxDeletedSuffix = " (deleted)"

func HoldPIDForTermination(pid int) (HeldPIDGeneration, error) {
	return nil, fmt.Errorf("%w: held PID generation unavailable for PID %d on this platform", ErrProcessIdentityUnsupported, pid)
}

// PIDExecutableMatches compares /proc/<pid>/exe against expectedPath.
func PIDExecutableMatches(pid int, expectedPath string) bool {
	return verifyPIDExecutablePath(pid, expectedPath) == nil
}

func verifyPIDExecutablePath(pid int, expectedPath string) error {
	got, err := pidExecutablePath(pid)
	if err != nil {
		if errors.Is(err, ErrProcessAlreadyExited) {
			return err
		}
		return fmt.Errorf("%w: PID %d executable proof unavailable: %v", ErrProcessIdentityMismatch, pid, err)
	}
	// IDENTITY-GATE SHORT-CIRCUIT — the POSIX twin of the branch in
	// executablePathMatches (pid_identity_windows.go); see that comment for the
	// full invariant. In short: this gate proves the process at this PID is the
	// binary we spawned rather than an unrelated process that inherited a
	// recycled PID. Skipping normalization when the two spellings are already
	// identical cannot weaken it, because this branch only ever answers
	// "matches" — every difference still takes the full normalize-and-compare
	// below, unchanged. Nothing is retained between calls, so there is no entry
	// that can go stale against a binary swap or a repointed symlink.
	if got == expectedPath {
		return nil
	}
	expected, err := normalizeExpectedExecutablePath(expectedPath)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("%w: PID %d executable path mismatch", ErrProcessIdentityMismatch, pid)
	}
	return nil
}

func VerifyPIDIdentity(proof PIDIdentityProof) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	if err := verifyPIDExecutablePath(proof.PID, proof.ExecutablePath); err != nil {
		return err
	}
	recorded, err := parseExpectedStartedAt(proof.StartedAt)
	if err != nil {
		return err
	}
	observed, ok := ProcessStartTime(proof.PID)
	if !ok {
		state, stateErr := QueryPIDState(proof.PID)
		if stateErr == nil && state == PIDStateDead {
			return fmt.Errorf("process: PID %d exited before start-time proof: %w", proof.PID, ErrProcessAlreadyExited)
		}
		return fmt.Errorf("%w: PID %d start-time proof unavailable", ErrProcessIdentityMismatch, proof.PID)
	}
	if !startTimesMatchWithin(recorded, observed, pidIdentityStartToleranceFor(proof)) {
		return fmt.Errorf("%w: PID %d started_at mismatch recorded=%s observed=%s", ErrProcessIdentityMismatch, proof.PID, recorded.Format(timeFormatRFC3339Nano()), observed.Format(timeFormatRFC3339Nano()))
	}
	return nil
}

func TerminatePIDWithIdentity(proof PIDIdentityProof) error {
	return signalPIDWithIdentity(proof, syscall.SIGTERM)
}

func KillPIDWithIdentity(proof PIDIdentityProof, sig syscall.Signal) error {
	return signalPIDWithIdentity(proof, sig)
}

func signalPIDWithIdentity(proof PIDIdentityProof, sig syscall.Signal) error {
	if proof.PID <= 0 {
		return fmt.Errorf("process: invalid PID %d", proof.PID)
	}
	fd, err := unix.PidfdOpen(proof.PID, 0)
	if err == nil {
		defer unix.Close(fd)
		if verifyErr := VerifyPIDIdentity(proof); verifyErr != nil {
			return verifyErr
		}
		if sendErr := unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0); sendErr != nil {
			if errors.Is(sendErr, syscall.ESRCH) {
				return fmt.Errorf("process: PID %d already exited before pidfd signal: %w", proof.PID, ErrProcessAlreadyExited)
			}
			return fmt.Errorf("process: pidfd signal %s to PID %d: %w", sig.String(), proof.PID, sendErr)
		}
		return nil
	}
	if !errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.EPERM) {
		return fmt.Errorf("process: pidfd_open PID %d: %w", proof.PID, err)
	}
	if verifyErr := VerifyPIDIdentity(proof); verifyErr != nil {
		return verifyErr
	}
	if killErr := syscall.Kill(proof.PID, sig); killErr != nil {
		if errors.Is(killErr, syscall.ESRCH) {
			return fmt.Errorf("process: PID %d already exited before %s: %w", proof.PID, sig.String(), ErrProcessAlreadyExited)
		}
		return fmt.Errorf("process: send %s to PID %d: %w", sig.String(), proof.PID, killErr)
	}
	return nil
}

func pidExecutablePath(pid int) (string, error) {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("process: PID %d exited before executable proof: %w", pid, ErrProcessAlreadyExited)
		}
		return "", err
	}
	target = strings.TrimSuffix(target, linuxDeletedSuffix)
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	if abs, err := filepath.Abs(target); err == nil {
		target = abs
	}
	return filepath.Clean(target), nil
}

func timeFormatRFC3339Nano() string { return "2006-01-02T15:04:05.999999999Z07:00" }
