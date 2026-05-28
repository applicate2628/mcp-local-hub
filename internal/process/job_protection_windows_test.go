//go:build windows

package process

import (
	"errors"
	"testing"
)

func TestJobProtectionStatusWindows(t *testing.T) {
	protected := JobProtectionStatus(nil)
	if protected == nil || *protected != true {
		t.Fatalf("JobProtectionStatus(nil) = %v, want explicit true on Windows", protected)
	}

	unprotected := JobProtectionStatus(errors.New("CreateJobObject failed"))
	if unprotected == nil || *unprotected != false {
		t.Fatalf("JobProtectionStatus(error) = %v, want explicit false on Windows", unprotected)
	}
}
