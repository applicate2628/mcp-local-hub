package api

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
)

// This evidence belongs to ONE retained API composition. Copies of persisted
// operation phase text cannot construct it. New compositions start empty;
// same-process callers may inject this exact owner, rather than clone tokens.
type PreparedSerenaPublicationOwner struct {
	mu         sync.Mutex
	operations map[string]*preparedSerenaPublicationEvidence
}
type preparedSerenaPublicationEvidence struct {
	key, readySHA               string
	writer                      *StateFilePublicationObservation
	reader                      *StateFileReadObservation
	readyRaw                    []byte
	installation, qualification PreparedSerenaExecutionClosure
	cleanup                     PreparedSerenaCleanup
	outcome                     *StateFilePublicationObservation
	invalid                     bool
	preparationClose            *PreparedSerenaPreparationCloseObservation
	effects                     *preparedSerenaRuntimeEffects
	recovered                   *SerenaPublicationObservation
	outcomeRecovery             *preparedSerenaOutcomeRecovery
}

type preparedSerenaOutcomeRecovery struct {
	owner                        *PreparedSerenaPublicationOwner
	entry                        *preparedSerenaPublicationEvidence
	outcome                      *StateFilePublicationObservation
	operationRaw                 []byte
	operation                    PreparedSerenaOperationRecordV2
	readyReader, operationReader *StateFileReadObservation
}

func NewPreparedSerenaPublicationOwner() *PreparedSerenaPublicationOwner {
	return &PreparedSerenaPublicationOwner{operations: map[string]*preparedSerenaPublicationEvidence{}}
}
func (p *PreparedSerenaPublicationOwner) invalidate(operation string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if value := p.operations[operation]; value != nil {
		value.invalid = true
	}
}
func (p *PreparedSerenaPublicationOwner) prepublication(effects *preparedSerenaRuntimeEffects, r PreparedSerenaRequest, key string, installed PreparedSerenaExecutionClosure, qualification PreparedSerenaExecutionClosure, cleanup PreparedSerenaCleanup) error {
	if p == nil || !installed.complete(r.OperationID) || !qualification.complete(r.OperationID) || !cleanup.AttemptAbsent || !cleanup.LinksDischarged || !cleanup.NoPendingResources || !preparedSerenaDigest(cleanup.ReceiptSHA256) {
		return errors.New("SERENA_PREPARED_PUBLICATION_PREREQUISITES_MISSING")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.operations[r.OperationID] = &preparedSerenaPublicationEvidence{effects: effects, key: key, installation: installed, qualification: qualification, cleanup: cleanup, invalid: true}
	return nil
}

func (p *PreparedSerenaPublicationOwner) ReceivePreparationClose(ctx context.Context, r PreparedSerenaRequest, proof *PreparedSerenaPreparationCloseObservation) error {
	if p == nil || proof == nil || proof.owner == nil || proof.owner.publication != p {
		return errors.New("SERENA_PREPARATION_CLOSE_FOREIGN")
	}
	p.invalidate(r.OperationID)
	if err := proof.Validate(ctx, proof.owner, r); err != nil {
		return err
	}
	p.mu.Lock()
	value := p.operations[r.OperationID]
	if value == nil || value.effects != proof.owner {
		p.mu.Unlock()
		return errors.New("SERENA_PREPARATION_CLOSE_OPERATION_MISSING")
	}
	value.preparationClose = proof
	p.mu.Unlock()
	if err := p.outcomeWritten(ctx, r.OperationID, proof.lastOutcome); err != nil {
		return err
	}
	return nil
}
func (p *PreparedSerenaPublicationOwner) readyWritten(ctx context.Context, operation string, writer *StateFilePublicationObservation, reader *StateFileReadObservation, raw []byte) error {
	if p == nil || !writer.Complete() || reader == nil || writer.SHA256() != preparedSerenaHash(raw) || reader.Validate(ctx, writer.path, 1048576, raw, writer.SHA256()) != nil {
		return errors.New("SERENA_PREPARED_PUBLICATION_WRITER_UNSETTLED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	value := p.operations[operation]
	if value == nil {
		return errors.New("SERENA_PREPARED_PUBLICATION_PREREQUISITES_MISSING")
	}
	value.writer = writer
	value.reader = reader
	value.readySHA = writer.SHA256()
	value.readyRaw = append([]byte(nil), raw...)
	value.invalid = false
	return nil
}
func (p *PreparedSerenaPublicationOwner) outcomeWritten(ctx context.Context, operation string, outcome *StateFilePublicationObservation) error {
	if p == nil {
		return errors.New("SERENA_PREPARED_PUBLICATION_OWNER_UNAVAILABLE")
	}
	p.mu.Lock()
	value := p.operations[operation]
	if value == nil {
		p.mu.Unlock()
		return errors.New("SERENA_PREPARED_PUBLICATION_PREREQUISITES_MISSING")
	}
	value.outcome = outcome
	value.invalid = true
	value.outcomeRecovery = nil
	// Known FULL final-writer resource closure allows exact same-operation
	// receiving even when its body returned an error. That first error remains
	// with the caller; unknown closure never qualifies recovery.
	eligible := outcome != nil && outcome.ResourcesClosed() && value.writer.Complete() && value.installation.complete(operation) && value.qualification.complete(operation) && value.cleanup.NoPendingResources && value.readySHA != ""
	path, readySHA := "", value.readySHA
	if value.writer != nil {
		path = value.writer.path
	}
	p.mu.Unlock()
	if !eligible {
		return errors.New("SERENA_PREPARED_OUTCOME_RESOURCES_PENDING")
	}
	raw, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, 1048576)
	if readErr != nil {
		return readErr
	}
	if err := reader.Validate(ctx, path, 1048576, raw, readySHA); err != nil {
		return err
	}
	record, err := decodePreparedSerenaRecord(raw)
	if err != nil {
		return err
	}
	operationRaw, operationReader, err := ReadStateFileInodeAnchoredObserved(ctx, outcome.path, 64<<20)
	if err != nil {
		return err
	}
	if operationReader == nil {
		return errors.New("SERENA_PREPARED_CURRENT_READER_UNDEFINED")
	}
	if err = operationReader.Validate(ctx, outcome.path, 64<<20, operationRaw, preparedSerenaHash(operationRaw)); err != nil {
		return err
	}
	current, err := decodePreparedSerenaOperation(operationRaw)
	if err != nil {
		return err
	}
	if err = validateColdPublicationData(record, current, readySHA); err != nil {
		return err
	}
	if current.SourceEpoch != record.SourceSHA256 || current.NamespaceSchema != record.NamespaceSchema {
		return errors.New("SERENA_CONSUMER_PROTECTED_OPERATION_DRIFT")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.operations[operation] != value || value.outcome != outcome || value.readySHA != readySHA {
		return errors.New("SERENA_PREPARED_OUTCOME_RECEIVING_DRIFT")
	}
	value.invalid = false
	value.outcomeRecovery = &preparedSerenaOutcomeRecovery{owner: p, entry: value, outcome: outcome, operationRaw: append([]byte(nil), operationRaw...), operation: current, readyReader: reader, operationReader: operationReader}
	return nil
}
func (p *PreparedSerenaPublicationOwner) receivedPublication(ctx context.Context, r PreparedSerenaRuntimeRecord, readySHA string) (bool, error) {
	if ctx == nil {
		return false, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	p.mu.Lock()
	value := p.operations[r.OperationID]
	if value == nil {
		p.mu.Unlock()
		return false, nil
	} // cold publisher/forwarder is unqualified
	if value.recovered != nil {
		recovered := value.recovered
		if value.invalid || value.key != r.RequestIdentitySHA256 || value.readySHA != readySHA {
			p.mu.Unlock()
			return false, nil
		}
		p.mu.Unlock()
		if err := recovered.validate(ctx, p, r, readySHA); err != nil {
			return false, err
		}
		return true, nil
	}
	eligible := !value.invalid && value.preparationClose != nil && value.preparationClose.err == nil && value.preparationClose.view.InOriginalBound && value.key == r.RequestIdentitySHA256 && value.readySHA == readySHA && value.writer.Complete() && value.outcome.ResourcesClosed() && value.installation.complete(r.OperationID) && value.qualification.complete(r.OperationID)
	if !eligible {
		p.mu.Unlock()
		return false, nil
	}
	path := value.writer.path
	p.mu.Unlock()
	raw, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, path, 1048576)
	if readErr != nil {
		return false, readErr
	}
	if reader == nil {
		return false, errors.New("SERENA_PREPARED_CURRENT_READER_UNDEFINED")
	}
	if err := reader.Validate(ctx, path, 1048576, raw, readySHA); err != nil {
		return false, err
	}
	actual, err := decodePreparedSerenaRecord(raw)
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(actual, r) {
		return false, errors.New("SERENA_CONSUMER_PROTECTED_OPERATION_DRIFT")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.operations[r.OperationID] == value && !value.invalid && value.readySHA == readySHA, nil
}

// Publication alone owns admission of current protected operation bytes.
// Persisted phase text can never substitute for original recovery receiving.
func (p *PreparedSerenaPublicationOwner) validateCurrentOperation(ctx context.Context, record PreparedSerenaRuntimeRecord, readySHA string, operation PreparedSerenaOperationRecordV2, operationRaw []byte) error {
	if ctx == nil {
		return errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	if _, finite := ctx.Deadline(); !finite {
		return errors.New("SERENA_CONSUMER_INVOCATION_DEADLINE_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	decoded, err := decodePreparedSerenaOperation(operationRaw)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(decoded, operation) || operation.SourceEpoch != record.SourceSHA256 || operation.NamespaceSchema != record.NamespaceSchema {
		return errors.New("SERENA_CONSUMER_PROTECTED_OPERATION_DRIFT")
	}
	if err = validateColdPublicationData(record, operation, readySHA); err != nil {
		return err
	}
	if p == nil {
		return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
	}
	p.mu.Lock()
	value := p.operations[record.OperationID]
	if value == nil || value.invalid || value.key != record.RequestIdentitySHA256 || value.readySHA != readySHA {
		p.mu.Unlock()
		return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
	}
	cold, recovery := value.recovered, value.outcomeRecovery
	writer, outcome, readyRaw := value.writer, value.outcome, append([]byte(nil), value.readyRaw...)
	p.mu.Unlock()
	if cold != nil {
		if cold.reads == nil || !bytes.Equal(cold.reads.operationRaw, operationRaw) || !reflect.DeepEqual(cold.operation, operation) {
			return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
		}
	} else if recovery != nil {
		if writer == nil || outcome == nil || recovery.readyReader == nil || recovery.operationReader == nil {
			return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
		}
		if recovery.owner != p || recovery.entry != value || recovery.outcome != outcome || !recovery.outcome.ResourcesClosed() || !bytes.Equal(recovery.operationRaw, operationRaw) || !reflect.DeepEqual(recovery.operation, operation) {
			return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
		}
		if err = recovery.readyReader.Validate(ctx, writer.path, 1048576, readyRaw, readySHA); err != nil {
			return err
		}
		if err = recovery.operationReader.Validate(ctx, recovery.outcome.path, 64<<20, operationRaw, preparedSerenaHash(operationRaw)); err != nil {
			return err
		}
	} else if operation.Phase != "published" {
		return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
	}
	available, err := p.receivedPublication(ctx, record, readySHA)
	if err != nil {
		return err
	}
	if !available {
		return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.operations[record.OperationID] != value || value.invalid || value.recovered != cold || value.outcomeRecovery != recovery || value.writer != writer || value.outcome != outcome || value.readySHA != readySHA {
		return errors.New("SERENA_CONSUMER_PUBLICATION_UNRECEIVED")
	}
	return ctx.Err()
}

func (p *PreparedSerenaPublicationOwner) available(ctx context.Context, record PreparedSerenaRuntimeRecord, readySHA string) (bool, error) {
	if ctx == nil {
		return false, errors.New("SERENA_PREPARED_CONTEXT_MISSING")
	}
	if _, finite := ctx.Deadline(); !finite {
		return false, errors.New("SERENA_CONSUMER_INVOCATION_DEADLINE_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if p == nil {
		return false, nil
	}
	p.mu.Lock()
	value := p.operations[record.OperationID]
	path := ""
	if value != nil {
		if value.recovered != nil && value.recovered.reads != nil && value.recovered.reads.operationNamespace != nil {
			path = value.recovered.reads.operationNamespace.path
		} else if value.outcome != nil {
			path = value.outcome.path
		}
	}
	p.mu.Unlock()
	if path == "" {
		return false, nil
	}
	raw, reader, err := ReadStateFileInodeAnchoredObserved(ctx, path, 64<<20)
	if err != nil {
		return false, err
	}
	if reader == nil {
		return false, errors.New("SERENA_PREPARED_CURRENT_READER_UNDEFINED")
	}
	if err = reader.Validate(ctx, path, 64<<20, raw, preparedSerenaHash(raw)); err != nil {
		return false, err
	}
	operation, err := decodePreparedSerenaOperation(raw)
	if err != nil {
		return false, err
	}
	if err = p.validateCurrentOperation(ctx, record, readySHA, operation, raw); err != nil {
		return false, err
	}
	return true, nil
}
