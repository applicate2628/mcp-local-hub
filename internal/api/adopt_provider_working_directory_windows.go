//go:build windows

package api

import (
	"context"
	"debug/pe"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

type providerFileStandardInfo struct {
	allocationSize int64
	endOfFile      int64
	numberOfLinks  uint32
	deletePending  byte
	directory      byte
	_              [2]byte
}

type providerFileIDInfo struct {
	volumeSerialNumber uint64
	fileID             [16]byte
}

var (
	_ [24 - unsafe.Sizeof(providerFileStandardInfo{})]byte
	_ [unsafe.Sizeof(providerFileStandardInfo{}) - 24]byte
	_ [21 - unsafe.Offsetof(providerFileStandardInfo{}.directory)]byte
	_ [unsafe.Offsetof(providerFileStandardInfo{}.directory) - 21]byte
	_ [24 - unsafe.Sizeof(providerFileIDInfo{})]byte
	_ [unsafe.Sizeof(providerFileIDInfo{}) - 24]byte
	_ [8 - unsafe.Offsetof(providerFileIDInfo{}.fileID)]byte
	_ [unsafe.Offsetof(providerFileIDInfo{}.fileID) - 8]byte
)

type providerWorkingDirectoryLayout struct {
	pointerSize                  uintptr
	processInfoClass             int32
	pebProcessParametersOffset   uintptr
	currentDirectoryHandleOffset uintptr
}

func providerWorkingDirectoryLayoutFor(processMachine, nativeMachine uint16, hostPointerSize uintptr) (providerWorkingDirectoryLayout, bool) {
	if hostPointerSize != 8 {
		return providerWorkingDirectoryLayout{}, false
	}
	switch processMachine {
	case pe.IMAGE_FILE_MACHINE_UNKNOWN:
		if nativeMachine != pe.IMAGE_FILE_MACHINE_AMD64 && nativeMachine != pe.IMAGE_FILE_MACHINE_ARM64 {
			return providerWorkingDirectoryLayout{}, false
		}
		return providerWorkingDirectoryLayout{pointerSize: 8, processInfoClass: int32(windows.ProcessBasicInformation), pebProcessParametersOffset: 0x20, currentDirectoryHandleOffset: 0x48}, true
	case pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_MACHINE_ARM64:
		if nativeMachine != pe.IMAGE_FILE_MACHINE_AMD64 && nativeMachine != pe.IMAGE_FILE_MACHINE_ARM64 {
			return providerWorkingDirectoryLayout{}, false
		}
		return providerWorkingDirectoryLayout{pointerSize: 8, processInfoClass: int32(windows.ProcessBasicInformation), pebProcessParametersOffset: 0x20, currentDirectoryHandleOffset: 0x48}, true
	case pe.IMAGE_FILE_MACHINE_I386, pe.IMAGE_FILE_MACHINE_ARMNT:
		if nativeMachine != pe.IMAGE_FILE_MACHINE_AMD64 && nativeMachine != pe.IMAGE_FILE_MACHINE_ARM64 {
			return providerWorkingDirectoryLayout{}, false
		}
		return providerWorkingDirectoryLayout{pointerSize: 4, processInfoClass: int32(windows.ProcessWow64Information), pebProcessParametersOffset: 0x10, currentDirectoryHandleOffset: 0x2c}, true
	default:
		return providerWorkingDirectoryLayout{}, false
	}
}

func providerWorkingDirectoryObjectIdentityFromPath(path string) (result providerDirectoryObjectIdentityV1, resultErr error) {
	if !providerTrustedMetadataDirectoryPath(path) {
		return providerDirectoryObjectIdentityV1{}, fmt.Errorf("provider working-directory metadata is unavailable")
	}
	pathW, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return providerDirectoryObjectIdentityV1{}, fmt.Errorf("provider working-directory metadata is unavailable")
	}
	handle, err := windows.CreateFile(
		pathW,
		windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return providerDirectoryObjectIdentityV1{}, fmt.Errorf("provider working-directory metadata is unavailable")
	}
	defer func() {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			result = providerDirectoryObjectIdentityV1{}
			resultErr = fmt.Errorf("provider working-directory metadata handle close failed")
		}
	}()
	identity, ok := providerDirectoryObjectIdentityFromHandle(handle)
	if !ok {
		return providerDirectoryObjectIdentityV1{}, fmt.Errorf("provider working-directory metadata is unavailable")
	}
	return identity, nil
}

func providerTrustedMetadataDirectoryPath(path string) bool {
	if path == "" || strings.IndexByte(path, 0) >= 0 || !filepath.IsAbs(path) {
		return false
	}
	path = strings.ReplaceAll(path, "/", `\`)
	lower := strings.ToLower(path)
	if strings.HasPrefix(lower, `\\.\`) || strings.HasPrefix(lower, `\??\`) || strings.HasPrefix(lower, `\device\`) {
		return false
	}
	if strings.HasPrefix(lower, `\\?\`) {
		rest := path[4:]
		lowerRest := lower[4:]
		if strings.HasPrefix(lowerRest, `unc\`) {
			return providerUNCMetadataPathHasShare(rest[4:])
		}
		return providerDriveAbsolutePath(rest)
	}
	if strings.HasPrefix(path, `\\`) {
		return providerUNCMetadataPathHasShare(path[2:])
	}
	return providerDriveAbsolutePath(path)
}

func providerDriveAbsolutePath(path string) bool {
	return len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) && path[1] == ':' && path[2] == '\\'
}

func providerUNCMetadataPathHasShare(rest string) bool {
	parts := strings.Split(rest, `\`)
	return len(parts) >= 2 && parts[0] != "" && parts[1] != "" && parts[0] != "." && parts[0] != "?"
}

type providerDirectoryObjectHandleOps struct {
	fileType     func(windows.Handle) (uint32, error)
	standardInfo func(windows.Handle) (providerFileStandardInfo, error)
	fileIDInfo   func(windows.Handle) (providerDirectoryObjectIdentityV1, error)
}

func providerDirectoryObjectIdentityFromHandle(handle windows.Handle) (providerDirectoryObjectIdentityV1, bool) {
	ops := providerDirectoryObjectHandleOps{
		fileType: windows.GetFileType,
		standardInfo: func(handle windows.Handle) (providerFileStandardInfo, error) {
			var info providerFileStandardInfo
			err := windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
			return info, err
		},
		fileIDInfo: func(handle windows.Handle) (providerDirectoryObjectIdentityV1, error) {
			var info providerFileIDInfo
			err := windows.GetFileInformationByHandleEx(handle, windows.FileIdInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
			if err != nil {
				return providerDirectoryObjectIdentityV1{}, err
			}
			return providerDirectoryObjectIdentityV1{volumeSerialNumber: info.volumeSerialNumber, fileID: info.fileID, valid: true}, nil
		},
	}
	return providerDirectoryObjectIdentityFromHandleWithOps(handle, ops)
}

func providerDirectoryObjectIdentityFromHandleWithOps(handle windows.Handle, ops providerDirectoryObjectHandleOps) (providerDirectoryObjectIdentityV1, bool) {
	fileType, err := ops.fileType(handle)
	if err != nil || fileType != windows.FILE_TYPE_DISK {
		return providerDirectoryObjectIdentityV1{}, false
	}
	standard, err := ops.standardInfo(handle)
	if err != nil || standard.directory == 0 {
		return providerDirectoryObjectIdentityV1{}, false
	}
	identity, err := ops.fileIDInfo(handle)
	if err != nil || !identity.valid {
		return providerDirectoryObjectIdentityV1{}, false
	}
	return identity, true
}

type providerWorkingDirectoryProbeOps struct {
	openProcess      func(uint32, bool, uint32) (windows.Handle, error)
	closeHandle      func(windows.Handle) error
	ownerMatches     func(windows.Handle) (bool, error)
	matchesCandidate func(windows.Handle, providerProcessWorkingDirectoryCandidateV1) bool
	layoutFromHandle func(windows.Handle) (providerWorkingDirectoryLayout, bool)
	selfProbe        func(context.Context) bool
	readStableObject func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error)
	wait             func(windows.Handle, uint32) (uint32, error)
}

func probeProviderProcessWorkingDirectory(ctx context.Context, candidate providerProcessWorkingDirectoryCandidateV1, selected providerDirectoryObjectIdentityV1) providerProcessWorkingDirectoryResultV1 {
	ops := providerWorkingDirectoryProbeOps{
		openProcess:      windows.OpenProcess,
		closeHandle:      windows.CloseHandle,
		ownerMatches:     providerProcessHandleOwnerMatchesCurrent,
		matchesCandidate: providerHandleMatchesCandidate,
		layoutFromHandle: providerWorkingDirectoryLayoutFromHandle,
		selfProbe:        providerWorkingDirectorySelfProbe,
		readStableObject: readStableProviderWorkingDirectoryObject,
		wait:             windows.WaitForSingleObject,
	}
	return probeProviderProcessWorkingDirectoryWithOps(ctx, candidate, selected, ops)
}

func probeProviderProcessWorkingDirectoryWithOps(ctx context.Context, candidate providerProcessWorkingDirectoryCandidateV1, selected providerDirectoryObjectIdentityV1, ops providerWorkingDirectoryProbeOps) (result providerProcessWorkingDirectoryResultV1) {
	if err := ctx.Err(); err != nil {
		return providerWorkingDirectoryUnknown("provider-working-directory-unstable", err)
	}
	if candidate.PID <= 0 || candidate.SnapshotStarted.IsZero() || candidate.ExecutablePath == "" || !selected.valid {
		return providerWorkingDirectoryUnknown("provider-working-directory-layout-invalid", fmt.Errorf("provider working-directory candidate is incomplete"))
	}
	const access = windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ | windows.PROCESS_DUP_HANDLE | windows.SYNCHRONIZE
	handle, err := ops.openProcess(access, false, uint32(candidate.PID))
	if err != nil {
		return providerWorkingDirectoryUnknown("provider-working-directory-access-unavailable", fmt.Errorf("provider process is unavailable"))
	}
	defer func() {
		if closeErr := ops.closeHandle(handle); closeErr != nil {
			result = providerWorkingDirectoryUnknown("provider-working-directory-access-unavailable", fmt.Errorf("provider process handle close failed"))
		}
	}()

	ownerMatch, err := ops.ownerMatches(handle)
	if err != nil {
		return providerWorkingDirectoryUnknown("provider-working-directory-access-unavailable", err)
	}
	if !ownerMatch {
		return providerWorkingDirectoryUnknown("provider-working-directory-owner-mismatch", fmt.Errorf("provider process owner differs"))
	}
	if !ops.matchesCandidate(handle, candidate) {
		return providerWorkingDirectoryUnknown("provider-working-directory-generation-race", fmt.Errorf("provider process generation changed"))
	}
	layout, ok := ops.layoutFromHandle(handle)
	if !ok || !ops.selfProbe(ctx) {
		return providerWorkingDirectoryUnknown("provider-working-directory-layout-unsupported", fmt.Errorf("provider working-directory layout is unavailable"))
	}
	observed, failureID, err := ops.readStableObject(ctx, handle, layout)
	if err != nil {
		return providerWorkingDirectoryUnknown(failureID, err)
	}
	if !ops.matchesCandidate(handle, candidate) {
		return providerWorkingDirectoryUnknown("provider-working-directory-generation-race", fmt.Errorf("provider process generation changed"))
	}
	if status, waitErr := ops.wait(handle, 0); waitErr != nil || status != uint32(windows.WAIT_TIMEOUT) {
		return providerWorkingDirectoryUnknown("provider-working-directory-unstable", fmt.Errorf("provider process exited or could not be observed"))
	}
	if observed == selected {
		return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryEqual}
	}
	return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryDifferent}
}

func providerWorkingDirectoryUnknown(failureID string, err error) providerProcessWorkingDirectoryResultV1 {
	if failureID == "" {
		failureID = "provider-working-directory-layout-invalid"
	}
	if err == nil {
		err = fmt.Errorf("provider working-directory evidence is unavailable")
	}
	return providerProcessWorkingDirectoryResultV1{State: providerProcessWorkingDirectoryUnknown, FailureID: failureID, Err: err}
}

type providerWorkingDirectoryTokenOps struct {
	openToken  func(windows.Handle, uint32, *windows.Token) error
	closeToken func(windows.Token) error
	targetSID  func(windows.Token) (*windows.SID, error)
	currentSID func() (*windows.SID, error)
}

func providerProcessHandleOwnerMatchesCurrent(handle windows.Handle) (bool, error) {
	ops := providerWorkingDirectoryTokenOps{
		openToken:  windows.OpenProcessToken,
		closeToken: func(token windows.Token) error { return token.Close() },
		targetSID: func(token windows.Token) (*windows.SID, error) {
			user, err := token.GetTokenUser()
			if err != nil || user == nil || user.User.Sid == nil {
				return nil, fmt.Errorf("provider process owner is unavailable")
			}
			return user.User.Sid, nil
		},
		currentSID: func() (*windows.SID, error) {
			user, err := windows.GetCurrentProcessToken().GetTokenUser()
			if err != nil || user == nil || user.User.Sid == nil {
				return nil, fmt.Errorf("current process owner is unavailable")
			}
			return user.User.Sid, nil
		},
	}
	return providerProcessHandleOwnerMatchesCurrentWithOps(handle, ops)
}

func providerProcessHandleOwnerMatchesCurrentWithOps(handle windows.Handle, ops providerWorkingDirectoryTokenOps) (match bool, resultErr error) {
	var targetToken windows.Token
	if err := ops.openToken(handle, windows.TOKEN_QUERY, &targetToken); err != nil {
		return false, fmt.Errorf("provider process token is unavailable")
	}
	defer func() {
		if err := ops.closeToken(targetToken); err != nil {
			match = false
			resultErr = fmt.Errorf("provider process token close failed")
		}
	}()
	target, err := ops.targetSID(targetToken)
	if err != nil || target == nil {
		return false, fmt.Errorf("provider process owner is unavailable")
	}
	current, err := ops.currentSID()
	if err != nil || current == nil {
		return false, fmt.Errorf("current process owner is unavailable")
	}
	return target.Equals(current), nil
}

func providerHandleMatchesCandidate(handle windows.Handle, candidate providerProcessWorkingDirectoryCandidateV1) bool {
	started, image, err := providerProcessHandleGeneration(handle)
	if err != nil {
		return false
	}
	return started.UTC().Truncate(time.Microsecond).Equal(candidate.SnapshotStarted.UTC().Truncate(time.Microsecond)) && providerExecutableEqual(candidate.ExecutablePath, image)
}

func providerProcessHandleGeneration(handle windows.Handle) (time.Time, string, error) {
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, "", fmt.Errorf("provider process creation time is unavailable")
	}
	size := uint32(windows.MAX_PATH)
	for size <= 16384 {
		buf := make([]uint16, size)
		n := size
		err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &n)
		if err == nil {
			if n == 0 {
				return time.Time{}, "", fmt.Errorf("provider process image is empty")
			}
			return time.Unix(0, creation.Nanoseconds()).UTC(), windows.UTF16ToString(buf[:n]), nil
		}
		if !isWindowsBufferTooSmall(err) || size == 16384 {
			return time.Time{}, "", fmt.Errorf("provider process image is unavailable")
		}
		size *= 2
	}
	return time.Time{}, "", fmt.Errorf("provider process image exceeds limit")
}

func isWindowsBufferTooSmall(err error) bool {
	return err == windows.ERROR_INSUFFICIENT_BUFFER || err == windows.ERROR_MORE_DATA
}

func providerWorkingDirectoryLayoutFromHandle(handle windows.Handle) (providerWorkingDirectoryLayout, bool) {
	var processMachine, nativeMachine uint16
	if err := windows.IsWow64Process2(handle, &processMachine, &nativeMachine); err != nil {
		return providerWorkingDirectoryLayout{}, false
	}
	return providerWorkingDirectoryLayoutFor(processMachine, nativeMachine, unsafe.Sizeof(uintptr(0)))
}

func providerWorkingDirectorySelfProbe(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	layout, ok := providerWorkingDirectoryLayoutFromHandle(windows.CurrentProcess())
	if !ok {
		return false
	}
	observed, _, err := readStableProviderWorkingDirectoryObject(ctx, windows.CurrentProcess(), layout)
	if err != nil {
		return false
	}
	cwd, err := os.Getwd()
	if err != nil {
		return false
	}
	expected, err := providerWorkingDirectoryObjectIdentityFromPath(cwd)
	if err != nil {
		return false
	}
	return observed == expected
}

type providerWorkingDirectoryObjectSample struct {
	remoteHandle   uintptr
	objectIdentity providerDirectoryObjectIdentityV1
}

type providerWorkingDirectoryObjectSampler func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerWorkingDirectoryObjectSample, string, error)

type providerWorkingDirectorySampleOps struct {
	processPEBAddress func(windows.Handle, providerWorkingDirectoryLayout) (uintptr, bool)
	readRemotePointer func(windows.Handle, uintptr, uintptr, uintptr) (uintptr, bool)
	duplicateObject   func(windows.Handle, uintptr) (providerDirectoryObjectIdentityV1, string, error)
}

func readStableProviderWorkingDirectoryObject(ctx context.Context, handle windows.Handle, layout providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
	return readStableProviderWorkingDirectoryObjectWithSampler(ctx, handle, layout, readProviderWorkingDirectorySample)
}

func readStableProviderWorkingDirectoryObjectWithSampler(ctx context.Context, handle windows.Handle, layout providerWorkingDirectoryLayout, sample providerWorkingDirectoryObjectSampler) (providerDirectoryObjectIdentityV1, string, error) {
	first, failureID, err := sample(ctx, handle, layout)
	if err != nil {
		return providerDirectoryObjectIdentityV1{}, failureID, err
	}
	second, failureID, err := sample(ctx, handle, layout)
	if err != nil {
		return providerDirectoryObjectIdentityV1{}, failureID, err
	}
	if first.remoteHandle != second.remoteHandle || first.objectIdentity != second.objectIdentity {
		return providerDirectoryObjectIdentityV1{}, "provider-working-directory-unstable", fmt.Errorf("provider working-directory handle changed")
	}
	return first.objectIdentity, "", nil
}

func readProviderWorkingDirectorySample(ctx context.Context, handle windows.Handle, layout providerWorkingDirectoryLayout) (providerWorkingDirectoryObjectSample, string, error) {
	ops := providerWorkingDirectorySampleOps{
		processPEBAddress: providerProcessPEBAddress,
		readRemotePointer: providerReadRemoteValue,
		duplicateObject:   duplicateProviderDirectoryObjectIdentity,
	}
	return readProviderWorkingDirectorySampleWithOps(ctx, handle, layout, ops)
}

func readProviderWorkingDirectorySampleWithOps(ctx context.Context, handle windows.Handle, layout providerWorkingDirectoryLayout, ops providerWorkingDirectorySampleOps) (providerWorkingDirectoryObjectSample, string, error) {
	if err := ctx.Err(); err != nil {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-unstable", err
	}
	pebAddress, ok := ops.processPEBAddress(handle, layout)
	if !ok {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-layout-invalid", fmt.Errorf("provider process PEB is unavailable")
	}
	paramsAddress, ok := ops.readRemotePointer(handle, pebAddress, layout.pebProcessParametersOffset, layout.pointerSize)
	if !ok || paramsAddress == 0 || paramsAddress%layout.pointerSize != 0 {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-layout-invalid", fmt.Errorf("provider process parameters are unavailable")
	}
	remoteHandle, ok := ops.readRemotePointer(handle, paramsAddress, layout.currentDirectoryHandleOffset, layout.pointerSize)
	if !ok || remoteHandle == 0 {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-layout-invalid", fmt.Errorf("provider working-directory handle is unavailable")
	}
	identity, failureID, err := ops.duplicateObject(handle, remoteHandle)
	if err != nil {
		return providerWorkingDirectoryObjectSample{}, failureID, err
	}
	afterHandle, ok := ops.readRemotePointer(handle, paramsAddress, layout.currentDirectoryHandleOffset, layout.pointerSize)
	if !ok || afterHandle != remoteHandle {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-unstable", fmt.Errorf("provider working-directory handle changed")
	}
	if err := ctx.Err(); err != nil {
		return providerWorkingDirectoryObjectSample{}, "provider-working-directory-unstable", err
	}
	return providerWorkingDirectoryObjectSample{remoteHandle: remoteHandle, objectIdentity: identity}, "", nil
}

type providerDirectoryDuplicateOps struct {
	currentProcess func() windows.Handle
	duplicate      func(windows.Handle, windows.Handle, windows.Handle, *windows.Handle, uint32, bool, uint32) error
	closeHandle    func(windows.Handle) error
	objectIdentity func(windows.Handle) (providerDirectoryObjectIdentityV1, bool)
}

func duplicateProviderDirectoryObjectIdentity(process windows.Handle, remoteHandle uintptr) (providerDirectoryObjectIdentityV1, string, error) {
	ops := providerDirectoryDuplicateOps{
		currentProcess: windows.CurrentProcess,
		duplicate:      windows.DuplicateHandle,
		closeHandle:    windows.CloseHandle,
		objectIdentity: providerDirectoryObjectIdentityFromHandle,
	}
	return duplicateProviderDirectoryObjectIdentityWithOps(process, remoteHandle, ops)
}

func duplicateProviderDirectoryObjectIdentityWithOps(process windows.Handle, remoteHandle uintptr, ops providerDirectoryDuplicateOps) (result providerDirectoryObjectIdentityV1, failureID string, resultErr error) {
	if remoteHandle == 0 {
		return providerDirectoryObjectIdentityV1{}, "provider-working-directory-layout-invalid", fmt.Errorf("provider working-directory handle is unavailable")
	}
	var duplicate windows.Handle
	if err := ops.duplicate(process, windows.Handle(remoteHandle), ops.currentProcess(), &duplicate, 0, false, 0); err != nil || duplicate == 0 {
		return providerDirectoryObjectIdentityV1{}, "provider-working-directory-access-unavailable", fmt.Errorf("provider working-directory handle duplicate is unavailable")
	}
	defer func() {
		if closeErr := ops.closeHandle(duplicate); closeErr != nil {
			result = providerDirectoryObjectIdentityV1{}
			failureID = "provider-working-directory-object-unavailable"
			resultErr = fmt.Errorf("provider working-directory duplicate close failed")
		}
	}()
	identity, ok := ops.objectIdentity(duplicate)
	if !ok {
		return providerDirectoryObjectIdentityV1{}, "provider-working-directory-object-unavailable", fmt.Errorf("provider working-directory object is unavailable")
	}
	return identity, "", nil
}

func providerProcessPEBAddress(handle windows.Handle, layout providerWorkingDirectoryLayout) (uintptr, bool) {
	var address uintptr
	var returned uint32
	if layout.processInfoClass == int32(windows.ProcessBasicInformation) {
		var info windows.PROCESS_BASIC_INFORMATION
		if err := windows.NtQueryInformationProcess(handle, layout.processInfoClass, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &returned); err != nil || info.PebBaseAddress == nil {
			return 0, false
		}
		address = uintptr(unsafe.Pointer(info.PebBaseAddress))
	} else if layout.processInfoClass == int32(windows.ProcessWow64Information) {
		if err := windows.NtQueryInformationProcess(handle, layout.processInfoClass, unsafe.Pointer(&address), uint32(unsafe.Sizeof(address)), &returned); err != nil || address == 0 {
			return 0, false
		}
	} else {
		return 0, false
	}
	return address, address%layout.pointerSize == 0
}

func providerReadRemoteValue(handle windows.Handle, base, offset, pointerSize uintptr) (uintptr, bool) {
	address, ok := checkedProviderRemoteAddress(base, offset, pointerSize)
	if !ok {
		return 0, false
	}
	raw, ok := providerReadRemoteBytes(handle, address, pointerSize)
	if !ok {
		return 0, false
	}
	if pointerSize == 8 {
		return uintptr(binary.LittleEndian.Uint64(raw)), true
	}
	if pointerSize == 4 {
		return uintptr(binary.LittleEndian.Uint32(raw)), true
	}
	return 0, false
}

func checkedProviderRemoteAddress(base, offset, alignment uintptr) (uintptr, bool) {
	if base == 0 || alignment == 0 || base%alignment != 0 || offset%alignment != 0 || base+offset < base {
		return 0, false
	}
	return base + offset, true
}

func providerReadRemoteBytes(handle windows.Handle, address, size uintptr) ([]byte, bool) {
	ops := providerWorkingDirectoryRemoteReadOps{
		query: func(handle windows.Handle, address uintptr) (windows.MemoryBasicInformation, error) {
			var info windows.MemoryBasicInformation
			err := windows.VirtualQueryEx(handle, address, &info, unsafe.Sizeof(info))
			return info, err
		},
		read: func(handle windows.Handle, address, size uintptr) ([]byte, uintptr, error) {
			buf := make([]byte, size)
			var read uintptr
			err := windows.ReadProcessMemory(handle, address, &buf[0], size, &read)
			return buf, read, err
		},
	}
	return providerReadRemoteBytesWithOps(handle, address, size, ops)
}

type providerWorkingDirectoryRemoteReadOps struct {
	query func(windows.Handle, uintptr) (windows.MemoryBasicInformation, error)
	read  func(windows.Handle, uintptr, uintptr) ([]byte, uintptr, error)
}

func providerReadRemoteBytesWithOps(handle windows.Handle, address, size uintptr, ops providerWorkingDirectoryRemoteReadOps) ([]byte, bool) {
	if address == 0 || size == 0 || size > 8 || address+size < address {
		return nil, false
	}
	info, err := ops.query(handle, address)
	if err != nil || info.State != windows.MEM_COMMIT || !providerMemoryProtectionReadable(info.Protect) {
		return nil, false
	}
	regionEnd := info.BaseAddress + info.RegionSize
	if regionEnd < info.BaseAddress || address < info.BaseAddress || address+size > regionEnd {
		return nil, false
	}
	buf, read, err := ops.read(handle, address, size)
	if err != nil || read != size || uintptr(len(buf)) != size {
		return nil, false
	}
	return buf, true
}

func providerMemoryProtectionReadable(protect uint32) bool {
	if protect&windows.PAGE_GUARD != 0 || protect&windows.PAGE_NOACCESS != 0 {
		return false
	}
	switch protect & 0xff {
	case windows.PAGE_READONLY, windows.PAGE_READWRITE, windows.PAGE_WRITECOPY, windows.PAGE_EXECUTE_READ, windows.PAGE_EXECUTE_READWRITE, windows.PAGE_EXECUTE_WRITECOPY:
		return true
	default:
		return false
	}
}
