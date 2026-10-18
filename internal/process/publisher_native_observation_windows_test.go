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

// Only native operations/handles are fake; current capture and SID owners run.
func nativeFacetFixtureOperations(t *testing.T, closes *int, primary, closeErr error) publisherCaptureOperations {
	t.Helper()
	birth := uint64(133000000000000000)
	ft := windows.Filetime{HighDateTime: uint32(birth >> 32), LowDateTime: uint32(birth)}
	started := time.Unix(0, ft.Nanoseconds()).UTC()
	held := &heldPIDObservation{heldPIDJobObservation: &heldPIDJobObservation{
		pid: 42, handle: 17,
		identity: func(int, windows.Handle) (processHandleIdentity, error) { return processHandleIdentity{ExecutablePath: `C:\fixture\hub.exe`, CreationTime: started}, nil },
		verify: func(PIDIdentityProof, windows.Handle) error { return nil },
		close: func(windows.Handle) error { (*closes)++; return closeErr },
	}, wait: func(windows.Handle, uint32) (uint32, error) { return uint32(windows.WAIT_TIMEOUT), nil }}
	return publisherCaptureOperations{
		currentPID: func() int { return 42 },
		hold: func(int) (*heldPIDObservation, error) { return held, primary },
		birth: func(windows.Handle) (uint64, error) { return birth, nil },
		principal: func(actual *heldPIDObservation) (string, *SIDFormatObservation, error) {
			if actual != held { t.Fatal("foreign retained query") }
			input := make([]byte, 16); sid := (*windows.SID)(unsafe.Pointer(&input[0])); output := make([]uint16, 256)
			value, proof, err := formatSIDObservedWithOperations(sid, sidFormatOperations{
				input: func(*windows.SID) ([]byte, error) { return []byte{1, 2, 3}, nil },
				convert: func(*windows.SID) (*uint16, error) { return &output[0], nil },
				decode: func(*uint16) (string, error) { return "S-1-5-21-123", nil },
				free: func(windows.Handle) (windows.Handle, error) { return 0, nil },
			})
			if err == nil { err = proof.ValidateInput(sid, value) }
			actual.ownerSIDFormatting = proof
			return value, proof, err
		},
	}
}

func TestGenuineNativeFacetOriginalCloseWithoutBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel()
	closes := 0
	facet, err := observeCurrentPublisherNativeWithOperations(ctx, nativeFacetFixtureOperations(t, &closes, nil, nil))
	if err != nil || facet.Validate(ctx) != nil || closes != 1 || facet.NativeImagePathForQualification() == "" { t.Fatal("original unbound self evidence unavailable") }
	raw, err := json.Marshal(facet.NativeSnapshot()); if err != nil { t.Fatal(err) }
	var forged PublisherNativeObservation
	if err := json.Unmarshal(raw, &forged); err != nil { t.Fatal(err) }
	if forged.Validate(ctx) == nil { t.Fatal("DTO acquired native authority") }
	facet.Validate(ctx); facet.NativeSnapshot()
	if closes != 1 { t.Fatal("validation repeated original close") }
	binding := PublisherIncarnationBinding{OwnerInstanceID: "fixture-owner", OperationID: "fixture-operation", NamespaceSHA256: strings.Repeat("a",64), SourceEpoch: "fixture-epoch", ResourceScope: PublisherExclusiveLocalResources}
	boundCloses := 0
	bound, err := capturePublisherIncarnationWithOperations(ctx, binding, nativeFacetFixtureOperations(t, &boundCloses, nil, nil))
	if err != nil || bound.ValidateCapture(ctx, binding) != nil || boundCloses != 1 { t.Fatal("existing bound owner changed") }
	binding.OperationID = "foreign"
	if bound.ValidateCapture(ctx, binding) == nil { t.Fatal("exact original binding lost") }
	cancel()
	if facet.Validate(ctx) == nil { t.Fatal("canceled original validated") }
}

func TestGenuineNativeFacetPartialAndCloseFailureKeepCauses(t *testing.T) {
	for _, partial := range []bool{true, false} { t.Run(map[bool]string{true:"partial-query",false:"close-failed"}[partial], func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute); defer cancel()
		cause := errors.New("synthetic original lower failure"); var primary, cleanup error
		if partial { primary = cause } else { cleanup = cause }
		closes := 0
		facet, err := observeCurrentPublisherNativeWithOperations(ctx, nativeFacetFixtureOperations(t, &closes, primary, cleanup))
		if !errors.Is(err, cause) || closes != 1 || facet.Validate(ctx) == nil || facet.NativeImagePathForQualification() != "" { t.Fatal("partial or close failure acquired authority/lost cause") }
	}) }
}
