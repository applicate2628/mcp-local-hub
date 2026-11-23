package api

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"mcp-local-hub/internal/scheduler"
)

// InstallSchedulerRollbackIncompleteError reports scheduler mutations or
// compensations whose prior definitions or absence could not be confirmed.
// Outer install owners must retain recovery material rather than treat the
// original failure as unmutated.
// Tasks contains names only; cause preserves the install and compensation errors.
type InstallSchedulerRollbackIncompleteError struct {
	Tasks []string
	cause error
}

func (e *InstallSchedulerRollbackIncompleteError) Error() string {
	return fmt.Sprintf("install scheduler rollback incomplete (tasks: %s): %v", strings.Join(e.Tasks, ", "), e.cause)
}

func (e *InstallSchedulerRollbackIncompleteError) Unwrap() error { return e.cause }

// installSchedulerRecoveryTasks is the shared outer-owner classification for
// both a later rollback and the existing current-step recovery. A nil Recovery
// on InstallTaskRecoveryError still means acknowledged registration of the prior
// definition; it must not manufacture an incomplete-recovery outcome.
func installSchedulerRecoveryTasks(err error) []string {
	var names []string
	var incomplete *InstallSchedulerRollbackIncompleteError
	if errors.As(err, &incomplete) {
		names = append(names, incomplete.Tasks...)
	}
	var current *InstallTaskRecoveryError
	if errors.As(err, &current) && current.Recovery != nil {
		found := false
		for _, name := range names {
			if name == current.TaskName {
				found = true
				break
			}
		}
		if !found {
			names = append(names, current.TaskName)
		}
	}
	return names
}

// installTaskMutationFailure observes a failed current-step mutation. Only a
// confirmed absent name can settle an operation whose prior state was absent.
// Matching XML does not settle a prior generation or prove a failed operation
// succeeded. Never retry, delete an occupant, or restore by name here.
func installTaskMutationFailure(s interface {
	ExportXML(string) ([]byte, error)
}, name string, priorXML []byte, cause error) error {
	current, readErr := s.ExportXML(name)
	if len(priorXML) == 0 {
		if errors.Is(readErr, scheduler.ErrTaskNotFound) && len(current) == 0 {
			return cause
		}
	}
	if readErr != nil {
		cause = errors.Join(cause, fmt.Errorf("observe task %s after failed mutation: %w", name, readErr))
	}
	return &InstallSchedulerRollbackIncompleteError{Tasks: []string{name}, cause: cause}
}

// rollbackInstallTaskCreation can confirm absence, but cannot remove the
// occupant of a task name. An acknowledged Create and identical XML do not
// establish exclusive generation ownership. Retain the current observation for
// recovery and propagate read/storage failures with the ownership refusal.
func rollbackInstallTaskCreation(s interface {
	ExportXML(string) ([]byte, error)
}, name string) error {
	current, readErr := s.ExportXML(name)
	if errors.Is(readErr, scheduler.ErrTaskNotFound) && len(current) == 0 {
		return nil
	}
	if readErr != nil {
		return fmt.Errorf("rollback scheduler task %s: %w", name, errors.Join(ErrSchedulerReplacementUnowned, readErr))
	}
	if len(current) == 0 {
		return fmt.Errorf("rollback scheduler task %s: %w", name, errors.Join(ErrSchedulerReplacementUnowned, errors.New("empty successful export; task absence was not confirmed")))
	}
	return fmt.Errorf("rollback scheduler task %s: %w", name, refuseSchedulerTaskReplacement(name, bytes.Clone(current)))
}
