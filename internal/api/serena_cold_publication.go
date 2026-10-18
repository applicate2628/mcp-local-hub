package api

import (
	"context"
	"encoding/json"
	"errors"
	"mcp-local-hub/internal/process"
	"reflect"
)

// Cold receiving retains original typed observations, never live-install
// Effects or synthetic native/IO settlements reconstructed from stored fields.
type PreparedSerenaColdPublicationReadObservation struct {
	record                     PreparedSerenaRuntimeRecord
	operation                  PreparedSerenaOperationRecordV2
	readyRaw, operationRaw     []byte
	io                         PreparedSerenaRecordIO
	identity                   serenaPackageNamespaceIdentity
	ready, operationNamespace  *SerenaRecordNamespaceObservation
	readyClose, operationClose *StateFileFreshNamespaceCloseObservation
	published                  *SerenaPublishedGenerationObservation
	publishedClose             *SerenaPublishedGenerationCloseObservation
	intentNamespace            *SupervisorStateNamespaceObservation
	intentNamespaceClose       *StateFileFreshNamespaceCloseObservation
	intentLease, packageLease  *StateFileObservedLease
	intentClose, packageClose  *StateFileLeaseCloseObservation
	readers                    []*StateFileReadObservation
	closedInBound              bool
	err                        error
}

type SerenaPublicationObservation struct {
	owner      *PreparedSerenaPublicationOwner
	record     PreparedSerenaRuntimeRecord
	operation  PreparedSerenaOperationRecordV2
	quiescence *process.PublisherQuiescenceObservation
	reads      *PreparedSerenaColdPublicationReadObservation
}

func coldPublisherBinding(p process.PublisherIncarnationRecordV1) process.PublisherIncarnationBinding {
	return process.PublisherIncarnationBinding{OwnerInstanceID: p.OwnerInstanceID, OperationID: p.OperationID, NamespaceSHA256: p.NamespaceSHA256, SourceEpoch: p.SourceEpoch, ResourceScope: p.ResourceScope}
}

func validateColdPublicationData(record PreparedSerenaRuntimeRecord, operation PreparedSerenaOperationRecordV2, readySHA string) error {
	if err := operation.Validate(); err != nil {
		return err
	}
	ready := operation.ReadyPublication
	if !ready.Known || ready.Error != nil || ready.ReadyRecordSHA256 == nil || *ready.ReadyRecordSHA256 != readySHA || record.OperationID != operation.OperationID || record.GenerationID != operation.GenerationID || record.RequestIdentitySHA256 != operation.PackageKey || record.NamespaceSHA256 != operation.NamespaceSHA256 || operation.PublisherIncarnation == nil {
		return errors.New("SERENA_COLD_KNOWN_READY_BINDING_MISSING")
	}
	if operation.Phase != "published" && operation.Phase != "publication-unavailable-pending" {
		return errors.New("SERENA_COLD_OUTCOME_NOT_RECOVERABLE")
	}
	for _, pair := range [][2]*string{{ready.InstallationReceiptSHA256, operation.InstallationReceiptSHA256}, {ready.QualificationReceiptSHA256, operation.QualificationReceiptSHA256}, {ready.CleanupReceiptSHA256, operation.CleanupReceiptSHA256}} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] || !preparedSerenaDigest(*pair[0]) {
			return errors.New("SERENA_COLD_PREPUBLICATION_RECEIPT_DRIFT")
		}
	}
	if ready.QualificationReceiptSHA256 == nil || *ready.QualificationReceiptSHA256 != record.QualificationReceiptSHA256 || ready.CleanupReceiptSHA256 == nil || *ready.CleanupReceiptSHA256 != record.CleanupReceiptSHA256 {
		return errors.New("SERENA_COLD_RECORD_RECEIPT_DRIFT")
	}
	return operation.PublisherIncarnation.Validate(coldPublisherBinding(*operation.PublisherIncarnation))
}

func (r *PreparedSerenaColdPublicationReadObservation) validateClosed(record PreparedSerenaRuntimeRecord, operation PreparedSerenaOperationRecordV2) error {
	if r == nil || r.err != nil || !r.closedInBound || !reflect.DeepEqual(record, r.record) || !reflect.DeepEqual(operation, r.operation) || r.ready == nil || r.operationNamespace == nil || r.published == nil {
		return errors.New("SERENA_COLD_LOCAL_RECEIVING_UNAVAILABLE")
	}
	if _, err := record.Binding(r.readyRaw); err != nil {
		return err
	}
	if err := validateColdPublicationData(record, operation, preparedSerenaHash(r.readyRaw)); err != nil {
		return err
	}
	for _, reader := range r.readers {
		if reader == nil || reader.issuer == nil || !reader.finalized || !reader.view.Complete || !reader.view.OwnedResourcesComplete || !reader.view.BorrowedResourcesComplete {
			return errors.New("SERENA_COLD_READER_CLOSE_UNQUALIFIED")
		}
	}
	return errors.Join(r.readyClose.ValidateSource(r.ready), r.operationClose.ValidateSource(r.operationNamespace), r.publishedClose.Validate(r.published), r.intentNamespaceClose.ValidateSource(r.intentNamespace), r.intentClose.Validate(r.intentLease), r.packageClose.Validate(r.packageLease))
}

func (p *PreparedSerenaPublicationOwner) RecoverColdPublicationObserved(ctx context.Context, record PreparedSerenaRuntimeRecord, operation PreparedSerenaOperationRecordV2, quiescence *process.PublisherQuiescenceObservation, reads *PreparedSerenaColdPublicationReadObservation) (*SerenaPublicationObservation, error) {
	if p == nil || ctx == nil || ctx.Err() != nil || operation.PublisherIncarnation == nil {
		return nil, errors.New("SERENA_COLD_PUBLICATION_INPUT_MISSING")
	}
	if err := reads.validateClosed(record, operation); err != nil {
		return nil, err
	}
	publisher := *operation.PublisherIncarnation
	if err := quiescence.Validate(ctx, publisher, coldPublisherBinding(publisher)); err != nil {
		return nil, err
	}
	observation := &SerenaPublicationObservation{owner: p, record: record, operation: operation, quiescence: quiescence, reads: reads}
	p.mu.Lock()
	defer p.mu.Unlock()
	if current := p.operations[record.OperationID]; current != nil {
		if current.recovered != nil && reflect.DeepEqual(current.recovered.record, record) && current.readySHA == preparedSerenaHash(reads.readyRaw) && !current.invalid {
			return current.recovered, nil
		}
		return nil, errors.New("SERENA_COLD_PUBLICATION_FOREIGN_REENTRY")
	}
	p.operations[record.OperationID] = &preparedSerenaPublicationEvidence{key: record.RequestIdentitySHA256, readySHA: preparedSerenaHash(reads.readyRaw), recovered: observation}
	return observation, nil
}

func (o *SerenaPublicationObservation) validate(ctx context.Context, owner *PreparedSerenaPublicationOwner, record PreparedSerenaRuntimeRecord, readySHA string) error {
	if o == nil || ctx == nil || ctx.Err() != nil || o.owner != owner || o.reads==nil || !reflect.DeepEqual(o.record, record) || o.operation.PublisherIncarnation == nil || preparedSerenaHash(o.reads.readyRaw) != readySHA {
		return errors.New("SERENA_COLD_PUBLICATION_SOURCE_DRIFT")
	}
	if err := o.reads.validateClosed(record, o.operation); err != nil {
		return err
	}
	publisher := *o.operation.PublisherIncarnation
	return o.quiescence.Validate(ctx, publisher, coldPublisherBinding(publisher))
}

func cloneColdOperation(r PreparedSerenaOperationRecordV2) (PreparedSerenaOperationRecordV2, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return PreparedSerenaOperationRecordV2{}, err
	}
	return decodePreparedSerenaOperation(raw)
}
