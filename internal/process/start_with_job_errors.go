package process

import (
	"errors"
	"fmt"
)

// ErrSpawnPostCreate marks the narrow orphan window after the OS created a
// child but Go could not acquire the process handle required to reap it.
var ErrSpawnPostCreate = errors.New("spawn completed but process handle unavailable")

// StartWithJobPhase identifies the owner that failed before or while starting
// an at-create-contained child. Callers retain errors.Is/errors.As through the
// wrapped Cause; they must not infer a phase from operating-system text.
type StartWithJobPhase string

const (
	StartWithJobInvalid     StartWithJobPhase = "invalid"
	StartWithJobContainment StartWithJobPhase = "containment"
	StartWithJobLaunch      StartWithJobPhase = "launch"
)

// StartWithJobError preserves the phase without changing StartWithJob's
// established exported signature.
type StartWithJobError struct {
	Phase StartWithJobPhase
	Cause error
}

func (e *StartWithJobError) Error() string {
	if e == nil || e.Cause == nil {
		return "start with job: " + string(e.Phase)
	}
	return fmt.Sprintf("start with job %s: %v", e.Phase, e.Cause)
}

func (e *StartWithJobError) Unwrap() error { return e.Cause }

func startWithJobError(phase StartWithJobPhase, cause error) error {
	return &StartWithJobError{Phase: phase, Cause: cause}
}
