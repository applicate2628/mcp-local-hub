package api

import (
	"context"
	"debug/pe"
	"encoding/json"
	"errors"
	"runtime"

	"mcp-local-hub/internal/binaryadmission"
	"mcp-local-hub/internal/buildinfo"
	"mcp-local-hub/internal/process"
)

type PublisherImageSnapshotV1 struct {
	SourceEpoch         string `json:"source_epoch"`
	ImageIdentitySHA256 string `json:"image_identity_sha256"`
	ImageSHA256         string `json:"image_sha256"`
	Version             string `json:"version"`
	Commit              string `json:"commit"`
	BuildDate           string `json:"build_date"`
	Machine             uint16 `json:"machine"`
	Subsystem           uint16 `json:"subsystem"`
	ResourcesClosed     bool   `json:"resources_closed"`
}

// Only CapturePublisherImageObserved issues this observation. Expected epoch,
// parsed PE metadata and a physical digest separately cannot construct it.
type PublisherImageObservation struct {
	capture         *process.PublisherIncarnation
	source          *buildinfo.PublisherSourceObservation
	file            PreparedSerenaFile
	view            PublisherImageSnapshotV1
	finishedInBound bool
}

type publisherImageQualification struct {
	source *buildinfo.PublisherSourceObservation
	file PreparedSerenaFile
	view PublisherImageSnapshotV1
	finishedInBound bool
}

func CapturePublisherImageObserved(ctx context.Context, capture *process.PublisherIncarnation, expectedSourceEpoch string) (*PublisherImageObservation, error) {
	if ctx == nil || capture == nil || !preparedSerenaDigest(expectedSourceEpoch) {
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_INPUT_INVALID")
	}
	qualified, err := qualifyPublisherImage(ctx, capture.NativeImagePathForQualification(), capture.NativeSnapshot(), expectedSourceEpoch)
	if err != nil { return nil, err }
	return &PublisherImageObservation{capture: capture, source: qualified.source, file: qualified.file, view: qualified.view, finishedInBound: qualified.finishedInBound}, nil
}

// CurrentPublisherImageObservation is read-only image evidence from its
// original native self observation. BindPublisherIncarnation cannot accept it.
type CurrentPublisherImageObservation struct {
	native *process.PublisherNativeObservation
	qualification *publisherImageQualification
}

func NewCurrentPublisherImageObservation(ctx context.Context, native *process.PublisherNativeObservation) (*CurrentPublisherImageObservation, error) {
	if err := native.Validate(ctx); err != nil { return nil, err }
	qualified, err := qualifyPublisherImage(ctx, native.NativeImagePathForQualification(), native.NativeSnapshot(), "")
	if err != nil { return nil, err }
	return &CurrentPublisherImageObservation{native: native, qualification: qualified}, nil
}

func (o *CurrentPublisherImageObservation) Snapshot() PublisherImageSnapshotV1 {
	if o == nil || o.qualification == nil { return PublisherImageSnapshotV1{} }
	return o.qualification.view
}

func (o *CurrentPublisherImageObservation) Validate(ctx context.Context, native *process.PublisherNativeObservation) error {
	if o == nil || o.qualification == nil || native == nil || native != o.native {
		return errors.New("SERENA_PUBLISHER_IMAGE_OBSERVATION_UNAVAILABLE")
	}
	if err := native.Validate(ctx); err != nil { return err }
	q := o.qualification
	return validatePublisherImageQualification(ctx, q.source, q.file, q.view, q.finishedInBound, native.NativeImagePathForQualification(), q.view.SourceEpoch)
}

func qualifyPublisherImage(ctx context.Context, path string, native process.PublisherNativeIncarnationV1, expectedSourceEpoch string) (*publisherImageQualification, error) {
	if _, finite := ctx.Deadline(); !finite {
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_DEADLINE_MISSING")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source, err := buildinfo.CurrentPublisherSource()
	if err != nil {
		return nil, err
	}
	if expectedSourceEpoch == "" { expectedSourceEpoch = source.Epoch() }
	if err = source.Validate(expectedSourceEpoch); err != nil {
		return nil, err
	}
	if path == "" || preparedSerenaHash([]byte(path)) != native.NativeImagePathSHA256 {
		return nil, errors.New("SERENA_PUBLISHER_NATIVE_IMAGE_UNQUALIFIED")
	}
	file, raw, err := qualifyPreparedSerenaFile(ctx, path, 64<<20)
	if err != nil {
		return nil, err
	}
	parsed, err := binaryadmission.ParseWindowsPublisherSourceIdentity(raw)
	if err != nil {
		return nil, err
	}
	version, commit, date := buildinfo.Get()
	if parsed.SourceEpoch != source.Epoch() || parsed.SourceEpoch != expectedSourceEpoch || parsed.Version != version || parsed.Commit != commit || parsed.BuildDate != date || parsed.Role != binaryadmission.WindowsArtifactRoleProduct || !preparedPublisherRuntimeMachine(parsed.Machine) {
		return nil, errors.New("SERENA_PUBLISHER_IMAGE_SOURCE_DRIFT")
	}
	identity, err := json.Marshal(struct {
		Schema string `json:"schema"`
		Volume string `json:"volume_id"`
		FileID string `json:"file_id128"`
	}{"publisher-image-file-v1", file.Device, file.FileID})
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return &publisherImageQualification{source: source, file: file, finishedInBound: true, view: PublisherImageSnapshotV1{SourceEpoch: source.Epoch(), ImageIdentitySHA256: preparedSerenaHash(identity), ImageSHA256: file.SHA256, Version: version, Commit: commit, BuildDate: date, Machine: parsed.Machine, Subsystem: parsed.Subsystem, ResourcesClosed: true}}, nil
}

func preparedPublisherRuntimeMachine(machine uint16) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	// These are PE protocol values compared with this compiled runtime's ABI,
	// not a global machine allowlist in the shared byte parser.
	switch runtime.GOARCH {
	case "amd64":
		return machine == pe.IMAGE_FILE_MACHINE_AMD64
	case "arm64":
		return machine == pe.IMAGE_FILE_MACHINE_ARM64
	case "386":
		return machine == pe.IMAGE_FILE_MACHINE_I386
	}
	return false
}

func (o *PublisherImageObservation) Snapshot() PublisherImageSnapshotV1 {
	if o == nil {
		return PublisherImageSnapshotV1{}
	}
	return o.view
}

func (o *PublisherImageObservation) Validate(ctx context.Context, capture *process.PublisherIncarnation, expectedSourceEpoch string) error {
	if o == nil || capture == nil || capture != o.capture {
		return errors.New("SERENA_PUBLISHER_IMAGE_OBSERVATION_UNAVAILABLE")
	}
	return validatePublisherImageQualification(ctx, o.source, o.file, o.view, o.finishedInBound, capture.NativeImagePathForQualification(), expectedSourceEpoch)
}

func validatePublisherImageQualification(ctx context.Context, source *buildinfo.PublisherSourceObservation, file PreparedSerenaFile, view PublisherImageSnapshotV1, finishedInBound bool, path string, expectedSourceEpoch string) error {
	if ctx == nil || source == nil || !finishedInBound || !view.ResourcesClosed || !preparedSerenaFile(file) || view.ImageSHA256 != file.SHA256 || !preparedSerenaDigest(view.ImageIdentitySHA256) || view.SourceEpoch != expectedSourceEpoch || path != file.Path {
		return errors.New("SERENA_PUBLISHER_IMAGE_OBSERVATION_UNAVAILABLE")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := source.Validate(expectedSourceEpoch); err != nil {
		return err
	}
	version, commit, date := buildinfo.Get()
	if version != view.Version || commit != view.Commit || date != view.BuildDate {
		return errors.New("SERENA_PUBLISHER_RUNTIME_TUPLE_DRIFT")
	}
	return revalidatePreparedSerenaFile(ctx, file)
}

func BindPublisherIncarnation(ctx context.Context, binding process.PublisherIncarnationBinding, capture *process.PublisherIncarnation, image *PublisherImageObservation, namespace *SerenaRecordNamespaceObservation) (process.PublisherIncarnationRecordV1, error) {
	if namespace == nil || namespace.io.operationID != binding.OperationID || namespace.io.sourceSHA256 != binding.SourceEpoch {
		return process.PublisherIncarnationRecordV1{}, errors.New("SERENA_PUBLISHER_NAMESPACE_UNQUALIFIED")
	}
	common, err := namespace.NamespaceSHA256(ctx)
	if err != nil || common != binding.NamespaceSHA256 {
		return process.PublisherIncarnationRecordV1{}, errors.Join(err, errors.New("SERENA_PUBLISHER_NAMESPACE_DRIFT"))
	}
	if err := capture.ValidateCapture(ctx, binding); err != nil {
		return process.PublisherIncarnationRecordV1{}, err
	}
	if err := image.Validate(ctx, capture, binding.SourceEpoch); err != nil {
		return process.PublisherIncarnationRecordV1{}, err
	}
	n := capture.NativeSnapshot()
	i := image.Snapshot()
	return process.PublisherIncarnationRecordV1{Schema: process.PublisherIncarnationSchema, Version: 1, OwnerInstanceID: binding.OwnerInstanceID, PID: n.PID, NativeBirthFILETIME: n.NativeBirthFILETIME, NativeImagePathSHA256: n.NativeImagePathSHA256, ImageIdentitySHA256: i.ImageIdentitySHA256, ImageSHA256: i.ImageSHA256, PrincipalSIDSHA256: n.PrincipalSIDSHA256, SourceEpoch: i.SourceEpoch, OperationID: binding.OperationID, NamespaceSHA256: binding.NamespaceSHA256, ResourceScope: binding.ResourceScope, NativeCaptureSHA256: n.NativeCaptureSHA256}, nil
}
