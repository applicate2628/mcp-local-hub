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

// installTaskMutationFailure observes the prior definition or absence after a
// failed current-step mutation. An error alone does not establish its outcome.
// Reuse the scheduler recovery owner unless that prior state is observed;
// never retry, delete an occupant, or restore by name in this classifier.
// Like the later rollback's checks, this is a definition observation, not an
// atomic object-generation or whole-process-tree settlement guarantee.
func installTaskMutationFailure(s scheduler.Scheduler, name string, priorXML []byte, cause error) error {
	current, readErr := s.ExportXML(name)
	if len(priorXML) == 0 {
		if errors.Is(readErr, scheduler.ErrTaskNotFound) && len(current) == 0 {
			return cause
		}
	} else if readErr == nil && bytes.Equal(current, priorXML) {
		return cause
	}
	if readErr != nil {
		cause = errors.Join(cause, fmt.Errorf("observe task %s after failed mutation: %w", name, readErr))
	}
	return &InstallSchedulerRollbackIncompleteError{Tasks: []string{name}, cause: cause}
}

// installTaskRollback captures one acknowledged creation in this execution.
// createdXML is an observation, not native ownership or an object-generation
// receipt. A replacement before that capture, between comparison and Delete,
// or with identical XML remains outside this bounded comparison contract.
type installTaskRollback struct {
	name         string
	priorXML     []byte
	createdXML   []byte
	snapshotPath string
	creator      scheduler.XMLCreator
}

func (r *installTaskRollback) rollback(s scheduler.Scheduler) error {
	if len(r.createdXML) == 0 {
		return r.failure("verify created definition", errors.New("created definition was not captured; task retained"))
	}
	current, readErr := s.ExportXML(r.name)
	if errors.Is(readErr, scheduler.ErrTaskNotFound) && len(current) == 0 {
		if len(r.priorXML) == 0 {
			return nil
		}
		return r.restore(s)
	}
	if readErr != nil {
		return r.failure("read current definition", readErr)
	}
	if len(current) == 0 {
		return r.failure("read current definition", errors.New("empty successful export; task retained"))
	}
	if len(r.priorXML) > 0 && bytes.Equal(current, r.priorXML) {
		return nil
	}
	if !bytes.Equal(current, r.createdXML) {
		return r.failure("compare current definition", errors.New("current definition changed; task retained"))
	}
	// Capability is required before removing an observed candidate, not after.
	if len(r.priorXML) > 0 && r.creator == nil {
		return r.failure("restore capability", scheduler.ErrNotImplemented)
	}
	// This read/check followed by name-based Delete is not atomic. In particular,
	// an external writer in this interval is not excluded by the manifest lease.
	if err := s.Delete(r.name); err != nil {
		return r.failure("delete candidate; outcome not established", err)
	}
	remaining, absenceErr := s.ExportXML(r.name)
	if !errors.Is(absenceErr, scheduler.ErrTaskNotFound) || len(remaining) != 0 {
		return r.failure("confirm absence after delete", errors.Join(absenceErr, errors.New("task absence was not confirmed; no restoration attempted")))
	}
	if len(r.priorXML) == 0 {
		return nil
	}
	return r.restore(s)
}

func (r *installTaskRollback) restore(s scheduler.Scheduler) error {
	if r.creator == nil {
		return r.failure("restore capability", scheduler.ErrNotImplemented)
	}
	// Native create-only semantics decide occupancy at the mutation. Never use
	// ImportXML, retry, or delete an occupant after a conflict/uncertain result.
	if err := r.creator.CreateXMLIfAbsent(r.name, r.priorXML); err != nil {
		return r.failure("register prior definition without replacement", err)
	}
	restored, readErr := s.ExportXML(r.name)
	if readErr != nil {
		return r.failure("read restored definition", readErr)
	}
	if !bytes.Equal(restored, r.priorXML) {
		return r.failure("verify restored definition", errors.New("registered definition does not match the retained prior XML; task retained"))
	}
	return nil
}

func (r *installTaskRollback) failure(phase string, cause error) error {
	if r.snapshotPath != "" {
		return fmt.Errorf("rollback scheduler task %s (%s; recovery snapshot retained at %q): %w", r.name, phase, r.snapshotPath, cause)
	}
	return fmt.Errorf("rollback scheduler task %s (%s): %w", r.name, phase, cause)
}
