//go:build windows && cst_vptr_probe

package main

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
	"mcp-local-hub/internal/e2e/cstwatchdogwire"
)

var errF4WaitDeadline = errors.New("F4 parent wait deadline")
var errF4HostExited = errors.New("F4 test host exited before next receipt")

type f4Control struct {
	mapping       windows.Handle
	view          uintptr
	ready         windows.Handle
	ack           windows.Handle
	helperRelease windows.Handle
	binding       cstwatchdogwire.Binding
	lastSequence  uint64
	lastPhase     uint32
	lastFlags     uint32
}

func f4NewControl(binding cstwatchdogwire.Binding) (*f4Control, error) {
	c := &f4Control{binding: binding}
	var err error
	c.mapping, err = windows.CreateFileMapping(windows.InvalidHandle, nil, windows.PAGE_READWRITE, 0, cstwatchdogwire.Size, nil)
	if err != nil {
		return nil, err
	}
	c.view, err = windows.MapViewOfFile(c.mapping, windows.FILE_MAP_READ|windows.FILE_MAP_WRITE, 0, 0, cstwatchdogwire.Size)
	if err != nil {
		_ = c.close()
		return nil, err
	}
	c.ready, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = c.close()
		return nil, err
	}
	c.ack, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = c.close()
		return nil, err
	}
	c.helperRelease, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		_ = c.close()
		return nil, err
	}
	return c, nil
}

func f4DuplicateInheritable(handle windows.Handle) (windows.Handle, error) {
	if handle == 0 {
		return 0, errors.New("cannot allowlist zero handle")
	}
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), handle, windows.CurrentProcess(), &duplicate, 0, true, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return 0, err
	}
	return duplicate, nil
}

func (c *f4Control) childHandles() ([]windows.Handle, error) {
	var list []windows.Handle
	for _, source := range []windows.Handle{c.mapping, c.ready, c.ack} {
		copy, err := f4DuplicateInheritable(source)
		if err != nil {
			for _, handle := range list {
				_ = windows.CloseHandle(handle)
			}
			return nil, err
		}
		list = append(list, copy)
	}
	return list, nil
}

func f4CloseHandles(handles []windows.Handle) error {
	var problems []error
	for _, handle := range handles {
		if handle != 0 {
			problems = append(problems, windows.CloseHandle(handle))
		}
	}
	return errors.Join(problems...)
}

func (c *f4Control) read() (cstwatchdogwire.Frame, error) {
	var first, second [cstwatchdogwire.Size]byte
	for _, target := range []*[cstwatchdogwire.Size]byte{&first, &second} {
		var copied uintptr
		if err := windows.ReadProcessMemory(windows.CurrentProcess(), c.view, &target[0], uintptr(len(target)), &copied); err != nil || copied != uintptr(len(target)) {
			return cstwatchdogwire.Frame{}, fmt.Errorf("F4 mapped-cell read n=%d: %w", copied, err)
		}
	}
	return cstwatchdogwire.Decode(first[:], second[:], c.binding)
}

func f4WaitMilliseconds(deadline time.Time) uint32 {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return 0
	}
	ms := (remaining + time.Millisecond - 1) / time.Millisecond
	if ms >= time.Duration(windows.INFINITE) {
		return windows.INFINITE - 1
	}
	return uint32(ms)
}

// poll always places the original host process before ready. A death/ready tie
// cannot upgrade a frame from a dead writer into parent custody.
func (c *f4Control) poll(hostOriginal windows.Handle, deadline time.Time) (cstwatchdogwire.Frame, error) {
	if hostOriginal == 0 {
		return cstwatchdogwire.Frame{}, errors.New("missing original test-host handle")
	}
	if !time.Now().Before(deadline) {
		return cstwatchdogwire.Frame{}, errF4WaitDeadline
	}
	result, err := windows.WaitForMultipleObjects([]windows.Handle{hostOriginal, c.ready}, false, f4WaitMilliseconds(deadline))
	if err != nil {
		return cstwatchdogwire.Frame{}, err
	}
	switch result {
	case windows.WAIT_OBJECT_0:
		return cstwatchdogwire.Frame{}, errF4HostExited
	case windows.WAIT_OBJECT_0 + 1:
		if !time.Now().Before(deadline) {
			return cstwatchdogwire.Frame{}, errF4WaitDeadline
		}
		return c.read()
	case uint32(windows.WAIT_TIMEOUT):
		return cstwatchdogwire.Frame{}, errF4WaitDeadline
	default:
		return cstwatchdogwire.Frame{}, fmt.Errorf("unexpected F4 wait result %d", result)
	}
}

func (c *f4Control) validateNext(frame cstwatchdogwire.Frame, requireBarrier bool) error {
	if frame.Sequence != c.lastSequence+1 {
		return fmt.Errorf("F4 sequence got=%d want=%d", frame.Sequence, c.lastSequence+1)
	}
	want := cstwatchdogwire.Ready
	switch c.lastPhase {
	case cstwatchdogwire.Ready:
		want = cstwatchdogwire.Started
	case cstwatchdogwire.Started:
		if requireBarrier {
			want = cstwatchdogwire.Barrier
		} else {
			want = cstwatchdogwire.Fired
		}
	case cstwatchdogwire.Barrier:
		want = cstwatchdogwire.Fired
	case cstwatchdogwire.Fired:
		want = cstwatchdogwire.Finished
	case cstwatchdogwire.Finished, cstwatchdogwire.Fault:
		return errors.New("late or duplicate F4 terminal frame")
	}
	if frame.Phase != want && frame.Phase != cstwatchdogwire.Fault {
		return fmt.Errorf("F4 phase got=%d want=%d", frame.Phase, want)
	}
	wantFlags := c.lastFlags
	switch frame.Phase {
	case cstwatchdogwire.Started:
		wantFlags |= cstwatchdogwire.Attempted
	case cstwatchdogwire.Barrier:
		wantFlags |= cstwatchdogwire.BarrierSeen
	case cstwatchdogwire.Fired:
		wantFlags |= cstwatchdogwire.InjectionFired
	case cstwatchdogwire.Finished:
		wantFlags |= cstwatchdogwire.FinishedSeen
	case cstwatchdogwire.Ready, cstwatchdogwire.Fault:
		// A FAULT acknowledges no new witnessed phase.
	}
	if frame.Flags != wantFlags {
		return fmt.Errorf("F4 flags got=%#x want=%#x for phase=%d", frame.Flags, wantFlags, frame.Phase)
	}
	return nil
}

func (c *f4Control) accept(frame cstwatchdogwire.Frame) error {
	if err := windows.SetEvent(c.ack); err != nil {
		return err
	}
	c.lastSequence, c.lastPhase, c.lastFlags = frame.Sequence, frame.Phase, frame.Flags
	return nil
}

func (c *f4Control) close() error {
	if c == nil {
		return nil
	}
	var problems []error
	if c.view != 0 {
		problems = append(problems, windows.UnmapViewOfFile(c.view))
		c.view = 0
	}
	for _, handle := range []windows.Handle{c.mapping, c.ready, c.ack, c.helperRelease} {
		if handle != 0 {
			problems = append(problems, windows.CloseHandle(handle))
		}
	}
	c.mapping, c.ready, c.ack, c.helperRelease = 0, 0, 0, 0
	return errors.Join(problems...)
}
