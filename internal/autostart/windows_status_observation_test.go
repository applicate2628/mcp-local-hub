//go:build windows

package autostart

import (
	"errors"
	"fmt"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

func TestWindowsBackend_StatusSchedulerUnavailableMapsToObservationSentinel(t *testing.T) {
	for _, tc := range []struct {
		name       string
		factoryErr error
		statusErr  error
	}{
		{name: "factory", factoryErr: fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)},
		{name: "status", statusErr: fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := schedulerFactoryFn
			schedulerFactoryFn = func() (scheduler.Scheduler, error) {
				if tc.factoryErr != nil {
					return nil, tc.factoryErr
				}
				return &fakeScheduler{statusErr: tc.statusErr}, nil
			}
			t.Cleanup(func() { schedulerFactoryFn = prev })

			b, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = b.Status(Options{MCPHubPath: "test-mcphub.exe"})
			if !errors.Is(err, ErrStatusObservationUnavailable) {
				t.Fatalf("Status error = %v, want ErrStatusObservationUnavailable", err)
			}
		})
	}
}

func TestWindowsBackend_StatusSnapshotSchedulerUnavailableRemainsFailClosed(t *testing.T) {
	prev := schedulerFactoryFn
	schedulerFactoryFn = func() (scheduler.Scheduler, error) {
		return nil, fmt.Errorf("scheduler bridge: %w: protocol", scheduler.ErrUnavailable)
	}
	t.Cleanup(func() { schedulerFactoryFn = prev })

	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = b.StatusSnapshot(Options{MCPHubPath: "test-mcphub.exe"})
	if !errors.Is(err, scheduler.ErrUnavailable) {
		t.Fatalf("StatusSnapshot error = %v, want typed scheduler.ErrUnavailable", err)
	}
	if errors.Is(err, ErrStatusObservationUnavailable) {
		t.Fatalf("StatusSnapshot error = %v, must not map strict snapshot failure to observation sentinel", err)
	}
}
