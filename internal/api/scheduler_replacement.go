package api

import (
	"errors"
	"fmt"

	"mcp-local-hub/internal/autostart"
	"mcp-local-hub/internal/scheduler"
)

// ErrSchedulerReplacementUnowned means a destructive replacement or deletion
// was refused. The backend does not provide exclusive authority over the
// observed registration; an XML snapshot or a cooperating lock cannot grant it.
var ErrSchedulerReplacementUnowned = scheduler.ErrTaskReplacementUnowned

// SchedulerReplacementRefusedError retains the observed definition without
// claiming that it owns the current registration. SnapshotPath is populated only
// after the protected snapshot write and readback succeeded. Cause describes a
// failed snapshot retention attempt, not a scheduler mutation or restoration.
type SchedulerReplacementRefusedError struct {
	TaskName     string
	SnapshotPath string
	Cause        error
}

func (e *SchedulerReplacementRefusedError) Error() string {
	message := fmt.Sprintf("refuse replacement of task %s: %v", e.TaskName, ErrSchedulerReplacementUnowned)
	if e.Cause != nil {
		return fmt.Sprintf("%s; %v", message, e.Cause)
	}
	if e.SnapshotPath == "" {
		return message
	}
	return fmt.Sprintf("%s; observed definition snapshot retained at %q", message, e.SnapshotPath)
}

func (e *SchedulerReplacementRefusedError) Unwrap() []error {
	if e.Cause != nil {
		return []error{ErrSchedulerReplacementUnowned, e.Cause}
	}
	return []error{ErrSchedulerReplacementUnowned}
}

func refuseSchedulerTaskReplacement(name string, priorXML []byte) error {
	refusal := &SchedulerReplacementRefusedError{TaskName: name}
	path, err := retainInstallTaskSnapshot(name, priorXML)
	if err != nil {
		refusal.Cause = fmt.Errorf("retain observed definition at %q: %w", path, err)
		return refusal
	}
	refusal.SnapshotPath = path
	return refusal
}

// RetainAutostartTaskReplacementRefusal finalizes a direct Enable result.
// Snapshot the exact observation carried by the refusal, preserving any
// original create error. Other Enable results pass through.
func RetainAutostartTaskReplacementRefusal(err error) error {
	refusal, ok := err.(*autostart.ObservedTaskReplacementRefusal)
	if !ok {
		return err
	}
	retained := refuseSchedulerTaskReplacement(refusal.TaskName, refusal.ObservedXML())
	if refusal.CreateErr != nil {
		return errors.Join(refusal.CreateErr, retained)
	}
	return retained
}
