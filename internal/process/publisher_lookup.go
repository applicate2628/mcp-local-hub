package process

import (
	"context"
	"encoding/json"
	"errors"
)

type PublisherQuiescenceSnapshotV1 struct {
	Schema            string               `json:"schema"`
	Version           uint32               `json:"version"`
	Disposition       string               `json:"disposition"`
	NativeWait        *uint32              `json:"native_wait"`
	NativeStatus      *uint32              `json:"native_status"`
	ResourcesComplete bool                 `json:"resources_complete"`
	Complete          bool                 `json:"complete"`
	PrimaryError      *SIDFormatFailureV1  `json:"primary_error"`
	CleanupErrors     []SIDFormatFailureV1 `json:"cleanup_errors"`
}

type PublisherQuiescenceObservation struct {
	issuer                     *publisherIncarnationIssuer
	record                     PublisherIncarnationRecordV1
	binding                    PublisherIncarnationBinding
	view                       PublisherQuiescenceSnapshotV1
	missing                    *PublisherNativeMissingObservation
	primaryErr, cleanupErr     error
	finalized, finishedInBound bool
}

func (o *PublisherQuiescenceObservation) Snapshot() PublisherQuiescenceSnapshotV1 {
	if o == nil {
		return PublisherQuiescenceSnapshotV1{}
	}
	v := o.view
	if v.NativeWait != nil {
		x := *v.NativeWait
		v.NativeWait = &x
	}
	if v.NativeStatus != nil {
		x := *v.NativeStatus
		v.NativeStatus = &x
	}
	if v.PrimaryError != nil {
		x := *v.PrimaryError
		v.PrimaryError = &x
	}
	v.CleanupErrors = append([]SIDFormatFailureV1{}, v.CleanupErrors...)
	return v
}

func (o *PublisherQuiescenceObservation) Validate(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) error {
	if o == nil || o.issuer == nil || ctx == nil || !publisherRecordValid(record, binding) || o.record != record || o.binding != binding || !o.finalized || !o.finishedInBound || !o.view.Complete || !o.view.ResourcesComplete {
		if o != nil {
			return errors.Join(errors.New("original closed publisher lookup required"), o.primaryErr, o.cleanupErr)
		}
		return errors.New("original closed publisher lookup required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	switch o.view.Disposition {
	case "original-signaled", "pid-generation-replaced":
	case "qualified-invalid-cid":
		if err := o.missing.Validate(ctx, record, binding); err != nil {
			return err
		}
	default:
		return errors.New("publisher is not positively quiescent")
	}
	return errors.Join(o.primaryErr, o.cleanupErr)
}

type PublisherNativeMissingSnapshotV1 struct {
	Schema                   string               `json:"schema"`
	Version                  uint32               `json:"version"`
	OriginalBindingSHA256    string               `json:"original_binding_sha256"`
	ABISourceSHA256          string               `json:"abi_source_sha256"`
	Win32ErrorDefined        bool                 `json:"win32_error_defined"`
	Win32ErrorCode           *uint32              `json:"win32_error_code"`
	ControlStatusDefined     bool                 `json:"control_status_defined"`
	ControlStatus            *uint32              `json:"control_status"`
	ControlIdentityVerified  bool                 `json:"control_identity_verified"`
	ControlResourcesComplete bool                 `json:"control_resources_complete"`
	TargetStatusDefined      bool                 `json:"target_status_defined"`
	TargetStatus             *uint32              `json:"target_status"`
	TargetHandleDisposition  string               `json:"target_handle_disposition"`
	ResourcesComplete        bool                 `json:"resources_complete"`
	Complete                 bool                 `json:"complete"`
	PrimaryError             *SIDFormatFailureV1  `json:"primary_error"`
	CleanupErrors            []SIDFormatFailureV1 `json:"cleanup_errors"`
}

type PublisherNativeMissingObservation struct {
	issuer                     *publisherIncarnationIssuer
	record                     PublisherIncarnationRecordV1
	binding                    PublisherIncarnationBinding
	view                       PublisherNativeMissingSnapshotV1
	primaryErr, cleanupErr     error
	finalized, finishedInBound bool
}

func publisherLookupBindingDigest(record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) string {
	raw, _ := json.Marshal(struct {
		Record  PublisherIncarnationRecordV1 `json:"record"`
		Binding PublisherIncarnationBinding  `json:"binding"`
	}{record, binding})
	return sidFormatObservationDigest(raw)
}

func (o *PublisherNativeMissingObservation) Snapshot() PublisherNativeMissingSnapshotV1 {
	if o == nil {
		return PublisherNativeMissingSnapshotV1{}
	}
	v := o.view
	if v.Win32ErrorCode != nil {
		x := *v.Win32ErrorCode
		v.Win32ErrorCode = &x
	}
	if v.ControlStatus != nil {
		x := *v.ControlStatus
		v.ControlStatus = &x
	}
	if v.TargetStatus != nil {
		x := *v.TargetStatus
		v.TargetStatus = &x
	}
	if v.PrimaryError != nil {
		x := *v.PrimaryError
		v.PrimaryError = &x
	}
	v.CleanupErrors = append([]SIDFormatFailureV1{}, v.CleanupErrors...)
	return v
}

func (o *PublisherNativeMissingObservation) Validate(ctx context.Context, record PublisherIncarnationRecordV1, binding PublisherIncarnationBinding) error {
	if o == nil || o.issuer == nil || ctx == nil || !publisherRecordValid(record, binding) || o.record != record || o.binding != binding || !o.finalized || !o.finishedInBound || !o.view.Complete || !o.view.ResourcesComplete || !o.view.Win32ErrorDefined || o.view.Win32ErrorCode == nil || *o.view.Win32ErrorCode != 87 || !o.view.ControlResourcesComplete || !o.view.ControlIdentityVerified || !o.view.ControlStatusDefined || o.view.ControlStatus == nil || *o.view.ControlStatus != 0 || !o.view.TargetStatusDefined || o.view.TargetStatus == nil || *o.view.TargetStatus != 0xc000000b || o.view.TargetHandleDisposition != "no-handle" || o.view.OriginalBindingSHA256 != publisherLookupBindingDigest(record, binding) || !publisherDigestValid(o.view.ABISourceSHA256) {
		if o != nil {
			return errors.Join(errors.New("original paired native missing observation required"), o.primaryErr, o.cleanupErr)
		}
		return errors.New("original paired native missing observation required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.Join(o.primaryErr, o.cleanupErr)
}
