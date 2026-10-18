package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mcp-local-hub/internal/clients"
	"mcp-local-hub/internal/process"
	"path/filepath"
)

const PreparedSerenaOperationSchema = "serena-preparation-operation-v2"

type PreparedSerenaRootDispositionV1 struct {
	Role                 string  `json:"role"`
	AcquisitionID        *string `json:"acquisition_id"`
	IdentitySHA256       *string `json:"identity_sha256"`
	Disposition          *string `json:"disposition"`
	CleanupReceiptSHA256 *string `json:"cleanup_receipt_sha256"`
}
type PreparedSerenaReadyPublicationV1 struct {
	Known                      bool    `json:"known"`
	ReadyRecordSHA256          *string `json:"ready_record_sha256"`
	SourceOperationID          *string `json:"source_operation_id"`
	WriterReceiptSHA256        *string `json:"writer_receipt_sha256"`
	ReaderReceiptSHA256        *string `json:"reader_receipt_sha256"`
	LeaseReceiptSHA256         *string `json:"lease_receipt_sha256"`
	NamespaceReceiptSHA256     *string `json:"namespace_receipt_sha256"`
	InstallationReceiptSHA256  *string `json:"installation_receipt_sha256"`
	QualificationReceiptSHA256 *string `json:"qualification_receipt_sha256"`
	CleanupReceiptSHA256       *string `json:"cleanup_receipt_sha256"`
	Error                      *string `json:"error"`
}
type PreparedSerenaOutcomePublicationV1 struct {
	Attempted           bool    `json:"attempted"`
	Phase               string  `json:"phase"`
	IntendedReadySHA256 *string `json:"intended_ready_sha256"`
	Error               *string `json:"error"`
}
type PreparedSerenaOperationRecordV2 struct {
	Schema                          string                                `json:"schema"`
	Version                         int                                   `json:"version"`
	OperationID                     string                                `json:"operation_id"`
	GenerationID                    string                                `json:"generation_id"`
	PackageKey                      string                                `json:"package_key"`
	RequestIdentitySHA256           string                                `json:"request_identity_sha256"`
	PackageManifestProjectionSHA256 string                                `json:"package_manifest_projection_sha256"`
	BuildEnvironmentSHA256          string                                `json:"build_environment_sha256"`
	NamespaceSHA256                 string                                `json:"namespace_sha256"`
	NamespaceSchema                 string                                `json:"namespace_schema"`
	LeafParentSHA256                string                                `json:"leaf_parent_sha256"`
	SourceEpoch                     string                                `json:"source_epoch"`
	Phase                           string                                `json:"phase"`
	PublisherIncarnation            *process.PublisherIncarnationRecordV1 `json:"publisher_incarnation"`
	RootDispositions                []PreparedSerenaRootDispositionV1     `json:"root_dispositions"`
	InstallationReceiptSHA256       *string                               `json:"installation_receipt_sha256"`
	QualificationReceiptSHA256      *string                               `json:"qualification_receipt_sha256"`
	CleanupReceiptSHA256            *string                               `json:"cleanup_receipt_sha256"`
	ReadyPublication                PreparedSerenaReadyPublicationV1      `json:"ready_publication"`
	OutcomePublication              PreparedSerenaOutcomePublicationV1    `json:"outcome_publication"`
	FirstCause                      *string                               `json:"first_cause"`
	CleanupErrors                   []string                              `json:"cleanup_errors"`
}
type PreparedSerenaOperationUpdateV2 struct {
	Record                PreparedSerenaOperationRecordV2
	ExpectedBeforeDefined bool
	ExpectedBeforeSHA256  string
	NamespaceSource       *SerenaRecordNamespaceObservation
}

func validPreparedSerenaOperationPhase(phase string) bool {
	switch phase {
	case "prearmed", "root-acquisition-pending", "creator-closure-pending", "disposable-cleanup-pending", "install-refused-cleaned", "resolution-drift-pending", "qualification-pending", "publication-unavailable-pending", "publication-failed-prior-restored", "published":
		return true
	}
	return false
}
func (r PreparedSerenaOperationRecordV2) Validate() error {
	if r.Schema != PreparedSerenaOperationSchema || r.Version != 2 || r.NamespaceSchema != SerenaPackageNamespaceSchema {
		return errors.New("SERENA_PREPARED_NAMESPACE_SCHEMA_UNSUPPORTED")
	}
	if !preparedSerenaID(r.OperationID) || !preparedSerenaID(r.GenerationID) || !validPreparedSerenaOperationPhase(r.Phase) || r.PackageKey != r.RequestIdentitySHA256 || r.CleanupErrors == nil || !preparedSerenaDigest(r.SourceEpoch) || !preparedSerenaDigest(r.LeafParentSHA256) {
		return errors.New("SERENA_OPERATION_RECORD_INVALID")
	}
	for _, v := range []string{r.PackageKey, r.PackageManifestProjectionSHA256, r.BuildEnvironmentSHA256, r.NamespaceSHA256} {
		if !preparedSerenaDigest(v) {
			return errors.New("SERENA_OPERATION_RECORD_DIGEST_INVALID")
		}
	}
	for _, v := range []*string{r.InstallationReceiptSHA256, r.QualificationReceiptSHA256, r.CleanupReceiptSHA256} {
		if v != nil && !preparedSerenaDigest(*v) {
			return errors.New("SERENA_OPERATION_RECORD_RECEIPT_INVALID")
		}
	}
	if r.PublisherIncarnation == nil {
		return errors.New("SERENA_OPERATION_PUBLISHER_UNQUALIFIED")
	}
	publisher := r.PublisherIncarnation
	if publisher.OperationID != r.OperationID || publisher.NamespaceSHA256 != r.NamespaceSHA256 || publisher.SourceEpoch != r.SourceEpoch {
		return errors.New("SERENA_OPERATION_PUBLISHER_BINDING_DRIFT")
	}
	seen := map[string]bool{}
	for _, root := range r.RootDispositions {
		if (root.Role != "generation" && root.Role != "attempt" && root.Role != "qualification") || seen[root.Role] {
			return errors.New("SERENA_OPERATION_ROOT_DISPOSITION_INVALID")
		}
		seen[root.Role] = true
		if root.AcquisitionID != nil && !preparedSerenaID(*root.AcquisitionID) {
			return errors.New("SERENA_OPERATION_ROOT_ACQUISITION_INVALID")
		}
		for _, v := range []*string{root.IdentitySHA256, root.CleanupReceiptSHA256} {
			if v != nil && !preparedSerenaDigest(*v) {
				return errors.New("SERENA_OPERATION_ROOT_RECEIPT_INVALID")
			}
		}
		if root.Disposition != nil && *root.Disposition == "" {
			return errors.New("SERENA_OPERATION_ROOT_DISPOSITION_INVALID")
		}
	}
	if r.ReadyPublication.Known {
		if r.ReadyPublication.Error != nil || r.ReadyPublication.SourceOperationID == nil || *r.ReadyPublication.SourceOperationID != r.OperationID {
			return errors.New("SERENA_OPERATION_READY_PREREQUISITE_INVALID")
		}
		for _, v := range []*string{r.ReadyPublication.ReadyRecordSHA256, r.ReadyPublication.WriterReceiptSHA256, r.ReadyPublication.ReaderReceiptSHA256, r.ReadyPublication.LeaseReceiptSHA256, r.ReadyPublication.NamespaceReceiptSHA256, r.ReadyPublication.InstallationReceiptSHA256, r.ReadyPublication.QualificationReceiptSHA256, r.ReadyPublication.CleanupReceiptSHA256} {
			if v == nil || !preparedSerenaDigest(*v) {
				return errors.New("SERENA_OPERATION_READY_PREREQUISITE_INVALID")
			}
		}
	}
	if !validPreparedSerenaOperationPhase(r.OutcomePublication.Phase) || r.OutcomePublication.Phase != r.Phase {
		return errors.New("SERENA_OPERATION_OUTCOME_PHASE_INVALID")
	}
	if r.OutcomePublication.IntendedReadySHA256 != nil && !preparedSerenaDigest(*r.OutcomePublication.IntendedReadySHA256) {
		return errors.New("SERENA_OPERATION_OUTCOME_DIGEST_INVALID")
	}
	return nil
}
func decodePreparedSerenaOperation(raw []byte) (PreparedSerenaOperationRecordV2, error) {
	var result PreparedSerenaOperationRecordV2
	if len(raw) == 0 || len(raw) > 64<<20 {
		return result, errors.New("SERENA_OPERATION_RECORD_SIZE")
	}
	if err := rejectSerenaJSONDuplicates(raw); err != nil {
		return result, err
	}
	// Required fixed fields are a producer contract, even when their value is
	// explicitly undefined/null. A legacy/missing receipt is not virgin state.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	var schema string
	if err := json.Unmarshal(fields["schema"], &schema); err != nil || schema != PreparedSerenaOperationSchema {
		return result, errors.New("SERENA_PREPARED_NAMESPACE_SCHEMA_UNSUPPORTED")
	}
	required := []string{"schema", "version", "operation_id", "generation_id", "package_key", "request_identity_sha256", "package_manifest_projection_sha256", "build_environment_sha256", "namespace_sha256", "namespace_schema", "leaf_parent_sha256", "source_epoch", "phase", "publisher_incarnation", "root_dispositions", "installation_receipt_sha256", "qualification_receipt_sha256", "cleanup_receipt_sha256", "ready_publication", "outcome_publication", "first_cause", "cleanup_errors"}
	if len(fields) != len(required) {
		return result, errors.New("SERENA_OPERATION_RECORD_FIELDS")
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return result, errors.New("SERENA_OPERATION_RECORD_FIELDS")
		}
	}
	if err := requireSerenaOperationFields(fields["ready_publication"], []string{"known", "ready_record_sha256", "source_operation_id", "writer_receipt_sha256", "reader_receipt_sha256", "lease_receipt_sha256", "namespace_receipt_sha256", "installation_receipt_sha256", "qualification_receipt_sha256", "cleanup_receipt_sha256", "error"}); err != nil {
		return result, err
	}
	if err := requireSerenaOperationFields(fields["outcome_publication"], []string{"attempted", "phase", "intended_ready_sha256", "error"}); err != nil {
		return result, err
	}
	var ready, outcome map[string]json.RawMessage
	_ = json.Unmarshal(fields["ready_publication"], &ready)
	_ = json.Unmarshal(fields["outcome_publication"], &outcome)
	for _, boolean := range []json.RawMessage{ready["known"], outcome["attempted"]} {
		if !bytes.Equal(boolean, []byte("true")) && !bytes.Equal(boolean, []byte("false")) {
			return result, errors.New("SERENA_OPERATION_DEFINED_BOOLEAN_REQUIRED")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return result, errors.New("SERENA_OPERATION_RECORD_TRAILING")
	}
	return result, result.Validate()
}
func requireSerenaOperationFields(raw []byte, required []string) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || len(fields) != len(required) {
		return errors.New("SERENA_OPERATION_RECORD_FIELDS")
	}
	for _, key := range required {
		if _, found := fields[key]; !found {
			return errors.New("SERENA_OPERATION_RECORD_FIELDS")
		}
	}
	return nil
}

func (io PreparedSerenaRecordIO) PersistOperation(ctx context.Context, update PreparedSerenaOperationUpdateV2) (publication *StateFilePublicationObservation, retErr error) {
	record := update.Record
	if err := record.Validate(); err != nil {
		return nil, err
	}
	source := update.NamespaceSource
	if ctx == nil || source == nil || source.io.operationID != io.operationID || source.io.Root != io.Root || source.io.sourceSHA256 != io.sourceSHA256 || source.operationID != record.OperationID || source.key != record.PackageKey || record.OperationID != io.operationID || record.SourceEpoch != io.sourceSHA256 {
		return nil, errors.New("SERENA_OPERATION_WRITE_BINDING_INVALID")
	}
	path := filepath.Join(io.Root, record.PackageKey, "operations", record.OperationID+".json")
	if err := source.validateStateFileFreshNamespace(ctx, path, "prepared-operation"); err != nil {
		return nil, err
	}
	namespaceSHA, err := source.NamespaceSHA256(ctx)
	if err != nil || namespaceSHA != record.NamespaceSHA256 {
		return nil, errors.Join(err, errors.New("SERENA_OPERATION_NAMESPACE_DRIFT"))
	}
	leafSHA, err := source.LeafParentSHA256(ctx)
	if err != nil || record.LeafParentSHA256 != leafSHA {
		return nil, errors.Join(err, errors.New("SERENA_OPERATION_LEAF_PARENT_DRIFT"))
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	lease, err := AcquireStateFileObservedLease(ctx, path)
	if err != nil {
		if lease != nil {
			_, closeErr := lease.CloseObserved()
			err = errors.Join(err, closeErr)
		}
		return nil, err
	}
	defer func() {
		_, leaseErr := lease.CloseObserved()
		retErr = errors.Join(retErr, leaseErr)
		if publication != nil {
			retErr = errors.Join(retErr, publication.bindProgressNamespace(source))
			source.retainPublication(publication)
			if retErr == nil {
				retErr = publication.ValidateProgress(ctx, source, publication.SHA256())
			}
		}
	}()
	before, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, 64<<20)
	request := clients.ConfigWriteRequest{Context: ctx, Path: path, Contents: raw}
	if !update.ExpectedBeforeDefined {
		if update.ExpectedBeforeSHA256 != "" || reader == nil || reader.ValidateAbsence(ctx, path, StateFileReadLeafNotFound) != nil {
			return nil, errors.Join(readErr, errors.New("SERENA_OPERATION_EXPECTED_ABSENCE_UNQUALIFIED"))
		}
		request.FreshCreationEvidence, err = NewStateFileFreshCreationEvidence(ctx, source, path, "prepared-operation")
		if err != nil {
			return nil, err
		}
		publication, retErr = WriteStateFileBytesLockHeldObserved(request, lease)
		return publication, retErr
	}
	if !preparedSerenaDigest(update.ExpectedBeforeSHA256) || readErr != nil || reader == nil {
		return nil, errors.Join(readErr, errors.New("SERENA_OPERATION_EXPECTED_BEFORE_UNQUALIFIED"))
	}
	if err := reader.Validate(ctx, path, 64<<20, before, update.ExpectedBeforeSHA256); err != nil {
		return nil, err
	}
	prior, err := decodePreparedSerenaOperation(before)
	if err != nil {
		return nil, err
	}
	if prior.OperationID != record.OperationID || prior.GenerationID != record.GenerationID || prior.PackageKey != record.PackageKey || prior.SourceEpoch != record.SourceEpoch {
		return nil, errors.New("SERENA_OPERATION_PRIOR_BINDING_DRIFT")
	}
	transaction, err := clients.CaptureStateFileWriteSnapshotObserved(ctx, path, 64<<20, update.ExpectedBeforeSHA256)
	if err != nil {
		return nil, err
	}
	request.ExpectedSnapshot = transaction.Snapshot()
	if request.ExpectedSnapshot == nil {
		_, closeErr := transaction.CloseObserved()
		return nil, errors.Join(closeErr, errors.New("SERENA_OPERATION_SNAPSHOT_UNAVAILABLE"))
	}
	publication, writeErr := WriteStateFileBytesLockHeldObserved(request, lease)
	close, closeErr := transaction.CloseObserved()
	if publication != nil {
		closeErr = errors.Join(closeErr, publication.ReceiveBorrowedParentClose(close))
	}
	return publication, errors.Join(writeErr, closeErr)
}
