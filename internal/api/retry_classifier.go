// Package api — error retryability classifier. Memo D9.
package api

import (
	"errors"
	"io/fs"
	"strings"
)

// Sentinel errors for the documented non-retryable classes (memo D9).
var (
	ErrBinaryNotFound         = errors.New("binary not found")
	ErrPermissionDenied       = errors.New("permission denied")
	ErrBadConfig              = errors.New("bad config")
	ErrUnrecoverableLockState = errors.New("unrecoverable lock state")
)

// IsRetryableError returns false for documented non-retryable classes
// and true otherwise. Nil returns false (defensive degenerate).
func IsRetryableError(err error) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{
		ErrBinaryNotFound, ErrPermissionDenied, ErrBadConfig, ErrUnrecoverableLockState,
	} {
		if errors.Is(err, sentinel) {
			return false
		}
	}
	if errors.Is(err, fs.ErrPermission) {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	for _, sig := range []string{
		"executable not found in $PATH",
		"the system cannot find the file specified",
	} {
		if strings.Contains(err.Error(), sig) {
			return false
		}
	}
	return true
}
