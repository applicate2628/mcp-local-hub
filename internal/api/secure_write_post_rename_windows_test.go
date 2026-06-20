//go:build windows

package api

import (
	"fmt"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecureWritePostRenameRetryableNTStatusReopenFailures(t *testing.T) {
	for _, err := range []error{
		windows.STATUS_SHARING_VIOLATION,
		windows.STATUS_PENDING,
		windows.STATUS_OPLOCK_BREAK_IN_PROGRESS,
		windows.STATUS_LOCK_NOT_GRANTED,
		windows.STATUS_DELETE_PENDING,
		fmt.Errorf("wrapped: %w", windows.STATUS_SHARING_VIOLATION),
	} {
		if !isRetryablePostRenameOpenErrWindows(err) {
			t.Fatalf("isRetryablePostRenameOpenErrWindows(%v) = false, want true", err)
		}
	}
}
