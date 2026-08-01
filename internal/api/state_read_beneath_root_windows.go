//go:build windows

package api

import (
	"context"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ReadStateFileBeneathRootNoFollow reads one bounded regular state file through
// a retained no-follow handle chain. The bytes returned are the bytes hashed
// from the final handle; no pathname is reopened after loading.
func ReadStateFileBeneathRootNoFollow(ctx context.Context, root string, relativeComponents []string, expectedSHA256 string) ([]byte, error) {
	return readStateFileBeneathRootNoFollow(ctx, root, relativeComponents, expectedSHA256, nil)
}

func readStateFileBeneathRootNoFollow(
	ctx context.Context,
	root string,
	relativeComponents []string,
	expectedSHA256 string,
	step stateReadBeneathRootStepFunc,
) (result []byte, retErr error) {
	if err := validateStateReadBeneathRootInput(root, relativeComponents, expectedSHA256); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-root-open", "", err)
	}
	rootHandle, err := ntOpenStateReadRoot(root)
	if err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-root", "", err)
	}
	if err := refuseReparsePointHandle(rootHandle); err != nil {
		_ = windows.CloseHandle(rootHandle)
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-root", "", err)
	}
	currentHandle := rootHandle
	defer func() {
		if closeErr := windows.CloseHandle(currentHandle); closeErr != nil && retErr == nil {
			zeroStateSecretBytes(result)
			result = nil
			retErr = newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-parent", "", closeErr)
		}
	}()

	for i, component := range relativeComponents[:len(relativeComponents)-1] {
		if err := ctx.Err(); err != nil {
			return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-component-open", component, err)
		}
		if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
			Event: stateReadBeneathRootBeforeComponentOpen, ComponentIndex: i, Component: component,
		}); err != nil {
			return nil, err
		}
		nextHandle, openErr := ntOpenStateReadRelative(currentHandle, component, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES, true)
		if openErr != nil {
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-component", component, openErr)
		}
		if infoErr := refuseReparsePointHandle(nextHandle); infoErr != nil {
			_ = windows.CloseHandle(nextHandle)
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-component", component, infoErr)
		}
		if closeErr := windows.CloseHandle(currentHandle); closeErr != nil {
			_ = windows.CloseHandle(nextHandle)
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-parent", component, closeErr)
		}
		currentHandle = nextHandle
	}
	finalComponent := relativeComponents[len(relativeComponents)-1]
	if err := ctx.Err(); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-final-open", finalComponent, err)
	}
	if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
		Event: stateReadBeneathRootBeforeComponentOpen, ComponentIndex: len(relativeComponents) - 1, Component: finalComponent,
	}); err != nil {
		return nil, err
	}
	finalHandle, err := ntOpenStateReadRelative(currentHandle, finalComponent, windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES, false)
	if err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "open-final", finalComponent, err)
	}
	defer func() {
		if closeErr := windows.CloseHandle(finalHandle); closeErr != nil && retErr == nil {
			zeroStateSecretBytes(result)
			result = nil
			retErr = newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "close-final", finalComponent, closeErr)
		}
	}()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(finalHandle, &info); err != nil {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "stat-final", finalComponent, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "stat-final", finalComponent, nil)
	}
	size := int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow)
	if size > maxStateFileBytes {
		return nil, newStateFileReadError(StateFileReadErrorTooLarge, "stat-final", finalComponent, nil)
	}
	buf := make([]byte, 0, minStateReadCapacity(size))
	chunk := make([]byte, stateReadChunkSize)
	defer zeroStateSecretBytes(chunk)
	for {
		if err := ctx.Err(); err != nil {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorCanceled, "before-read", finalComponent, err)
		}
		requested := stateReadRequestLimit(len(buf), len(chunk))
		if requested == 0 {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorTooLarge, "before-read", finalComponent, nil)
		}
		if err := invokeStateReadBeneathRootStep(step, stateReadBeneathRootStep{
			Event: stateReadBeneathRootBeforeRead, ComponentIndex: len(relativeComponents) - 1,
			Component: finalComponent, Requested: requested,
		}); err != nil {
			zeroStateSecretBytes(buf)
			return nil, err
		}
		var n uint32
		readErr := windows.ReadFile(finalHandle, chunk[:requested], &n, nil)
		remaining := maxStateFileBytes - len(buf)
		if int(n) > remaining {
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorTooLarge, "read-final", finalComponent, nil)
		}
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if readErr != nil {
			if readErr == windows.ERROR_HANDLE_EOF {
				break
			}
			zeroStateSecretBytes(buf)
			return nil, newStateFileReadError(StateFileReadErrorUnsafeObjectOrIO, "read-final", finalComponent, readErr)
		}
		if n == 0 {
			break
		}
	}
	if buf == nil {
		buf = []byte{}
	}
	if !stateReadChecksumMatches(buf, expectedSHA256) {
		zeroStateSecretBytes(buf)
		return nil, newStateFileReadError(StateFileReadErrorChecksumMismatch, "verify-checksum", finalComponent, nil)
	}
	return buf, nil
}

func ntOpenStateReadRelative(parent windows.Handle, name string, desiredAccess uint32, directory bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		ObjectName: objectName, RootDirectory: parent,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocSize int64
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle, desiredAccess|windows.SYNCHRONIZE, oa, &iosb, &allocSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options, 0, 0,
	); err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func ntOpenStateReadRoot(root string) (windows.Handle, error) {
	cleanRoot := filepath.Clean(root)
	nativeRoot := `\??\` + cleanRoot
	if strings.HasPrefix(cleanRoot, `\\`) {
		nativeRoot = `\??\UNC\` + strings.TrimPrefix(cleanRoot, `\\`)
	}
	objectName, err := windows.NewNTUnicodeString(nativeRoot)
	if err != nil {
		return windows.InvalidHandle, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		ObjectName: objectName,
		Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var allocSize int64
	var handle windows.Handle
	if err := windows.NtCreateFile(
		&handle, windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, oa, &iosb, &allocSize,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0, 0,
	); err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}
