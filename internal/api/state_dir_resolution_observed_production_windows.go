//go:build windows && !serena_qualification

package api

import (
	"context"
	"testing"
)

func ResolveDaemonStateDirObserved(ctx context.Context) (string, *StateDirResolutionObservation, error) {
	if testEnvFallbackBuild {
		return resolveTestStateDirObserved(ctx, daemonStateDirReadOnly, "test-state-path-v1|test_state_path_env|daemonStateDirReadOnly")
	}
	if testing.Testing() && daemonStateRootOverride != "" {
		return resolveTestStateDirObserved(ctx, func() (string, error) {
			return daemonStateRootOverride, nil
		}, "test-state-path-v1|testing.Testing|daemonStateRootOverride")
	}
	return resolveDaemonStateDirWithOperations(ctx, stateDirResolverNativeOperations())
}

// Test resolution receives the existing test path owner without claiming a
// KnownFolder allocation. Its original getter remains bound for later receiving.
