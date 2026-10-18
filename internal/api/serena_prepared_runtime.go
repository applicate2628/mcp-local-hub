package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

const PreparedSerenaRecordSchema = "serena-prepared-runtime-v2"
const PreparedSerenaBindingSchema = "serena-prepared-runtime-ref-v1"

// BuildPolicy is produced by the existing configuration/environment owner.
// Secrets and per-workspace arguments are deliberately absent from identity.
type PreparedSerenaRequest struct {
	OperationID                     string             `json:"operation_id"`
	GenerationID                    string             `json:"generation_id"`
	Namespace                       string             `json:"namespace"`
	ManifestSHA256                  string             `json:"-"` // private exact source/CAS audit; excluded from package identity
	PackageManifestProjectionSHA256 string             `json:"package_manifest_projection_sha256"`
	BuildEnvironmentSHA256          string             `json:"build_environment_sha256"`
	SourceRequirement               string             `json:"source_requirement"`
	SourceCommit                    string             `json:"source_commit"`
	Package                         string             `json:"package"`
	UV                              PreparedSerenaFile `json:"uv"`
	Interpreter                     PreparedSerenaFile `json:"interpreter"`
	Launcher                        PreparedSerenaFile `json:"launcher"`
	PlatformABISHA256               string             `json:"platform_abi_sha256"`
	PolicySHA256                    string             `json:"policy_sha256"`
	BuildSettingsSHA256             string             `json:"build_settings_sha256"`
	ExpectedPriorRecordSHA256       string             `json:"expected_prior_record_sha256"`
	WorkLimit                       time.Duration      `json:"work_limit_ns"`
	CleanupLimit                    time.Duration      `json:"cleanup_limit_ns"`
	WorkDeadline                    time.Time          `json:"work_deadline_utc"`
	AbsoluteCleanupDeadline         time.Time          `json:"cleanup_deadline_utc"`
	OwnedTreeLimits                 SerenaTreeLimits   `json:"owned_tree_limits"`
}
type PreparedSerenaFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Device string `json:"device"`
	FileID string `json:"file_id"`
}
type PreparedSerenaRuntimeBinding struct {
	Schema                string `json:"schema"`
	RequestIdentitySHA256 string `json:"request_identity_sha256"`
	GenerationID          string `json:"generation_id"`
	RecordSHA256          string `json:"record_sha256"`
	SourceSHA256          string `json:"source_sha256"`
	InterpreterSHA256     string `json:"interpreter_sha256"`
	EntrypointSHA256      string `json:"entrypoint_sha256"`
	ResolutionSHA256      string `json:"resolution_sha256"`
}
type PreparedSerenaRuntimeRecord struct {
	Schema                     string             `json:"schema"`
	OperationID                string             `json:"operation_id"`
	GenerationID               string             `json:"generation_id"`
	RequestIdentitySHA256      string             `json:"request_identity_sha256"`
	NamespaceSHA256            string             `json:"namespace_sha256"`
	NamespaceSchema            string             `json:"namespace_schema"`
	LeafParentSHA256           string             `json:"leaf_parent_sha256"`
	SourceSHA256               string             `json:"source_sha256"`
	UVSHA256                   string             `json:"uv_sha256"`
	InterpreterSHA256          string             `json:"interpreter_sha256"`
	PlatformABISHA256          string             `json:"platform_abi_sha256"`
	PolicySHA256               string             `json:"policy_sha256"`
	ResolutionSHA256           string             `json:"resolution_sha256"`
	EnvironmentInventorySHA256 string             `json:"environment_inventory_sha256"`
	EntrypointIdentity         PreparedSerenaFile `json:"entrypoint_identity"`
	QualificationReceiptSHA256 string             `json:"qualification_receipt_sha256"`
	CleanupReceiptSHA256       string             `json:"cleanup_receipt_sha256"`
	Ready                      *bool              `json:"ready"`
	PublishedAtUTC             string             `json:"published_at_utc"`
}
type PreparedSerenaOwnedRoots struct {
	Generation PreparedSerenaFile
	Attempt    PreparedSerenaFile
}
type PreparedSerenaExecutionClosure struct {
	nativeEvidence                 preparedSerenaNativeEvidence
	OriginalOperationID            string
	RootExitKnown                  bool
	RootExitCode                   int
	MembersExited                  bool
	HandlesClosed                  bool
	IOClosed                       bool
	NoChildKnown                   bool
	NoChildReleased                bool
	Pending                        []string
	ReceiptSHA256                  string
	OriginalStartKnown             bool
	OriginalStartLive              bool
	ManagedGenerationReceiptSHA256 string
}

type preparedSerenaNativeEvidence interface{ Complete() bool }

func (c PreparedSerenaExecutionClosure) complete(id string) bool {
	return c.nativeEvidence != nil && c.nativeEvidence.Complete() && c.OriginalOperationID == id && c.HandlesClosed && c.IOClosed && len(c.Pending) == 0 && preparedSerenaDigest(c.ReceiptSHA256) && ((c.RootExitKnown && c.MembersExited) || (c.NoChildKnown && c.NoChildReleased))
}

type PreparedSerenaInstallation struct {
	Closure          PreparedSerenaExecutionClosure
	SourceSHA256     string
	ResolutionSHA256 string
	InventorySHA256  string
	Entrypoint       PreparedSerenaFile
	UVReceiptSHA256  string
}
type PreparedSerenaQualification struct {
	observation                     *PreparedSerenaQualificationObservation
	Closure                         PreparedSerenaExecutionClosure
	ReceiptSHA256                   string
	RuntimeProtocolQualified        bool
	IndependentOfAttemptRoots       bool
	FinalEnvironmentInventorySHA256 string
}
type PreparedSerenaCleanup struct {
	ReceiptSHA256      string
	AttemptAbsent      bool
	LinksDischarged    bool
	NoPendingResources bool
}
type PreparedSerenaConsumerReference struct {
	OperationID           string `json:"operation_id"`
	ConsumerID            string `json:"consumer_id"`
	RequestIdentitySHA256 string `json:"request_identity_sha256"`
	GenerationID          string `json:"generation_id"`
	ReadyRecordSHA256     string `json:"ready_record_sha256"`
	Disposition           string `json:"disposition"`
}
type PreparedSerenaReferenceSnapshot struct {
	Current                 []PreparedSerenaConsumerReference
	Rollback                []PreparedSerenaConsumerReference
	Pending                 []PreparedSerenaConsumerReference
	OriginalConsumersClosed bool
}

// This package-specific port is implemented by the canonical state/intent,
// original creator and uv receipt owners; tests use finite in-memory fakes.
// It grants no alternative writer, native retainer or unbounded installer.
type PreparedSerenaRuntimeEffects interface {
	LockPackage(context.Context, string) (func() error, error)
	LockIntentOperation(context.Context) (func() error, error)
	ReadReady(context.Context, string) ([]byte, bool, error)
	ReadyNamespace(context.Context, string) (*SerenaRecordNamespaceObservation, error)
	WriteReady(context.Context, string, []byte) error
	ValidateRecord(context.Context, PreparedSerenaRuntimeRecord) error
	PublicationAvailable(context.Context, PreparedSerenaRuntimeRecord) (bool, error)
	AcquireRoots(context.Context, PreparedSerenaRequest, string) (PreparedSerenaOwnedRoots, error)
	Install(context.Context, PreparedSerenaRequest, PreparedSerenaOwnedRoots) (PreparedSerenaInstallation, error)
	Qualify(context.Context, PreparedSerenaRequest, PreparedSerenaOwnedRoots, PreparedSerenaInstallation) (PreparedSerenaQualification, error)
	CleanupAttempt(context.Context, PreparedSerenaOwnedRoots, PreparedSerenaExecutionClosure) (PreparedSerenaCleanup, error)
	BeginOperation(context.Context, PreparedSerenaRequest, string) (PreparedSerenaOperationUpdateV2, error)
	PersistOperation(context.Context, PreparedSerenaOperationUpdateV2) (*StateFilePublicationObservation, error)
	FinishPreparation(context.Context, PreparedSerenaRequest, *StateFilePublicationObservation) (*PreparedSerenaPreparationCloseObservation, error)
	References(context.Context, string) (PreparedSerenaReferenceSnapshot, error)
	RetireGeneration(context.Context, PreparedSerenaRuntimeRecord, PreparedSerenaReferenceSnapshot) error
}
type PreparedSerenaRuntimeOwner struct {
	effects          PreparedSerenaRuntimeEffects
	consumer         *preparedSerenaConsumerCoordinator
	completedRecord  *PreparedSerenaRuntimeRecord
	completedBinding *PreparedSerenaRuntimeBinding
}

func NewPreparedSerenaRuntimeOwner(effects PreparedSerenaRuntimeEffects) (*PreparedSerenaRuntimeOwner, error) {
	if effects == nil {
		return nil, errors.New("SERENA_PREPARED_OWNER_UNAVAILABLE")
	}
	return &PreparedSerenaRuntimeOwner{effects: effects}, nil
}
func preparedSerenaDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == sha256.Size && s == strings.ToLower(s)
}
func preparedSerenaHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func preparedSerenaFile(f PreparedSerenaFile) bool {
	return filepath.IsAbs(f.Path) && filepath.Clean(f.Path) == f.Path && preparedSerenaDigest(f.SHA256) && f.Device != "" && f.FileID != ""
}
func preparedSerenaID(s string) bool {
	if s == "" || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
func (r PreparedSerenaRequest) Identity() (string, error) {
	if !r.OwnedTreeLimits.valid() {
		return "", errors.New("SERENA_PREPARED_TREE_LIMITS_INVALID")
	}
	if !preparedSerenaID(r.OperationID) || !preparedSerenaID(r.GenerationID) || !filepath.IsAbs(r.Namespace) || filepath.Clean(r.Namespace) != r.Namespace || r.Package != "serena-agent" || !preparedSerenaFile(r.UV) || !preparedSerenaFile(r.Interpreter) || !preparedSerenaFile(r.Launcher) || r.WorkLimit <= 0 || r.CleanupLimit <= 0 || r.WorkDeadline.IsZero() || r.AbsoluteCleanupDeadline.IsZero() || r.AbsoluteCleanupDeadline.After(r.WorkDeadline.Add(r.CleanupLimit)) || r.AbsoluteCleanupDeadline.Before(r.WorkDeadline) {
		return "", errors.New("SERENA_PREPARED_REQUEST_INVALID")
	}
	for _, d := range []string{r.ManifestSHA256, r.PackageManifestProjectionSHA256, r.BuildEnvironmentSHA256, r.PlatformABISHA256, r.PolicySHA256, r.BuildSettingsSHA256} {
		if !preparedSerenaDigest(d) {
			return "", errors.New("SERENA_PREPARED_IDENTITY_INVALID")
		}
	}
	if r.ExpectedPriorRecordSHA256 != "" && !preparedSerenaDigest(r.ExpectedPriorRecordSHA256) {
		return "", errors.New("SERENA_PREPARED_PRIOR_INVALID")
	}
	u, e := url.Parse(strings.TrimPrefix(r.SourceRequirement, "git+"))
	if e != nil || u.User != nil || u.Scheme != "https" || u.RawQuery != "" || len(r.SourceCommit) != 40 || !strings.HasSuffix(r.SourceRequirement, "@"+r.SourceCommit) {
		return "", errors.New("SERENA_PREPARED_SOURCE_INVALID")
	}
	if _, e = hex.DecodeString(r.SourceCommit); e != nil {
		return "", errors.New("SERENA_PREPARED_SOURCE_INVALID")
	}
	identity := struct{ ManifestProjection, EnvironmentProjection, Source, Commit, Package, UV, Interpreter, Launcher, ABI, Policy, Build string }{r.PackageManifestProjectionSHA256, r.BuildEnvironmentSHA256, r.SourceRequirement, r.SourceCommit, r.Package, r.UV.SHA256, r.Interpreter.SHA256, r.Launcher.SHA256, r.PlatformABISHA256, r.PolicySHA256, r.BuildSettingsSHA256}
	raw, e := json.Marshal(identity)
	if e != nil {
		return "", e
	}
	return preparedSerenaHash(raw), nil
}
func (r PreparedSerenaRuntimeRecord) Binding(raw []byte) (PreparedSerenaRuntimeBinding, error) {
	if r.Schema != PreparedSerenaRecordSchema || r.NamespaceSchema != SerenaPackageNamespaceSchema {
		return PreparedSerenaRuntimeBinding{}, errors.New("SERENA_PREPARED_NAMESPACE_SCHEMA_UNSUPPORTED")
	}
	if r.Schema != PreparedSerenaRecordSchema || r.NamespaceSchema != SerenaPackageNamespaceSchema || !preparedSerenaDigest(r.LeafParentSHA256) || !preparedSerenaID(r.OperationID) || r.Ready == nil || !*r.Ready || !preparedSerenaID(r.GenerationID) || !preparedSerenaFile(r.EntrypointIdentity) {
		return PreparedSerenaRuntimeBinding{}, errors.New("SERENA_PREPARED_RECORD_INVALID")
	}
	for _, d := range []string{r.RequestIdentitySHA256, r.NamespaceSHA256, r.SourceSHA256, r.UVSHA256, r.InterpreterSHA256, r.PlatformABISHA256, r.PolicySHA256, r.ResolutionSHA256, r.EnvironmentInventorySHA256, r.QualificationReceiptSHA256, r.CleanupReceiptSHA256} {
		if !preparedSerenaDigest(d) {
			return PreparedSerenaRuntimeBinding{}, errors.New("SERENA_PREPARED_RECORD_INVALID")
		}
	}
	if _, e := time.Parse(time.RFC3339Nano, r.PublishedAtUTC); e != nil {
		return PreparedSerenaRuntimeBinding{}, errors.New("SERENA_PREPARED_RECORD_INVALID")
	}
	return PreparedSerenaRuntimeBinding{PreparedSerenaBindingSchema, r.RequestIdentitySHA256, r.GenerationID, preparedSerenaHash(raw), r.SourceSHA256, r.InterpreterSHA256, r.EntrypointIdentity.SHA256, r.ResolutionSHA256}, nil
}
func decodePreparedSerenaRecord(raw []byte) (PreparedSerenaRuntimeRecord, error) {
	var schema struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return PreparedSerenaRuntimeRecord{}, err
	}
	if schema.Schema != PreparedSerenaRecordSchema {
		return PreparedSerenaRuntimeRecord{}, errors.New("SERENA_PREPARED_NAMESPACE_SCHEMA_UNSUPPORTED")
	}
	allowed := map[string]bool{"schema": true, "operation_id": true, "generation_id": true, "request_identity_sha256": true, "namespace_sha256": true, "namespace_schema": true, "leaf_parent_sha256": true, "source_sha256": true, "uv_sha256": true, "interpreter_sha256": true, "platform_abi_sha256": true, "policy_sha256": true, "resolution_sha256": true, "environment_inventory_sha256": true, "entrypoint_identity": true, "qualification_receipt_sha256": true, "cleanup_receipt_sha256": true, "ready": true, "published_at_utc": true}
	tokens := json.NewDecoder(bytes.NewReader(raw))
	first, e := tokens.Token()
	if e != nil || first != json.Delim('{') {
		return PreparedSerenaRuntimeRecord{}, errors.New("SERENA_PREPARED_RECORD_SHAPE")
	}
	seen := map[string]bool{}
	for tokens.More() {
		t, e := tokens.Token()
		if e != nil {
			return PreparedSerenaRuntimeRecord{}, e
		}
		key, ok := t.(string)
		if !ok || !allowed[key] || seen[key] {
			return PreparedSerenaRuntimeRecord{}, errors.New("SERENA_PREPARED_RECORD_FIELDS")
		}
		seen[key] = true
		var value json.RawMessage
		if e = tokens.Decode(&value); e != nil {
			return PreparedSerenaRuntimeRecord{}, e
		}
	}
	if _, e = tokens.Token(); e != nil {
		return PreparedSerenaRuntimeRecord{}, e
	}
	if len(seen) != len(allowed) {
		return PreparedSerenaRuntimeRecord{}, errors.New("SERENA_PREPARED_RECORD_FIELDS")
	}
	var r PreparedSerenaRuntimeRecord
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&r); e != nil {
		return r, e
	}
	_, e = r.Binding(raw)
	return r, e
}
func preparedSerenaRelease(err *error, release func() error) {
	if release == nil {
		*err = errors.Join(*err, errors.New("SERENA_PREPARED_LOCK_RELEASE_MISSING"))
		return
	}
	*err = errors.Join(*err, release())
}

// Prepare never takes the intent exclusion while holding a package lock.
func (o *PreparedSerenaRuntimeOwner) Prepare(ctx context.Context, r PreparedSerenaRequest) (binding PreparedSerenaRuntimeBinding, err error) {
	if o == nil || o.effects == nil {
		return binding, errors.New("SERENA_CONSUMER_ONLY_PREPARATION_UNAVAILABLE")
	}
	// Composition acquired local resources before Prepare. Even a malformed
	// request or nil work context must receive those original resources. Its
	// immutable acquisition bound, rather than caller data, owns that cleanup.
	cleanupEnd := r.AbsoluteCleanupDeadline
	if concrete, ok := o.effects.(*preparedSerenaRuntimeEffects); ok && concrete != nil {
		cleanupEnd = concrete.acquisition.inputs.Request.AbsoluteCleanupDeadline
	}
	cleanupParent := context.Background()
	if ctx != nil {
		cleanupParent = context.WithoutCancel(ctx)
	}
	cleanup, cleanupCancel := context.WithDeadline(cleanupParent, cleanupEnd)
	defer cleanupCancel()
	var lastOutcome *StateFilePublicationObservation
	var candidate PreparedSerenaRuntimeBinding
	var candidateRecord PreparedSerenaRuntimeRecord
	defer func() {
		proof, closeErr := o.effects.FinishPreparation(cleanup, r, lastOutcome)
		if closeErr == nil {
			closeErr = proof.Validate(cleanup, o.effects, r)
		}
		err = errors.Join(err, closeErr)
		binding = PreparedSerenaRuntimeBinding{}
		if err != nil || candidate.Schema == "" {
			return
		}
		if concrete, ok := o.effects.(*preparedSerenaRuntimeEffects); ok && candidateRecord.OperationID == r.OperationID {
			err = concrete.publication.ReceivePreparationClose(cleanup, r, proof)
			if err != nil {
				return
			}
		}
		available, availabilityErr := o.effects.PublicationAvailable(cleanup, candidateRecord)
		if availabilityErr != nil || !available {
			err = errors.Join(availabilityErr, errors.New("SERENA_PREPARED_PUBLICATION_PENDING"))
			return
		}
		binding = candidate
		recordCopy, bindingCopy := candidateRecord, candidate
		o.completedRecord, o.completedBinding = &recordCopy, &bindingCopy
	}()
	if ctx == nil {
		return binding, errors.New("SERENA_PREPARED_OWNER_CONTEXT_MISSING")
	}
	key, err := r.Identity()
	if err != nil {
		return binding, err
	}
	end := r.WorkDeadline
	if d, ok := ctx.Deadline(); ok && d.Before(end) {
		end = d
	}
	work, cancel := context.WithDeadline(ctx, end)
	defer cancel()
	if err = work.Err(); err != nil {
		return binding, err
	}
	release, err := o.effects.LockPackage(work, key)
	if err != nil {
		return binding, err
	}
	// The same Effects finalizer receives the observed package release.
	_ = release
	prior, exists, err := o.effects.ReadReady(work, key)
	if err != nil {
		return binding, err
	}
	var previous PreparedSerenaRuntimeRecord
	if exists {
		current, e := decodePreparedSerenaRecord(prior)
		if e != nil {
			return binding, e
		}
		if current.RequestIdentitySHA256 != key {
			return binding, errors.New("SERENA_PREPARED_REQUEST_DRIFT")
		}
		if e = o.effects.ValidateRecord(work, current); e != nil {
			return binding, e
		}
		previous = current
		if r.ExpectedPriorRecordSHA256 == "" {
			candidate, e = current.Binding(prior)
			candidateRecord = current
			return binding, e
		}
		if preparedSerenaHash(prior) != r.ExpectedPriorRecordSHA256 || r.GenerationID == current.GenerationID {
			return binding, errors.New("SERENA_PREPARED_PRIOR_DRIFT")
		}
	}
	if !exists && r.ExpectedPriorRecordSHA256 != "" {
		return binding, errors.New("SERENA_PREPARED_PRIOR_MISSING")
	}
	operation, err := o.effects.BeginOperation(work, r, key)
	if err != nil {
		return binding, err
	}
	if operation.Record.OperationID != r.OperationID || operation.Record.GenerationID != r.GenerationID || operation.Record.PackageKey != key || operation.Record.PackageManifestProjectionSHA256 != r.PackageManifestProjectionSHA256 || operation.Record.BuildEnvironmentSHA256 != r.BuildEnvironmentSHA256 || operation.ExpectedBeforeDefined {
		return binding, errors.New("SERENA_PREPARED_OPERATION_INPUT_DRIFT")
	}
	if err = operation.Record.Validate(); err != nil {
		return binding, err
	}
	persist := func(ctx context.Context, phase string, cause error) error {
		operation.Record.Phase = phase
		operation.Record.OutcomePublication.Phase = phase
		operation.Record.OutcomePublication.Attempted = true
		if cause != nil && operation.Record.FirstCause == nil {
			text := cause.Error()
			operation.Record.FirstCause = &text
		}
		publication, writeErr := o.effects.PersistOperation(ctx, operation)
		lastOutcome = publication
		if writeErr == nil {
			writeErr = publication.ValidateProgress(ctx, operation.NamespaceSource, publication.SHA256())
		}
		if writeErr == nil {
			operation.ExpectedBeforeDefined = true
			operation.ExpectedBeforeSHA256 = publication.SHA256()
		}
		return writeErr
	}
	if err = persist(work, "prearmed", nil); err != nil {
		return binding, err
	}
	roots, err := o.effects.AcquireRoots(work, r, key)
	for _, root := range []struct {
		role, id string
		file     PreparedSerenaFile
	}{{"generation", r.GenerationID, roots.Generation}, {"attempt", r.OperationID, roots.Attempt}} {
		if preparedSerenaFile(root.file) {
			role, id, identity, disposition := root.role, root.id, root.file.SHA256, "acquired"
			operation.Record.RootDispositions = append(operation.Record.RootDispositions, PreparedSerenaRootDispositionV1{Role: role, AcquisitionID: &id, IdentitySHA256: &identity, Disposition: &disposition})
		}
	}
	if err != nil {
		return binding, errors.Join(err, persist(cleanup, "root-acquisition-pending", err))
	}
	installed, installErr := o.effects.Install(work, r, roots)
	if preparedSerenaDigest(installed.Closure.ReceiptSHA256) {
		operation.Record.InstallationReceiptSHA256 = &installed.Closure.ReceiptSHA256
	}
	if !installed.Closure.complete(r.OperationID) {
		cause := errors.Join(installErr, errors.New("SERENA_PREPARED_CREATOR_UNSETTLED"))
		pending := persist(cleanup, "creator-closure-pending", cause)
		return binding, errors.Join(cause, pending)
	}
	disposal, disposeErr := o.effects.CleanupAttempt(cleanup, roots, installed.Closure)
	if disposeErr != nil || !disposal.AttemptAbsent || !disposal.LinksDischarged || !disposal.NoPendingResources || !preparedSerenaDigest(disposal.ReceiptSHA256) {
		pending := persist(cleanup, "disposable-cleanup-pending", errors.Join(installErr, disposeErr))
		return binding, errors.Join(installErr, disposeErr, pending, errors.New("SERENA_PREPARED_CLEANUP_UNVERIFIED"))
	}
	if installErr != nil || !installed.Closure.RootExitKnown || installed.Closure.RootExitCode != 0 {
		return binding, errors.Join(installErr, persist(cleanup, "install-refused-cleaned", installErr), errors.New("SERENA_PREPARED_INSTALL_REFUSED"))
	}
	operation.Record.CleanupReceiptSHA256 = &disposal.ReceiptSHA256
	for _, d := range []string{installed.SourceSHA256, installed.ResolutionSHA256, installed.InventorySHA256, installed.UVReceiptSHA256} {
		if !preparedSerenaDigest(d) {
			return binding, errors.New("SERENA_PREPARED_INSTALL_RECEIPT_INVALID")
		}
	}
	if exists && installed.ResolutionSHA256 != previous.ResolutionSHA256 {
		cause := errors.New("SERENA_PREPARED_RESOLUTION_DRIFT")
		return binding, errors.Join(persist(cleanup, "resolution-drift-pending", cause), cause)
	}
	if !preparedSerenaFile(installed.Entrypoint) {
		return binding, errors.New("SERENA_PREPARED_ENTRYPOINT_INVALID")
	}
	qualified, qualErr := o.effects.Qualify(work, r, roots, installed)
	if qualErr != nil || !qualified.validOutcome(cleanup, r) || !qualified.RuntimeProtocolQualified || !qualified.IndependentOfAttemptRoots || !preparedSerenaDigest(qualified.ReceiptSHA256) || !preparedSerenaDigest(qualified.FinalEnvironmentInventorySHA256) {
		cause := errors.Join(qualErr, errors.New("SERENA_PREPARED_RUNTIME_UNQUALIFIED"))
		pending := persist(cleanup, "qualification-pending", cause)
		return binding, errors.Join(cause, pending)
	}
	operation.Record.QualificationReceiptSHA256 = &qualified.ReceiptSHA256
	namespace, namespaceErr := o.effects.ReadyNamespace(work, key)
	if namespaceErr != nil || namespace == nil || namespace.role != "" || namespace.key != key || namespace.io.operationID != r.OperationID || namespace.io.sourceSHA256 != operation.Record.SourceEpoch || namespace.io.Root != r.Namespace {
		return binding, errors.Join(namespaceErr, errors.New("SERENA_PREPARED_READY_NAMESPACE_UNQUALIFIED"))
	}
	common, namespaceErr := namespace.NamespaceSHA256(work)
	if namespaceErr != nil || common != operation.Record.NamespaceSHA256 {
		return binding, errors.Join(namespaceErr, errors.New("SERENA_PREPARED_READY_NAMESPACE_DRIFT"))
	}
	leafParent, namespaceErr := namespace.LeafParentSHA256(work)
	if namespaceErr != nil {
		return binding, namespaceErr
	}
	ready := true
	record := PreparedSerenaRuntimeRecord{Schema: PreparedSerenaRecordSchema, OperationID: r.OperationID, GenerationID: r.GenerationID, RequestIdentitySHA256: key, NamespaceSHA256: common, NamespaceSchema: SerenaPackageNamespaceSchema, LeafParentSHA256: leafParent, SourceSHA256: installed.SourceSHA256, UVSHA256: r.UV.SHA256, InterpreterSHA256: r.Interpreter.SHA256, PlatformABISHA256: r.PlatformABISHA256, PolicySHA256: r.PolicySHA256, ResolutionSHA256: installed.ResolutionSHA256, EnvironmentInventorySHA256: qualified.FinalEnvironmentInventorySHA256, EntrypointIdentity: installed.Entrypoint, QualificationReceiptSHA256: qualified.ReceiptSHA256, CleanupReceiptSHA256: disposal.ReceiptSHA256, Ready: &ready, PublishedAtUTC: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, e := json.Marshal(record)
	if e != nil {
		return binding, e
	}
	intendedReady := preparedSerenaHash(raw)
	operation.Record.OutcomePublication.IntendedReadySHA256 = &intendedReady
	if e = o.effects.ValidateRecord(work, record); e != nil {
		return binding, e
	}
	before, beforeExists, e := o.effects.ReadReady(work, key)
	if e != nil || beforeExists != exists || !bytes.Equal(before, prior) {
		return binding, errors.Join(e, errors.New("SERENA_PREPARED_PUBLICATION_CAS_DRIFT"))
	}
	writeErr := o.effects.WriteReady(work, key, raw)
	after, afterExists, readErr := o.effects.ReadReady(work, key)
	if writeErr != nil || readErr != nil || !afterExists || !bytes.Equal(after, raw) {
		pending := persist(cleanup, "publication-unavailable-pending", errors.Join(writeErr, readErr))
		// Compensation only overwrites our exact positively observed new bytes,
		// or a defined missing reference, never foreign or unreadable state.
		if readErr == nil && exists && ((afterExists && bytes.Equal(after, raw)) || !afterExists) {
			restoreErr := o.effects.WriteReady(cleanup, key, prior)
			restored, restoredExists, verifyErr := o.effects.ReadReady(cleanup, key)
			if restoreErr == nil && verifyErr == nil && restoredExists && bytes.Equal(restored, prior) {
				verifyErr = o.effects.ValidateRecord(cleanup, previous)
				if verifyErr == nil {
					pending = errors.Join(pending, persist(cleanup, "publication-failed-prior-restored", writeErr))
				} else {
					pending = errors.Join(pending, verifyErr, errors.New("SERENA_PREPARED_COMPENSATION_UNVERIFIED"))
				}
			} else {
				pending = errors.Join(pending, restoreErr, verifyErr, errors.New("SERENA_PREPARED_COMPENSATION_UNVERIFIED"))
			}
		}
		return binding, errors.Join(writeErr, readErr, pending, errors.New("SERENA_PREPARED_PUBLICATION_FAILED"))
	}
	if err = persist(work, "published", nil); err != nil {
		return binding, err
	}
	candidate, candidateErr := record.Binding(raw)
	candidateRecord = record
	return binding, candidateErr
}

func (q PreparedSerenaQualification) validOutcome(ctx context.Context, r PreparedSerenaRequest) bool {
	if !q.Closure.complete(r.OperationID) || !q.Closure.RootExitKnown {
		return false
	}
	if q.observation == nil {
		return false
	}
	return q.observation.request.Request == r && q.observation.Validate(ctx, q.observation.request) == nil
}

// ResolveReady enters the SAME ticket registration transaction. No scalar
// confirmation or free snapshot can authorize a later consumer creation.
func (o *PreparedSerenaRuntimeOwner) ResolveReady(ctx context.Context, request PreparedConsumerRequestV1) (*PreparedConsumerTicket, error) {
	return o.RegisterPendingConsumer(ctx, request)
}
func (o *PreparedSerenaRuntimeOwner) RetireUnused(ctx context.Context, r PreparedSerenaRuntimeRecord) (err error) {
	if o == nil || o.effects == nil {
		return errors.New("SERENA_CONSUMER_ONLY_RETIREMENT_UNAVAILABLE")
	}
	releaseIntent, err := o.effects.LockIntentOperation(ctx)
	if err != nil {
		return err
	}
	defer preparedSerenaRelease(&err, releaseIntent)
	releasePackage, err := o.effects.LockPackage(ctx, r.RequestIdentitySHA256)
	if err != nil {
		return err
	}
	defer preparedSerenaRelease(&err, releasePackage)
	references, err := o.effects.References(ctx, r.GenerationID)
	if err != nil {
		return err
	}
	if len(references.Current) > 0 || len(references.Rollback) > 0 || len(references.Pending) > 0 || !references.OriginalConsumersClosed {
		return errors.New("SERENA_PREPARED_GENERATION_REFERENCED")
	}
	if err = o.effects.ValidateRecord(ctx, r); err != nil {
		return err
	}
	references, err = o.effects.References(ctx, r.GenerationID)
	if err != nil {
		return err
	}
	if len(references.Current) > 0 || len(references.Rollback) > 0 || len(references.Pending) > 0 || !references.OriginalConsumersClosed {
		return errors.New("SERENA_PREPARED_GENERATION_REFERENCED")
	}
	return o.effects.RetireGeneration(ctx, r, references)
}

func ValidatePreparedSerenaEnvironment(environment map[string]string) error {
	for key, value := range environment {
		switch strings.ToUpper(key) {
		case "PYTHONHOME", "PYTHONPATH", "VIRTUAL_ENV":
			if value != "" {
				return fmt.Errorf("SERENA_PREPARED_ENV_CONFLICT: %s", strings.ToUpper(key))
			}
		}
	}
	if environment["PYTHONDONTWRITEBYTECODE"] != "1" {
		return errors.New("SERENA_PREPARED_BYTECODE_POLICY_REQUIRED")
	}
	return nil
}

func preparedSerenaBytecodeEnvironment(environment map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(environment)+1)
	for key, value := range environment {
		if strings.EqualFold(key, "PYTHONDONTWRITEBYTECODE") {
			if value != "1" {
				return nil, errors.New("SERENA_PREPARED_ENV_CONFLICT: PYTHONDONTWRITEBYTECODE")
			}
			continue
		}
		result[key] = value
	}
	result["PYTHONDONTWRITEBYTECODE"] = "1"
	return result, nil
}

// Caller owns the SAME package lock; consumer assembly additionally owns the
// intent exclusion outside it. One receiver validates every ready selection.
func (o *PreparedSerenaRuntimeOwner) inspectPreparedSerenaReadyLocked(ctx context.Context, want PreparedSerenaRuntimeBinding) (record PreparedSerenaRuntimeRecord, err error) {
	raw, exists, err := o.effects.ReadReady(ctx, want.RequestIdentitySHA256)
	if err != nil || !exists {
		return record, errors.Join(err, errors.New("SERENA_RUNTIME_INSTALL_NEEDED"))
	}
	record, err = decodePreparedSerenaRecord(raw)
	if err != nil {
		return record, err
	}
	actual, err := record.Binding(raw)
	if err != nil || actual != want {
		return record, errors.Join(err, errors.New("SERENA_PREPARED_BINDING_DRIFT"))
	}
	if err = o.effects.ValidateRecord(ctx, record); err != nil {
		return record, err
	}
	available, err := o.effects.PublicationAvailable(ctx, record)
	if err != nil || !available {
		return record, errors.Join(err, errors.New("SERENA_PREPARED_PUBLICATION_PENDING"))
	}
	return record, nil
}
