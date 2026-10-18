//go:build windows

package process

import (
	"encoding/json"
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// All SDK operations are per-call fakes; no native SID/token/converter/free,
// process, Job, Task, IPC, port or helper is invoked by these selected tests.
func TestSIDFormatSourceOriginalAllocationAndFailures(t *testing.T) {
	inputErr := errors.New("fixture invalid SID input")
	conversionErr := errors.New("fixture SDK conversion false")
	decodeErr := errors.New("fixture decode failure")
	freeErr := errors.New("fixture actual free error")
	for _, tc := range []struct {
		name                                             string
		inputError, convertError, decodeError, freeError error
		nilOutput                                        bool
		remaining                                        windows.Handle
		wantFree                                         int
		complete                                         bool
	}{
		{name: "success", wantFree: 1, complete: true},
		{name: "input-invalid", inputError: inputErr},
		{name: "conversion-false", convertError: conversionErr, nilOutput: true},
		{name: "conversion-partial-unknown", convertError: conversionErr},
		{name: "conversion-output-undefined", nilOutput: true},
		{name: "decode-failed", decodeError: decodeErr, wantFree: 1},
		{name: "decode-plus-free-failed", decodeError: decodeErr, freeError: freeErr, remaining: 17, wantFree: 1},
		{name: "decoded-value-plus-free-failed", freeError: freeErr, remaining: 17, wantFree: 1},
		{name: "free-nonnull-without-error", remaining: 17, wantFree: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inputStorage := make([]byte, 16)
			sid := (*windows.SID)(unsafe.Pointer(&inputStorage[0]))
			output := make([]uint16, 256)
			frees, conversions := 0, 0
			ops := sidFormatOperations{
				input: func(*windows.SID) ([]byte, error) { return []byte{1, 2, 3, 4}, tc.inputError },
				convert: func(actual *windows.SID) (*uint16, error) {
					conversions++
					if actual != sid {
						t.Fatal("original SID input changed")
					}
					if tc.nilOutput {
						return nil, tc.convertError
					}
					return &output[0], tc.convertError
				},
				decode: func(actual *uint16) (string, error) {
					if actual != &output[0] {
						t.Fatal("original allocated buffer changed")
					}
					return "S-1-5-21-123", tc.decodeError
				},
				free: func(actual windows.Handle) (windows.Handle, error) {
					frees++
					if actual != windows.Handle(uintptr(unsafe.Pointer(&output[0]))) {
						t.Fatal("foreign allocation freed")
					}
					return tc.remaining, tc.freeError
				},
			}
			value, proof, err := formatSIDObservedWithOperations(sid, ops)
			if frees != tc.wantFree {
				t.Fatalf("free count%d, expected%d", frees, tc.wantFree)
			}
			for _, cause := range []error{tc.inputError, tc.convertError, tc.decodeError, tc.freeError} {
				if cause != nil && !errors.Is(err, cause) {
					t.Fatal("original cause lost")
				}
			}
			if (err == nil) != tc.complete || proof.Snapshot().Complete != tc.complete {
				t.Fatalf("completion differs: %v", err)
			}
			if tc.inputError != nil && conversions != 0 {
				t.Fatal("invalid input crossed conversion boundary")
			}
			if (proof.ValidateInput(sid, value) == nil) != tc.complete {
				t.Fatal("actual source binding eligibility differs")
			}
			proof.Validate(value)
			proof.ValidateInput(sid, value)
			proof.Snapshot()
			if frees != tc.wantFree {
				t.Fatal("validation repeated native free")
			}
			if tc.freeError != nil && tc.decodeError == nil && value != "S-1-5-21-123" {
				t.Fatal("actual decoded diagnostic text lost")
			}
			if tc.convertError != nil || tc.nilOutput {
				if proof.Snapshot().AllocationAcquired || proof.Snapshot().FreeAttempted {
					t.Fatal("unknown output became an owned allocation")
				}
			}
		})
	}
	value, proof, err := FormatSIDObserved(nil)
	if err == nil || value != "" || proof.Snapshot().ConversionAttempted || proof.Validate(value) == nil {
		t.Fatal("nil input became eligible")
	}
}

func TestSIDFormatSourceForeignInputSnapshotAndDrift(t *testing.T) {
	inputA := make([]byte, 16)
	inputB := make([]byte, 16)
	sidA := (*windows.SID)(unsafe.Pointer(&inputA[0]))
	sidB := (*windows.SID)(unsafe.Pointer(&inputB[0]))
	current := []byte{1, 2, 3}
	output := make([]uint16, 256)
	frees := 0
	ops := sidFormatOperations{input: func(*windows.SID) ([]byte, error) { return append([]byte(nil), current...), nil }, convert: func(*windows.SID) (*uint16, error) { return &output[0], nil }, decode: func(*uint16) (string, error) { return "S-1-5-21-123", nil }, free: func(windows.Handle) (windows.Handle, error) { frees++; return 0, nil }}
	value, original, err := formatSIDObservedWithOperations(sidA, ops)
	if err != nil {
		t.Fatal(err)
	}
	_, foreign, err := formatSIDObservedWithOperations(sidB, ops)
	if err != nil {
		t.Fatal(err)
	}
	if foreign.ValidateInput(sidA, value) == nil || original.ValidateInput(sidB, value) == nil {
		t.Fatal("identical foreign text substituted input")
	}
	if original.Validate("other") == nil {
		t.Fatal("different value accepted")
	}
	snapshot := original.Snapshot()
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var forged SIDFormatObservation
	if err := json.Unmarshal(raw, &forged); err != nil {
		t.Fatal(err)
	}
	if forged.Validate(value) == nil {
		t.Fatal("snapshot reconstructed authority")
	}
	*snapshot.ValueSHA256 = "mutated"
	if *original.Snapshot().ValueSHA256 == "mutated" {
		t.Fatal("snapshot aliased opaque source")
	}
	current[0] = 9
	if original.ValidateInput(sidA, value) == nil {
		t.Fatal("original input byte drift accepted")
	}
	if frees != 2 {
		t.Fatal("validation retried allocation release")
	}
}
