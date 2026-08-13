//go:build linux

package gui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Residual 1(a) review fix: classifyKillError must carry ambiguous kernel
// errors as Indeterminate, never coerce them to Alive:false — only ESRCH
// (kill(2)'s documented "no such process" signal) may claim definitive
// death.
//
// UNVERIFIED ON A REAL LINUX HOST this session (the implementer environment
// for this change is Windows-only — no Linux runner was available to
// execute this file). Verification step: run this file's tests, plus a
// smoke test that Kill(exitedPID, 0) returns exactly syscall.ESRCH, on a
// real Linux CI runner or dev host before relying on the ESRCH mapping
// beyond this static/logical review. This is the same construction already
// smoke-tested for the equivalent Windows classifier
// (classifyOpenProcessError in probe_windows_test.go).
// ---------------------------------------------------------------------------

// TestClassifyKillError_ESRCHIsDefinitiveDead is the ONLY case that may
// report Alive:false.
func TestClassifyKillError_ESRCHIsDefinitiveDead(t *testing.T) {
	got, err := classifyKillError(syscall.ESRCH)
	if err != nil {
		t.Fatalf("err = %v, want nil (a definitive dead verdict reports no error)", err)
	}
	if got.Alive {
		t.Errorf("Alive = true, want false")
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false (this is the ONE definitive-dead case)")
	}
}

// TestClassifyKillError_EPERMIsAliveDenied pins the pre-existing
// EPERM-mirroring behavior: permission denied means the process EXISTS but
// we cannot signal it — never Indeterminate, never dead.
func TestClassifyKillError_EPERMIsAliveDenied(t *testing.T) {
	got, err := classifyKillError(syscall.EPERM)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !got.Alive || !got.Denied {
		t.Errorf("got = %+v, want Alive:true Denied:true", got)
	}
	if got.Indeterminate {
		t.Errorf("Indeterminate = true, want false")
	}
}

// TestClassifyKillError_EveryOtherErrorIsIndeterminate reproduces the
// residual 1(a) danger directly: BEFORE this fix, "ESRCH or other: not
// alive" collapsed every non-EPERM Kill(0) failure into Alive:false — which
// single_instance.go's probeOnce turns into VerdictDeadPID, the ONLY class
// that authorizes a destructive relaunch/kill. kill(2) documents only
// EINVAL/EPERM/ESRCH and EINVAL cannot occur for signal 0, but a future
// kernel/libc surprise (or any unrecognized errno) must never reach
// VerdictDeadPID.
//
// MUTATION: revert classifyKillError to return ProcessIdentity{Alive: false}
// for any error that is not EPERM — this test's "want Indeterminate:true,
// Alive:false" assertions fail for EINVAL and the synthetic error.
func TestClassifyKillError_EveryOtherErrorIsIndeterminate(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"EINVAL (documented but unreachable for signal 0)", syscall.EINVAL},
		{"unrecognized synthetic error", errors.New("injected ambiguous kernel failure")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := classifyKillError(tc.err)
			if got.Alive {
				t.Errorf("Alive = true, want false (an ambiguous error must never claim liveness either)")
			}
			if !got.Indeterminate {
				t.Errorf("Indeterminate = false, want true — this error must NOT be treated as proof of death")
			}
			if !errors.Is(err, tc.err) {
				t.Errorf("returned err = %v, want the original classified error preserved (%v)", err, tc.err)
			}
		})
	}
}

const (
	pidfdTestChildDeadline = 3 * time.Second
	pidfdTestWaitDelay     = 100 * time.Millisecond
	pidfdTestReapBound     = time.Second
)

func TestRetainedPIDFDAlive_LinuxChildHelper(_ *testing.T) {
	if os.Getenv(pidfdTestChildEnv) != "1" {
		return
	}

	ready := os.NewFile(uintptr(3), "child-ready")
	release := os.NewFile(uintptr(4), "parent-release")
	if ready == nil || release == nil {
		_, _ = fmt.Fprintln(os.Stderr, "open child barrier files")
		os.Exit(2)
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "signal child readiness: %v\n", err)
		os.Exit(2)
	}
	if os.Getenv(pidfdTestChildStallEnv) == "1" {
		select {}
	}
	if _, err := io.ReadFull(release, make([]byte, 1)); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "wait for parent release: %v\n", err)
		os.Exit(2)
	}
	os.Exit(0)
}

type pidfdTestChild struct {
	cmd           *exec.Cmd
	cancel        context.CancelFunc
	readyReader   *os.File
	readyWriter   *os.File
	releaseReader *os.File
	releaseWriter *os.File
	waited        bool
	released      bool
}

func startPIDFDTestChild(t *testing.T, stalled bool) *pidfdTestChild {
	t.Helper()

	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create child readiness pipe: %v", err)
	}
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		_ = readyReader.Close()
		_ = readyWriter.Close()
		t.Fatalf("create child release pipe: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), pidfdTestChildDeadline)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRetainedPIDFDAlive_LinuxChildHelper$")
	cmd.WaitDelay = pidfdTestWaitDelay
	cmd.Env = append(withoutGUITestHelperEnvironment(os.Environ(), runtime.GOOS), pidfdTestChildEnv+"=1", "GORACE=atexit_sleep_ms=0")
	if stalled {
		cmd.Env = append(cmd.Env, pidfdTestChildStallEnv+"=1")
	}
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{readyWriter, releaseReader}
	if err := cmd.Start(); err != nil {
		cancel()
		_ = readyReader.Close()
		_ = readyWriter.Close()
		_ = releaseReader.Close()
		_ = releaseWriter.Close()
		t.Fatalf("start child: %v", err)
	}

	child := &pidfdTestChild{
		cmd:           cmd,
		cancel:        cancel,
		readyReader:   readyReader,
		readyWriter:   readyWriter,
		releaseReader: releaseReader,
		releaseWriter: releaseWriter,
	}
	t.Cleanup(func() { child.cleanup(t) })

	// Nil each parent copy before Close: a failed Close still consumes the file,
	// so cleanup must not issue a deterministic second close.
	if err := child.close(&child.readyWriter); err != nil {
		t.Fatalf("close parent copy of child readiness writer: %v", err)
	}
	if err := child.close(&child.releaseReader); err != nil {
		t.Fatalf("close parent copy of child release reader: %v", err)
	}
	return child
}

func (child *pidfdTestChild) close(file **os.File) error {
	f := *file
	*file = nil
	if f == nil {
		return nil
	}
	return f.Close()
}

func (child *pidfdTestChild) release(t *testing.T) {
	t.Helper()
	if child.released {
		return
	}
	child.released = true
	writer := child.releaseWriter
	child.releaseWriter = nil
	if writer == nil {
		return
	}
	if _, err := writer.Write([]byte{1}); err != nil {
		t.Errorf("release child: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Errorf("close child release pipe: %v", err)
	}
}

func (child *pidfdTestChild) wait() error {
	if child.waited {
		return errors.New("child already reaped")
	}
	err := child.cmd.Wait()
	// Set waited before examining err: cmd.Wait must be called exactly once.
	child.waited = true
	return err
}

func (child *pidfdTestChild) cancelAndWait() (error, error) {
	child.cancel()
	var killErr error
	if err := child.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		killErr = fmt.Errorf("kill child: %w", err)
	}
	return killErr, child.wait()
}

func expectedPIDFDTestCancellation(err error) bool {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func (child *pidfdTestChild) cleanup(t *testing.T) {
	if !child.waited {
		// CommandContext's Kill plus WaitDelay bounds this cleanup to the context
		// deadline plus pidfdTestWaitDelay. Cancel and kill happen before its only Wait.
		killErr, waitErr := child.cancelAndWait()
		if killErr != nil {
			t.Errorf("kill child during cleanup: %v", killErr)
		}
		if !expectedPIDFDTestCancellation(waitErr) {
			t.Errorf("reap child during cleanup: %v", waitErr)
		}
	}
	child.cancel()
	if err := child.close(&child.readyReader); err != nil {
		t.Errorf("close child readiness pipe: %v", err)
	}
	if err := child.close(&child.readyWriter); err != nil {
		t.Errorf("close child readiness writer: %v", err)
	}
	if err := child.close(&child.releaseReader); err != nil {
		t.Errorf("close child release reader: %v", err)
	}
	if err := child.close(&child.releaseWriter); err != nil {
		t.Errorf("close child release pipe: %v", err)
	}
}

func waitForPIDFDTestChildReady(t *testing.T, readyReader *os.File) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		fds := []unix.PollFd{{Fd: int32(readyReader.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		if err != nil {
			t.Fatalf("poll child readiness: %v", err)
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			var ready [1]byte
			if _, err := io.ReadFull(readyReader, ready[:]); err != nil {
				t.Fatalf("read child readiness: %v", err)
			}
			return
		}
		if n != 0 || fds[0].Revents != 0 {
			t.Fatalf("unexpected child readiness poll state: n=%d revents=%#x", n, fds[0].Revents)
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not report readiness before pidfd acquisition")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRetainedProcessID_LinuxSelfPinsStablePIDFD(t *testing.T) {
	identity, err := retainedProcessIDImpl(os.Getpid())
	if err != nil {
		t.Fatalf("retainedProcessIDImpl(self): %v", err)
	}
	if !identity.Alive || identity.Denied || identity.Handle == 0 {
		t.Fatalf("retained self identity = %+v, want live permitted pidfd", identity)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("close retained self identity: %v", err)
	}
	if err := identity.Close(); err != nil {
		t.Fatalf("second close retained self identity: %v", err)
	}
}

func TestSameLinuxProcessIdentity_RequiresExactStableSnapshot(t *testing.T) {
	base := ProcessIdentity{Alive: true, ImagePath: "/proc/self/exe", Cmdline: []string{"mcphub", "gui"}}
	if !sameLinuxProcessIdentity(base, base) {
		t.Fatal("identical snapshots did not compare equal")
	}
	changed := base
	changed.Cmdline = []string{"mcphub", "daemon"}
	if sameLinuxProcessIdentity(base, changed) {
		t.Fatal("different argv snapshots compared equal")
	}
}

func TestRetainedPIDFDAlive_LinuxRejectsExitedUnreapedChild(t *testing.T) {
	child := startPIDFDTestChild(t, false)
	pidfd := -1
	t.Cleanup(func() {
		if pidfd >= 0 {
			if err := unix.Close(pidfd); err != nil {
				t.Errorf("close pidfd: %v", err)
			}
		}
	})

	waitForPIDFDTestChildReady(t, child.readyReader)

	pidfd, err := unix.PidfdOpen(child.cmd.Process.Pid, 0)
	if err != nil {
		if errors.Is(err, unix.ENOSYS) {
			t.Skipf("pidfd_open unavailable: %v", err)
		}
		t.Fatalf("pidfd_open(%d): %v", child.cmd.Process.Pid, err)
	}
	if err := retainedPIDFDAlive(pidfd); err != nil {
		t.Fatalf("retainedPIDFDAlive(live child): %v", err)
	}

	child.release(t)
	deadline := time.Now().Add(time.Second)
	for {
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		if err != nil {
			t.Fatalf("poll child pidfd: %v", err)
		}
		if fds[0].Revents&unix.POLLIN != 0 {
			break
		}
		if n != 0 || fds[0].Revents != 0 {
			t.Fatalf("unexpected pidfd poll state before child exit: n=%d revents=%#x", n, fds[0].Revents)
		}
		if time.Now().After(deadline) {
			t.Fatal("pidfd did not become readable after child exit without a wait")
		}
		time.Sleep(time.Millisecond)
	}

	if err := retainedPIDFDAlive(pidfd); err == nil {
		t.Fatal("retainedPIDFDAlive accepted an exited, unreaped child pidfd")
	}

	err = child.wait()
	if err != nil {
		t.Fatalf("reap child: %v", err)
	}
	if err := retainedPIDFDAlive(pidfd); err == nil {
		t.Fatal("retainedPIDFDAlive accepted an already-reaped child pidfd")
	}
}

func TestRetainedPIDFDAlive_LinuxStalledChildCancellationReapsBoundedly(t *testing.T) {
	child := startPIDFDTestChild(t, true)
	waitForPIDFDTestChildReady(t, child.readyReader)

	started := time.Now()
	killErr, waitErr := child.cancelAndWait()
	if elapsed := time.Since(started); elapsed > pidfdTestReapBound {
		t.Fatalf("cancel and reap stalled child took %v, want at most %v", elapsed, pidfdTestReapBound)
	}
	if killErr != nil {
		t.Fatalf("kill stalled child: %v", killErr)
	}
	if !expectedPIDFDTestCancellation(waitErr) {
		t.Fatalf("reap stalled child after cancellation: %v", waitErr)
	}
}
