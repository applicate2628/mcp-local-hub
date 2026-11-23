//go:build windows

package process

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

type containedSetupFixtureChild struct {
	*observedScriptedContainedChild
	job *Job
}

func (c *containedSetupFixtureChild) containedSetupObservation() *ContainedSetupObservation {
	job := c.job.foregroundCloseObservation()
	return &ContainedSetupObservation{owner: c, job: c.job,
		jobClose: ContainedStreamSlot{Acquired: job.Acquired, CloseAttempted: job.CloseAttempted, Closed: job.Closed, CloseError: job.Error}}
}

func TestObservedContainedPreDispatchCancelRetainsSetupClosure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	var jobCloses int
	job, err := newJobObserved(JobOptions{KillOnClose: true}, jobAcquisitionOps{
		create:    func() (windows.Handle, error) { return 13, nil },
		configure: func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error { return nil },
		close: func(handle windows.Handle) error {
			if handle != 13 {
				t.Fatalf("closed substitute Job: %v", handle)
			}
			jobCloses++
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	child := &containedSetupFixtureChild{job: job, observedScriptedContainedChild: &observedScriptedContainedChild{
		scriptedContainedChild: newScriptedContainedChild(containedWaitResult{}),
	}}
	child.closeFn = job.Close
	deps := containedStreamDependencies{
		newChild: func(*exec.Cmd) (containedChild, error) { return child, nil },
		pipe: func() (containedReadFile, containedWriteFile, error) {
			r, w := newMemoryContainedPipe()
			return r, w, nil
		},
		openNull: func() (containedInputFile, error) {
			cancel() // both pipes and the original Job already acquired
			return &fakeContainedInput{}, nil
		},
	}
	var receipt ContainedStreamReceiptV1
	err = runContainedStreamCore(ctx, &exec.Cmd{Path: "synthetic"}, ContainedStreamOptions{
		CleanupTimeout: time.Second, AbsoluteCleanupDeadline: end.Add(time.Second), StdoutByteLimit: 64, StderrByteLimit: 64, Stderr: io.Discard,
	}, func(io.Reader) error { return nil }, deps, &receipt)
	if !errors.Is(err, context.Canceled) || receipt.StartAttempted || child.startCalls.Load() != 0 ||
		jobCloses != 1 || receipt.setup == nil || receipt.setup.job != job || !receipt.ResourcesFinalized() {
		t.Fatalf("pre-dispatch cancel lost exact setup closure or started a child: %v", err)
	}
}
