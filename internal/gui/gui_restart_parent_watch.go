package gui

// restartParentDeathWatcher owns an exact parent-process observation for the
// standby lifetime. Done closes only when that retained process identity exits;
// Close releases the observation without reporting a death.
type restartParentDeathWatcher interface {
	Done() <-chan struct{}
	Close() error
}

type closedRestartParentDeathWatcher struct {
	done chan struct{}
}

func newClosedRestartParentDeathWatcher() restartParentDeathWatcher {
	done := make(chan struct{})
	close(done)
	return &closedRestartParentDeathWatcher{done: done}
}

func (w *closedRestartParentDeathWatcher) Done() <-chan struct{} { return w.done }
func (w *closedRestartParentDeathWatcher) Close() error          { return nil }
