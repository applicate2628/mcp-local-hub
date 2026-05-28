//go:build !windows

package process

import (
	"errors"
	"testing"
)

func TestJobProtectionStatusOther(t *testing.T) {
	if got := JobProtectionStatus(nil); got != nil {
		t.Fatalf("JobProtectionStatus(nil) = %v, want nil on non-Windows no-op Job", *got)
	}
	if got := JobProtectionStatus(errors.New("stub error")); got != nil {
		t.Fatalf("JobProtectionStatus(error) = %v, want nil on non-Windows no-op Job", *got)
	}
}
