//go:build linux

package process

import (
	"errors"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func initializePlatformContainedWait(child *posixContainedChild) {
	child.waitObserved = make(chan struct{})
	child.reapDone = make(chan struct{})
}

// wait observes direct-child exit without reaping it. The resulting zombie
// pins both PID and PGID until RunContainedStream has signaled the group,
// closing the recycled-numeric-identity race.
func (c *posixContainedChild) wait() containedWaitResult {
	if c == nil || c.pid <= 0 || c.waitObserved == nil {
		return containedWaitResult{err: errors.New("invalid POSIX child wait")}
	}
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, c.pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		c.waitObserveErr = err
		close(c.waitObserved)
		if err != nil {
			return containedWaitResult{err: errors.New("POSIX_EXIT_OBSERVE_FAILED")}
		}
		return containedWaitResult{}
	}
}

func startPlatformContainedLeaderReapAfterSignal(c *posixContainedChild) {
	if c == nil || c.waitObserved == nil || c.reapDone == nil {
		return
	}
	c.reapStartOnce.Do(func() {
		go func() {
			<-c.waitObserved
			c.reapResult = c.waitCommand()
			if c.waitObserveErr != nil {
				c.reapResult.err = errors.Join(errors.New("POSIX_EXIT_OBSERVE_FAILED"), c.reapResult.err)
			}
			close(c.reapDone)
		}()
	})
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
