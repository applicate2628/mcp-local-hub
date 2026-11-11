//go:build windows

package process

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This is the STARTUPINFOEX ABI with its native allocation kept as an opaque
// address. LocalAlloc memory is not a Go pointer; only the embedded StartupInfo
// is passed to CreateProcess. The size and field offset are checked in tests.
type procThreadAttributeStartupInfo struct {
	windows.StartupInfo
	ProcThreadAttributeList uintptr
}

// Compile-time equality checks cover every Windows architecture, including
// cross-builds that cannot execute the native layout test on the build host.
var (
	_ [unsafe.Sizeof(procThreadAttributeStartupInfo{}) - unsafe.Sizeof(windows.StartupInfoEx{})]byte
	_ [unsafe.Sizeof(windows.StartupInfoEx{}) - unsafe.Sizeof(procThreadAttributeStartupInfo{})]byte
	_ [unsafe.Offsetof(procThreadAttributeStartupInfo{}.ProcThreadAttributeList) - unsafe.Offsetof(windows.StartupInfoEx{}.ProcThreadAttributeList)]byte
	_ [unsafe.Offsetof(windows.StartupInfoEx{}.ProcThreadAttributeList) - unsafe.Offsetof(procThreadAttributeStartupInfo{}.ProcThreadAttributeList)]byte
)

var procAttributeDLL = windows.NewLazySystemDLL("kernel32.dll")
var procAttributeInitialize = procAttributeDLL.NewProc("InitializeProcThreadAttributeList")
var procAttributeUpdate = procAttributeDLL.NewProc("UpdateProcThreadAttribute")
var procAttributeDelete = procAttributeDLL.NewProc("DeleteProcThreadAttributeList")

func newProcThreadAttributeListObserved(count uint32) (*procThreadAttributeListObserved, error) {
	return newProcThreadAttributeListWithNative(count, procThreadAttributeNative{
		insufficientBuffer: windows.ERROR_INSUFFICIENT_BUFFER,
		initialize: func(data uintptr, count uint32, size *uintptr) error {
			result, _, err := procAttributeInitialize.Call(data, uintptr(count), 0, uintptr(unsafe.Pointer(size)))
			if result == 0 {
				if errors.Is(err, windows.ERROR_SUCCESS) {
					return errors.New("InitializeProcThreadAttributeList returned false/error0")
				}
				return err
			}
			return nil
		},
		allocate: func(size uint32) (uintptr, error) { return windows.LocalAlloc(windows.LMEM_FIXED, size) },
		update: func(data uintptr, attribute uintptr, value unsafe.Pointer, size uintptr) error {
			result, _, err := procAttributeUpdate.Call(data, 0, attribute, uintptr(value), size, 0, 0)
			if result == 0 {
				if errors.Is(err, windows.ERROR_SUCCESS) {
					return errors.New("UpdateProcThreadAttribute returned false/error0")
				}
				return err
			}
			return nil
		},
		delete: func(data uintptr) error {
			if err := procAttributeDelete.Find(); err != nil {
				return err
			}
			// VOID: defined return is recorded; LastError is not a success code.
			procAttributeDelete.Call(data)
			return nil
		},
		free: func(data uintptr) error {
			remainder, err := windows.LocalFree(windows.Handle(data))
			if remainder != 0 {
				return errors.Join(err, errors.New("LocalFree returned original allocation"))
			}
			return err
		},
	})
}
