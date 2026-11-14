//go:build linux || darwin

package binaryadmission

import (
	"errors"
	"os"
	"syscall"
)

func observeCanonicalEntryBuildFileV1(file *os.File) (canonicalEntryBuildFileObservationV1, error) {
	info, err := file.Stat()
	if err != nil {
		return canonicalEntryBuildFileObservationV1{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || stat.Nlink != 1 {
		return canonicalEntryBuildFileObservationV1{}, errors.New("build artifact has unreceived native type/link count or hardlink aliases")
	}
	return canonicalEntryBuildFileObservationV1{Device: uint64(stat.Dev), Index: uint64(stat.Ino), Links: uint64(stat.Nlink)}, nil
}
