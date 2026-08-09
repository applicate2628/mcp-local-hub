package process

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryContainedPipe struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	writeClosed bool
	readClosed  bool
	ready       *sync.Cond
	readCloses  int
	writeCloses int
}

func newMemoryContainedPipe() (*memoryContainedReader, *memoryContainedWriter) {
	p := &memoryContainedPipe{}
	p.ready = sync.NewCond(&p.mu)
	return &memoryContainedReader{pipe: p}, &memoryContainedWriter{pipe: p}
}

type memoryContainedReader struct{ pipe *memoryContainedPipe }

func (r *memoryContainedReader) Read(dst []byte) (int, error) {
	r.pipe.mu.Lock()
	defer r.pipe.mu.Unlock()
	for r.pipe.buf.Len() == 0 && !r.pipe.writeClosed && !r.pipe.readClosed {
		r.pipe.ready.Wait()
	}
	if r.pipe.readClosed {
		return 0, io.ErrClosedPipe
	}
	if r.pipe.buf.Len() == 0 && r.pipe.writeClosed {
		return 0, io.EOF
	}
	return r.pipe.buf.Read(dst)
}

func (r *memoryContainedReader) Close() error {
	r.pipe.mu.Lock()
	defer r.pipe.mu.Unlock()
	r.pipe.readCloses++
	r.pipe.readClosed = true
	r.pipe.ready.Broadcast()
	return nil
}

func (*memoryContainedReader) Fd() uintptr { return 1 }

type memoryContainedWriter struct{ pipe *memoryContainedPipe }

func (w *memoryContainedWriter) Write(src []byte) (int, error) {
	w.pipe.mu.Lock()
	defer w.pipe.mu.Unlock()
	if w.pipe.writeClosed {
		return 0, io.ErrClosedPipe
	}
	n, err := w.pipe.buf.Write(src)
	w.pipe.ready.Broadcast()
	return n, err
}

func (w *memoryContainedWriter) Close() error {
	w.pipe.mu.Lock()
	defer w.pipe.mu.Unlock()
	w.pipe.writeCloses++
	w.pipe.writeClosed = true
	w.pipe.ready.Broadcast()
	return nil
}

func (*memoryContainedWriter) Fd() uintptr { return 1 }

type fakeContainedInput struct {
	closes atomic.Int32
}

func (*fakeContainedInput) Read([]byte) (int, error) { return 0, io.EOF }
func (f *fakeContainedInput) Close() error {
	f.closes.Add(1)
	return nil
}
func (*fakeContainedInput) Fd() uintptr { return 1 }

type scriptedContainedChild struct {
	startFn     func(containedInputFile, containedWriteFile, containedWriteFile) error
	waitCh      chan containedWaitResult
	terminateFn func(uint32) error
	closeFn     func() error

	startCalls     atomic.Int32
	waitCalls      atomic.Int32
	terminateCalls atomic.Int32
	closeCalls     atomic.Int32
	terminateOnce  sync.Once
	receivedMs     atomic.Uint32
}

func newScriptedContainedChild(wait containedWaitResult) *scriptedContainedChild {
	waitCh := make(chan containedWaitResult, 1)
	waitCh <- wait
	return &scriptedContainedChild{waitCh: waitCh}
}

func (c *scriptedContainedChild) start(
	_ *exec.Cmd,
	stdin containedInputFile,
	stdout containedWriteFile,
	stderr containedWriteFile,
) error {
	c.startCalls.Add(1)
	if c.startFn != nil {
		return c.startFn(stdin, stdout, stderr)
	}
	return nil
}

func (c *scriptedContainedChild) wait() containedWaitResult {
	c.waitCalls.Add(1)
	return <-c.waitCh
}

func (c *scriptedContainedChild) terminate(ms uint32) error {
	c.terminateCalls.Add(1)
	c.receivedMs.Store(ms)
	if c.terminateFn != nil {
		return c.terminateFn(ms)
	}
	c.terminateOnce.Do(func() {
		select {
		case c.waitCh <- containedWaitResult{exitCode: 0, exited: true}:
		default:
		}
	})
	return nil
}

func (c *scriptedContainedChild) close() error {
	c.closeCalls.Add(1)
	if c.closeFn != nil {
		return c.closeFn()
	}
	return nil
}

type containedDependencyHarness struct {
	child        *scriptedContainedChild
	newChildErr  error
	failPipeCall int
	nullErr      error

	newChildCalls atomic.Int32
	pipeCalls     atomic.Int32
	nullCalls     atomic.Int32

	mu     sync.Mutex
	pipes  []*memoryContainedPipe
	inputs []*fakeContainedInput
}

func (h *containedDependencyHarness) dependencies() containedStreamDependencies {
	return containedStreamDependencies{
		newChild: func(*exec.Cmd) (containedChild, error) {
			h.newChildCalls.Add(1)
			if h.newChildErr != nil {
				return nil, h.newChildErr
			}
			return h.child, nil
		},
		pipe: func() (containedReadFile, containedWriteFile, error) {
			call := int(h.pipeCalls.Add(1))
			if h.failPipeCall == call {
				return nil, nil, errors.New("pipe setup sentinel")
			}
			reader, writer := newMemoryContainedPipe()
			h.mu.Lock()
			h.pipes = append(h.pipes, reader.pipe)
			h.mu.Unlock()
			return reader, writer, nil
		},
		openNull: func() (containedInputFile, error) {
			h.nullCalls.Add(1)
			if h.nullErr != nil {
				return nil, h.nullErr
			}
			input := &fakeContainedInput{}
			h.mu.Lock()
			h.inputs = append(h.inputs, input)
			h.mu.Unlock()
			return input, nil
		},
	}
}

func runHarness(
	ctx context.Context,
	h *containedDependencyHarness,
	timeout time.Duration,
	stderr io.Writer,
	consume func(io.Reader) error,
) error {
	return runContainedStreamWithDependencies(
		ctx,
		exec.Command("contained-test-helper"),
		ContainedStreamOptions{CleanupTimeout: timeout, Stderr: stderr},
		consume,
		h.dependencies(),
	)
}

func drainContainedReader(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

func TestRunContainedStream_PreCanceledStartsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := &containedDependencyHarness{child: newScriptedContainedChild(containedWaitResult{})}
	err := runHarness(ctx, h, time.Second, io.Discard, func(io.Reader) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if h.newChildCalls.Load() != 0 || h.pipeCalls.Load() != 0 || h.nullCalls.Load() != 0 {
		t.Fatalf("validation allocated resources: child=%d pipes=%d null=%d",
			h.newChildCalls.Load(), h.pipeCalls.Load(), h.nullCalls.Load())
	}
}

func TestRunContainedStream_RejectsUnrepresentableCleanupTimeoutBeforeAllocation(t *testing.T) {
	tests := []time.Duration{
		-time.Millisecond,
		0,
		time.Millisecond - time.Nanosecond,
		time.Millisecond + time.Nanosecond,
		maxContainedCleanupTimeout + time.Millisecond,
	}
	for _, timeout := range tests {
		t.Run(timeout.String(), func(t *testing.T) {
			h := &containedDependencyHarness{child: newScriptedContainedChild(containedWaitResult{})}
			err := runHarness(context.Background(), h, timeout, io.Discard, func(io.Reader) error { return nil })
			var lifecycle *ContainedRunError
			if !errors.As(err, &lifecycle) || lifecycle.Stage != ContainedStageValidate ||
				!errors.Is(err, ErrInvalidCleanupTimeout) {
				t.Fatalf("err=%#v, want validate-stage ErrInvalidCleanupTimeout", err)
			}
			if h.newChildCalls.Load() != 0 || h.pipeCalls.Load() != 0 || h.nullCalls.Load() != 0 {
				t.Fatalf("rejected timeout allocated resources")
			}
		})
	}
}

func TestRunContainedStream_MaxCleanupTimeoutReachesAdapterUnchanged(t *testing.T) {
	child := newScriptedContainedChild(containedWaitResult{exitCode: 0, exited: true})
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, maxContainedCleanupTimeout, io.Discard, drainContainedReader)
	if err != nil {
		t.Fatalf("RunContainedStream: %v", err)
	}
	if got := child.receivedMs.Load(); got != math.MaxUint32 {
		t.Fatalf("cleanup ms=%d, want %d", got, uint32(math.MaxUint32))
	}
}

func TestRunContainedStream_MinCleanupTimeoutReachesAdapterUnchanged(t *testing.T) {
	child := newScriptedContainedChild(containedWaitResult{exitCode: 0, exited: true})
	h := &containedDependencyHarness{child: child}
	_ = runHarness(context.Background(), h, time.Millisecond, io.Discard, drainContainedReader)
	if got := child.receivedMs.Load(); got != 1 {
		t.Fatalf("cleanup ms=%d, want 1", got)
	}
}

func TestRunContainedStream_PipeSetupFailureClosesEarlierResources(t *testing.T) {
	tests := []struct {
		name         string
		failPipeCall int
		nullErr      error
	}{
		{name: "stdout", failPipeCall: 1},
		{name: "stderr", failPipeCall: 2},
		{name: "null", nullErr: errors.New("null setup sentinel")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			child := newScriptedContainedChild(containedWaitResult{})
			h := &containedDependencyHarness{
				child:        child,
				failPipeCall: tc.failPipeCall,
				nullErr:      tc.nullErr,
			}
			err := runHarness(context.Background(), h, time.Second, io.Discard, drainContainedReader)
			var lifecycle *ContainedRunError
			if !errors.As(err, &lifecycle) || lifecycle.Stage != ContainedStagePipeSetup {
				t.Fatalf("err=%#v, want pipe-setup stage", err)
			}
			if child.startCalls.Load() != 0 || child.waitCalls.Load() != 0 {
				t.Fatalf("child started/waited after setup failure")
			}
			if child.closeCalls.Load() != 1 {
				t.Fatalf("child close calls=%d, want 1", child.closeCalls.Load())
			}
			for i, pipe := range h.pipes {
				if pipe.readCloses != 1 || pipe.writeCloses != 1 {
					t.Fatalf("pipe %d closes=(%d,%d), want (1,1)", i, pipe.readCloses, pipe.writeCloses)
				}
			}
		})
	}
}

func TestRunContainedStream_StartFailureClosesAllResources(t *testing.T) {
	startErr := errors.New("start sentinel")
	child := newScriptedContainedChild(containedWaitResult{})
	child.startFn = func(containedInputFile, containedWriteFile, containedWriteFile) error { return startErr }
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, drainContainedReader)
	if !errors.Is(err, startErr) {
		t.Fatalf("err=%v, want start sentinel", err)
	}
	if child.waitCalls.Load() != 0 || child.closeCalls.Load() != 1 {
		t.Fatalf("wait=%d close=%d, want 0/1", child.waitCalls.Load(), child.closeCalls.Load())
	}
	for i, pipe := range h.pipes {
		if pipe.readCloses != 1 || pipe.writeCloses != 1 {
			t.Fatalf("pipe %d closes=(%d,%d), want (1,1)", i, pipe.readCloses, pipe.writeCloses)
		}
	}
	if got := h.inputs[0].closes.Load(); got != 1 {
		t.Fatalf("stdin close=%d, want 1", got)
	}
}

func TestRunContainedStream_ConsumerReadFailureTerminatesThenWaitsOnce(t *testing.T) {
	readErr := errors.New("consumer sentinel")
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, func(io.Reader) error { return readErr })
	if !errors.Is(err, readErr) {
		t.Fatalf("err=%v, want consumer sentinel", err)
	}
	if child.terminateCalls.Load() != 1 || child.waitCalls.Load() != 1 || child.closeCalls.Load() != 1 {
		t.Fatalf("terminate/wait/close=%d/%d/%d, want 1/1/1",
			child.terminateCalls.Load(), child.waitCalls.Load(), child.closeCalls.Load())
	}
}

type failingContainedWriter struct{ err error }

func (w failingContainedWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunContainedStream_StderrWriteFailureTerminatesAndSettles(t *testing.T) {
	writeErr := errors.New("stderr sentinel")
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	child.startFn = func(_ containedInputFile, _ containedWriteFile, stderr containedWriteFile) error {
		_, err := stderr.Write([]byte("diagnostic"))
		return err
	}
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, failingContainedWriter{err: writeErr}, drainContainedReader)
	if !errors.Is(err, writeErr) {
		t.Fatalf("err=%v, want stderr sentinel", err)
	}
	if child.terminateCalls.Load() != 1 || child.waitCalls.Load() != 1 {
		t.Fatalf("terminate/wait=%d/%d, want 1/1", child.terminateCalls.Load(), child.waitCalls.Load())
	}
}

func TestRunContainedStream_CancellationTerminatesThenWaitsOnce(t *testing.T) {
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	started := make(chan struct{})
	child.startFn = func(containedInputFile, containedWriteFile, containedWriteFile) error {
		close(started)
		return nil
	}
	h := &containedDependencyHarness{child: child}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runHarness(ctx, h, time.Second, io.Discard, drainContainedReader)
	}()
	<-started
	cancel()
	err := <-done
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if child.startCalls.Load() != 1 || child.terminateCalls.Load() != 1 || child.waitCalls.Load() != 1 {
		t.Fatalf("start/terminate/wait=%d/%d/%d, want 1/1/1",
			child.startCalls.Load(), child.terminateCalls.Load(), child.waitCalls.Load())
	}
}

func TestRunContainedStream_WaitFailureStillCleansUp(t *testing.T) {
	waitErr := errors.New("wait sentinel")
	child := newScriptedContainedChild(containedWaitResult{err: waitErr})
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, drainContainedReader)
	if !errors.Is(err, waitErr) {
		t.Fatalf("err=%v, want wait sentinel", err)
	}
	if child.waitCalls.Load() != 1 || child.terminateCalls.Load() != 1 || child.closeCalls.Load() != 1 {
		t.Fatalf("wait/terminate/close=%d/%d/%d, want 1/1/1",
			child.waitCalls.Load(), child.terminateCalls.Load(), child.closeCalls.Load())
	}
}

func TestRunContainedStream_NonzeroExitCarriesCode(t *testing.T) {
	child := newScriptedContainedChild(containedWaitResult{exitCode: 37, exited: true})
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, drainContainedReader)
	var lifecycle *ContainedRunError
	if !errors.As(err, &lifecycle) || lifecycle.Stage != ContainedStageExit ||
		lifecycle.ExitCode == nil || *lifecycle.ExitCode != 37 {
		t.Fatalf("err=%#v, want exit stage/code 37", err)
	}
	if got := err.Error(); got != "contained process exit failed" {
		t.Fatalf("error text=%q, want fixed stage text", got)
	}
}

func TestRunContainedStream_PrimaryPreservedWhenCleanupAlsoFails(t *testing.T) {
	primary := errors.New("consumer primary")
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	child.terminateFn = func(uint32) error {
		child.terminateOnce.Do(func() {
			child.waitCh <- containedWaitResult{exitCode: 1, exited: true}
		})
		return ErrCleanupTimeout
	}
	h := &containedDependencyHarness{child: child}
	err := runHarness(context.Background(), h, time.Second, io.Discard, func(io.Reader) error { return primary })
	var lifecycle *ContainedRunError
	if !errors.As(err, &lifecycle) || !errors.Is(lifecycle.Cause, primary) ||
		!errors.Is(lifecycle.CleanupCause, ErrCleanupTimeout) {
		t.Fatalf("err=%#v, want primary plus cleanup timeout", err)
	}
}

func TestRunContainedStream_SuccessStreamsBeforeExit(t *testing.T) {
	allowExit := make(chan struct{})
	child := &scriptedContainedChild{waitCh: make(chan containedWaitResult, 1)}
	child.startFn = func(_ containedInputFile, stdout, _ containedWriteFile) error {
		_, err := stdout.Write([]byte("first chunk"))
		return err
	}
	child.waitCh = make(chan containedWaitResult, 1)
	go func() {
		<-allowExit
		child.waitCh <- containedWaitResult{exitCode: 0, exited: true}
	}()
	h := &containedDependencyHarness{child: child}
	seen := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runHarness(context.Background(), h, time.Second, io.Discard, func(r io.Reader) error {
			data, err := io.ReadAll(r)
			if string(data) != "first chunk" {
				return errors.New("unexpected streamed bytes")
			}
			close(seen)
			return err
		})
	}()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("consumer did not observe stdout before child exit")
	}
	close(allowExit)
	if err := <-done; err != nil {
		t.Fatalf("RunContainedStream: %v", err)
	}
}
