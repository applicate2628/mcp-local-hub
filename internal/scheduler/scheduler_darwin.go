//go:build darwin

package scheduler

import (
	"context"
	"fmt"
)

// darwinScheduler is a stub that compiles but returns "not implemented".
// Full launchd agent integration is out of scope for Phase 0-1.
type darwinScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("darwin scheduler backend: %w", ErrNotImplemented)
}

func (darwinScheduler) Create(TaskSpec) error {
	return fmt.Errorf("darwin scheduler Create: %w", ErrNotImplemented)
}
func (darwinScheduler) Delete(string) error {
	return fmt.Errorf("darwin scheduler Delete: %w", ErrNotImplemented)
}
func (darwinScheduler) Run(string) error {
	return fmt.Errorf("darwin scheduler Run: %w", ErrNotImplemented)
}
func (darwinScheduler) Stop(string) error {
	return fmt.Errorf("darwin scheduler Stop: %w", ErrNotImplemented)
}
func (darwinScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("darwin scheduler Status: %w", ErrNotImplemented)
}
func (darwinScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("darwin scheduler List: %w", ErrNotImplemented)
}
func (darwinScheduler) ListContext(context.Context, string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("darwin scheduler ListContext: %w", ErrNotImplemented)
}
func (darwinScheduler) ExportXML(string) ([]byte, error) {
	return nil, fmt.Errorf("darwin scheduler ExportXML: %w", ErrNotImplemented)
}
func (darwinScheduler) ImportXML(string, []byte) error {
	return fmt.Errorf("darwin scheduler ImportXML: %w", ErrNotImplemented)
}
