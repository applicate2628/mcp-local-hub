package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"testing"
	"time"
)

type observedScriptedContainedChild struct {
	*scriptedContainedChild
	settlements      int
	deadline         time.Time
	expectedDeadline time.Time
	forced           bool
}

func (c *observedScriptedContainedChild) prepareContainedObservation() {}
func (c *observedScriptedContainedChild) containedCreated() bool       { return c.startCalls.Load() > 0 }
func (c *observedScriptedContainedChild) containedStartLive(context.Context) (bool, error) {
	return true, nil
}
func (c *observedScriptedContainedChild) closeContainedJobBackstop() error { return nil }
func (c *observedScriptedContainedChild) containedRootIdentity() (ContainedRootIdentity, error) {
	return ContainedRootIdentity{Known: true, PID: 4242, StartedAt: "2026-10-11T00:00:00Z"}, nil
}
func (c *observedScriptedContainedChild) settleContained(ctx context.Context, natural bool) (ContainedNativeSnapshot, error) {
	c.settlements++
	c.deadline, _ = ctx.Deadline()
	c.forced = !natural
	if c.startCalls.Load() == 0 {
		return ContainedNativeSnapshot{NoChildKnown: true, AccountingKnown: true, TerminalAction: "not_requested"}, nil
	}
	identity, _ := c.containedRootIdentity()
	if !natural {
		select {
		case c.waitCh <- containedWaitResult{exited: true, exitCode: 1}:
		default:
		}
	}
	action := "not_requested"
	if !natural {
		action = "request_succeeded"
	}
	return ContainedNativeSnapshot{RootIdentity: identity, MembersExited: true, AccountingKnown: true, InitialTotal: 1, FinalActive: 0, TerminalAction: action}, nil
}
func observedContainedFixture(t *testing.T, code int, options func(*ContainedStreamOptions), configure func(*observedScriptedContainedChild)) (ContainedStreamReceiptV1, error, *observedScriptedContainedChild) {
	t.Helper()
	child := &observedScriptedContainedChild{scriptedContainedChild: newScriptedContainedChild(containedWaitResult{exited: true, exitCode: code})}
	if configure != nil {
		configure(child)
	}
	var stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	opts := ContainedStreamOptions{CleanupTimeout: time.Second, AbsoluteCleanupDeadline: end.Add(time.Second), StdoutByteLimit: 64, StderrByteLimit: 64, Stderr: &stderr}
	if options != nil {
		options(&opts)
	}
	child.expectedDeadline = opts.AbsoluteCleanupDeadline
	deps := containedStreamDependencies{newChild: func(*exec.Cmd) (containedChild, error) { return child, nil }, pipe: func() (containedReadFile, containedWriteFile, error) {
		r, w := newMemoryContainedPipe()
		return r, w, nil
	}, openNull: func() (containedInputFile, error) { return &fakeContainedInput{}, nil }}
	var receipt ContainedStreamReceiptV1
	err := runContainedStreamCore(ctx, &exec.Cmd{Path: "synthetic"}, opts, func(r io.Reader) error { _, e := io.Copy(io.Discard, r); return e }, deps, &receipt)
	return receipt, err, child
}

func TestContainedObservedNaturalRefusal(t *testing.T) {
	r, err, c := observedContainedFixture(t, 2, nil, nil)
	var refusal *ContainedRunError
	if !errors.As(err, &refusal) || refusal.ExitCode == nil || *refusal.ExitCode != 2 || !r.RootWaitKnown || r.RootExitCode != 2 || r.TerminalAction != "not_requested" || !r.NativeSettlement.Complete() || !r.IOSettlement.Complete() {
		t.Fatalf("natural nonzero receiving: %+v %v", r, err)
	}
	if c.settlements != 1 || c.waitCalls.Load() != 1 || c.closeCalls.Load() != 1 || c.terminateCalls.Load() != 0 {
		t.Fatal("duplicated original action/wait/close")
	}
	if !c.deadline.Equal(c.expectedDeadline) {
		t.Fatal("original cleanup deadline renewed")
	}
}
func TestContainedObservedCloseFault(t *testing.T) {
	r, err, c := observedContainedFixture(t, 0, nil, func(c *observedScriptedContainedChild) {
		c.closeFn = func() error { return errors.New("synthetic-close-fault") }
	})
	if err == nil || r.NativeSettlement.Complete() || r.NativeSettlement.Snapshot().HandlesClosed || c.closeCalls.Load() != 1 {
		t.Fatal("close fault became native settlement")
	}
}
func TestContainedObservedOutputCap(t *testing.T) {
	r, err, c := observedContainedFixture(t, 0, func(o *ContainedStreamOptions) { o.StdoutByteLimit = 2 }, func(c *observedScriptedContainedChild) {
		c.startFn = func(_ containedInputFile, out, _ containedWriteFile) error {
			_, e := out.Write([]byte("three"))
			return e
		}
	})
	if !errors.Is(err, ErrContainedOutputLimit) || r.IOSettlement.Complete() || !r.IOSettlement.Stdout.LimitExceeded || c.settlements != 1 {
		t.Fatal("output cap weakened receiving", err)
	}
}
func TestContainedObservedMissingDeadline(t *testing.T) {
	r, err, c := observedContainedFixture(t, 0, func(o *ContainedStreamOptions) { o.AbsoluteCleanupDeadline = time.Time{} }, nil)
	if err == nil || r.StartAttempted || c.startCalls.Load() != 0 || c.closeCalls.Load() != 0 {
		t.Fatal("missing deadline allocated actor")
	}
}
func TestContainedObservedCloseSlotOnce(t *testing.T) {
	calls := 0
	slot := ownCloser(closerFunc(func() error { calls++; return errors.New("close-fault") }))
	_ = slot.close()
	_ = slot.close()
	v := slot.snapshot()
	if calls != 1 || !v.CloseAttempted || v.Closed || v.CloseError == nil {
		t.Fatal("uncertain slot retried or reported closed")
	}
}
func TestContainedObservedNoChildPartialAcquisition(t *testing.T) {
	c := &observedScriptedContainedChild{scriptedContainedChild: newScriptedContainedChild(containedWaitResult{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	deps := containedStreamDependencies{newChild: func(*exec.Cmd) (containedChild, error) { return c, nil }, pipe: func() (containedReadFile, containedWriteFile, error) {
		return nil, nil, errors.New("pipe-acquisition-fault")
	}, openNull: func() (containedInputFile, error) { t.Fatal("unreached acquisition"); return nil, nil }}
	var r ContainedStreamReceiptV1
	err := runContainedStreamCore(ctx, &exec.Cmd{Path: "synthetic"}, ContainedStreamOptions{CleanupTimeout: time.Second, AbsoluteCleanupDeadline: end.Add(time.Second), StdoutByteLimit: 64, StderrByteLimit: 64, Stderr: io.Discard}, func(io.Reader) error { return nil }, deps, &r)
	if err == nil || r.StartAttempted || !r.NativeSettlement.Complete() || !r.NativeSettlement.Snapshot().NoChildKnown || !r.IOSettlement.Complete() || c.settlements != 1 || c.closeCalls.Load() != 1 {
		t.Fatal("pre-child acquisition lost original release", err)
	}
}
