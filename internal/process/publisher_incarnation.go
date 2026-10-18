package process

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const PublisherIncarnationSchema = "serena-publisher-incarnation-v1"
const PublisherExclusiveLocalResources = "exclusive-local-non-inherited"

// The API publisher owns these declarations. Native capture echoes them and
// never claims operation, namespace, image-file or resource-scope ownership.
type PublisherIncarnationBinding struct {
	OwnerInstanceID string
	OperationID     string
	NamespaceSHA256 string
	SourceEpoch     string
	ResourceScope   string
}

type PublisherNativeIncarnationV1 struct {
	PID                   uint32 `json:"pid"`
	NativeBirthFILETIME   uint64 `json:"native_birth_filetime"`
	NativeImagePathSHA256 string `json:"native_image_path_sha256"`
	PrincipalSIDSHA256    string `json:"principal_sid_sha256"`
	NativeCaptureSHA256   string `json:"native_capture_sha256"`
}

type PublisherIncarnationRecordV1 struct {
	Schema                string `json:"schema"`
	Version               uint32 `json:"version"`
	OwnerInstanceID       string `json:"owner_instance_id"`
	PID                   uint32 `json:"pid"`
	NativeBirthFILETIME   uint64 `json:"native_birth_filetime"`
	NativeImagePathSHA256 string `json:"native_image_path_sha256"`
	ImageIdentitySHA256   string `json:"image_identity_sha256"`
	ImageSHA256           string `json:"image_sha256"`
	PrincipalSIDSHA256    string `json:"principal_sid_sha256"`
	SourceEpoch           string `json:"source_epoch"`
	OperationID           string `json:"operation_id"`
	NamespaceSHA256       string `json:"namespace_sha256"`
	ResourceScope         string `json:"resource_scope"`
	NativeCaptureSHA256   string `json:"native_capture_sha256"`
}

type publisherIncarnationIssuer struct{ marker byte }

// This state belongs only to the current-self native query. It declares no
// publisher namespace, operation, source epoch or publication resource scope.
type publisherNativeCaptureState struct {
	native PublisherNativeIncarnationV1
	imagePath string
	sidFormatting *SIDFormatObservation
	principal string
	queryClosed, finishedInBound, finalized bool
	primaryErr, cleanupErr error
	originalContext context.Context
	deadline time.Time
}

type publisherNativeObservationIssuer struct{ marker byte }

// PublisherNativeObservation is current-self diagnostic evidence only. Its
// private type is distinct from PublisherIncarnation and cannot be bound.
type PublisherNativeObservation struct {
	issuer *publisherNativeObservationIssuer
	state *publisherNativeCaptureState
}

func (o *PublisherNativeObservation) NativeSnapshot() PublisherNativeIncarnationV1 {
	if o == nil || o.issuer == nil || o.state == nil { return PublisherNativeIncarnationV1{} }
	return o.state.native
}

func (o *PublisherNativeObservation) NativeImagePathForQualification() string {
	if o == nil || o.issuer == nil || o.state == nil || !o.state.finalized || !o.state.finishedInBound || !o.state.queryClosed || o.state.primaryErr != nil || o.state.cleanupErr != nil { return "" }
	return o.state.imagePath
}

func (o *PublisherNativeObservation) CaptureErrors() (error,error) {
	if o == nil || o.issuer == nil || o.state == nil { return errors.New("original native self observation unavailable"),nil }
	return o.state.primaryErr,o.state.cleanupErr
}

func (o *PublisherNativeObservation) Validate(ctx context.Context) error {
	if o == nil || o.issuer == nil || o.state == nil || ctx == nil { return errors.New("original closed native self observation required") }
	s:=o.state
	if !s.finalized || !s.finishedInBound || !s.queryClosed || s.originalContext == nil || s.deadline.IsZero() || !time.Now().Before(s.deadline) || s.imagePath == "" || s.native.PID == 0 || s.native.NativeBirthFILETIME == 0 || !publisherDigestValid(s.native.NativeImagePathSHA256) || !publisherDigestValid(s.native.PrincipalSIDSHA256) || !publisherDigestValid(s.native.NativeCaptureSHA256) { return errors.New("original closed native self observation required") }
	deadline,finite:=ctx.Deadline()
	if !finite || deadline.After(s.deadline) { return errors.New("native self observation deadline differs") }
	if err:=errors.Join(s.originalContext.Err(),ctx.Err(),s.primaryErr,s.cleanupErr);err!=nil { return err }
	if err:=s.sidFormatting.Validate(s.principal);err!=nil { return err }
	expected,err:=publisherNativeDigest(s.native)
	if err!=nil || expected!=s.native.NativeCaptureSHA256 { return errors.Join(err,errors.New("native self observation digest differs")) }
	return nil
}

func (s *publisherNativeCaptureState) copyToIncarnation(p *PublisherIncarnation) {
	p.native=s.native;p.imagePath=s.imagePath;p.sidFormatting=s.sidFormatting;p.principal=s.principal
	p.queryClosed=s.queryClosed;p.finishedInBound=s.finishedInBound;p.finalized=s.finalized
	p.primaryErr=s.primaryErr;p.cleanupErr=s.cleanupErr
}

type PublisherIncarnation struct {
	issuer                                  *publisherIncarnationIssuer
	binding                                 PublisherIncarnationBinding
	native                                  PublisherNativeIncarnationV1
	imagePath                               string
	sidFormatting                           *SIDFormatObservation
	principal                               string
	queryClosed, finishedInBound, finalized bool
	primaryErr, cleanupErr                  error
}

func publisherDigestValid(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func publisherBindingValid(binding PublisherIncarnationBinding) bool {
	return binding.OwnerInstanceID != "" && strings.TrimSpace(binding.OwnerInstanceID) == binding.OwnerInstanceID && binding.OperationID != "" && strings.TrimSpace(binding.OperationID) == binding.OperationID && publisherDigestValid(binding.NamespaceSHA256) && binding.SourceEpoch != "" && strings.TrimSpace(binding.SourceEpoch) == binding.SourceEpoch && binding.ResourceScope == PublisherExclusiveLocalResources
}

func publisherNativeDigest(native PublisherNativeIncarnationV1) (string, error) {
	// Explicit canonical body excludes its own digest, so there is no cycle.
	body := struct {
		PID       uint32 `json:"pid"`
		Birth     uint64 `json:"native_birth_filetime"`
		ImagePath string `json:"native_image_path_sha256"`
		Principal string `json:"principal_sid_sha256"`
	}{native.PID, native.NativeBirthFILETIME, native.NativeImagePathSHA256, native.PrincipalSIDSHA256}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return sidFormatObservationDigest(raw), nil
}

func (p *PublisherIncarnation) NativeSnapshot() PublisherNativeIncarnationV1 {
	if p == nil || p.issuer == nil {
		return PublisherNativeIncarnationV1{}
	}
	return p.native
}

func (p *PublisherIncarnation) NativeImagePathForQualification() string {
	if p == nil || p.issuer == nil || !p.finalized || !p.finishedInBound || !p.queryClosed || p.primaryErr != nil || p.cleanupErr != nil {
		return ""
	}
	return p.imagePath
}

func (p *PublisherIncarnation) ValidateCapture(ctx context.Context, binding PublisherIncarnationBinding) error {
	if p == nil || p.issuer == nil || ctx == nil || !publisherBindingValid(binding) || p.binding != binding || !p.finalized || !p.finishedInBound || !p.queryClosed || p.imagePath == "" || p.native.PID == 0 || p.native.NativeBirthFILETIME == 0 || !publisherDigestValid(p.native.NativeImagePathSHA256) || !publisherDigestValid(p.native.PrincipalSIDSHA256) || !publisherDigestValid(p.native.NativeCaptureSHA256) {
		return errors.New("original closed publisher native capture required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.sidFormatting.Validate(p.principal); err != nil {
		return errors.Join(err, p.primaryErr, p.cleanupErr)
	}
	expected, err := publisherNativeDigest(p.native)
	if err != nil || expected != p.native.NativeCaptureSHA256 {
		return errors.Join(err, errors.New("publisher native capture digest differs"))
	}
	return errors.Join(p.primaryErr, p.cleanupErr)
}

func publisherRecordValid(record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) bool {
	if !publisherBindingValid(binding) || record.Schema != PublisherIncarnationSchema || record.Version != 1 || record.OwnerInstanceID != binding.OwnerInstanceID || record.OperationID != binding.OperationID || record.NamespaceSHA256 != binding.NamespaceSHA256 || record.SourceEpoch != binding.SourceEpoch || record.ResourceScope != binding.ResourceScope || record.PID == 0 || record.NativeBirthFILETIME == 0 {
		return false
	}
	for _, digest := range []string{record.NativeImagePathSHA256, record.ImageIdentitySHA256, record.ImageSHA256, record.PrincipalSIDSHA256, record.NativeCaptureSHA256} {
		if !publisherDigestValid(digest) {
			return false
		}
	}
	native := PublisherNativeIncarnationV1{PID: record.PID, NativeBirthFILETIME: record.NativeBirthFILETIME, NativeImagePathSHA256: record.NativeImagePathSHA256, PrincipalSIDSHA256: record.PrincipalSIDSHA256}
	digest, err := publisherNativeDigest(native)
	return err == nil && digest == record.NativeCaptureSHA256
}

// Validate checks persisted data only. It issues no live/closed incarnation or
// namespace/resource authority; those require their original observations.
func (record PublisherIncarnationRecordV1) Validate(binding PublisherIncarnationBinding) error {
	if !publisherRecordValid(record, binding) {
		return errors.New("publisher incarnation record binding or native digest invalid")
	}
	return nil
}
