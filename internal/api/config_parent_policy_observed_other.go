//go:build !windows

package api

import (
	"context"
	"errors"
)

func SecureConfigLockParentPolicyObserved(ctx context.Context, dir string, createMissing bool) (bool, *ConfigParentPolicyObservation, error) {
	return false, nil, errors.New("observed Windows parent preflight unavailable on this platform")
}
