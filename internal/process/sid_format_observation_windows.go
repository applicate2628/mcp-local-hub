//go:build windows

package process

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These operations are supplied per invocation for Source failure fixtures.
// Production always selects the same installed SDK operations below.
type sidFormatOperations struct {
	input   func(*windows.SID) ([]byte, error)
	convert func(*windows.SID) (*uint16, error)
	decode  func(*uint16) (string, error)
	free    func(windows.Handle) (windows.Handle, error)
}

func sidFormatNativeOperations() sidFormatOperations {
	return sidFormatOperations{
		input: func(sid *windows.SID) ([]byte, error) {
			if sid == nil || !sid.IsValid() {
				return nil, errors.New("original SID input is invalid")
			}
			length := sid.Len()
			if length <= 0 {
				return nil, errors.New("original SID input length is undefined")
			}
			return append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(sid)), length)...), nil
		},
		convert: func(sid *windows.SID) (*uint16, error) {
			var text *uint16
			err := windows.ConvertSidToStringSid(sid, &text)
			return text, err
		},
		decode: func(text *uint16) (string, error) {
			if text == nil {
				return "", errors.New("original SID string allocation is undefined")
			}
			// Same pinned SDK256-unit UTF16 decoder, without its hidden free.
			value := windows.UTF16ToString((*[256]uint16)(unsafe.Pointer(text))[:])
			if value == "" {
				return "", errors.New("original SID decoded value is undefined")
			}
			return value, nil
		},
		free: windows.LocalFree,
	}
}

func FormatSIDObserved(sid *windows.SID) (string, *SIDFormatObservation, error) {
	return formatSIDObservedWithOperations(sid, sidFormatNativeOperations())
}

// The input owner must still hold its descriptor or Go SID buffer. This is
// binding validation, never a constructor from a borrowed/native pointer.
func (o *SIDFormatObservation) ValidateInput(sid *windows.SID, value string) error {
	if o == nil || sid == nil || o.inputOwner != sid || o.inputRead == nil {
		return errors.New("original borrowed SID formatting input required")
	}
	if err := o.Validate(value); err != nil {
		return err
	}
	raw, err := o.inputRead()
	if err != nil {
		return err
	}
	if len(raw) == 0 || o.view.InputSIDSHA256 == nil || sidFormatObservationDigest(raw) != *o.view.InputSIDSHA256 {
		return errors.New("original borrowed SID bytes changed")
	}
	return nil
}

func formatSIDObservedWithOperations(sid *windows.SID, ops sidFormatOperations) (value string, observation *SIDFormatObservation, retErr error) {
	o := &SIDFormatObservation{issuer: &sidFormatIssuer{marker: 1}, view: SIDFormatSnapshotV1{Schema: "sid-format-observation-v1", Version: 1}, inputOwner: sid}
	observation = o
	defer func() {
		o.value = value
		if value != "" {
			digest := sidFormatObservationDigest([]byte(value))
			o.view.ValueSHA256 = &digest
		}
		o.finalized = true
		o.view.Complete = o.primaryErr == nil && o.cleanupErr == nil && o.view.InputSIDSHA256 != nil && o.view.ConversionSucceeded && o.view.AllocationAcquired && o.view.DecodeSucceeded && o.view.FreeAttempted && o.view.FreeDefined && o.view.Freed
		retErr = errors.Join(o.primaryErr, o.cleanupErr)
	}()
	primary := func(code string, err error) {
		o.primaryErr = errors.Join(o.primaryErr, err)
		o.view.PrimaryError = &SIDFormatFailureV1{Code: code}
	}
	if sid == nil || ops.input == nil || ops.convert == nil || ops.decode == nil || ops.free == nil {
		primary("SID_FORMAT_INPUT_UNAVAILABLE", errors.New("original SID and formatter operations required"))
		return
	}
	raw, err := ops.input(sid)
	if err != nil || len(raw) == 0 {
		primary("SID_FORMAT_INPUT_INVALID", errors.Join(err, errors.New("original SID bytes unavailable")))
		return
	}
	digest := sidFormatObservationDigest(raw)
	o.view.InputSIDSHA256 = &digest
	o.inputRead = func() ([]byte, error) { return ops.input(sid) }
	o.view.ConversionAttempted = true
	text, err := ops.convert(sid)
	if err != nil {
		primary("SID_FORMAT_CONVERSION_FAILED", err)
		if text != nil {
			primary("SID_FORMAT_PARTIAL_ALLOCATION_UNKNOWN", errors.New("failed conversion cannot qualify partial output allocation for free"))
		}
		return
	}
	o.view.ConversionSucceeded = true
	if text == nil {
		primary("SID_FORMAT_ALLOCATION_UNDEFINED", errors.New("successful conversion returned no defined original allocation"))
		return
	}
	o.view.AllocationAcquired = true
	o.allocation = uintptr(unsafe.Pointer(text))
	defer func() {
		o.view.FreeAttempted = true
		remaining, freeErr := ops.free(windows.Handle(o.allocation))
		o.view.FreeDefined = true
		o.view.Freed = remaining == 0 && freeErr == nil
		if !o.view.Freed {
			o.cleanupErr = errors.Join(freeErr, errors.New("original SID string LocalFree failed or is uncertain"))
			o.view.CleanupError = &SIDFormatFailureV1{Code: "SID_FORMAT_FREE_FAILED"}
		}
		if o.view.Freed {
			o.allocation = 0
		}
	}()
	value, err = ops.decode(text)
	if err != nil || value == "" {
		value = ""
		primary("SID_FORMAT_DECODE_FAILED", errors.Join(err, errors.New("original SID decoded value unavailable")))
		return
	}
	o.view.DecodeSucceeded = true
	return
}
