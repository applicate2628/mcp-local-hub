//go:build !windows && !unix

package clients

import (
	"errors"
	"os"
)

func openRemovalSourceFile(string) (*os.File, error) {
	return nil, errors.New("removal source identity unsupported on this platform")
}

func openRemovalDirectoryIdentity(string) (string, uintptr, func(), error) {
	return "", 0, nil, errors.New("removal source identity unsupported on this platform")
}
