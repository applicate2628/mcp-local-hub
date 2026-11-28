//go:build windows

package binaryadmission

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func runtimeProductWin32Path(path string) string {
	// Match Go's short-path cutoff in os/path_windows.go. Its native full
	// path resolver normalizes long DOS/UNC paths before extended syntax.
	const longPathThreshold = 248
	if len(path) < longPathThreshold || !filepath.IsAbs(path) ||
		strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path
	}
	full, err := syscall.FullPath(path)
	if err != nil {
		return path
	}
	if strings.HasPrefix(full, `\\`) {
		return `\\?\UNC\` + full[2:]
	}
	return `\\?\` + full
}

func openWindowsRuntimeProduct(path string) (*os.File, error) {
	name, err := windows.UTF16PtrFromString(runtimeProductWin32Path(path))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// Keep ordinary read access and sharing, adding only the share required
	// by the upgrade owner's DELETE access on this file. Opening the parent
	// separately would conflict with a retained newly-created directory.
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		return nil, errors.Join(errors.New("runtime Product file wrapper unavailable"), windows.CloseHandle(h))
	}
	return f, nil
}
