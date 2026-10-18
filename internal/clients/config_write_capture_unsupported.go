//go:build !windows

package clients

import (
	"errors"
	"os"
)

func openRemovalSourceFileObserved(string) (*os.File, error) {
	return nil, errors.New("CONFIG_WRITE_CAPTURE_PLATFORM_UNQUALIFIED")
}
func openRemovalDirectoryIdentityObserved(string) (string, uintptr, func() error, error) {
	return "", 0, nil, errors.New("CONFIG_WRITE_CAPTURE_PLATFORM_UNQUALIFIED")
}
