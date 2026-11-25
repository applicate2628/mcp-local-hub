//go:build !windows || !serena_qualification || !test_state_path_env

package api

import (
	"context"
	"errors"
)

// An observation from the qualification schema never grants production
// authority, even if a caller supplies a complete-looking snapshot.
func validateSerenaQualificationResolution(context.Context, *StateDirResolutionObservation, string) error {
	return errors.New("SERENA_QUALIFICATION_NAMESPACE_BUILD_UNAVAILABLE")
}
