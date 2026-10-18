//go:build windows

package clients

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openRemovalSourceFile(path string) (*os.File, error) {
	return openRemovalSourceFileMode(path, false)
}
func openRemovalSourceFileObserved(path string) (*os.File, error) {
	return openRemovalSourceFileMode(path, true)
}
func openRemovalSourceFileMode(path string, observed bool) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		closeErr := windows.CloseHandle(handle)
		if observed {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		closeErr := windows.CloseHandle(handle)
		err := fmt.Errorf("nonregular or redirected removal source")
		if observed {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

// The caller owns the returned close function, retaining the verified directory
// handle across its source comparison/write. Reparse components are refused.
func openRemovalDirectoryIdentity(path string) (string, uintptr, func(), error) {
	id, handle, closeObserved, err := openRemovalDirectoryIdentityMode(path, false)
	if closeObserved == nil {
		return id, handle, nil, err
	}
	return id, handle, func() { _ = closeObserved() }, err
}
func openRemovalDirectoryIdentityObserved(path string) (string, uintptr, func() error, error) {
	return openRemovalDirectoryIdentityMode(path, true)
}
func openRemovalDirectoryIdentityMode(path string, observed bool) (string, uintptr, func() error, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, nil, err
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	if observed {
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	handle, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES,
		share, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return "", 0, nil, err
	}
	closeAttempted := false
	var closeErr error
	closeHandle := func() error {
		if !closeAttempted {
			closeAttempted = true
			closeErr = windows.CloseHandle(handle)
		}
		return closeErr
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		closeErr := closeHandle()
		if observed {
			return "", 0, nil, errors.Join(err, closeErr)
		}
		return "", 0, nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		closeErr := closeHandle()
		err := fmt.Errorf("non-directory or redirected removal source component")
		if observed {
			return "", 0, nil, errors.Join(err, closeErr)
		}
		return "", 0, nil, err
	}
	return fmt.Sprintf("%08x:%08x:%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), uintptr(handle), closeHandle, nil
}
