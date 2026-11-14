//go:build windows && amd64 && cgo

package caller

/*
#include "../include/identity_v1.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"syscall"
	"unsafe"
)

// IdentityObservation owns its image/SID copies. All fields were queried through
// the retained at-create Job-bound process before resume; they are not an atomic
// snapshot, a formatted SID receipt or the product's CreatorIdentity capability.
type IdentityObservation struct {
	PID                   uint32
	CreationTime100ns     uint64
	OriginalJobMembership JobMembership
	WaitState             IdentityWaitState
	ImagePathUTF16        []uint16
	TokenUserSID          []byte
}

type JobMembership uint32
type IdentityWaitState uint32

const (
	JobMember           JobMembership     = C.MH_ID_JOB_MEMBER
	JobNotMember        JobMembership     = C.MH_ID_JOB_NOT_MEMBER
	IdentityNotSignaled IdentityWaitState = C.MH_ID_WAIT_NOT_SIGNALED
	IdentitySignaled    IdentityWaitState = C.MH_ID_WAIT_SIGNALED
)

type IdentityStage uint32
type IdentityErrorKind uint32

const (
	IdentityStageNone     IdentityStage = C.MH_ID_STAGE_NONE
	IdentityStagePID      IdentityStage = C.MH_ID_STAGE_PID
	IdentityStageImage    IdentityStage = C.MH_ID_STAGE_IMAGE
	IdentityStageCreation IdentityStage = C.MH_ID_STAGE_CREATION
	IdentityStageJob      IdentityStage = C.MH_ID_STAGE_JOB
	IdentityStageWait     IdentityStage = C.MH_ID_STAGE_WAIT
	IdentityStageToken    IdentityStage = C.MH_ID_STAGE_TOKEN
	IdentityStageComplete IdentityStage = C.MH_ID_STAGE_COMPLETE

	IdentityNoError     IdentityErrorKind = C.MH_ID_ERROR_NONE
	IdentityNativeError IdentityErrorKind = C.MH_ID_ERROR_NATIVE
	IdentityInvalidData IdentityErrorKind = C.MH_ID_ERROR_INVALID_DATA
	IdentityNoMemory    IdentityErrorKind = C.MH_ID_ERROR_NO_MEMORY
	IdentityOtherError  IdentityErrorKind = C.MH_ID_ERROR_OTHER
)

var ErrIdentityUnsupported = errors.New("native library does not support identity observation")

// IdentityError preserves a query failure and token-close failure independently.
// NativeError is valid only for IdentityNativeError. A close-only error has
// IdentityNoError and NativeError==0; it still rejects the observation.
type IdentityError struct {
	Stage           IdentityStage
	Kind            IdentityErrorKind
	NativeError     int32
	TokenCloseError int32
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("identity stage=%d query_kind=%d native=%d token_close=%d",
		e.Stage, e.Kind, e.NativeError, e.TokenCloseError)
}
func (e *IdentityError) Unwrap() []error {
	var causes []error
	if e.NativeError != 0 {
		causes = append(causes, syscall.Errno(uint32(e.NativeError)))
	}
	if e.TokenCloseError != 0 {
		causes = append(causes, syscall.Errno(uint32(e.TokenCloseError)))
	}
	return causes
}

func invalidIdentity(reason string) error {
	return fmt.Errorf("%w: %s", ErrAdmissionObservation, reason)
}

// The fixed report carries no native pointers. Validate metadata before reading
// semantic fields, including on native/denial/argument failure statuses.
func identityReportResult(raw C.mh_identity_report) (IdentityStage, error) {
	if raw.size != C.sizeof_mh_identity_report || raw.version != C.MH_ID_VERSION {
		return IdentityStageNone, invalidIdentity("incompatible identity report")
	}
	e := &IdentityError{
		Stage: IdentityStage(raw.stage), Kind: IdentityErrorKind(raw.query_error_kind),
		NativeError: int32(raw.query_native_error), TokenCloseError: int32(raw.token_close_error),
	}
	if e.Stage > IdentityStageComplete || e.Kind > IdentityOtherError ||
		(e.Kind == IdentityNativeError) != (e.NativeError != 0) ||
		(e.TokenCloseError != 0 && e.Stage != IdentityStageToken) {
		return IdentityStageNone, invalidIdentity("incoherent identity error fields")
	}
	failed := e.Kind != IdentityNoError || e.TokenCloseError != 0
	if e.Stage == IdentityStageNone || e.Stage == IdentityStageComplete {
		if failed {
			return IdentityStageNone, invalidIdentity("identity stage contradicts failure")
		}
		return e.Stage, nil
	}
	if !failed {
		return IdentityStageNone, invalidIdentity("query stage omitted its failure")
	}
	return e.Stage, e
}

// Validate the complete fixed schema before traversing the image pointer.
// Native memory validity still belongs to the trusted DLL's caller contract.
func copyIdentity(raw *C.mh_identity, bytes C.uint32_t) (IdentityObservation, error) {
	if raw == nil || bytes != C.sizeof_mh_identity ||
		uintptr(unsafe.Pointer(raw))%unsafe.Alignof(C.mh_identity{}) != 0 {
		return IdentityObservation{}, invalidIdentity("invalid identity storage")
	}
	if raw.size != C.sizeof_mh_identity || raw.version != C.MH_ID_VERSION || raw.reserved != 0 {
		return IdentityObservation{}, invalidIdentity("incompatible identity layout")
	}
	v := IdentityObservation{
		PID: uint32(raw.pid), CreationTime100ns: uint64(raw.creation_time_100ns),
		OriginalJobMembership: JobMembership(raw.job_membership),
		WaitState:             IdentityWaitState(raw.wait_state),
	}
	if v.PID == 0 || v.CreationTime100ns == 0 ||
		(v.OriginalJobMembership != JobMember && v.OriginalJobMembership != JobNotMember) ||
		(v.WaitState != IdentityNotSignaled && v.WaitState != IdentitySignaled) ||
		(v.WaitState == IdentityNotSignaled && v.OriginalJobMembership != JobMember) {
		return IdentityObservation{}, invalidIdentity("missing or inconsistent process facts")
	}
	n := int(raw.sid_len)
	if n < 8 || n > C.MH_ID_SID_CAPACITY {
		return IdentityObservation{}, invalidIdentity("invalid SID length")
	}
	sid := unsafe.Slice((*byte)(unsafe.Pointer(&raw.sid[0])), C.MH_ID_SID_CAPACITY)
	// SID revision 1, fixed 8-byte header and DWORD subauthorities. The native
	// owner already validates IsValidSid; this independently validates wire shape.
	if sid[0] != 1 || sid[1] > 15 || n != 8+4*int(sid[1]) {
		return IdentityObservation{}, invalidIdentity("invalid SID structure")
	}
	for _, value := range sid[n:] {
		if value != 0 {
			return IdentityObservation{}, invalidIdentity("nonzero unused SID storage")
		}
	}
	image := raw.image
	if image.data == nil || image.len == 0 || image.len > 32768 ||
		uintptr(unsafe.Pointer(image.data))%unsafe.Alignof(uint16(0)) != 0 {
		return IdentityObservation{}, invalidIdentity("invalid identity image storage")
	}
	v.ImagePathUTF16 = append([]uint16(nil), unsafe.Slice((*uint16)(unsafe.Pointer(image.data)), int(image.len))...)
	for _, unit := range v.ImagePathUTF16 {
		if unit == 0 {
			return IdentityObservation{}, invalidIdentity("embedded image terminator")
		}
	}
	v.TokenUserSID = append([]byte(nil), sid[:n]...)
	return v, nil
}

type identityCall struct {
	check func(IdentityObservation) error
	calls int
	err   error
}

//export mh_go_identity
func mh_go_identity(raw *C.mh_identity, bytes C.uint32_t, token C.uintptr_t) (decision C.uint32_t) {
	var state *identityCall
	returned := false
	defer func() {
		if !returned {
			_ = recover()
			decision = 0
			if state != nil {
				state.err = errors.Join(state.err, ErrAdmissionPanic)
			}
		}
	}()
	// A call-scoped integer handle crosses C; no Go pointer is kept in Rust.
	// https://pkg.go.dev/runtime/cgo#Handle
	state = cgo.Handle(token).Value().(*identityCall)
	state.calls++
	if state.calls != 1 {
		state.err = errors.Join(state.err, invalidIdentity("repeated identity callback"))
		returned = true
		return 0
	}
	value, err := copyIdentity(raw, bytes)
	if err != nil {
		state.err = errors.Join(state.err, err)
		returned = true
		return 0
	}
	state.err = state.check(value)
	returned = true
	if state.err != nil {
		return 0
	}
	return 1
}
