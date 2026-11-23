//go:build !windows

package clients

import (
	"errors"
	"os"
)

func openRemovalSourceFileObserved(string, func(*configWriteCaptureResource)) (*os.File, error) {
	return nil, errors.New("CONFIG_WRITE_CAPTURE_PLATFORM_UNQUALIFIED")
}
func openRemovalDirectoryIdentityObserved(string, func(*configWriteCaptureResource)) (string, uintptr, func() error, error) {
	return "", 0, nil, errors.New("CONFIG_WRITE_CAPTURE_PLATFORM_UNQUALIFIED")
}
