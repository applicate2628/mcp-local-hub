//go:build darwin

package scheduler

func notImplementedSchedulerForTest() Scheduler {
	return darwinScheduler{}
}
