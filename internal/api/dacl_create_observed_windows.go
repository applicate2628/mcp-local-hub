//go:build windows

package api

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var observedSDFromStringProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")

type createDescriptorOperations struct {
	convert func(string) (*windows.SECURITY_DESCRIPTOR, bool, error)
	valid   func(*windows.SECURITY_DESCRIPTOR) bool
	free    func(windows.Handle) (windows.Handle, error)
}

func createDescriptorNativeOperations() createDescriptorOperations {
	return createDescriptorOperations{
		convert: func(sddl string) (*windows.SECURITY_DESCRIPTOR, bool, error) {
			input, err := windows.UTF16PtrFromString(sddl)
			if err != nil {
				return nil, false, err
			}
			if err := observedSDFromStringProc.Find(); err != nil {
				return nil, false, err
			}
			var descriptor *windows.SECURITY_DESCRIPTOR
			result, _, cause := observedSDFromStringProc.Call(uintptr(unsafe.Pointer(input)), 1, uintptr(unsafe.Pointer(&descriptor)), 0)
			if result == 0 {
				if cause == nil || cause == syscall.Errno(0) {
					cause = errors.New("descriptor conversion failed without a defined native error")
				}
				return descriptor, false, cause
			}
			return descriptor, true, nil
		},
		valid: func(sd *windows.SECURITY_DESCRIPTOR) bool { return sd.IsValid() && sd.Length() > 0 },
		free:  windows.LocalFree,
	}
}

func buildAllowlistSDObserved(b *stateFileReadBuilder, slot string) (*windows.SECURITY_DESCRIPTOR, func() error, error) {
	if b == nil {
		return nil, nil, errors.New("original descriptor observation owner required")
	}
	if err := b.checkContext(); err != nil {
		return nil, nil, err
	}
	sid, err := currentUserSID()
	if err != nil {
		return nil, nil, err
	}
	value, err := b.formatSIDObserved(sid, slot+"-current-sid")
	if err != nil {
		return nil, nil, err
	}
	sddl, err := buildAllowlistSDDLForUser(AllowlistMaskFile, value)
	if err != nil {
		return nil, nil, err
	}
	return buildAllowlistDescriptorWithOperations(b, slot, sddl, createDescriptorNativeOperations())
}

func buildAllowlistDescriptorWithOperations(b *stateFileReadBuilder, slot, sddl string, ops createDescriptorOperations) (descriptor *windows.SECURITY_DESCRIPTOR, release func() error, retErr error) {
	if b == nil || ops.convert == nil || ops.valid == nil || ops.free == nil {
		return nil, nil, errors.New("original descriptor conversion owner required")
	}
	if err := b.checkContext(); err != nil {
		return nil, nil, err
	}
	row := b.prearmResource(slot)
	descriptor, succeeded, err := ops.convert(sddl)
	if !succeeded {
		if descriptor != nil {
			row.view.Ownership = "borrowed"
			b.borrowedErr = errors.Join(b.borrowedErr, errors.New("failed descriptor conversion has undefined nonnull output; no free authority"))
		}
		return nil, nil, errors.Join(err, errors.New("original create-time descriptor conversion failed"))
	}
	if descriptor == nil {
		return nil, nil, errors.Join(err, errors.New("successful descriptor conversion has no allocation"))
	}
	allocation := windows.Handle(uintptr(unsafe.Pointer(descriptor)))
	b.acquire(slot, func() error {
		remaining, closeErr := ops.free(allocation)
		if remaining != 0 {
			closeErr = errors.Join(closeErr, errors.New("create-time descriptor LocalFree returned nonnull"))
		}
		return closeErr
	})
	release = func() error { return b.closeResource(slot) }
	if err != nil {
		return nil, release, err
	}
	if err := b.checkContext(); err != nil {
		return nil, release, err
	}
	if !ops.valid(descriptor) {
		return nil, release, errors.New("original create-time descriptor is invalid")
	}
	b.identity(slot, stateFileReadObservationDigest([]byte(fmt.Sprintf("create-descriptor-allocation-v1|%x", allocation))))
	return descriptor, release, nil
}
