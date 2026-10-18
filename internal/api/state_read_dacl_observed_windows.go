//go:build windows

package api

import (
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"mcp-local-hub/internal/process"

	"golang.org/x/sys/windows"
)

var stateReadGetSecurityInfoProc = windows.NewLazySystemDLL("advapi32.dll").NewProc("GetSecurityInfo")

type stateReadSecurityInfoOps struct {
	get   func(windows.Handle) (*windows.SECURITY_DESCRIPTOR, uint32)
	valid func(*windows.SECURITY_DESCRIPTOR) bool
	size  func(*windows.SECURITY_DESCRIPTOR) uint32
	free  func(windows.Handle) (windows.Handle, error)
}

func stateReadSecurityInfoNativeOps() stateReadSecurityInfoOps {
	return stateReadSecurityInfoOps{
		get: func(handle windows.Handle) (*windows.SECURITY_DESCRIPTOR, uint32) {
			var descriptor *windows.SECURITY_DESCRIPTOR
			// Identical eight-argument DWORD ABI to pinned SDK getSecurityInfo.
			status, _, _ := stateReadGetSecurityInfoProc.Call(uintptr(handle), uintptr(windows.SE_FILE_OBJECT), uintptr(windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION), 0, 0, 0, 0, uintptr(unsafe.Pointer(&descriptor)))
			return descriptor, uint32(status)
		},
		valid: func(descriptor *windows.SECURITY_DESCRIPTOR) bool { return descriptor.IsValid() },
		size:  func(descriptor *windows.SECURITY_DESCRIPTOR) uint32 { return descriptor.Length() },
		free:  windows.LocalFree,
	}
}

func (b *stateFileReadBuilder) prearmResource(slot string) *stateFileReadResource {
	resource := &stateFileReadResource{view: StateFileReadResourceSnapshotV1{Slot: slot, Ownership: "not-acquired"}}
	b.resources = append(b.resources, resource)
	return resource
}

func readStateSecurityDescriptorObserved(handle windows.Handle, b *stateFileReadBuilder, slot string, ops stateReadSecurityInfoOps) (descriptor *windows.SECURITY_DESCRIPTOR, release func() error, retErr error) {
	if b == nil || ops.get == nil || ops.valid == nil || ops.size == nil || ops.free == nil {
		return nil, nil, errors.New("actual descriptor owner operations required")
	}
	if err := b.checkContext(); err != nil {
		return nil, nil, err
	}
	resource := b.prearmResource(slot)
	descriptor, status := ops.get(handle)
	if status != 0 {
		cause := windows.Errno(status)
		kind := "win32"
		code := uint64(status)
		resource.view.NativeErrorDefined = true
		resource.view.NativeErrorKind = &kind
		resource.view.NativeErrorCode = &code
		if descriptor != nil {
			b.borrowedErr = errors.Join(b.borrowedErr, errors.New("ambiguous partial descriptor allocation is not qualified for free"))
			resource.view.Ownership = "borrowed"
		}
		return nil, nil, cause
	}
	if descriptor == nil {
		b.borrowedErr = errors.Join(b.borrowedErr, errors.New("successful security-info call has no defined original allocation"))
		return nil, nil, b.borrowedErr
	}
	allocation := windows.Handle(uintptr(unsafe.Pointer(descriptor)))
	b.acquire(slot, func() error {
		remaining, err := ops.free(allocation)
		if remaining != 0 {
			return errors.Join(err, errors.New("original descriptor LocalFree returned non-NULL"))
		}
		return err
	})
	release = func() error { return b.closeResource(slot) }
	if err := b.checkContext(); err != nil {
		return nil, release, err
	}
	if !ops.valid(descriptor) || ops.size(descriptor) == 0 {
		return nil, release, errors.New("original security descriptor output is invalid or has no defined size")
	}
	digest := stateFileReadObservationDigest([]byte(fmt.Sprintf("security-descriptor-allocation-v1|%x|%d", allocation, ops.size(descriptor))))
	b.identity(slot, digest)
	return descriptor, release, nil
}

func windowsDACLAllowlistViolationsObserved(handle windows.Handle, significantBits uint32, b *stateFileReadBuilder, sourceSlot string) (violations []DACLAllowlistViolation, retErr error) {
	current, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	slot := sourceSlot + "-descriptor-" + strconv.Itoa(len(b.resources))
	descriptor, release, err := readStateSecurityDescriptorObserved(handle, b, slot, stateReadSecurityInfoNativeOps())
	if release != nil {
		defer func() { retErr = errors.Join(retErr, release()) }()
	}
	if err != nil {
		return nil, err
	}
	// Keep the same parser and original descriptor alive while each actual
	// diagnostic SID conversion is received in this reader's private slot.
	var formattingErr error
	format := func(sid *windows.SID) string {
		value, err := b.formatSIDObserved(sid, sourceSlot+"-sid-format-"+strconv.Itoa(len(b.resources)))
		if err != nil {
			formattingErr = errors.Join(formattingErr, err)
		}
		if value == "" {
			return "<unresolved-sid>"
		}
		return value
	}
	violations, parserErr := windowsDACLAllowlistViolationsFromDescriptor(descriptor, significantBits, []*windows.SID{current, system, admin}, format)
	return violations, errors.Join(parserErr, formattingErr)
}

// Both descriptor verification and the original create-time descriptor owner
// register the observation returned by their own conversion in this same slot.
func (b *stateFileReadBuilder) formatSIDObserved(sid *windows.SID, slot string) (string, error) {
	resource := b.prearmResource(slot)
	value, observation, err := process.FormatSIDObserved(sid)
	resource.sidFormatting = observation // exact pointer returned by this call
	if observation == nil {
		err = errors.Join(err, errors.New("original SID formatter observation missing"))
	} else {
		snapshot := observation.Snapshot()
		resource.view.Acquired = snapshot.AllocationAcquired
		if snapshot.AllocationAcquired {
			resource.view.Ownership = "owned"
		}
		resource.view.CloseAttempted = snapshot.FreeAttempted
		resource.view.CloseResultDefined = snapshot.FreeDefined
		resource.view.Closed = snapshot.Freed
		err = errors.Join(err, observation.ValidateInput(sid, value))
		if snapshot.CleanupError != nil {
			resource.closeErr = err
			resource.view.NativeErrorDefined = true
			kind := "go"
			resource.view.NativeErrorKind = &kind
			b.view.CleanupErrors = append(b.view.CleanupErrors, StateFileReadFailureV1{Code: "RESOURCE_CLOSE_FAILED", Stage: "close", Slot: resource.view.Slot})
		}
	}
	if err != nil {
		b.borrowedErr = errors.Join(b.borrowedErr, err)
	}
	return value, err
}
