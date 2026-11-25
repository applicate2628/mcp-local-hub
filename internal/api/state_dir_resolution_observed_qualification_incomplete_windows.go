//go:build windows && serena_qualification && !test_state_path_env

package api

import (
	"context"
	"errors"
)

func ResolveDaemonStateDirObserved(context.Context) (string, *StateDirResolutionObservation, error) {
	return "", nil, errors.New("SERENA_QUALIFICATION_STATE_ISOLATION_TAG_REQUIRED")
}
