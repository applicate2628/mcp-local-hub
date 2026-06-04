//go:build linux

package scheduler

func notImplementedSchedulerForTest() Scheduler {
	return linuxScheduler{}
}
