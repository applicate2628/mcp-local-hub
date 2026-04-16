//go:build linux

package scheduler

import "fmt"

// linuxScheduler is a stub that compiles but returns "not implemented" for all operations.
// Full systemd-user-unit integration is out of scope for Phase 0-1 of this plan.
type linuxScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("linux scheduler not yet implemented (Phase 0-1 is Windows-first)")
}

func (linuxScheduler) Create(TaskSpec) error          { return fmt.Errorf("not implemented") }
func (linuxScheduler) Delete(string) error            { return fmt.Errorf("not implemented") }
func (linuxScheduler) Run(string) error               { return fmt.Errorf("not implemented") }
func (linuxScheduler) Stop(string) error              { return fmt.Errorf("not implemented") }
func (linuxScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("not implemented")
}
func (linuxScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("not implemented")
}
