//go:build windows

package process

import (
	"errors"
	"testing"

	"golang.org/x/sys/windows"
)

func TestJobObservedConfigurationFailureRetainsFirstClose(t *testing.T) {
	configureFailure := errors.New("injected configuration refusal")
	closeFailure := errors.New("injected reported close failure")
	var nativeClosed bool
	closeCalls := 0
	job, err := newJobObserved(JobOptions{KillOnClose: true}, jobAcquisitionOps{
		configure: func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error { return configureFailure },
		close: func(handle windows.Handle) error {
			closeCalls++
			nativeErr := windows.CloseHandle(handle)
			nativeClosed = nativeErr == nil
			return errors.Join(nativeErr, closeFailure)
		},
	})
	if job == nil || !errors.Is(err, configureFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("partial acquisition erased owner/causes: owner=%t err=%v", job != nil, err)
	}
	if !nativeClosed {
		t.Fatal("actual Job close failed; reported fault is not native closure")
	}
	if repeated := job.Close(); !errors.Is(repeated, closeFailure) || closeCalls != 1 {
		t.Fatalf("first close was lost or retried: err=%v calls=%d", repeated, closeCalls)
	}
}

func TestJobObservedNativeConfigureRefusalClosesOwnedHandle(t *testing.T) {
	failure := errors.New("injected required limit refusal")
	job, err := newJobObserved(JobOptions{KillOnClose: true}, jobAcquisitionOps{
		configure: func(windows.Handle, *windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION) error { return failure },
	})
	if job == nil || !errors.Is(err, failure) || !job.closeAttempted || job.closeError != nil || job.Handle() != 0 {
		t.Fatalf("no-child Job acquisition was not positively closed: owner=%t err=%v", job != nil, err)
	}
}
