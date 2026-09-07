//go:build !windows

package api

import (
	"errors"
	"testing"

	"mcp-local-hub/internal/scheduler"
)

func TestLivenessPrincipalSIDNonWindowsPreservesSchedulerNotImplemented(t *testing.T) {
	_, err := livenessPrincipalSID()
	if !errors.Is(err, scheduler.ErrNotImplemented) {
		t.Fatalf("livenessPrincipalSID error = %v, want scheduler.ErrNotImplemented", err)
	}
}
