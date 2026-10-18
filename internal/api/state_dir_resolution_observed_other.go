//go:build !windows

package api

import (
	"context"
	"errors"
)

func ResolveDaemonStateDirObserved(ctx context.Context) (string, *StateDirResolutionObservation, error) {
	return "", nil, errors.New("observed canonical known-folder resolver unavailable on this platform")
}
