package api

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func openDaemonLog(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(daemonLogWindowsPath(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil // The File now owns the non-inheritable handle.
}

// Match os.Open's extended-length path handling for composed log paths.
// Preserve device and verbatim names, and keep the original name on errors.
func daemonLogWindowsPath(path string) string {
	if strings.HasPrefix(path, "\\\\?\\") || strings.HasPrefix(path, "\\??\\") || strings.HasPrefix(path, "\\\\.\\") {
		return path
	}
	// Go's Windows os.Open uses extended syntax when the effective absolute
	// path reaches 248 bytes (MAX_PATH minus room for an 8.3 filename).
	const extendedPathThreshold = 248
	if filepath.IsAbs(path) && len(path) < extendedPathThreshold {
		return path
	}
	absolute, err := filepath.Abs(path)
	if err != nil || len(absolute) < extendedPathThreshold {
		return path
	}
	if strings.HasPrefix(absolute, "\\\\") {
		return "\\\\?\\UNC\\" + absolute[2:]
	}
	return "\\\\?\\" + absolute
}
