//go:build windows

package clients

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

func openRemovalSourceFile(path string) (*os.File, error) {
	return openRemovalSourceFileMode(path, nil)
}
func openRemovalSourceFileObserved(path string, register func(*configWriteCaptureResource)) (*os.File, error) {
	return openRemovalSourceFileMode(path, register)
}
func registerRemovalCaptureHandle(handle windows.Handle, register func(*configWriteCaptureResource)) *configWriteCaptureResource {
	if register == nil || handle == 0 || handle == windows.InvalidHandle {
		return nil
	}
	resource := &configWriteCaptureResource{native: uintptr(handle), closeFn: func() error { return windows.CloseHandle(handle) }}
	register(resource)
	return resource
}

func openRemovalSourceFileMode(path string, register func(*configWriteCaptureResource)) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	resource := registerRemovalCaptureHandle(handle, register)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		if resource == nil {
			_ = windows.CloseHandle(handle)
		}
		return nil, err
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 {
		if resource == nil {
			_ = windows.CloseHandle(handle)
		}
		return nil, fmt.Errorf("nonregular or redirected removal source")
	}
	file := os.NewFile(uintptr(handle), path)
	if resource != nil {
		if err := resource.transferFile(file); err != nil {
			return nil, err
		}
	}
	return file, nil
}

// The caller owns the returned close function, retaining the verified directory
// handle across its source comparison/write. Reparse components are refused.
func openRemovalDirectoryIdentity(path string) (string, uintptr, func(), error) {
	id, handle, closeObserved, err := openRemovalDirectoryIdentityMode(path, nil)
	if closeObserved == nil {
		return id, handle, nil, err
	}
	return id, handle, func() { _ = closeObserved() }, err
}
func openRemovalDirectoryIdentityObserved(path string, register func(*configWriteCaptureResource)) (string, uintptr, func() error, error) {
	return openRemovalDirectoryIdentityMode(path, register)
}
func openRemovalDirectoryIdentityMode(path string, register func(*configWriteCaptureResource)) (string, uintptr, func() error, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0, nil, err
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	if register != nil {
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	handle, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY|windows.READ_CONTROL|windows.SYNCHRONIZE|windows.FILE_READ_ATTRIBUTES,
		share, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	resource := registerRemovalCaptureHandle(handle, register)
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
	if resource != nil {
		closeHandle = func() error { return resource.close().err }
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		if resource == nil {
			_ = closeHandle()
		}
		return "", 0, nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		if resource == nil {
			_ = closeHandle()
		}
		return "", 0, nil, fmt.Errorf("non-directory or redirected removal source component")
	}
	return fmt.Sprintf("%08x:%08x:%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), uintptr(handle), closeHandle, nil
}
