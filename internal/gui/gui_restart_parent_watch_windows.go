//go:build windows

package gui

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/windows"
)

type windowsRestartParentDeathWatcher struct {
	process  windows.Handle
	cancel   windows.Handle
	done     chan struct{}
	waitDone chan struct{}
	close    sync.Once
}

func newRestartParentDeathWatcher(pid int) (restartParentDeathWatcher, error) {
	processHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return newClosedRestartParentDeathWatcher(), nil
		}
		return nil, fmt.Errorf("OpenProcess(SYNCHRONIZE, pid=%d): %w", pid, err)
	}
	cancelHandle, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		windows.CloseHandle(processHandle)
		return nil, fmt.Errorf("CreateEvent(parent watcher): %w", err)
	}
	w := &windowsRestartParentDeathWatcher{
		process:  processHandle,
		cancel:   cancelHandle,
		done:     make(chan struct{}),
		waitDone: make(chan struct{}),
	}
	go w.wait()
	return w, nil
}

func (w *windowsRestartParentDeathWatcher) wait() {
	defer close(w.waitDone)
	event, err := windows.WaitForMultipleObjects([]windows.Handle{w.process, w.cancel}, false, windows.INFINITE)
	if err != nil || event == windows.WAIT_OBJECT_0 {
		close(w.done)
	}
}

func (w *windowsRestartParentDeathWatcher) Done() <-chan struct{} { return w.done }

func (w *windowsRestartParentDeathWatcher) Close() error {
	var closeErr error
	w.close.Do(func() {
		if err := windows.SetEvent(w.cancel); err != nil {
			closeErr = fmt.Errorf("signal parent watcher cancellation: %w", err)
		}
		<-w.waitDone
		if err := windows.CloseHandle(w.cancel); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close parent watcher event: %w", err))
		}
		if err := windows.CloseHandle(w.process); err != nil {
			closeErr = errors.Join(closeErr, fmt.Errorf("close retained parent process: %w", err))
		}
	})
	return closeErr
}
