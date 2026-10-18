//go:build windows

package process

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native process, token, SID, wait and close operations are all per-call fakes.
func TestPublisherCaptureSourceOriginalCloseAndBindings(t *testing.T) {
	primary := errors.New("fixture original query failure")
	closeFailure := errors.New("fixture original close failure")
	for _, name := range []string{"complete", "partial-hold", "birth-drift", "verify-failed", "principal-failed", "signaled", "close-failed", "canceled-at-close", "missing-binding"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			binding := PublisherIncarnationBinding{OwnerInstanceID: "fixture-owner", OperationID: "fixture-operation", NamespaceSHA256: strings.Repeat("a", 64), SourceEpoch: "fixture-epoch", ResourceScope: PublisherExclusiveLocalResources}
			if name == "missing-binding" {
				binding.OperationID = ""
			}
			birth := uint64(133000000000000000)
			ft := windows.Filetime{HighDateTime: uint32(birth >> 32), LowDateTime: uint32(birth)}
			started := time.Unix(0, ft.Nanoseconds()).UTC()
			closes, holds := 0, 0
			held := &heldPIDObservation{heldPIDJobObservation: &heldPIDJobObservation{
				pid: 42, handle: 17,
				identity: func(int, windows.Handle) (processHandleIdentity, error) {
					return processHandleIdentity{ExecutablePath: `C:\fixture\hub.exe`, CreationTime: started}, nil
				},
				verify: func(PIDIdentityProof, windows.Handle) error {
					if name == "verify-failed" {
						return primary
					}
					return nil
				},
				close: func(windows.Handle) error {
					closes++
					if name == "canceled-at-close" {
						cancel()
					}
					if name == "close-failed" {
						return closeFailure
					}
					return nil
				},
			}, wait: func(windows.Handle, uint32) (uint32, error) {
				if name == "signaled" {
					return windows.WAIT_OBJECT_0, nil
				}
				return uint32(windows.WAIT_TIMEOUT), nil
			}}
			ops := publisherCaptureOperations{
				currentPID: func() int { return 42 },
				hold: func(int) (*heldPIDObservation, error) {
					holds++
					if name == "partial-hold" {
						return held, primary
					}
					return held, nil
				},
				birth: func(windows.Handle) (uint64, error) {
					if name == "birth-drift" {
						return birth + 1, nil
					}
					return birth, nil
				},
				principal: func(actual *heldPIDObservation) (string, *SIDFormatObservation, error) {
					if actual != held {
						t.Fatal("foreign held object")
					}
					if name == "principal-failed" {
						return "", nil, primary
					}
					input := make([]byte, 16)
					sid := (*windows.SID)(unsafe.Pointer(&input[0]))
					output := make([]uint16, 256)
					value, proof, err := formatSIDObservedWithOperations(sid, sidFormatOperations{
						input:   func(*windows.SID) ([]byte, error) { return []byte{1, 2, 3}, nil },
						convert: func(*windows.SID) (*uint16, error) { return &output[0], nil },
						decode:  func(*uint16) (string, error) { return "S-1-5-21-123", nil },
						free:    func(windows.Handle) (windows.Handle, error) { return 0, nil },
					})
					if err == nil {
						err = proof.ValidateInput(sid, value)
					}
					actual.ownerSIDFormatting = proof
					return value, proof, err
				},
			}
			capture, err := capturePublisherIncarnationWithOperations(ctx, binding, ops)
			eligible := name == "complete"
			if (err == nil) != eligible || (capture.ValidateCapture(ctx, binding) == nil) != eligible {
				t.Fatalf("eligibility differs: %v", err)
			}
			wantCloses := 1
			if name == "missing-binding" {
				wantCloses = 0
			}
			if closes != wantCloses || holds != wantCloses {
				t.Fatalf("holds%d closes%d", holds, closes)
			}
			if name == "partial-hold" && !errors.Is(err, primary) {
				t.Fatal("partial query cause lost")
			}
			if name == "close-failed" && !errors.Is(err, closeFailure) {
				t.Fatal("actual close failure lost")
			}
			if !eligible && capture.NativeImagePathForQualification() != "" {
				t.Fatal("failed capture exposed qualification input")
			}
			if eligible {
				if capture.NativeImagePathForQualification() != `C:\fixture\hub.exe` {
					t.Fatal("original path lost")
				}
				changed := binding
				changed.OperationID = "foreign-operation"
				if capture.ValidateCapture(ctx, changed) == nil {
					t.Fatal("foreign operation accepted")
				}
				native := capture.NativeSnapshot()
				raw, _ := json.Marshal(native)
				var forged PublisherIncarnation
				if err := json.Unmarshal(raw, &forged); err != nil {
					t.Fatal(err)
				}
				if forged.ValidateCapture(ctx, binding) == nil {
					t.Fatal("wire constructed authority")
				}
				capture.NativeSnapshot()
				capture.ValidateCapture(ctx, binding)
				if closes != 1 {
					t.Fatal("capture validation repeated close")
				}
			}
		})
	}
}
