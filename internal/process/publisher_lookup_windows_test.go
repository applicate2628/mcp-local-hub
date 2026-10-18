//go:build windows

package process

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Selected operations are pure fakes, including the native adapter/control.
func TestPublisherLookupSourcePositiveAndPairedNegative(t *testing.T) {
	for _, name := range []string{"original-signaled", "replaced", "live", "denied", "ambiguous-no-control", "paired-invalid-cid", "control-wrong-pid", "control-wrong-birth", "target-partial-unknown", "target-positive", "control-close-failed", "target-close-failed", "wrong-abi"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			binding := PublisherIncarnationBinding{OwnerInstanceID: "owner", OperationID: "operation", NamespaceSHA256: strings.Repeat("a", 64), SourceEpoch: "epoch", ResourceScope: PublisherExclusiveLocalResources}
			birth := uint64(133000000000000000)
			ft := windows.Filetime{HighDateTime: uint32(birth >> 32), LowDateTime: uint32(birth)}
			started := time.Unix(0, ft.Nanoseconds()).UTC()
			native := PublisherNativeIncarnationV1{PID: 43, NativeBirthFILETIME: birth, NativeImagePathSHA256: sidFormatObservationDigest([]byte(`C:\fixture\hub.exe`)), PrincipalSIDSHA256: sidFormatObservationDigest([]byte("S-1-5-21-123"))}
			native.NativeCaptureSHA256, _ = publisherNativeDigest(native)
			record := PublisherIncarnationRecordV1{Schema: PublisherIncarnationSchema, Version: 1, OwnerInstanceID: binding.OwnerInstanceID, PID: 43, NativeBirthFILETIME: birth, NativeImagePathSHA256: native.NativeImagePathSHA256, PrincipalSIDSHA256: native.PrincipalSIDSHA256, NativeCaptureSHA256: native.NativeCaptureSHA256, NamespaceSHA256: binding.NamespaceSHA256, SourceEpoch: binding.SourceEpoch, OperationID: binding.OperationID, ResourceScope: binding.ResourceScope, ImageIdentitySHA256: strings.Repeat("b", 64), ImageSHA256: strings.Repeat("c", 64)}
			if name == "replaced" {
				record.NativeBirthFILETIME++
				changed := native
				changed.NativeBirthFILETIME = record.NativeBirthFILETIME
				record.NativeCaptureSHA256, _ = publisherNativeDigest(changed)
			}
			closes, nativeCalls := map[windows.Handle]int{}, 0
			closeErr := errors.New("fixture native close failure")
			close := func(h windows.Handle) error {
				closes[h]++
				if (name == "control-close-failed" && h == 72) || (name == "target-close-failed" && h == 73) {
					return closeErr
				}
				return nil
			}
			newHeld := func(pid uint32, h windows.Handle) *heldPIDObservation {
				return &heldPIDObservation{heldPIDJobObservation: &heldPIDJobObservation{pid: int(pid), handle: h, identity: func(int, windows.Handle) (processHandleIdentity, error) {
					actual := started
					if name == "control-wrong-birth" && h == 72 {
						actual = actual.Add(time.Second)
					}
					return processHandleIdentity{ExecutablePath: `C:\fixture\hub.exe`, CreationTime: actual}, nil
				}, verify: func(PIDIdentityProof, windows.Handle) error { return nil }, close: close}, wait: func(windows.Handle, uint32) (uint32, error) {
					if pid == 43 && name != "live" {
						return windows.WAIT_OBJECT_0, nil
					}
					return uint32(windows.WAIT_TIMEOUT), nil
				}}
			}
			ops := publisherLookupOperations{close: close, processID: func(windows.Handle) (uint32, error) {
				if name == "control-wrong-pid" {
					return 99, nil
				}
				return 42, nil
			}, bindControl: func(pid uint32, h windows.Handle) (*heldPIDObservation, error) { return newHeld(pid, h), nil }, abi: func() (string, error) {
				if name == "wrong-abi" {
					return "", errors.New("fixture wrong ABI")
				}
				return strings.Repeat("d", 64), nil
			}}
			ops.capture = publisherCaptureOperations{currentPID: func() int { return 42 }, hold: func(pid int) (*heldPIDObservation, error) {
				if pid == 43 && name == "denied" {
					return nil, windows.ERROR_ACCESS_DENIED
				}
				if pid == 43 && name != "original-signaled" && name != "replaced" && name != "live" {
					return nil, windows.ERROR_INVALID_PARAMETER
				}
				return newHeld(uint32(pid), windows.Handle(pid)), nil
			}, birth: func(windows.Handle) (uint64, error) { return birth, nil }, principal: func(*heldPIDObservation) (string, *SIDFormatObservation, error) {
				input := make([]byte, 16)
				sid := (*windows.SID)(unsafe.Pointer(&input[0]))
				out := make([]uint16, 256)
				value, proof, err := formatSIDObservedWithOperations(sid, sidFormatOperations{input: func(*windows.SID) ([]byte, error) { return []byte{1, 2, 3}, nil }, convert: func(*windows.SID) (*uint16, error) { return &out[0], nil }, decode: func(*uint16) (string, error) { return "S-1-5-21-123", nil }, free: func(windows.Handle) (windows.Handle, error) { return 0, nil }})
				if err == nil {
					err = proof.ValidateInput(sid, value)
				}
				return value, proof, err
			}}
			ops.openNative = func(pid uint32) (publisherNativeOpenResult, error) {
				nativeCalls++
				if pid == 42 {
					if name == "ambiguous-no-control" {
						return publisherNativeOpenResult{}, errors.New("fixture control unavailable")
					}
					return publisherNativeOpenResult{handle: 72, status: 0, defined: true}, nil
				}
				if name == "target-partial-unknown" {
					return publisherNativeOpenResult{handle: 73, status: uint32(windows.STATUS_INVALID_CID), defined: true}, nil
				}
				if name == "target-positive" || name == "target-close-failed" {
					return publisherNativeOpenResult{handle: 73, status: 0, defined: true}, nil
				}
				return publisherNativeOpenResult{status: uint32(windows.STATUS_INVALID_CID), defined: true}, nil
			}
			proof, err := lookupPublisherIncarnationWithOperations(ctx, record, binding, ops)
			eligible := name == "original-signaled" || name == "replaced" || name == "paired-invalid-cid"
			if (err == nil) != eligible || (proof.Validate(ctx, record, binding) == nil) != eligible {
				t.Fatalf("eligibility differs: %v", err)
			}
			if name == "denied" {
				if nativeCalls != 0 || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
					t.Fatal("denial retried or lost")
				}
			}
			if name == "original-signaled" || name == "replaced" || name == "live" {
				if nativeCalls != 0 || closes[43] != 1 {
					t.Fatal("mature positive query changed native route or close")
				}
			}
			if name == "paired-invalid-cid" {
				if nativeCalls != 2 || closes[42] != 1 || closes[72] != 1 || closes[73] != 0 {
					t.Fatal("paired control/target receiving differs")
				}
				changed := binding
				changed.OperationID = "foreign"
				if proof.Validate(ctx, record, changed) == nil {
					t.Fatal("foreign binding accepted")
				}
			}
			if name == "target-partial-unknown" && closes[73] != 0 {
				t.Fatal("undefined acquisition treated as owned")
			}
			if name == "target-positive" || name == "target-close-failed" {
				if closes[73] != 1 {
					t.Fatal("unexpected positive target not closed once")
				}
			}
			if name == "control-close-failed" || name == "target-close-failed" {
				if !errors.Is(err, closeErr) {
					t.Fatal("original close failure lost")
				}
			}
			before := nativeCalls
			proof.Snapshot()
			proof.Validate(ctx, record, binding)
			if nativeCalls != before {
				t.Fatal("validation retried native action")
			}
		})
	}
}
