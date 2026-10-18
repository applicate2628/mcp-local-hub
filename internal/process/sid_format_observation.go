package process

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

type SIDFormatFailureV1 struct {
	Code string `json:"code"`
}

type SIDFormatSnapshotV1 struct {
	Schema              string              `json:"schema"`
	Version             uint32              `json:"version"`
	InputSIDSHA256      *string             `json:"input_sid_sha256"`
	ValueSHA256         *string             `json:"value_sha256"`
	ConversionAttempted bool                `json:"conversion_attempted"`
	ConversionSucceeded bool                `json:"conversion_succeeded"`
	AllocationAcquired  bool                `json:"allocation_acquired"`
	DecodeSucceeded     bool                `json:"decode_succeeded"`
	FreeAttempted       bool                `json:"free_attempted"`
	FreeDefined         bool                `json:"free_defined"`
	Freed               bool                `json:"freed"`
	Complete            bool                `json:"complete"`
	PrimaryError        *SIDFormatFailureV1 `json:"primary_error"`
	CleanupError        *SIDFormatFailureV1 `json:"cleanup_error"`
}

type sidFormatIssuer struct{ marker byte }

// Only the synchronous original conversion/free owner issues this receipt.
// There is no close/retry method or constructor from its diagnostic snapshot.
type SIDFormatObservation struct {
	issuer                 *sidFormatIssuer
	view                   SIDFormatSnapshotV1
	inputOwner             any
	inputRead              func() ([]byte, error)
	value                  string
	allocation             uintptr
	primaryErr, cleanupErr error
	finalized              bool
}

func sidFormatObservationDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (o *SIDFormatObservation) Snapshot() SIDFormatSnapshotV1 {
	if o == nil {
		return SIDFormatSnapshotV1{}
	}
	v := o.view
	if v.InputSIDSHA256 != nil {
		copied := *v.InputSIDSHA256
		v.InputSIDSHA256 = &copied
	}
	if v.ValueSHA256 != nil {
		copied := *v.ValueSHA256
		v.ValueSHA256 = &copied
	}
	if v.PrimaryError != nil {
		copied := *v.PrimaryError
		v.PrimaryError = &copied
	}
	if v.CleanupError != nil {
		copied := *v.CleanupError
		v.CleanupError = &copied
	}
	return v
}

func (o *SIDFormatObservation) Validate(value string) error {
	if o == nil || o.issuer == nil {
		return errors.New("original complete SID formatting observation required")
	}
	if !o.finalized || !o.view.Complete || !o.view.ConversionSucceeded || !o.view.AllocationAcquired || !o.view.DecodeSucceeded || !o.view.FreeAttempted || !o.view.FreeDefined || !o.view.Freed || o.view.InputSIDSHA256 == nil || o.view.ValueSHA256 == nil || o.value == "" || value != o.value || sidFormatObservationDigest([]byte(value)) != *o.view.ValueSHA256 {
		return errors.Join(errors.New("original complete SID formatting observation required"), o.primaryErr, o.cleanupErr)
	}
	return errors.Join(o.primaryErr, o.cleanupErr)
}
