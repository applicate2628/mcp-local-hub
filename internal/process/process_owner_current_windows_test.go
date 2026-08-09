//go:build windows

package process

import (
	"os"
	"testing"
)

func TestProcessOwnerMatchesCurrentWindows_CurrentPID(t *testing.T) {
	matches, err := ProcessOwnerMatchesCurrent(os.Getpid())
	if err != nil || !matches {
		t.Fatalf("ProcessOwnerMatchesCurrent(current PID) = %v, %v; want true, nil", matches, err)
	}
}
