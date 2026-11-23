//go:build windows

package cli

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func observeConcurrentSecretsObject(path string, dir bool) (concurrentSecretsObjectIdentity, os.FileInfo, error) {
	var identity concurrentSecretsObjectIdentity
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return identity, nil, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return identity, nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	var info windows.ByHandleFileInformation
	nativeErr := windows.GetFileInformationByHandle(handle, &info)
	stat, statErr := file.Stat()
	if err := errors.Join(nativeErr, statErr, file.Close()); err != nil {
		return identity, nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		stat.IsDir() != dir || (!dir && (!stat.Mode().IsRegular() || info.NumberOfLinks != 1)) {
		return identity, nil, errors.New("concurrent secrets fixture requires an ordinary object")
	}
	identity = concurrentSecretsObjectIdentity{
		Volume:   uint64(info.VolumeSerialNumber),
		File:     uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		Creation: uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime),
	}
	if identity.File == 0 {
		return identity, nil, errors.New("concurrent secrets fixture file identity is unavailable")
	}
	return identity, stat, nil
}
