//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This harmless child uses the test executable, no Go compiler or Hub state.
// Its inherited stdin is an explicit EOF barrier; it exits with a known code.
func TestForegroundFixtureProcess(t *testing.T) {
	if os.Getenv("MCPHUB_FOREGROUND_TEST_CHILD") != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte{'R'}); err != nil {
		os.Exit(91)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		os.Exit(92)
	}
	os.Exit(37)
}

type foregroundTestAdmission struct {
	hook          func(*AtCreateJobProcess) error
	close         func() error
	hooks, closes int
	once          sync.Once
	closeError    error
}

func (a *foregroundTestAdmission) AfterCreateBeforeResume(p *AtCreateJobProcess) error {
	a.hooks++
	return a.hook(p)
}
func (a *foregroundTestAdmission) Close() error {
	a.once.Do(func() {
		a.closes++
		if a.close != nil {
			a.closeError = a.close()
		}
	})
	return a.closeError
}

func foregroundTestContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok || time.Until(deadline) <= RetainedTerminationAllowance() {
		t.Fatal("test deadline must reserve the existing native cleanup allowance")
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-RetainedTerminationAllowance()))
	t.Cleanup(cancel)
	return ctx
}

func foregroundTestCommand(t *testing.T) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestForegroundFixtureProcess$")
	cmd.Env = append(os.Environ(), "MCPHUB_FOREGROUND_TEST_CHILD=1")
	return cmd
}

func foregroundTestFiles(t *testing.T) (stdin, input, stdout, output, stderr *os.File) {
	t.Helper()
	var err error
	stdin, input, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	output, stdout, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderr, err = os.Create(filepath.Join(t.TempDir(), "stderr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []*os.File{stdin, input, stdout, output, stderr} {
		file := file
		t.Cleanup(func() { _ = file.Close() })
	}
	return
}

// foregroundOriginalPrimarySuspended borrows only the original creator's
// retained primary thread, inside its before-resume hook. SuspendThread's
// previous count proves CREATE_SUSPENDED independently of child scheduling.
// The probe's own increment is restored exactly once before any assertion;
// neither the borrowed thread nor any process is reopened or closed here.
func foregroundOriginalPrimarySuspended(original *AtCreateJobProcess) (resultErr error) {
	if original == nil {
		return errors.New("original creator unavailable for suspension probe")
	}
	child, ok := original.child.(*windowsContainedChild)
	if !ok || child == nil || child.thread == 0 {
		return errors.New("original primary thread unavailable for suspension probe")
	}
	thread := child.thread
	suspend := windows.NewLazySystemDLL("kernel32.dll").NewProc("SuspendThread")
	if err := suspend.Find(); err != nil {
		return fmt.Errorf("find System32 SuspendThread: %w", err)
	}
	previous, _, callErr := suspend.Call(uintptr(thread))
	if uint32(previous) == ^uint32(0) {
		return errors.Join(errors.New("SuspendThread returned failure sentinel"), callErr)
	}

	restored := false
	var restorePrevious uint32
	var restoreErr error
	restore := func() {
		if restored {
			return
		}
		restored = true
		restorePrevious, restoreErr = windows.ResumeThread(thread)
	}
	// Register the one restoration before checking either observed count.
	defer func() {
		restore()
		if restoreErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("restore suspension probe increment: %w", restoreErr))
		}
	}()
	restore()
	if uint32(previous) != 1 {
		resultErr = errors.Join(resultErr, fmt.Errorf("original primary suspend count=%d, want 1 before production ResumeThread", uint32(previous)))
	}
	if restoreErr == nil && restorePrevious != 2 {
		resultErr = errors.Join(resultErr, fmt.Errorf("restore probe previous suspend count=%d, want 2", restorePrevious))
	}
	return resultErr
}

func TestRunForegroundOriginalAdmissionBarrierAndEOF(t *testing.T) {
	ctx := foregroundTestContext(t)
	ctx, cancel := context.WithCancel(ctx)
	stdin, input, stdout, output, stderr := foregroundTestFiles(t)
	reached, release := make(chan struct{}), make(chan struct{})
	var admission *foregroundTestAdmission
	admission = &foregroundTestAdmission{hook: func(original *AtCreateJobProcess) error {
		identity, err := original.Identity()
		if err != nil || !identity.Known || !original.Created() {
			return errors.Join(err, errors.New("missing actual original identity"))
		}
		if err := foregroundOriginalPrimarySuspended(original); err != nil {
			return err
		}
		close(reached)
		select {
		case <-release:
			return admission.Close()
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	type received struct {
		result ForegroundResult
		err    error
	}
	done := make(chan received, 1)
	cmd := foregroundTestCommand(t)
	go func() {
		result, err := RunForegroundAdmitted(ctx, cmd, stdin, stdout, stderr, admission)
		done <- received{result, err}
	}()
	receivedDone := false
	var ready chan error
	receivedReady := false
	t.Cleanup(func() {
		cancel()
		if !receivedDone {
			got := <-done
			if !got.result.Settlement.Complete() {
				t.Errorf("original child cleanup incomplete: native=%+v err=%v", got.result.Native, got.err)
			}
		}
		if ready != nil && !receivedReady {
			_ = stdout.Close()
			_ = output.Close()
			<-ready
		}
	})
	select {
	case <-reached:
	case got := <-done:
		receivedDone = true
		t.Fatalf("original admission failed before barrier: %v", got.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// No execution is possible before this exact hook returns. No timing sleep
	// or naturally narrow race window supplies this assertion.
	select {
	case got := <-done:
		receivedDone = true
		t.Fatalf("returned before release: %v", got.err)
	default:
	}
	close(release)
	ready = make(chan error, 1)
	go func() {
		b := []byte{0}
		_, err := io.ReadFull(output, b)
		if err == nil && b[0] != 'R' {
			err = errors.New("wrong child readiness byte")
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		receivedReady = true
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case got := <-done:
		receivedDone = true
		t.Fatalf("foreground returned before actual EOF: %v", got.err)
	default:
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	var got received
	select {
	case got = <-done:
		receivedDone = true
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got.err != nil || !got.result.Root.Known || got.result.Root.ExitCode != 37 || !got.result.Settlement.Complete() || admission.hooks != 1 || admission.closes != 1 {
		t.Fatalf("original foreground outcome: root=%+v native=%+v pending=%v hook=%d close=%d err=%v", got.result.Root, got.result.Native, got.result.Outcome.Pending, admission.hooks, admission.closes, got.err)
	}
	for _, file := range []*os.File{stdin, stdout, stderr} {
		if _, err := file.Stat(); err != nil {
			t.Fatalf("borrowed standard file closed: %v", err)
		}
	}
}

func TestRunForegroundFailedAdmissionAndPartialWrapperNeverResume(t *testing.T) {
	for _, partialWrapper := range []bool{false, true} {
		t.Run(map[bool]string{false: "release-failure", true: "partial-wrapper"}[partialWrapper], func(t *testing.T) {
			ctx := foregroundTestContext(t)
			stdin, _, stdout, output, stderr := foregroundTestFiles(t)
			failure := errors.New("injected pre-resume failure")
			var suspensionError error
			var admission *foregroundTestAdmission
			admission = &foregroundTestAdmission{close: func() error { return failure }, hook: func(original *AtCreateJobProcess) error {
				suspensionError = foregroundOriginalPrimarySuspended(original)
				if suspensionError != nil {
					return suspensionError
				}
				return admission.Close()
			}}
			deps := foregroundDependencies{newJob: NewJobObserved, admission: admission, start: func(job *Job, cmd *exec.Cmd, in, out, errout *os.File) (*AtCreateJobProcess, error) {
				var options *startWithJobObserverOptions
				if partialWrapper {
					options = &startWithJobObserverOptions{findProcess: func(int) (*os.Process, error) { return nil, failure }}
				}
				return startWithJobFilesOriginalCore(job, cmd, in, out, errout, options, admission, ctx)
			}}
			result, err := runForeground(ctx, foregroundTestCommand(t), stdin, stdout, stderr, deps)
			if !errors.Is(err, failure) || result.Original == nil || !result.Root.Known || !result.Settlement.Complete() || admission.closes != 1 {
				t.Fatalf("partial original lost: root=%+v native=%+v pending=%v err=%v", result.Root, result.Native, result.Outcome.Pending, err)
			}
			if partialWrapper && admission.hooks != 0 || !partialWrapper && admission.hooks != 1 {
				t.Fatal("incorrect hook invocation count")
			}
			if suspensionError != nil {
				t.Fatalf("original primary suspension proof failed: %v", suspensionError)
			}
			// Once native settlement is received, closing the borrowed write end
			// yields deterministic EOF. Any byte proves the child was resumed.
			if err := stdout.Close(); err != nil {
				t.Fatal(err)
			}
			bytes, readErr := io.ReadAll(output)
			if readErr != nil || len(bytes) != 0 {
				t.Fatalf("failed admission executed child: %q %v", bytes, readErr)
			}
		})
	}
}

func TestRunForegroundPrecancelAndAdmissionClosePreserveBothErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	failure := errors.New("admission release failure")
	admission := &foregroundTestAdmission{close: func() error { return failure }}
	result, err := RunForegroundAdmitted(ctx, exec.Command(`C:\never-executed.exe`), os.Stdin, os.Stdout, os.Stderr, admission)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, failure) || !result.Cancelled || result.Job != nil || admission.hooks != 0 || admission.closes != 1 || len(result.Outcome.Pending) != 1 {
		t.Fatalf("precancel/close facts lost: %+v %v", result, err)
	}
}

func TestRunForegroundCancellationAfterBlockedAdmissionNeverResumes(t *testing.T) {
	ctx, cancel := context.WithCancel(foregroundTestContext(t))
	stdin, _, stdout, output, stderr := foregroundTestFiles(t)
	reached, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	closeFailure := errors.New("independent cached admission close failure")
	var suspensionError error
	admission := &foregroundTestAdmission{close: func() error { return closeFailure }, hook: func(original *AtCreateJobProcess) error {
		suspensionError = foregroundOriginalPrimarySuspended(original)
		if suspensionError != nil {
			return suspensionError
		}
		close(reached)
		<-release
		// Deliberately return nil even though cancellation completed while this
		// hook was held: only the creator's final context sample may refuse.
		return nil
	}}
	resumeCalls := 0
	deps := foregroundDependencies{newJob: NewJobObserved, admission: admission, start: func(job *Job, cmd *exec.Cmd, in, out, errout *os.File) (*AtCreateJobProcess, error) {
		options := &startWithJobObserverOptions{resumeThread: func(thread windows.Handle) (uint32, error) {
			resumeCalls++
			// A regression reaching this seam performs the actual native call;
			// it cannot obtain a fabricated success or fake child receipt.
			return windows.ResumeThread(thread)
		}}
		return startWithJobFilesOriginalCore(job, cmd, in, out, errout, options, admission, ctx)
	}}
	type received struct {
		result ForegroundResult
		err    error
	}
	done := make(chan received, 1)
	cmd := foregroundTestCommand(t)
	go func() {
		result, err := runForeground(ctx, cmd, stdin, stdout, stderr, deps)
		done <- received{result, err}
	}()
	receivedDone := false
	t.Cleanup(func() {
		cancel()
		unblock()
		if !receivedDone {
			got := <-done
			if !got.result.Settlement.Complete() {
				t.Errorf("cancelled original cleanup incomplete: native=%+v err=%v", got.result.Native, got.err)
			}
		}
	})
	select {
	case <-reached:
	case got := <-done:
		receivedDone = true
		t.Fatalf("admission failed before cancellation barrier: %v", got.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	<-ctx.Done() // cancellation completion is observed before hook release
	unblock()
	got := <-done
	receivedDone = true
	if suspensionError != nil || resumeCalls != 0 || !got.result.Cancelled || !errors.Is(got.err, context.Canceled) || !errors.Is(got.err, closeFailure) || !errors.Is(got.result.AdmissionCloseError, closeFailure) || !got.result.Root.Known || got.result.Original == nil || !got.result.Settlement.Complete() || admission.hooks != 1 || admission.closes != 1 {
		t.Fatalf("post-hook cancellation facts lost: resume=%d suspension=%v cancelled=%t root=%+v native=%+v hooks=%d closes=%d err=%v", resumeCalls, suspensionError, got.result.Cancelled, got.result.Root, got.result.Native, admission.hooks, admission.closes, got.err)
	}
	for _, file := range []*os.File{stdin, stdout, stderr} {
		if _, err := file.Stat(); err != nil {
			t.Fatalf("borrowed standard file closed before cancellation receiving: %v", err)
		}
	}
	if err := stdout.Close(); err != nil {
		t.Fatal(err)
	}
	bytes, readErr := io.ReadAll(output)
	if readErr != nil || len(bytes) != 0 {
		t.Fatalf("cancelled pre-resume child produced effects: %q %v", bytes, readErr)
	}
}
