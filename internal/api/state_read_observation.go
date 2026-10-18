package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/process"
)

const StateFileReadObservationSchema = "state-file-read-observation-v1"

type StateFileReadAbsence string

const (
	StateFileReadParentNotFound StateFileReadAbsence = "parent-not-found"
	StateFileReadLeafNotFound   StateFileReadAbsence = "leaf-not-found"
)

type StateFileReadFailureV1 struct {
	Code  string `json:"code"`
	Stage string `json:"stage"`
	Slot  string `json:"slot"`
}

type StateFileReadResourceSnapshotV1 struct {
	Slot               string  `json:"slot"`
	Ownership          string  `json:"ownership"`
	Acquired           bool    `json:"acquired"`
	IdentityDefined    bool    `json:"identity_defined"`
	IdentitySHA256     *string `json:"identity_sha256"`
	CloseAttempted     bool    `json:"close_attempted"`
	CloseResultDefined bool    `json:"close_result_defined"`
	Closed             bool    `json:"closed"`
	NativeErrorDefined bool    `json:"native_error_defined"`
	NativeErrorKind    *string `json:"native_error_kind"`
	NativeErrorCode    *uint64 `json:"native_error_code"`
}

type StateFileReadSnapshotV1 struct {
	Schema                    string                            `json:"schema"`
	Version                   uint32                            `json:"version"`
	Stage                     string                            `json:"stage"`
	ReadAttempted             bool                              `json:"read_attempted"`
	BytesDefined              bool                              `json:"bytes_defined"`
	BytesRead                 *int64                            `json:"bytes_read"`
	ContentSHA256             *string                           `json:"content_sha256"`
	PathIdentitySHA256        string                            `json:"path_identity_sha256"`
	ParentIdentityDefined     bool                              `json:"parent_identity_defined"`
	ParentIdentitySHA256      *string                           `json:"parent_identity_sha256"`
	LeafIdentityDefined       bool                              `json:"leaf_identity_defined"`
	LeafIdentitySHA256        *string                           `json:"leaf_identity_sha256"`
	PolicyQualified           bool                              `json:"policy_qualified"`
	Absence                   string                            `json:"absence"`
	Resources                 []StateFileReadResourceSnapshotV1 `json:"resources"`
	BorrowedResourcesComplete bool                              `json:"borrowed_resources_complete"`
	OwnedResourcesComplete    bool                              `json:"owned_resources_complete"`
	Complete                  bool                              `json:"complete"`
	PrimaryError              *StateFileReadFailureV1           `json:"primary_error"`
	CleanupErrors             []StateFileReadFailureV1          `json:"cleanup_errors"`
}

type stateFileReadIssuer struct{ marker byte }
type stateFileReadResource struct {
	view          StateFileReadResourceSnapshotV1
	close         func() error
	closeErr      error
	sidFormatting *process.SIDFormatObservation
}

// Only the actual anchored-reader finalizer issues this capability. Its
// snapshot is data and has no constructor that can reconstruct authority.
type StateFileReadObservation struct {
	issuer           *stateFileReadIssuer
	view             StateFileReadSnapshotV1
	path             string
	maxBytes         int64
	deadline         time.Time
	deadlineDefined  bool
	finalized        bool
	resources        []*stateFileReadResource
	absenceQualified bool
}

type stateFileReadBuilder struct {
	leafCapture   func(uintptr, []byte) error
	issuer        *stateFileReadIssuer
	ctx           context.Context
	path          string
	maxBytes      int64
	view          StateFileReadSnapshotV1
	resources     []*stateFileReadResource
	borrowed      []*StateFileReadObservation
	borrowedErr   error
	policy        *StateReadPolicyObservation
	absenceResult error
	absenceNative error
}

// The platform core calls this only after its existing native missing-result
// classifier succeeds. Preserve the exact returned primary object separately
// from its native cause; joined policy/close/cancel errors cannot reuse it.
func (b *stateFileReadBuilder) recordAbsence(kind StateFileReadAbsence, native, returned error) {
	b.view.Absence = string(kind)
	b.absenceNative = native
	b.absenceResult = returned
}

func stateFileReadObservationDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func stateReadRequestPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("state read path required")
	}
	return filepath.Abs(filepath.Clean(path))
}

func newStateFileReadBuilder(ctx context.Context, path string, maxBytes int64) *stateFileReadBuilder {
	b := &stateFileReadBuilder{issuer: &stateFileReadIssuer{marker: 1}, ctx: ctx, path: path, maxBytes: maxBytes, view: StateFileReadSnapshotV1{Schema: StateFileReadObservationSchema, Version: 1, Stage: "input", Absence: "none", Resources: make([]StateFileReadResourceSnapshotV1, 0), CleanupErrors: make([]StateFileReadFailureV1, 0)}}
	for _, slot := range []string{"parent", "leaf"} {
		b.resources = append(b.resources, &stateFileReadResource{view: StateFileReadResourceSnapshotV1{Slot: slot, Ownership: "not-acquired"}})
	}
	if resolved, err := stateReadRequestPath(path); err == nil {
		b.path = resolved
		b.view.PathIdentitySHA256 = stateFileReadObservationDigest([]byte(resolved))
	}
	return b
}

func (b *stateFileReadBuilder) checkContext() error {
	if b.ctx == nil {
		return errors.New("original observed read context required")
	}
	return b.ctx.Err()
}

func (b *stateFileReadBuilder) resource(slot string) *stateFileReadResource {
	for _, resource := range b.resources {
		if resource.view.Slot == slot {
			return resource
		}
	}
	return nil
}

func (b *stateFileReadBuilder) acquire(slot string, close func() error) {
	resource := b.resource(slot)
	resource.view.Ownership = "owned"
	resource.view.Acquired = true
	resource.close = close
}

func (b *stateFileReadBuilder) identity(slot, digest string) {
	resource := b.resource(slot)
	resource.view.IdentityDefined = true
	resource.view.IdentitySHA256 = &digest
	if slot == "parent" {
		b.view.ParentIdentityDefined = true
		b.view.ParentIdentitySHA256 = &digest
	}
	if slot == "leaf" {
		b.view.LeafIdentityDefined = true
		b.view.LeafIdentitySHA256 = &digest
	}
}

func (b *stateFileReadBuilder) closeResource(slot string) error {
	resource := b.resource(slot)
	if !resource.view.Acquired || resource.view.CloseAttempted {
		return resource.closeErr
	}
	resource.view.CloseAttempted = true
	if resource.close == nil {
		resource.closeErr = errors.New("actual acquired reader closer unavailable")
	} else {
		resource.closeErr = resource.close()
	}
	resource.view.CloseResultDefined = resource.close != nil
	resource.view.Closed = resource.view.CloseResultDefined && resource.closeErr == nil
	if resource.closeErr != nil {
		kind, code := stateReadNativeError(resource.closeErr)
		resource.view.NativeErrorDefined = true
		resource.view.NativeErrorKind = &kind
		resource.view.NativeErrorCode = code
		b.view.CleanupErrors = append(b.view.CleanupErrors, StateFileReadFailureV1{Code: "RESOURCE_CLOSE_FAILED", Stage: "close", Slot: slot})
	}
	return resource.closeErr
}

func (b *stateFileReadBuilder) captureBytes(raw []byte) {
	count := int64(len(raw))
	digest := stateFileReadObservationDigest(raw)
	b.view.BytesDefined = true
	b.view.BytesRead = &count
	b.view.ContentSHA256 = &digest
}

func (b *stateFileReadBuilder) finish(raw []byte, primary error) ([]byte, *StateFileReadObservation, error) {
	if raw != nil && !b.view.BytesDefined {
		b.captureBytes(raw)
	}
	if primary != nil {
		b.view.PrimaryError = &StateFileReadFailureV1{Code: "STATE_READ_FAILED", Stage: b.view.Stage, Slot: ""}
	}
	var cleanup []error
	b.view.OwnedResourcesComplete = true
	for _, resource := range b.resources {
		if resource.view.Acquired && !resource.view.CloseAttempted {
			b.closeResource(resource.view.Slot)
		}
		if resource.view.Acquired && (!resource.view.CloseResultDefined || !resource.view.Closed) {
			b.view.OwnedResourcesComplete = false
		}
		cleanup = append(cleanup, resource.closeErr)
		b.view.Resources = append(b.view.Resources, resource.view)
	}
	borrowedErr := b.borrowedErr
	if b.policy != nil {
		borrowedErr = errors.Join(borrowedErr, b.policy.Validate(b.ctx, b.policy.view.EnvIdentitySHA256))
	}
	b.view.BorrowedResourcesComplete = borrowedErr == nil
	for _, borrowed := range b.borrowed {
		if borrowed == nil || !borrowed.finalized || !borrowed.view.OwnedResourcesComplete || !borrowed.view.BorrowedResourcesComplete {
			b.view.BorrowedResourcesComplete = false
		}
	}
	contextErr := b.checkContext()
	err := errors.Join(primary, errors.Join(cleanup...), borrowedErr, contextErr)
	if contextErr != nil && b.view.PrimaryError == nil {
		b.view.PrimaryError = &StateFileReadFailureV1{Code: "READ_CONTEXT_FAILED", Stage: b.view.Stage, Slot: ""}
	}
	b.view.Complete = err == nil && b.view.ReadAttempted && b.view.BytesDefined && b.view.PolicyQualified && b.view.ParentIdentityDefined && b.view.LeafIdentityDefined && b.view.OwnedResourcesComplete && b.view.BorrowedResourcesComplete && b.view.Absence == "none"
	if b.view.Complete {
		b.view.Stage = "complete"
	}
	absenceQualified := b.absenceResult != nil && b.absenceNative != nil && primary == b.absenceResult && b.view.ReadAttempted && b.view.PolicyQualified && b.view.OwnedResourcesComplete && b.view.BorrowedResourcesComplete && borrowedErr == nil && contextErr == nil && errors.Join(cleanup...) == nil
	observation := &StateFileReadObservation{issuer: b.issuer, view: copyStateFileReadSnapshot(b.view), path: b.path, maxBytes: b.maxBytes, finalized: true, resources: append([]*stateFileReadResource(nil), b.resources...), absenceQualified: absenceQualified}
	if b.ctx != nil {
		observation.deadline, observation.deadlineDefined = b.ctx.Deadline()
	}
	if err != nil {
		zeroStateSecretBytes(raw)
		raw = nil
	}
	return raw, observation, err
}

func copyStateFileReadSnapshot(view StateFileReadSnapshotV1) StateFileReadSnapshotV1 {
	copyString := func(value *string) *string {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	view.ContentSHA256 = copyString(view.ContentSHA256)
	view.ParentIdentitySHA256 = copyString(view.ParentIdentitySHA256)
	view.LeafIdentitySHA256 = copyString(view.LeafIdentitySHA256)
	if view.BytesRead != nil {
		count := *view.BytesRead
		view.BytesRead = &count
	}
	if view.PrimaryError != nil {
		failure := *view.PrimaryError
		view.PrimaryError = &failure
	}
	view.CleanupErrors = append(make([]StateFileReadFailureV1, 0, len(view.CleanupErrors)), view.CleanupErrors...)
	view.Resources = append(make([]StateFileReadResourceSnapshotV1, 0, len(view.Resources)), view.Resources...)
	for index := range view.Resources {
		row := &view.Resources[index]
		row.IdentitySHA256 = copyString(row.IdentitySHA256)
		row.NativeErrorKind = copyString(row.NativeErrorKind)
		if row.NativeErrorCode != nil {
			code := *row.NativeErrorCode
			row.NativeErrorCode = &code
		}
	}
	return view
}

func (o *StateFileReadObservation) Snapshot() StateFileReadSnapshotV1 {
	if o == nil {
		return StateFileReadSnapshotV1{}
	}
	return copyStateFileReadSnapshot(o.view)
}

func (o *StateFileReadObservation) Validate(ctx context.Context, path string, maxBytes int64, raw []byte, expectedSHA256 string) error {
	if o == nil || o.issuer == nil || !o.finalized || !o.view.Complete || ctx == nil {
		return errors.New("original complete reader observation required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, err := stateReadRequestPath(path)
	if err != nil {
		return err
	}
	if resolved != o.path || maxBytes != o.maxBytes || maxBytes <= 0 || raw == nil || o.view.ContentSHA256 == nil || o.view.BytesRead == nil || int64(len(raw)) != *o.view.BytesRead || int64(len(raw)) > maxBytes || expectedSHA256 == "" || expectedSHA256 != *o.view.ContentSHA256 || stateFileReadObservationDigest(raw) != expectedSHA256 {
		return errors.New("original reader request/content/digest binding differs")
	}
	if o.deadlineDefined {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(o.deadline) {
			return errors.New("reader receiving deadline renewed")
		}
	}
	return nil
}

// ValidateAbsence qualifies only the captured original missing result. It does
// not qualify an owned receiving namespace, content, writer, or lease.
func (o *StateFileReadObservation) ValidateAbsence(ctx context.Context, path string, expected StateFileReadAbsence) error {
	if o == nil || o.issuer == nil || !o.finalized || !o.absenceQualified || ctx == nil || (expected != StateFileReadParentNotFound && expected != StateFileReadLeafNotFound) || o.view.Absence != string(expected) || o.maxBytes <= 0 {
		return errors.New("original qualified state read absence required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	resolved, err := stateReadRequestPath(path)
	if err != nil {
		return err
	}
	if resolved != o.path {
		return errors.New("original missing state path differs")
	}
	if o.deadlineDefined {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(o.deadline) {
			return errors.New("absence receiving deadline renewed")
		}
	}
	return nil
}

func ReadStateFileInodeAnchoredObserved(ctx context.Context, path string, maxBytes int64) ([]byte, *StateFileReadObservation, error) {
	return readStateFileInodeAnchoredObservedWithLeafCapture(ctx, path, maxBytes, nil)
}

func readStateFileInodeAnchoredObservedWithLeafCapture(ctx context.Context, path string, maxBytes int64, capture func(uintptr, []byte) error) ([]byte, *StateFileReadObservation, error) {
	b := newStateFileReadBuilder(ctx, path, maxBytes)
	b.leafCapture = capture
	if err := b.checkContext(); err != nil {
		return b.finish(nil, err)
	}
	maximum := int64(int(^uint(0) >> 1))
	if b.view.PathIdentitySHA256 == "" || maxBytes <= 0 || maxBytes > maximum-4096 {
		return b.finish(nil, errors.New("positive non-overflowing observed read path/cap required"))
	}
	policy, policyErr := ResolveStateReadPolicyObserved(ctx)
	b.policy = policy
	if policy == nil {
		return b.finish(nil, errors.Join(policyErr, errors.New("original observed state policy unavailable")))
	}
	if policyErr != nil {
		return b.finish(nil, policyErr)
	}
	if err := policy.Validate(ctx, policy.view.EnvIdentitySHA256); err != nil {
		return b.finish(nil, err)
	}
	if policy.bootstrap != nil {
		b.borrowed = append(b.borrowed, policy.bootstrap)
	}
	b.view.PolicyQualified = true
	b.view.ReadAttempted = true
	effectiveStrict := policy.view.EffectiveStrict
	raw, err := readStateFileInodeAnchoredWithObservation(b.path, func() bool { return effectiveStrict }, maxBytes, true, false, LogHubMcpEvent, stateReadDefaultParentFallbacksObserved, ctx, b)
	return b.finish(raw, err)
}

func observedStateReadBoundary(ctx context.Context, b *stateFileReadBuilder, stage string) error {
	if b == nil {
		return nil
	}
	b.view.Stage = stage
	if ctx == nil {
		return errors.New("original state read context required")
	}
	return ctx.Err()
}
