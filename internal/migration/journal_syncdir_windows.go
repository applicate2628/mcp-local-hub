//go:build windows

package migration

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func syncDirectory(dir string) error {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		path,
		windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.FlushFileBuffers(handle); err != nil {
		return fmt.Errorf("flush directory: %w", err)
	}
	return nil
}
