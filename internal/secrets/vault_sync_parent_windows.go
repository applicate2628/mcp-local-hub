//go:build windows

package secrets

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func syncParentDir(dir string) error {
	dirW, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return fmt.Errorf("utf16 parent dir %q: %w", dir, err)
	}
	handle, err := windows.CreateFile(
		dirW,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("open parent dir %s: %w", dir, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("flush parent dir %s: %w", dir, err)
	}
	return nil
}

func isUnsupportedParentDirSyncError(err error) bool {
	return errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
		errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}
