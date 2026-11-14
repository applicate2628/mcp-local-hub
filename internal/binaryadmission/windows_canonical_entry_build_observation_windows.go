//go:build windows

package binaryadmission

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func observeCanonicalEntryBuildFileV1(file *os.File) (canonicalEntryBuildFileObservationV1, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return canonicalEntryBuildFileObservationV1{}, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_REPARSE_POINT|windows.FILE_ATTRIBUTE_DIRECTORY) != 0 || info.NumberOfLinks != 1 {
		return canonicalEntryBuildFileObservationV1{}, errors.New("build artifact is reparse/wrong-kind or has hardlink aliases")
	}
	return canonicalEntryBuildFileObservationV1{uint64(info.VolumeSerialNumber), uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), uint64(info.NumberOfLinks), info.FileAttributes}, nil
}
