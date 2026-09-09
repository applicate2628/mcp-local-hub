//go:build windows

package api

import (
	"bufio"
	"bytes"
	"context"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"

	"mcp-local-hub/internal/clients"

	"golang.org/x/sys/windows"
)

// TestProbeProviderProcessWorkingDirectoryReadsOwnedCurrentProcess catches a
// reader that classifies from metadata alone or accepts a stale process
// generation. The current test process is an owned native Windows process and
// does not require discovery, signalling, elevation, or an external provider.
func TestProbeProviderProcessWorkingDirectoryReadsOwnedCurrentProcess(t *testing.T) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatalf("OpenProcess(self): %v", err)
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		t.Fatalf("GetProcessTimes(self): %v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	selectedIdentity, err := providerWorkingDirectoryObjectIdentityFromPath(cwd)
	if err != nil {
		t.Fatalf("selected identity: %v", err)
	}
	candidate := providerProcessWorkingDirectoryCandidateV1{
		PID:             os.Getpid(),
		SnapshotStarted: time.Unix(0, creation.Nanoseconds()).UTC().Truncate(time.Microsecond),
		ExecutablePath:  exe,
	}

	if got := probeProviderProcessWorkingDirectory(context.Background(), candidate, selectedIdentity); got.State != providerProcessWorkingDirectoryEqual {
		t.Fatalf("equal probe=%+v", got)
	}
	otherIdentity, err := providerWorkingDirectoryObjectIdentityFromPath(t.TempDir())
	if err != nil {
		t.Fatalf("other identity: %v", err)
	}
	if got := probeProviderProcessWorkingDirectory(context.Background(), candidate, otherIdentity); got.State != providerProcessWorkingDirectoryDifferent {
		t.Fatalf("different probe=%+v", got)
	}
	stale := candidate
	stale.SnapshotStarted = stale.SnapshotStarted.Add(time.Microsecond)
	if got := probeProviderProcessWorkingDirectory(context.Background(), stale, selectedIdentity); got.State != providerProcessWorkingDirectoryUnknown || got.FailureID != "provider-working-directory-generation-race" {
		t.Fatalf("stale-generation probe=%+v", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := probeProviderProcessWorkingDirectory(cancelled, candidate, selectedIdentity); got.State != providerProcessWorkingDirectoryUnknown || got.FailureID != "provider-working-directory-unstable" {
		t.Fatalf("cancelled probe=%+v", got)
	}
}

// TestProbeProviderProcessWorkingDirectoryFailureMatrix catches any path that
// treats inaccessible, foreign-owner, stale, unsupported, unstable, exited,
// or incomparable evidence as absence/difference. It also asserts the exact
// candidate-only rights and process-handle close contract.
func TestProbeProviderProcessWorkingDirectoryFailureMatrix(t *testing.T) {
	candidate := providerProcessWorkingDirectoryCandidateV1{PID: 41, SnapshotStarted: time.Unix(100, 0).UTC(), ExecutablePath: `C:\owned\provider.exe`}
	selected := providerDirectoryObjectIdentityForTest(1)
	for _, tc := range []struct {
		name        string
		mutate      func(*providerWorkingDirectoryProbeOps, *int, *int)
		wantState   providerProcessWorkingDirectoryStateV1
		wantFailure string
		wantCloses  int
		wantReads   int
	}{
		{"equal", nil, providerProcessWorkingDirectoryEqual, "", 1, 1},
		{"different", func(ops *providerWorkingDirectoryProbeOps, _, reads *int) {
			ops.readStableObject = func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
				*reads++
				return providerDirectoryObjectIdentityForTest(2), "", nil
			}
		}, providerProcessWorkingDirectoryDifferent, "", 1, 1},
		{"open-denied", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.openProcess = func(uint32, bool, uint32) (windows.Handle, error) { return 0, errors.New("denied") }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-access-unavailable", 0, 0},
		{"token-denied", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.ownerMatches = func(windows.Handle) (bool, error) { return false, errors.New("denied") }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-access-unavailable", 1, 0},
		{"owner-mismatch", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.ownerMatches = func(windows.Handle) (bool, error) { return false, nil }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-owner-mismatch", 1, 0},
		{"generation-before", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.matchesCandidate = func(windows.Handle, providerProcessWorkingDirectoryCandidateV1) bool { return false }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-generation-race", 1, 0},
		{"layout-unsupported", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.layoutFromHandle = func(windows.Handle) (providerWorkingDirectoryLayout, bool) {
				return providerWorkingDirectoryLayout{}, false
			}
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-layout-unsupported", 1, 0},
		{"self-probe-mismatch", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.selfProbe = func(context.Context) bool { return false }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-layout-unsupported", 1, 0},
		{"read-denied", func(ops *providerWorkingDirectoryProbeOps, _, reads *int) {
			ops.readStableObject = func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
				*reads++
				return providerDirectoryObjectIdentityV1{}, "provider-working-directory-access-unavailable", errors.New("denied")
			}
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-access-unavailable", 1, 1},
		{"handle-changed", func(ops *providerWorkingDirectoryProbeOps, _, reads *int) {
			ops.readStableObject = func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
				*reads++
				return providerDirectoryObjectIdentityV1{}, "provider-working-directory-unstable", errors.New("changed")
			}
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-unstable", 1, 1},
		{"generation-after", func(ops *providerWorkingDirectoryProbeOps, matchCalls, _ *int) {
			ops.matchesCandidate = func(windows.Handle, providerProcessWorkingDirectoryCandidateV1) bool {
				*matchCalls++
				return *matchCalls == 1
			}
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-generation-race", 1, 1},
		{"target-exited", func(ops *providerWorkingDirectoryProbeOps, _, _ *int) {
			ops.wait = func(windows.Handle, uint32) (uint32, error) { return windows.WAIT_OBJECT_0, nil }
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-unstable", 1, 1},
		{"object-identity-unavailable", func(ops *providerWorkingDirectoryProbeOps, _, reads *int) {
			ops.readStableObject = func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
				*reads++
				return providerDirectoryObjectIdentityV1{}, "provider-working-directory-object-unavailable", errors.New("unavailable")
			}
		}, providerProcessWorkingDirectoryUnknown, "provider-working-directory-object-unavailable", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closes, reads, matchCalls := 0, 0, 0
			ops := providerWorkingDirectoryProbeOps{
				openProcess: func(access uint32, inherit bool, pid uint32) (windows.Handle, error) {
					wantAccess := uint32(windows.PROCESS_QUERY_INFORMATION | windows.PROCESS_VM_READ | windows.PROCESS_DUP_HANDLE | windows.SYNCHRONIZE)
					if access != wantAccess || inherit || pid != uint32(candidate.PID) {
						t.Fatalf("OpenProcess=(%#x,%t,%d), want (%#x,false,%d)", access, inherit, pid, wantAccess, candidate.PID)
					}
					return windows.Handle(7), nil
				},
				closeHandle:  func(handle windows.Handle) error { closes++; return nil },
				ownerMatches: func(windows.Handle) (bool, error) { return true, nil },
				matchesCandidate: func(windows.Handle, providerProcessWorkingDirectoryCandidateV1) bool {
					matchCalls++
					return true
				},
				layoutFromHandle: func(windows.Handle) (providerWorkingDirectoryLayout, bool) {
					return providerWorkingDirectoryLayout{pointerSize: 8}, true
				},
				selfProbe: func(context.Context) bool { return true },
				readStableObject: func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
					reads++
					return selected, "", nil
				},
				wait: func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil },
			}
			if tc.mutate != nil {
				tc.mutate(&ops, &matchCalls, &reads)
			}
			got := probeProviderProcessWorkingDirectoryWithOps(context.Background(), candidate, selected, ops)
			if got.State != tc.wantState || got.FailureID != tc.wantFailure || closes != tc.wantCloses || reads != tc.wantReads {
				t.Fatalf("result=%+v closes=%d reads=%d", got, closes, reads)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	opened := false
	ops := providerWorkingDirectoryProbeOps{openProcess: func(uint32, bool, uint32) (windows.Handle, error) { opened = true; return 0, nil }}
	got := probeProviderProcessWorkingDirectoryWithOps(cancelled, candidate, selected, ops)
	if got.State != providerProcessWorkingDirectoryUnknown || got.FailureID != "provider-working-directory-unstable" || opened {
		t.Fatalf("cancelled result=%+v opened=%t", got, opened)
	}

	closeCalled := false
	closeFailureOps := providerWorkingDirectoryProbeOps{
		openProcess:      func(uint32, bool, uint32) (windows.Handle, error) { return windows.Handle(7), nil },
		closeHandle:      func(windows.Handle) error { closeCalled = true; return errors.New("close failed") },
		ownerMatches:     func(windows.Handle) (bool, error) { return true, nil },
		matchesCandidate: func(windows.Handle, providerProcessWorkingDirectoryCandidateV1) bool { return true },
		layoutFromHandle: func(windows.Handle) (providerWorkingDirectoryLayout, bool) {
			return providerWorkingDirectoryLayout{pointerSize: 8}, true
		},
		selfProbe: func(context.Context) bool { return true },
		readStableObject: func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerDirectoryObjectIdentityV1, string, error) {
			return selected, "", nil
		},
		wait: func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil },
	}
	got = probeProviderProcessWorkingDirectoryWithOps(context.Background(), candidate, selected, closeFailureOps)
	if !closeCalled || got.State != providerProcessWorkingDirectoryUnknown || got.FailureID != "provider-working-directory-access-unavailable" {
		t.Fatalf("close failure result=%+v called=%t", got, closeCalled)
	}
}

func TestProviderProcessHandleOwnerMatchesCurrentClosesTokenOnEveryOpenedPath(t *testing.T) {
	current, err := windows.StringToSid("S-1-5-21-1")
	if err != nil {
		t.Fatal(err)
	}
	other, err := windows.StringToSid("S-1-5-21-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		openErr    error
		targetSID  *windows.SID
		targetErr  error
		currentSID *windows.SID
		currentErr error
		closeErr   error
		wantMatch  bool
		wantErr    bool
		wantCloses int
	}{
		{"match", nil, current, nil, current, nil, nil, true, false, 1},
		{"different", nil, other, nil, current, nil, nil, false, false, 1},
		{"open-denied", errors.New("denied"), nil, nil, current, nil, nil, false, true, 0},
		{"target-query-denied", nil, nil, errors.New("denied"), current, nil, nil, false, true, 1},
		{"current-query-denied", nil, current, nil, nil, errors.New("denied"), nil, false, true, 1},
		{"close-failure", nil, current, nil, current, nil, errors.New("close failed"), false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closes := 0
			ops := providerWorkingDirectoryTokenOps{
				openToken: func(_ windows.Handle, access uint32, token *windows.Token) error {
					if access != windows.TOKEN_QUERY {
						t.Fatalf("token access=%#x", access)
					}
					*token = windows.Token(9)
					return tc.openErr
				},
				closeToken: func(windows.Token) error { closes++; return tc.closeErr },
				targetSID:  func(windows.Token) (*windows.SID, error) { return tc.targetSID, tc.targetErr },
				currentSID: func() (*windows.SID, error) { return tc.currentSID, tc.currentErr },
			}
			match, err := providerProcessHandleOwnerMatchesCurrentWithOps(windows.Handle(7), ops)
			if match != tc.wantMatch || (err != nil) != tc.wantErr || closes != tc.wantCloses {
				t.Fatalf("match=%t err=%v closes=%d", match, err, closes)
			}
		})
	}
}

func TestProviderReadRemoteBytesRejectsPartialAndUnreadableRegions(t *testing.T) {
	readable := windows.MemoryBasicInformation{BaseAddress: 0x1000, RegionSize: 0x1000, State: windows.MEM_COMMIT, Protect: windows.PAGE_READWRITE}
	for _, tc := range []struct {
		name      string
		address   uintptr
		size      uintptr
		query     windows.MemoryBasicInformation
		queryErr  error
		readBytes []byte
		readCount uintptr
		readErr   error
		wantOK    bool
	}{
		{"complete", 0x1100, 4, readable, nil, []byte{1, 2, 3, 4}, 4, nil, true},
		{"partial", 0x1100, 4, readable, nil, []byte{1, 2}, 2, nil, false},
		{"read-denied", 0x1100, 4, readable, nil, nil, 0, errors.New("denied"), false},
		{"query-denied", 0x1100, 4, readable, errors.New("denied"), nil, 0, nil, false},
		{"reserved", 0x1100, 4, func() windows.MemoryBasicInformation { x := readable; x.State = windows.MEM_RESERVE; return x }(), nil, nil, 0, nil, false},
		{"guarded", 0x1100, 4, func() windows.MemoryBasicInformation { x := readable; x.Protect |= windows.PAGE_GUARD; return x }(), nil, nil, 0, nil, false},
		{"outside-region", 0x2000, 4, readable, nil, nil, 0, nil, false},
		{"address-overflow", ^uintptr(0) - 1, 4, readable, nil, nil, 0, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ops := providerWorkingDirectoryRemoteReadOps{
				query: func(windows.Handle, uintptr) (windows.MemoryBasicInformation, error) { return tc.query, tc.queryErr },
				read: func(windows.Handle, uintptr, uintptr) ([]byte, uintptr, error) {
					return tc.readBytes, tc.readCount, tc.readErr
				},
			}
			got, ok := providerReadRemoteBytesWithOps(windows.Handle(7), tc.address, tc.size, ops)
			if ok != tc.wantOK {
				t.Fatalf("ok=%t bytes=%v", ok, got)
			}
			if ok && !reflect.DeepEqual(got, tc.readBytes) {
				t.Fatalf("bytes=%v want=%v", got, tc.readBytes)
			}
		})
	}
}

// TestProviderWorkingDirectoryLayoutSelection catches architecture dispatch
// that guesses pointer width from the observer instead of the target process.
func TestProviderWorkingDirectoryLayoutSelection(t *testing.T) {
	cases := []struct {
		name                       string
		processMachine             uint16
		nativeMachine              uint16
		hostPointerSize            uintptr
		wantOK                     bool
		wantPointerSize            uintptr
		wantProcessInfoClass       int32
		wantPEBProcessParams       uintptr
		wantCurrentDirectoryHandle uintptr
	}{
		{"native-amd64", pe.IMAGE_FILE_MACHINE_UNKNOWN, pe.IMAGE_FILE_MACHINE_AMD64, 8, true, 8, windows.ProcessBasicInformation, 0x20, 0x48},
		{"native-arm64", pe.IMAGE_FILE_MACHINE_UNKNOWN, pe.IMAGE_FILE_MACHINE_ARM64, 8, true, 8, windows.ProcessBasicInformation, 0x20, 0x48},
		{"amd64-emulated-on-arm64", pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_MACHINE_ARM64, 8, true, 8, windows.ProcessBasicInformation, 0x20, 0x48},
		{"i386-wow64", pe.IMAGE_FILE_MACHINE_I386, pe.IMAGE_FILE_MACHINE_AMD64, 8, true, 4, windows.ProcessWow64Information, 0x10, 0x2c},
		{"armnt-wow64", pe.IMAGE_FILE_MACHINE_ARMNT, pe.IMAGE_FILE_MACHINE_ARM64, 8, true, 4, windows.ProcessWow64Information, 0x10, 0x2c},
		{"unknown-native", pe.IMAGE_FILE_MACHINE_UNKNOWN, pe.IMAGE_FILE_MACHINE_I386, 8, false, 0, 0, 0, 0},
		{"unknown-process", 0xffff, pe.IMAGE_FILE_MACHINE_AMD64, 8, false, 0, 0, 0, 0},
		{"32-bit-observer-cannot-address-64-bit-target", pe.IMAGE_FILE_MACHINE_UNKNOWN, pe.IMAGE_FILE_MACHINE_AMD64, 4, false, 0, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := providerWorkingDirectoryLayoutFor(tc.processMachine, tc.nativeMachine, tc.hostPointerSize)
			if ok != tc.wantOK {
				t.Fatalf("ok=%t layout=%+v, want ok=%t", ok, got, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.pointerSize != tc.wantPointerSize || got.processInfoClass != tc.wantProcessInfoClass || got.pebProcessParametersOffset != tc.wantPEBProcessParams || got.currentDirectoryHandleOffset != tc.wantCurrentDirectoryHandle {
				t.Fatalf("layout=%+v", got)
			}
		})
	}
}

func TestReadProviderWorkingDirectorySampleOrderedReadDuplicateLedger(t *testing.T) {
	layout := providerWorkingDirectoryLayout{pointerSize: 8, processInfoClass: windows.ProcessBasicInformation, pebProcessParametersOffset: 0x20, currentDirectoryHandleOffset: 0x48}
	wantIdentity := providerDirectoryObjectIdentityForTest(7)
	var ledger []string
	reads := 0
	ops := providerWorkingDirectorySampleOps{
		processPEBAddress: func(handle windows.Handle, got providerWorkingDirectoryLayout) (uintptr, bool) {
			ledger = append(ledger, fmt.Sprintf("peb:%d:%d", handle, got.processInfoClass))
			return 0x1000, true
		},
		readRemotePointer: func(handle windows.Handle, base, offset, pointerSize uintptr) (uintptr, bool) {
			ledger = append(ledger, fmt.Sprintf("read:%d:%x+%x/%d", handle, base, offset, pointerSize))
			reads++
			switch reads {
			case 1:
				return 0x2000, true
			case 2, 3:
				return 0x33, true // Opaque handle value: deliberately unaligned.
			default:
				t.Fatalf("unexpected read %d", reads)
				return 0, false
			}
		},
		duplicateObject: func(process windows.Handle, remoteHandle uintptr) (providerDirectoryObjectIdentityV1, string, error) {
			ledger = append(ledger, fmt.Sprintf("duplicate:%d:%x", process, remoteHandle))
			return wantIdentity, "", nil
		},
	}
	got, failureID, err := readProviderWorkingDirectorySampleWithOps(context.Background(), windows.Handle(7), layout, ops)
	wantLedger := []string{"peb:7:0", "read:7:1000+20/8", "read:7:2000+48/8", "duplicate:7:33", "read:7:2000+48/8"}
	if err != nil || failureID != "" || got.remoteHandle != 0x33 || got.objectIdentity != wantIdentity || !reflect.DeepEqual(ledger, wantLedger) {
		t.Fatalf("sample=%+v failure=%q err=%v ledger=%v", got, failureID, err, ledger)
	}
}

func TestReadProviderWorkingDirectorySampleRejectsHandleReuseDuringDuplicate(t *testing.T) {
	reads := 0
	ops := providerWorkingDirectorySampleOps{
		processPEBAddress: func(windows.Handle, providerWorkingDirectoryLayout) (uintptr, bool) { return 0x1000, true },
		readRemotePointer: func(windows.Handle, uintptr, uintptr, uintptr) (uintptr, bool) {
			reads++
			switch reads {
			case 1:
				return 0x2000, true
			case 2:
				return 0x33, true
			default:
				return 0x34, true
			}
		},
		duplicateObject: func(windows.Handle, uintptr) (providerDirectoryObjectIdentityV1, string, error) {
			return providerDirectoryObjectIdentityForTest(1), "", nil
		},
	}
	got, failureID, err := readProviderWorkingDirectorySampleWithOps(context.Background(), windows.Handle(7), providerWorkingDirectoryLayout{pointerSize: 8, pebProcessParametersOffset: 0x20, currentDirectoryHandleOffset: 0x48}, ops)
	if err == nil || failureID != "provider-working-directory-unstable" || got.objectIdentity.valid {
		t.Fatalf("sample=%+v failure=%q err=%v", got, failureID, err)
	}
}

func TestReadStableProviderWorkingDirectoryObjectRejectsHandleOrIdentityChange(t *testing.T) {
	base := providerWorkingDirectoryObjectSample{remoteHandle: 0x33, objectIdentity: providerDirectoryObjectIdentityForTest(1)}
	for _, tc := range []struct {
		name   string
		second providerWorkingDirectoryObjectSample
	}{
		{"handle-reuse", providerWorkingDirectoryObjectSample{remoteHandle: 0x34, objectIdentity: base.objectIdentity}},
		{"object-identity-change", providerWorkingDirectoryObjectSample{remoteHandle: base.remoteHandle, objectIdentity: providerDirectoryObjectIdentityForTest(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			sampler := func(context.Context, windows.Handle, providerWorkingDirectoryLayout) (providerWorkingDirectoryObjectSample, string, error) {
				calls++
				if calls == 1 {
					return base, "", nil
				}
				return tc.second, "", nil
			}
			got, failureID, err := readStableProviderWorkingDirectoryObjectWithSampler(context.Background(), windows.Handle(7), providerWorkingDirectoryLayout{}, sampler)
			if err == nil || failureID != "provider-working-directory-unstable" || got.valid || calls != 2 {
				t.Fatalf("identity=%+v failure=%q err=%v calls=%d", got, failureID, err, calls)
			}
		})
	}
}

func TestDuplicateProviderDirectoryObjectIdentityUsesZeroRightsAndCloses(t *testing.T) {
	wantIdentity := providerDirectoryObjectIdentityForTest(9)
	for _, tc := range []struct {
		name         string
		duplicateErr error
		objectOK     bool
		closeErr     error
		wantFailure  string
		wantCloses   int
	}{
		{"complete", nil, true, nil, "", 1},
		{"duplicate-denied", errors.New("denied"), true, nil, "provider-working-directory-access-unavailable", 0},
		{"not-a-disk-directory", nil, false, nil, "provider-working-directory-object-unavailable", 1},
		{"close-failed", nil, true, errors.New("close failed"), "provider-working-directory-object-unavailable", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			closes := 0
			ops := providerDirectoryDuplicateOps{
				currentProcess: func() windows.Handle { return windows.Handle(11) },
				duplicate: func(sourceProcess, sourceHandle, targetProcess windows.Handle, targetHandle *windows.Handle, desiredAccess uint32, inherit bool, options uint32) error {
					if sourceProcess != windows.Handle(7) || sourceHandle != windows.Handle(0x33) || targetProcess != windows.Handle(11) || desiredAccess != 0 || inherit || options != 0 {
						t.Fatalf("DuplicateHandle=(%d,%x,%d,%#x,%t,%#x)", sourceProcess, sourceHandle, targetProcess, desiredAccess, inherit, options)
					}
					if tc.duplicateErr == nil {
						*targetHandle = windows.Handle(13)
					}
					return tc.duplicateErr
				},
				closeHandle: func(handle windows.Handle) error {
					closes++
					if handle != windows.Handle(13) {
						t.Fatalf("closed handle=%d", handle)
					}
					return tc.closeErr
				},
				objectIdentity: func(handle windows.Handle) (providerDirectoryObjectIdentityV1, bool) {
					if handle != windows.Handle(13) {
						t.Fatalf("queried handle=%d", handle)
					}
					return wantIdentity, tc.objectOK
				},
			}
			got, failureID, err := duplicateProviderDirectoryObjectIdentityWithOps(windows.Handle(7), uintptr(0x33), ops)
			if (err != nil) != (tc.wantFailure != "") || failureID != tc.wantFailure || closes != tc.wantCloses {
				t.Fatalf("identity=%+v failure=%q err=%v closes=%d", got, failureID, err, closes)
			}
			if tc.wantFailure == "" && got != wantIdentity {
				t.Fatalf("identity=%+v want=%+v", got, wantIdentity)
			}
		})
	}
}

func TestProviderDirectoryObjectIdentityFromHandleRejectsPipeAndFile(t *testing.T) {
	want := providerDirectoryObjectIdentityForTest(3)
	for _, tc := range []struct {
		name         string
		fileType     uint32
		fileTypeErr  error
		directory    byte
		standardErr  error
		identity     providerDirectoryObjectIdentityV1
		identityErr  error
		wantOK       bool
		wantStandard int
		wantID       int
	}{
		{"disk-directory", windows.FILE_TYPE_DISK, nil, 1, nil, want, nil, true, 1, 1},
		{"pipe", windows.FILE_TYPE_PIPE, nil, 0, nil, want, nil, false, 0, 0},
		{"character-device", windows.FILE_TYPE_CHAR, nil, 0, nil, want, nil, false, 0, 0},
		{"unknown", windows.FILE_TYPE_UNKNOWN, errors.New("unknown"), 0, nil, want, nil, false, 0, 0},
		{"regular-file", windows.FILE_TYPE_DISK, nil, 0, nil, want, nil, false, 1, 0},
		{"standard-query-denied", windows.FILE_TYPE_DISK, nil, 0, errors.New("denied"), want, nil, false, 1, 0},
		{"file-id-unavailable", windows.FILE_TYPE_DISK, nil, 1, nil, providerDirectoryObjectIdentityV1{}, errors.New("denied"), false, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standardCalls, idCalls := 0, 0
			ops := providerDirectoryObjectHandleOps{
				fileType: func(windows.Handle) (uint32, error) { return tc.fileType, tc.fileTypeErr },
				standardInfo: func(windows.Handle) (providerFileStandardInfo, error) {
					standardCalls++
					return providerFileStandardInfo{directory: tc.directory}, tc.standardErr
				},
				fileIDInfo: func(windows.Handle) (providerDirectoryObjectIdentityV1, error) {
					idCalls++
					return tc.identity, tc.identityErr
				},
			}
			got, ok := providerDirectoryObjectIdentityFromHandleWithOps(windows.Handle(13), ops)
			if ok != tc.wantOK || standardCalls != tc.wantStandard || idCalls != tc.wantID {
				t.Fatalf("identity=%+v ok=%t standard=%d id=%d", got, ok, standardCalls, idCalls)
			}
		})
	}
}

func TestProviderWorkingDirectoryObjectIdentityUsesTrustedDirectoryHandle(t *testing.T) {
	dir := t.TempDir()
	identity, err := providerWorkingDirectoryObjectIdentityFromPath(dir)
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	trailingIdentity, err := providerWorkingDirectoryObjectIdentityFromPath(dir + string(os.PathSeparator))
	if err != nil || identity != trailingIdentity {
		t.Fatalf("trailing identity=%+v err=%v, base=%+v", trailingIdentity, err, identity)
	}
	caseVariant := dir
	if len(caseVariant) >= 1 {
		if caseVariant[0] >= 'a' && caseVariant[0] <= 'z' {
			caseVariant = string(caseVariant[0]-'a'+'A') + caseVariant[1:]
		} else if caseVariant[0] >= 'A' && caseVariant[0] <= 'Z' {
			caseVariant = string(caseVariant[0]-'A'+'a') + caseVariant[1:]
		}
	}
	caseIdentity, err := providerWorkingDirectoryObjectIdentityFromPath(caseVariant)
	if err != nil || identity != caseIdentity {
		t.Fatalf("case identity=%+v err=%v, base=%+v", caseIdentity, err, identity)
	}

	alias := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-junction")
	if err := os.Symlink(dir, alias); err == nil {
		t.Cleanup(func() { _ = os.Remove(alias) })
		aliasIdentity, identityErr := providerWorkingDirectoryObjectIdentityFromPath(alias)
		if identityErr != nil || identity != aliasIdentity {
			t.Fatalf("alias identity=%+v err=%v, base=%+v", aliasIdentity, identityErr, identity)
		}
	}
	finalNamespaceIdentity, err := providerWorkingDirectoryObjectIdentityFromPath(`\\?\` + dir)
	if err != nil || identity != finalNamespaceIdentity {
		t.Fatalf("final-namespace identity=%+v err=%v, base=%+v", finalNamespaceIdentity, err, identity)
	}
	longW, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatal(err)
	}
	shortBuf := make([]uint16, windows.MAX_PATH)
	if n, shortErr := windows.GetShortPathName(longW, &shortBuf[0], uint32(len(shortBuf))); shortErr == nil && n > 0 && n < uint32(len(shortBuf)) {
		shortPath := windows.UTF16ToString(shortBuf[:n])
		shortIdentity, identityErr := providerWorkingDirectoryObjectIdentityFromPath(shortPath)
		if identityErr != nil || identity != shortIdentity {
			t.Fatalf("short-path identity=%+v err=%v, base=%+v", shortIdentity, identityErr, identity)
		}
	} else {
		t.Logf("short-path alias unavailable on this volume: %v", shortErr)
	}

	for _, unsafePath := range []string{"relative", dir + "\x00suffix", filepath.Join(dir, "missing"), `\\.\pipe\provider-cwd-test`, `\??\C:\provider-cwd-test`, `\\?\GLOBALROOT\Device\NamedPipe\provider-cwd-test`} {
		if _, err := providerWorkingDirectoryObjectIdentityFromPath(unsafePath); err == nil {
			t.Fatalf("unsafe/non-directory metadata path admitted: %q", unsafePath)
		}
	}
	regularFile := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(regularFile, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := providerWorkingDirectoryObjectIdentityFromPath(regularFile); err == nil {
		t.Fatal("regular file admitted as provider working directory")
	}
}

type ownedProviderWorkingDirectoryHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	wait   <-chan error
	exited bool
}

func buildOwnedProviderWorkingDirectoryHelper(t *testing.T) string {
	return buildOwnedProviderWorkingDirectoryHelperForArch(t, runtime.GOARCH)
}

func buildOwnedProviderWorkingDirectoryHelperForArch(t *testing.T, arch string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "provider-cwd-helper-"+arch+".exe")
	cmd := exec.Command("go", "build", "-ldflags=-H=windowsgui", "-o", path, "./testdata/provider-cwd-helper")
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH="+arch, "CGO_ENABLED=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build owned provider helper: %v\n%s", err, output)
	}
	return path
}

// TestProbeProviderProcessWorkingDirectoryReadsOwnedWOW64Helper catches using
// the native 64-bit PEB offsets for a 32-bit candidate. It is the required
// owned WOW64 acceptance on an AMD64 host; other hosts keep the synthetic
// layout matrix and cross-compile gates.
func TestProbeProviderProcessWorkingDirectoryReadsOwnedWOW64Helper(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("owned I386 WOW64 runtime acceptance requires a Windows AMD64 host")
	}
	helperPath := buildOwnedProviderWorkingDirectoryHelperForArch(t, "386")
	cwd := t.TempDir()
	helper := startOwnedProviderWorkingDirectoryHelper(t, helperPath, cwd)
	assertOwnedProviderHelperAlive(t, helper)
	started, image := ownedProviderHelperGeneration(t, helper)
	identity, err := providerWorkingDirectoryObjectIdentityFromPath(cwd)
	if err != nil {
		t.Fatalf("WOW64 cwd identity: %v", err)
	}
	candidate := providerProcessWorkingDirectoryCandidateV1{PID: helper.cmd.Process.Pid, SnapshotStarted: started.Truncate(time.Microsecond), ExecutablePath: image}
	if got := probeProviderProcessWorkingDirectory(context.Background(), candidate, identity); got.State != providerProcessWorkingDirectoryEqual {
		t.Fatalf("WOW64 working-directory probe=%+v", got)
	}
	helper.stopByClosingInput(t)
}

// TestProviderWorkingDirectoryPEBDOSPathNamedPipeDoesNotConnect proves target
// path text is inert. The helper changes only its Process Environment Block
// DOS-path descriptor while retaining its legitimate current-directory handle.
// The server never impersonates or reads client data.
func TestProviderWorkingDirectoryPEBDOSPathNamedPipeDoesNotConnect(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("owned native PEB DOS-path spoof acceptance runs on Windows AMD64")
	}
	helperPath := buildOwnedProviderWorkingDirectoryHelper(t)
	cwd := t.TempDir()
	helper := startOwnedProviderWorkingDirectoryHelper(t, helperPath, cwd)
	assertOwnedProviderHelperAlive(t, helper)
	started, image := ownedProviderHelperGeneration(t, helper)
	selected, err := providerWorkingDirectoryObjectIdentityFromPath(cwd)
	if err != nil {
		t.Fatalf("selected directory identity: %v", err)
	}

	pipeName := fmt.Sprintf(`\\.\pipe\mcphub-provider-cwd-spoof-%d`, os.Getpid())
	pipeNameW, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		t.Fatal(err)
	}
	pipe, err := windows.CreateNamedPipe(pipeNameW, windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_OVERLAPPED, windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT, 1, 0, 0, 1000, nil)
	if err != nil {
		t.Fatalf("create owned pipe: %v", err)
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		_ = windows.CloseHandle(pipe)
		t.Fatalf("create owned pipe event: %v", err)
	}
	overlapped := &windows.Overlapped{HEvent: event}
	t.Cleanup(func() {
		_ = windows.CancelIoEx(pipe, overlapped)
		_ = windows.DisconnectNamedPipe(pipe)
		_ = windows.CloseHandle(pipe)
		_ = windows.CloseHandle(event)
	})
	if err := windows.ConnectNamedPipe(pipe, overlapped); err == nil || !errors.Is(err, windows.ERROR_IO_PENDING) {
		t.Fatalf("arm owned pipe connection: %v", err)
	}

	helper.spoofDOSPath(t, pipeName)
	candidate := providerProcessWorkingDirectoryCandidateV1{PID: helper.cmd.Process.Pid, SnapshotStarted: started.Truncate(time.Microsecond), ExecutablePath: image}
	if got := probeProviderProcessWorkingDirectory(context.Background(), candidate, selected); got.State != providerProcessWorkingDirectoryEqual {
		t.Fatalf("PEB DOS-path spoof changed handle-derived result: %+v", got)
	}
	status, err := windows.WaitForSingleObject(event, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("owned pipe observed MCPHub connection: status=%d err=%v", status, err)
	}
	helper.stopByClosingInput(t)
}

func startOwnedProviderWorkingDirectoryHelper(t *testing.T, executable, cwd string) *ownedProviderWorkingDirectoryHelper {
	t.Helper()
	cmd := exec.Command(executable, "child")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("owned helper stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		t.Fatalf("owned helper stdout: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		t.Fatalf("start owned helper: %v", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- cmd.Wait() }()
	helper := &ownedProviderWorkingDirectoryHelper{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), wait: wait}
	t.Cleanup(func() {
		if helper.exited {
			return
		}
		_ = helper.stdin.Close()
		select {
		case err := <-helper.wait:
			helper.exited = true
			if err != nil {
				t.Errorf("owned helper cleanup exit: %v; stderr=%s", err, stderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Errorf("owned helper did not exit after stdin closure")
			_ = helper.cmd.Process.Kill()
			<-helper.wait
			helper.exited = true
		}
	})
	helper.ping(t)
	return helper
}

func (h *ownedProviderWorkingDirectoryHelper) changeDirectory(t *testing.T, cwd string) {
	t.Helper()
	if h.exited {
		t.Fatal("owned helper already exited")
	}
	if _, err := fmt.Fprintf(h.stdin, "chdir\t%s\n", cwd); err != nil {
		t.Fatalf("send owned helper chdir: %v", err)
	}
	reply := make(chan string, 1)
	go func() {
		line, _ := h.stdout.ReadString('\n')
		reply <- line
	}()
	select {
	case line := <-reply:
		if line != "ok\n" {
			t.Fatalf("owned helper chdir reply=%q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned helper chdir timed out")
	}
}

func (h *ownedProviderWorkingDirectoryHelper) spoofDOSPath(t *testing.T, path string) {
	t.Helper()
	if h.exited {
		t.Fatal("owned helper already exited")
	}
	if _, err := fmt.Fprintf(h.stdin, "spoofdospath\t%s\n", path); err != nil {
		t.Fatalf("send owned helper DOS-path spoof: %v", err)
	}
	reply := make(chan string, 1)
	go func() {
		line, _ := h.stdout.ReadString('\n')
		reply <- line
	}()
	select {
	case line := <-reply:
		if line != "ok\n" {
			t.Fatalf("owned helper DOS-path spoof reply=%q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned helper DOS-path spoof timed out")
	}
}

func (h *ownedProviderWorkingDirectoryHelper) ping(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(h.stdin, "ping"); err != nil {
		t.Fatalf("send owned helper readiness probe: %v", err)
	}
	reply := make(chan string, 1)
	go func() {
		line, _ := h.stdout.ReadString('\n')
		reply <- line
	}()
	select {
	case line := <-reply:
		if line != "ok\n" {
			t.Fatalf("owned helper readiness reply=%q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owned helper readiness timed out")
	}
}

func (h *ownedProviderWorkingDirectoryHelper) stopByClosingInput(t *testing.T) {
	t.Helper()
	if h.exited {
		return
	}
	if err := h.stdin.Close(); err != nil {
		t.Fatalf("close owned helper stdin: %v", err)
	}
	select {
	case err := <-h.wait:
		h.exited = true
		if err != nil {
			t.Fatalf("owned helper graceful exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("owned helper did not exit after stdin closure")
		_ = h.cmd.Process.Kill()
		<-h.wait
		h.exited = true
		t.FailNow()
	}
}

func assertOwnedProviderHelperAlive(t *testing.T, helper *ownedProviderWorkingDirectoryHelper) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(helper.cmd.Process.Pid))
	if err != nil {
		t.Fatalf("open owned helper liveness handle: %v", err)
	}
	defer windows.CloseHandle(handle)
	status, err := windows.WaitForSingleObject(handle, 0)
	if err != nil || status != uint32(windows.WAIT_TIMEOUT) {
		t.Fatalf("owned helper is not alive: status=%d err=%v", status, err)
	}
}

func ownedProviderHelperGeneration(t *testing.T, helper *ownedProviderWorkingDirectoryHelper) (time.Time, string) {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(helper.cmd.Process.Pid))
	if err != nil {
		t.Fatalf("open owned helper identity handle: %v", err)
	}
	defer windows.CloseHandle(handle)
	started, image, err := providerProcessHandleGeneration(handle)
	if err != nil {
		t.Fatalf("owned helper identity: %v", err)
	}
	return started, image
}

// TestProviderWorkingDirectoryIdentityOwnsOnlySelectedLiveInstance is the
// full regression oracle: A and B are simultaneous owned helpers with the same
// executable and ordered arguments. A remains selected across a current-dir
// change through prior-generation retention; apply stops only A via the fake
// provider CAS, repeat is a no-op, and de-adopt restores metadata without
// changing B.
func TestProviderWorkingDirectoryIdentityOwnsOnlySelectedLiveInstance(t *testing.T) {
	helperPath := buildOwnedProviderWorkingDirectoryHelper(t)
	cwdA := t.TempDir()
	cwdB := t.TempDir()
	helperA := startOwnedProviderWorkingDirectoryHelper(t, helperPath, cwdA)
	helperB := startOwnedProviderWorkingDirectoryHelper(t, helperPath, cwdB)
	assertOwnedProviderHelperAlive(t, helperA)
	assertOwnedProviderHelperAlive(t, helperB)

	identityA, err := providerWorkingDirectoryObjectIdentityFromPath(cwdA)
	if err != nil {
		t.Fatalf("cwd A identity: %v", err)
	}
	identityB, err := providerWorkingDirectoryObjectIdentityFromPath(cwdB)
	if err != nil {
		t.Fatalf("cwd B identity: %v", err)
	}
	startedB, imageB := ownedProviderHelperGeneration(t, helperB)
	processIdentity := providerDirectProcessIdentityV1{ExecutablePath: helperPath, Args: []string{"child"}, WorkingDirectoryObject: identityA}
	pre := observeProviderDirectProcessTree(context.Background(), processIdentity, nil)
	if pre.State != providerProcessObservationComplete || len(pre.Active) == 0 {
		t.Fatalf("pre-observation=%+v, want A tree", pre)
	}
	for _, generation := range pre.Active {
		if generation.RootPID != helperA.cmd.Process.Pid || generation.PID == helperB.cmd.Process.Pid {
			t.Fatalf("pre-observation=%+v, want A/descendants only", pre)
		}
	}
	helperA.changeDirectory(t, cwdB)
	changed := observeProviderDirectProcessTree(context.Background(), processIdentity, pre.Active)
	if changed.State != providerProcessObservationComplete || len(changed.Active) == 0 {
		t.Fatalf("post-chdir observation=%+v, want prior A retained and B excluded", changed)
	}
	for _, generation := range changed.Active {
		if generation.RootPID != helperA.cmd.Process.Pid || generation.PID == helperB.cmd.Process.Pid {
			t.Fatalf("post-chdir observation=%+v, want prior A retained and B excluded", changed)
		}
	}
	helperA.changeDirectory(t, cwdA)

	entryName := "provider-working-directory-live-pair"
	_, _, _ = setupAdoptTestEnv(t, entryName, "[mcp_servers.keep]\ncommand = \"go\"\nargs = [\"version\"]\n")
	preparePreflightBinaryChecks(t)
	installFakeScheduler(t, newInstallFakeScheduler())
	installFakeAutostartBackend(t, &fakeInstallAutostartBackend{})
	t.Cleanup(setSupervisorReconcileApplyHookForTest(func(context.Context, bool) (ReconcileResponse, error) { return ReconcileResponse{}, nil }))
	provider := &providerLifecycleFake{entry: clients.ProviderMCPEntryV1{
		ProviderClient: "codex-cli", PluginRef: "fixture@catalog", ServerName: entryName,
		Transport: clients.ProviderMCPTransportStdio, Command: helperPath, Args: []string{"child"}, WorkingDir: &cwdA,
		Scope: clients.ProviderMCPScopeUser, ToolTimeoutSec: 30, Enabled: true, ReceiptFingerprint: "receipt",
		ActivationFingerprint: "activation", ActivationEnabledPresent: true, ActivationEnabled: true,
		DisabledActivationFingerprint: "disabled", PolicyState: clients.ProviderMCPPolicyNone, PolicyFingerprint: "policy",
	}}
	provider.onCAS = func() {
		if provider.entry.Enabled {
			helperA.stopByClosingInput(t)
		}
	}
	port := nextBindableAdoptPortForTest(t, collectUsedAdoptPorts())
	plan, err := NewAPI().buildProviderAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: port}, nil, provider)
	if err != nil {
		t.Fatalf("build provider adopt plan: %v", err)
	}
	if err := NewAPI().ExecuteAdoptWithOpts(plan, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("provider apply: %v", err)
	}
	if provider.calls != 1 || provider.entry.Enabled {
		t.Fatalf("provider after apply: calls=%d entry=%+v", provider.calls, provider.entry)
	}
	assertOwnedProviderHelperAlive(t, helperB)
	if gotStarted, gotImage := ownedProviderHelperGeneration(t, helperB); !gotStarted.Equal(startedB) || !providerExecutableEqual(gotImage, imageB) {
		t.Fatalf("B identity changed: started=(%s,%s) image=(%q,%q)", gotStarted, startedB, gotImage, imageB)
	}
	bCandidate := providerProcessWorkingDirectoryCandidateV1{PID: helperB.cmd.Process.Pid, SnapshotStarted: startedB.Truncate(time.Microsecond), ExecutablePath: imageB}
	if got := probeProviderProcessWorkingDirectory(context.Background(), bCandidate, identityB); got.State != providerProcessWorkingDirectoryEqual {
		t.Fatalf("B working directory changed after apply: %+v", got)
	}

	repeat, err := NewAPI().BuildAdoptPlan(AdoptOpts{EntryName: entryName, Client: "codex-cli", ManifestName: entryName, ProviderPluginRef: "fixture@catalog", Port: port})
	if err != nil || !repeat.alreadyAdopted {
		t.Fatalf("repeat plan=%+v err=%v", repeat, err)
	}
	if err := NewAPI().ExecuteAdoptWithOpts(repeat, nil, ExecuteAdoptOpts{providerDeps: providerTransactionDeps{source: provider}}); err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if provider.calls != 1 {
		t.Fatalf("repeat changed provider activation: calls=%d", provider.calls)
	}
	assertOwnedProviderHelperAlive(t, helperB)

	deAdopt, err := NewAPI().BuildDeAdoptPlan(entryName)
	if err != nil {
		t.Fatalf("build de-adopt plan: %v", err)
	}
	stop := func(_ context.Context, frozen SupervisorDaemon) (StoppedSettlement, error) {
		return StoppedSettlement{TaskName: frozen.TaskName, State: StoppedSettlementStopped, Reason: StoppedSettlementReasonStopped}, nil
	}
	report, err := NewAPI().executeDeAdoptPlanWithOpts(deAdopt, nil, ExecuteDeAdoptOpts{providerDeps: providerTransactionDeps{source: provider, stop: stop}})
	if err != nil {
		t.Fatalf("de-adopt: %v", err)
	}
	if report == nil || provider.calls != 2 || !provider.entry.Enabled {
		t.Fatalf("de-adopt report=%+v calls=%d entry=%+v", report, provider.calls, provider.entry)
	}
	assertOwnedProviderHelperAlive(t, helperB)
	if gotStarted, gotImage := ownedProviderHelperGeneration(t, helperB); !reflect.DeepEqual(gotStarted, startedB) || !providerExecutableEqual(gotImage, imageB) {
		t.Fatalf("B identity changed after de-adopt: started=(%s,%s) image=(%q,%q)", gotStarted, startedB, gotImage, imageB)
	}
}
