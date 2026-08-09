//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package process

import (
	"errors"
	"time"
)

func initializePlatformContainedWait(child *posixContainedChild) {
	child.waitObserved = make(chan struct{})
	child.reapDone = make(chan struct{})
}

func observeContainedLeaderExit(child *posixContainedChild, observe func(int) error) containedWaitResult {
	if child == nil || child.pid <= 0 || child.waitObserved == nil || observe == nil {
		return containedWaitResult{err: errors.New("invalid POSIX child wait")}
	}
	child.waitObserveErr = observe(child.pid)
	close(child.waitObserved)
	if child.waitObserveErr != nil {
		return containedWaitResult{err: errors.New("POSIX_EXIT_OBSERVE_FAILED")}
	}
	return containedWaitResult{}
}

func (c *posixContainedChild) wait() containedWaitResult {
	return observeContainedLeaderExit(c, observePlatformContainedExit)
}

func startContainedLeaderReap(child *posixContainedChild, reap func() containedWaitResult) {
	if child == nil || child.waitObserved == nil || child.reapDone == nil || reap == nil {
		return
	}
	child.reapStartOnce.Do(func() {
		go func() {
			<-child.waitObserved
			child.reapResult = reap()
			if child.waitObserveErr != nil {
				child.reapResult.err = errors.Join(errors.New("POSIX_EXIT_OBSERVE_FAILED"), child.reapResult.err)
			}
			close(child.reapDone)
		}()
	})
}

func startPlatformContainedLeaderReapAfterSignal(child *posixContainedChild) {
	if child == nil {
		return
	}
	startContainedLeaderReap(child, child.waitCommand)
}

func (c *posixContainedChild) reapBy(deadline time.Time) (containedWaitResult, bool) {
	if c == nil || c.reapDone == nil {
		return containedWaitResult{err: errors.New("invalid POSIX child reap")}, true
	}
	startPlatformContainedLeaderReapAfterSignal(c)
	remaining := time.Until(deadline)
	if remaining <= 0 {
		select {
		case <-c.reapDone:
			return c.reapResult, true
		default:
			return containedWaitResult{}, false
		}
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-c.reapDone:
		return c.reapResult, true
	case <-timer.C:
		return containedWaitResult{}, false
	}
}
