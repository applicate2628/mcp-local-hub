//go:build !windows && !linux

package process

import "time"

func initializePlatformContainedWait(*posixContainedChild) {}

func startPlatformContainedLeaderReapAfterSignal(*posixContainedChild) {}

func (c *posixContainedChild) wait() containedWaitResult {
	return c.waitCommand()
}

func (c *posixContainedChild) reapBy(time.Time) (containedWaitResult, bool) {
	return containedWaitResult{}, false
}
