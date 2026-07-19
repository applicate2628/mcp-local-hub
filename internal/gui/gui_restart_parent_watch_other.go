//go:build !windows

package gui

// Non-Windows GUI restart remains preview-only. Keep the ownership contract
// and test seam available without pretending a PID poll is an exact retained
// process identity; the Windows implementation supplies the production handle.
type unsupportedRestartParentDeathWatcher struct {
	done chan struct{}
}

func newRestartParentDeathWatcher(int) (restartParentDeathWatcher, error) {
	return &unsupportedRestartParentDeathWatcher{done: make(chan struct{})}, nil
}

func (w *unsupportedRestartParentDeathWatcher) Done() <-chan struct{} { return w.done }
func (w *unsupportedRestartParentDeathWatcher) Close() error          { return nil }
