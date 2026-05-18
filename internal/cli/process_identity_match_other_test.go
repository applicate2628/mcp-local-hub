//go:build !windows && !linux

package cli

import (
	"os"
	"testing"
)

func TestPidMatchesMcphub_macOSDefaultsClosed(t *testing.T) {
	if pidMatchesMcphub(os.Getpid()) {
		t.Fatalf("current test binary pid %d must not match on macOS/BSD fail-closed path", os.Getpid())
	}
}
