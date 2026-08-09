//go:build !linux

package cli

import "testing"

func TestPlatformInternalCommandsAreLinuxOnly(t *testing.T) {
	if commands := platformInternalCommands(); len(commands) != 0 {
		t.Fatalf("platform internal commands=%d, want zero", len(commands))
	}
}
