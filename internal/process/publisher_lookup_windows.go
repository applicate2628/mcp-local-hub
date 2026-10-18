//go:build windows

package process

import (
	"context"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

type publisherClientID struct{ Process, Thread windows.Handle }
type publisherNativeOpenResult struct {
	handle  windows.Handle
	status  uint32
	defined bool
}
type publisherLookupOperations struct {
	capture     publisherCaptureOperations
	openNative  func(uint32) (publisherNativeOpenResult, error)
	abi         func() (string, error)
	processID   func(windows.Handle) (uint32, error)
	close       func(windows.Handle) error
	bindControl func(uint32, windows.Handle) (*heldPIDObservation, error)
}

var publisherNtOpenProcessProc = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtOpenProcess")

func publisherLookupNativeOperations() publisherLookupOperations {
	ops := publisherLookupOperations{capture: publisherCaptureNativeOperations(), processID: windows.GetProcessId, close: windows.CloseHandle}
	ops.bindControl = func(pid uint32, handle windows.Handle) (*heldPIDObservation, error) {
		return &heldPIDObservation{heldPIDJobObservation: &heldPIDJobObservation{pid: int(pid), handle: handle, verify: verifyHandleIdentity, identity: identityFromHandle, close: ops.close}, ownerSID: processHandleOwnerUserSID, wait: windows.WaitForSingleObject}, nil
	}
	ops.abi = func() (string, error) {
		width := unsafe.Sizeof(uintptr(0))
		var cid publisherClientID
		var oa windows.OBJECT_ATTRIBUTES
		wantOA := uintptr(24)
		wantRoot := uintptr(4)
		if width == 8 {
			wantOA = 48
			wantRoot = 8
		} else if width != 4 {
			return "", errors.New("unsupported native pointer width")
		}
		if unsafe.Sizeof(cid) != 2*width || unsafe.Offsetof(cid.Thread) != width || unsafe.Sizeof(oa) != wantOA || unsafe.Offsetof(oa.RootDirectory) != wantRoot || unsafe.Offsetof(oa.ObjectName) != 2*wantRoot {
			return "", errors.New("native process-query ABI layout differs")
		}
		return sidFormatObservationDigest([]byte(fmt.Sprintf("publisher-nt-open-process-v1|width=%d|cid=%d|oa=%d|access=%d|attributes=0|root=0|name=0|thread=0|ntdll!NtOpenProcess", width, unsafe.Sizeof(cid), unsafe.Sizeof(oa), windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE))), nil
	}
	ops.openNative = func(pid uint32) (publisherNativeOpenResult, error) {
		if pid == 0 {
			return publisherNativeOpenResult{}, errors.New("positive native PID required")
		}
		if _, err := ops.abi(); err != nil {
			return publisherNativeOpenResult{}, err
		}
		if err := publisherNtOpenProcessProc.Find(); err != nil {
			return publisherNativeOpenResult{}, err
		}
		cid := publisherClientID{Process: windows.Handle(pid)}
		oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{}))}
		var handle windows.Handle
		status, _, _ := publisherNtOpenProcessProc.Call(uintptr(unsafe.Pointer(&handle)), uintptr(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE), uintptr(unsafe.Pointer(&oa)), uintptr(unsafe.Pointer(&cid)))
		return publisherNativeOpenResult{handle: handle, status: uint32(status), defined: true}, nil
	}
	return ops
}

func LookupPublisherIncarnation(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) (*PublisherQuiescenceObservation, error) {
	return lookupPublisherIncarnationWithOperations(ctx, record, binding, publisherLookupNativeOperations())
}

func lookupPublisherIncarnationWithOperations(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding, ops publisherLookupOperations) (observation *PublisherQuiescenceObservation, retErr error) {
	o := &PublisherQuiescenceObservation{issuer: &publisherIncarnationIssuer{marker: 1}, record: record, binding: binding, view: PublisherQuiescenceSnapshotV1{Schema: "publisher-quiescence-observation-v1", Version: 1, Disposition: "unavailable", CleanupErrors: []SIDFormatFailureV1{}}}
	observation = o
	defer func() {
		o.finalized = true
		o.finishedInBound = ctx != nil && ctx.Err() == nil
		o.view.Complete = o.finishedInBound && o.view.ResourcesComplete && o.primaryErr == nil && o.cleanupErr == nil && (o.view.Disposition == "original-signaled" || o.view.Disposition == "pid-generation-replaced" || o.view.Disposition == "qualified-invalid-cid")
		if o.primaryErr != nil {
			o.view.PrimaryError = &SIDFormatFailureV1{Code: "PUBLISHER_LOOKUP_FAILED"}
		}
		if o.cleanupErr != nil {
			o.view.CleanupErrors = append(o.view.CleanupErrors, SIDFormatFailureV1{Code: "PUBLISHER_QUERY_CLOSE_FAILED"})
		}
		retErr = errors.Join(o.primaryErr, o.cleanupErr)
	}()
	if err := creatorClosureContext(ctx); err != nil {
		o.primaryErr = err
		return
	}
	if !publisherRecordValid(record, binding) || ops.capture.hold == nil || ops.capture.birth == nil || ops.capture.principal == nil {
		o.primaryErr = errors.New("exact publisher record and query owner required")
		return
	}
	held, err := ops.capture.hold(int(record.PID))
	if held != nil {
		defer func() {
			closeErr := held.Close()
			o.cleanupErr = errors.Join(o.cleanupErr, closeErr, ctx.Err())
			o.view.ResourcesComplete = closeErr == nil && ctx.Err() == nil
		}()
	}
	if err != nil {
		// AccessDenied and every unknown result preserve the first failure. A
		// partial acquisition cannot cross into another native lookup attempt.
		if held != nil || !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			o.primaryErr = err
			return
		}
		missing, missingErr := observePublisherNativeMissingWithOperations(ctx, record, binding, err, ops)
		o.missing = missing
		if missingErr != nil {
			o.primaryErr = errors.Join(err, missingErr)
			return
		}
		if validateErr := missing.Validate(ctx, record, binding); validateErr != nil {
			o.primaryErr = errors.Join(err, validateErr)
			return
		}
		o.view.Disposition = "qualified-invalid-cid"
		o.view.NativeStatus = missing.Snapshot().TargetStatus
		o.view.ResourcesComplete = true
		return
	}
	if held == nil {
		o.primaryErr = errors.New("successful publisher query has no held object")
		return
	}
	p := &PublisherIncarnation{native: PublisherNativeIncarnationV1{PID: record.PID}}
	wait, err := readHeldPublisherIncarnation(ctx, p, held, ops.capture, false)
	o.view.NativeWait = wait
	if err != nil {
		o.primaryErr = err
		return
	}
	if p.native.NativeBirthFILETIME != record.NativeBirthFILETIME {
		o.view.Disposition = "pid-generation-replaced"
		return
	}
	if p.native.NativeImagePathSHA256 != record.NativeImagePathSHA256 || p.native.PrincipalSIDSHA256 != record.PrincipalSIDSHA256 {
		o.primaryErr = errors.New("same publisher generation identity differs")
		return
	}
	if wait != nil && *wait == windows.WAIT_OBJECT_0 {
		o.view.Disposition = "original-signaled"
		return
	}
	o.view.Disposition = "not-quiescent"
	o.primaryErr = errors.New("original publisher remains live")
	return
}

func observePublisherNativeMissingWithOperations(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding, win32Err error, ops publisherLookupOperations) (observation *PublisherNativeMissingObservation, retErr error) {
	o := &PublisherNativeMissingObservation{issuer: &publisherIncarnationIssuer{marker: 1}, record: record, binding: binding, view: PublisherNativeMissingSnapshotV1{Schema: "publisher-native-missing-observation-v1", Version: 1, OriginalBindingSHA256: publisherLookupBindingDigest(record, binding), TargetHandleDisposition: "not-called", CleanupErrors: []SIDFormatFailureV1{}}}
	observation = o
	defer func() {
		o.finalized = true
		o.finishedInBound = ctx != nil && ctx.Err() == nil
		o.view.Complete = o.finishedInBound && o.view.ResourcesComplete && o.view.ControlResourcesComplete && o.view.ControlIdentityVerified && o.primaryErr == nil && o.cleanupErr == nil && o.view.TargetStatusDefined && o.view.TargetStatus != nil && *o.view.TargetStatus == uint32(windows.STATUS_INVALID_CID) && o.view.TargetHandleDisposition == "no-handle"
		if o.primaryErr != nil {
			o.view.PrimaryError = &SIDFormatFailureV1{Code: "NATIVE_MISSING_RECEIVING_FAILED"}
		}
		if o.cleanupErr != nil {
			o.view.CleanupErrors = append(o.view.CleanupErrors, SIDFormatFailureV1{Code: "NATIVE_QUERY_CLOSE_FAILED"})
		}
		retErr = errors.Join(o.primaryErr, o.cleanupErr)
	}()
	if err := creatorClosureContext(ctx); err != nil {
		o.primaryErr = err
		return
	}
	if !publisherRecordValid(record, binding) || !errors.Is(win32Err, windows.ERROR_INVALID_PARAMETER) || ops.abi == nil || ops.openNative == nil || ops.processID == nil || ops.close == nil || ops.bindControl == nil {
		o.primaryErr = errors.Join(win32Err, errors.New("original ambiguous query and paired native owner required"))
		return
	}
	code := uint32(windows.ERROR_INVALID_PARAMETER)
	o.view.Win32ErrorDefined = true
	o.view.Win32ErrorCode = &code
	abi, err := ops.abi()
	if err != nil || !publisherDigestValid(abi) {
		o.primaryErr = errors.Join(err, errors.New("native ABI projection unavailable"))
		return
	}
	o.view.ABISourceSHA256 = abi
	control, err := capturePublisherIncarnationWithOperations(ctx, binding, ops.capture)
	if err != nil {
		o.primaryErr = err
		return
	}
	if err := control.ValidateCapture(ctx, binding); err != nil {
		o.primaryErr = err
		return
	}
	self := control.NativeSnapshot()
	result, err := ops.openNative(self.PID)
	o.view.ControlStatusDefined = result.defined
	if result.defined {
		x := result.status
		o.view.ControlStatus = &x
	}
	if !result.defined || result.status != 0 || result.handle == 0 {
		o.primaryErr = errors.Join(err, errors.New("native current-self control has no successful defined acquisition"))
		return
	}
	held, bindErr := ops.bindControl(self.PID, result.handle)
	if bindErr != nil || held == nil {
		o.cleanupErr = errors.Join(ops.close(result.handle), ctx.Err())
		o.primaryErr = errors.Join(bindErr, errors.New("original native control binding unavailable"))
		return
	}
	// The SAME read helpers are used for control and Win32 identity receiving.
	// All native operations are per-invocation replaceable only inside this owner.
	closeControl := func() error {
		err := held.Close()
		o.cleanupErr = errors.Join(o.cleanupErr, err, ctx.Err())
		o.view.ControlResourcesComplete = err == nil && ctx.Err() == nil
		return err
	}
	defer func() {
		if !held.closeAttempted {
			_ = closeControl()
		}
	}()
	if err != nil {
		o.primaryErr = err
		return
	}
	actualPID, err := ops.processID(result.handle)
	if err != nil || actualPID != self.PID {
		o.primaryErr = errors.Join(err, errors.New("native control PID differs"))
		return
	}
	controlRead := &PublisherIncarnation{native: PublisherNativeIncarnationV1{PID: self.PID}}
	_, err = readHeldPublisherIncarnation(ctx, controlRead, held, ops.capture, true)
	if err != nil || controlRead.native != self {
		o.primaryErr = errors.Join(err, errors.New("native control identity differs from original Win32 capture"))
		return
	}
	o.view.ControlIdentityVerified = true
	if err := closeControl(); err != nil {
		o.primaryErr = err
		return
	}
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
		return
	}
	target, err := ops.openNative(record.PID)
	o.view.TargetStatusDefined = target.defined
	if target.defined {
		x := target.status
		o.view.TargetStatus = &x
	}
	if target.handle == 0 {
		o.view.TargetHandleDisposition = "no-handle"
	} else if target.defined && target.status == 0 {
		o.view.TargetHandleDisposition = "acquired"
		closeErr := ops.close(target.handle)
		o.cleanupErr = errors.Join(o.cleanupErr, closeErr, ctx.Err())
		o.view.ResourcesComplete = closeErr == nil && ctx.Err() == nil
	} else {
		o.view.TargetHandleDisposition = "unknown"
	}
	if err != nil || !target.defined || target.status != uint32(windows.STATUS_INVALID_CID) || target.handle != 0 {
		o.primaryErr = errors.Join(err, errors.New("native target is not qualified INVALID_CID with no handle"))
		return
	}
	o.view.ResourcesComplete = o.view.ControlResourcesComplete && ctx.Err() == nil
	if err := ctx.Err(); err != nil {
		o.primaryErr = err
	}
	return
}
