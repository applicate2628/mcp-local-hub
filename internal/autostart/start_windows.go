//go:build windows

package autostart

import (
	"fmt"

	"mcp-local-hub/internal/scheduler"
)

// StartOwner starts the installed Windows autostart owner immediately by
// re-firing its Task Scheduler entry.
func StartOwner() error {
	sch, err := scheduler.New()
	if err != nil {
		return fmt.Errorf("autostart owner start: scheduler: %w", err)
	}
	if err := sch.Run(WindowsTaskName); err != nil {
		return fmt.Errorf("autostart owner start: schtasks /Run %s: %w", WindowsTaskName, err)
	}
	return nil
}
