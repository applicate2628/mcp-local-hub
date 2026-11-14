//go:build windows && serena_qualification && !test_state_path_env

package api

import (
	"context"
	"testing"
	"time"
)

func TestSerenaQualificationResolverRequiresStateIsolationBuild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	path, observation, err := ResolveDaemonStateDirObserved(ctx)
	if err == nil || path != "" || observation != nil {
		t.Fatalf("incomplete qualification build entered a state namespace: path=%q observation=%v err=%v", path, observation, err)
	}
}
