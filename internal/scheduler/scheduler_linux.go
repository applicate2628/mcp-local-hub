//go:build linux

package scheduler

import (
	"context"
	"fmt"
)

// linuxScheduler is a stub that compiles but returns "not implemented" for all operations.
// Full systemd-user-unit integration is out of scope for Phase 0-1 of this plan.
type linuxScheduler struct{}

func newPlatformScheduler() (Scheduler, error) {
	return nil, fmt.Errorf("linux scheduler backend: %w", ErrNotImplemented)
}

func (linuxScheduler) Create(TaskSpec) error {
	return fmt.Errorf("linux scheduler Create: %w", ErrNotImplemented)
}
func (linuxScheduler) Delete(string) error {
	return fmt.Errorf("linux scheduler Delete: %w", ErrNotImplemented)
}
func (linuxScheduler) Run(string) error {
	return fmt.Errorf("linux scheduler Run: %w", ErrNotImplemented)
}
func (linuxScheduler) Stop(string) error {
	return fmt.Errorf("linux scheduler Stop: %w", ErrNotImplemented)
}
func (linuxScheduler) Status(string) (TaskStatus, error) {
	return TaskStatus{}, fmt.Errorf("linux scheduler Status: %w", ErrNotImplemented)
}
func (linuxScheduler) List(string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("linux scheduler List: %w", ErrNotImplemented)
}
func (linuxScheduler) ListContext(context.Context, string) ([]TaskStatus, error) {
	return nil, fmt.Errorf("linux scheduler ListContext: %w", ErrNotImplemented)
}
func (linuxScheduler) ExportXML(string) ([]byte, error) {
	return nil, fmt.Errorf("linux scheduler ExportXML: %w", ErrNotImplemented)
}
func (linuxScheduler) ImportXML(string, []byte) error {
	return fmt.Errorf("linux scheduler ImportXML: %w", ErrNotImplemented)
}
