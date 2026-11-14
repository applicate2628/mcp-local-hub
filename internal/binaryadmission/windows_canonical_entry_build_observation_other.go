//go:build !windows && !linux && !darwin

package binaryadmission

import "os"

func observeCanonicalEntryBuildFileV1(*os.File) (canonicalEntryBuildFileObservationV1, error) {
	return canonicalEntryBuildFileObservationV1{}, ErrWindowsCanonicalEntryBuildObservationUnsupported
}
