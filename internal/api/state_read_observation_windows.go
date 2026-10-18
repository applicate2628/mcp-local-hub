//go:build windows

package api

import (
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
)

func stateReadNativeError(err error) (string, *uint64) {
	var status windows.NTStatus
	if errors.As(err, &status) {
		code := uint64(status)
		return "ntstatus", &code
	}
	var native windows.Errno
	if errors.As(err, &native) {
		code := uint64(native)
		return "win32", &code
	}
	return "go", nil
}

func observedStateReadWindowsIdentity(info windows.ByHandleFileInformation) string {
	return stateFileReadObservationDigest([]byte(fmt.Sprintf("windows-file-object-v1|%d|%d|%d|%d|%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow, info.CreationTime.HighDateTime, info.CreationTime.LowDateTime)))
}
