//go:build !windows && !linux

package process

import (
	"errors"
	"testing"
)

func TestProcessOwnerMatchesCurrentUnsupported(t *testing.T) {
	matches, err := ProcessOwnerMatchesCurrent(1)
	if matches || !errors.Is(err, ErrProcessOwnerUnsupported) {
		t.Fatalf("ProcessOwnerMatchesCurrent = %v, %v; want false, ErrProcessOwnerUnsupported", matches, err)
	}
}
