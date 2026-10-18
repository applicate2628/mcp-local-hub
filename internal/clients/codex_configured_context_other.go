//go:build !windows

package clients

import (
	"errors"
	"os"
)

func codexConfiguredKnownFolders() (string, string, error) {
	return "", "", errors.New("configured source Windows cohort unavailable")
}

func codexConfiguredFileIdentity(*os.File) (string, error) {
	return "", errors.New("configured source Windows identity unavailable")
}

func observeCodexConfiguredPathObject(path string) (out codexConfiguredPathObservation, err error) {
	out.path = path
	err = errors.New("configured source Windows identity unavailable")
	out.ownerError = err
	return out, err
}
