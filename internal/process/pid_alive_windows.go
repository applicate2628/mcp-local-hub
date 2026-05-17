//go:build windows

package process

import "golang.org/x/sys/windows"

// IsPidAlive reports whether pid currently refers to a live process.
func IsPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return ev != uint32(windows.WAIT_OBJECT_0)
}
