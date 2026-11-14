//go:build windows && !serena_qualification

package api

import "context"

func ResolveDaemonStateDirObserved(ctx context.Context) (string, *StateDirResolutionObservation, error) {
	return resolveDaemonStateDirWithOperations(ctx, stateDirResolverNativeOperations())
}
