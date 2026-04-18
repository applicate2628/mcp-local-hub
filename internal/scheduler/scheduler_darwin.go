//go:build darwin

package scheduler

import "fmt"

// darwinScheduler is a stub that compiles but returns "not implemented".
// Full launchd agent integration is out of scope for Phase 0-1.
type darwinScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("darwin scheduler not yet implemented (Phase 0-1 is Windows-first)")
}

func (darwinScheduler) Create(TaskSpec) error { return fmt.Errorf("not implemented") }
func (darwinScheduler) Delete(string) error   { return fmt.Errorf("not implemented") }
func (darwinScheduler) Run(string) error      { return fmt.Errorf("not implemented") }
func (darwinScheduler) Stop(string) error     { return fmt.Errorf("not implemented") }
func (darwinScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("not implemented")
}
func (darwinScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
