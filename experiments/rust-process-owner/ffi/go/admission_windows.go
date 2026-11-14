//go:build windows && amd64 && cgo

package caller

/*
#include <stdint.h>
#include <stddef.h>
*/
import "C"

import (
	"errors"
	"runtime/cgo"
	"unsafe"
)

// ProcessObservation contains a Go-owned copy of data queried using the original
// retained process handle, not a path echoed from the create request. It is not
// a file hash/signature, access-token identity or general authorization proof.
// ImagePathUTF16 preserves Windows units; callers choose policy/decoding.
type ProcessObservation struct {
	PID            uint32
	ImagePathUTF16 []uint16
}

var ErrAdmissionPanic = errors.New("admission policy panicked")
var ErrAdmissionObservation = errors.New("invalid native admission observation")

type admissionCall struct {
	check func(ProcessObservation) error
	calls int
	err   error
}

//export mh_go_admission
func mh_go_admission(pid C.uint32_t, image *C.uint16_t, length C.size_t, token C.uintptr_t) (decision C.uint32_t) {
	var state *admissionCall
	returned := false
	// A policy panic must be contained HERE, before unwinding through C/Rust.
	// Do not print its payload; it may contain sensitive caller data. The flag
	// also covers panic(nil) without relying on the runtime panicnil setting.
	// Goexit/longjmp/fatal runtime abort are not recoverable callback contracts.
	defer func() {
		if !returned {
			_ = recover()
			decision = 0
			if state != nil {
				state.err = ErrAdmissionPanic
			}
		}
	}()
	// The shim/DLL return this call-scoped integer unchanged. No Go pointer is
	// stored in native memory. AdmitChecked deletes it only after native return.
	state = cgo.Handle(token).Value().(*admissionCall)
	state.calls++
	if state.calls != 1 || pid == 0 || image == nil || length == 0 || length > 32768 || uintptr(unsafe.Pointer(image))%unsafe.Alignof(uint16(0)) != 0 {
		state.err = errors.Join(state.err, ErrAdmissionObservation)
		returned = true
		return 0
	}
	// The FFI contract supplies a live initialized read-only native view.
	// Copy before policy/GC/retention; never expose the Rust address to policy.
	units := append([]uint16(nil), unsafe.Slice((*uint16)(unsafe.Pointer(image)), int(length))...)
	for _, unit := range units {
		if unit == 0 {
			state.err = errors.Join(state.err, ErrAdmissionObservation)
			returned = true
			return 0
		}
	}
	state.err = state.check(ProcessObservation{PID: uint32(pid), ImagePathUTF16: units})
	returned = true
	if state.err != nil {
		return 0
	}
	return 1
}
