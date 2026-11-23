package api

import (
	"errors"
	"fmt"
)

// ErrSchedulerReplacementUnowned means a replacement was refused before any
// scheduler mutation. The backend does not provide exclusive authority over the
// observed registration; an XML snapshot or a cooperating lock cannot grant it.
var ErrSchedulerReplacementUnowned = errors.New("exclusive ownership of the existing scheduler task generation is unavailable")

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
