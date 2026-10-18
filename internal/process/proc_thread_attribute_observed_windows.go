//go:build windows

package process

import (
	"errors"
	"golang.org/x/sys/windows"
	"unsafe"
)

var procAttributeDLL = windows.NewLazySystemDLL("kernel32.dll")
var procAttributeInitialize = procAttributeDLL.NewProc("InitializeProcThreadAttributeList")
var procAttributeUpdate = procAttributeDLL.NewProc("UpdateProcThreadAttribute")
var procAttributeDelete = procAttributeDLL.NewProc("DeleteProcThreadAttributeList")

func newProcThreadAttributeListObserved(count uint32) (*procThreadAttributeListObserved, error) {
	return newProcThreadAttributeListWithNative(count, procThreadAttributeNative{
		insufficientBuffer: windows.ERROR_INSUFFICIENT_BUFFER,
		initialize: func(data unsafe.Pointer, count uint32, size *uintptr) error {
			result, _, err := procAttributeInitialize.Call(uintptr(data), uintptr(count), 0, uintptr(unsafe.Pointer(size)))
			if result == 0 {
				if errors.Is(err, windows.ERROR_SUCCESS) {
					return errors.New("InitializeProcThreadAttributeList returned false/error0")
				}
				return err
			}
			return nil
		},
		allocate: func(size uint32) (uintptr, error) { return windows.LocalAlloc(windows.LMEM_FIXED, size) },
		update: func(data unsafe.Pointer, attribute uintptr, value unsafe.Pointer, size uintptr) error {
			result, _, err := procAttributeUpdate.Call(uintptr(data), 0, attribute, uintptr(value), size, 0, 0)
			if result == 0 {
				if errors.Is(err, windows.ERROR_SUCCESS) {
					return errors.New("UpdateProcThreadAttribute returned false/error0")
				}
				return err
			}
			return nil
		},
		delete: func(data unsafe.Pointer) error {
			if err := procAttributeDelete.Find(); err != nil {
				return err
			}
			// VOID: defined return is recorded; LastError is not a success code.
			procAttributeDelete.Call(uintptr(data))
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
