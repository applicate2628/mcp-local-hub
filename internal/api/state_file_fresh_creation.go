package api

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
)

// Sealed sources are existing application-state acquisitions, never path or
// caller-boolean wrappers. Each source owns its original native close.
type StateFileFreshNamespaceSource interface {
	validateStateFileFreshNamespace(context.Context, string, string) error
	borrowStateFileFreshNamespace(context.Context, func() error) error
	validateStateFileFreshNamespaceClose(*StateFileFreshNamespaceCloseObservation) error
}
type StateFileFreshCreationEvidence struct {
	source     StateFileFreshNamespaceSource
	path, role string
	lease      *StateFileObservedLease
	absence    *StateFileReadObservation
}
type StateFileFreshNamespaceCloseObservation struct {
	source StateFileFreshNamespaceSource
	closed bool
	err    error
}

func (o *StateFileFreshNamespaceCloseObservation) Validate(e *StateFileFreshCreationEvidence) error {
	if e == nil {
		return errors.New("STATE_FRESH_NAMESPACE_CLOSE_UNSETTLED")
	}
	return o.ValidateSource(e.source)
}
func (o *StateFileFreshNamespaceCloseObservation) ValidateSource(source StateFileFreshNamespaceSource) error {
	if o == nil || source == nil || o.source == nil || o.source != source || !o.closed || o.err != nil {
		return errors.New("STATE_FRESH_NAMESPACE_CLOSE_UNSETTLED")
	}
	return source.validateStateFileFreshNamespaceClose(o)
}
func NewStateFileFreshCreationEvidence(ctx context.Context, source StateFileFreshNamespaceSource, path, role string) (*StateFileFreshCreationEvidence, error) {
	if source == nil || ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || role == "" {
		return nil, errors.New("STATE_FRESH_CREATION_INPUT_INVALID")
	}
	if err := source.validateStateFileFreshNamespace(ctx, path, role); err != nil {
		return nil, err
	}
	return &StateFileFreshCreationEvidence{source: source, path: path, role: role}, nil
}
func (e *StateFileFreshCreationEvidence) ValidateFreshCreation(ctx context.Context, path string) error {
	if e == nil || e.source == nil || path != e.path {
		return errors.New("STATE_FRESH_CREATION_SOURCE_MISMATCH")
	}
	return e.source.validateStateFileFreshNamespace(ctx, path, e.role)
}

type serenaRecordNamespacePlatform interface {
	validate(context.Context, string) error
	borrow(context.Context, func() error) error
	close() (*StateFileFreshNamespaceCloseObservation, error)
	validateClose(*StateFileFreshNamespaceCloseObservation) error
	namespaceDigest(context.Context) (string, error)
	packageIdentity(context.Context) (serenaPackageNamespaceIdentity, error)
	leafParentDigest(context.Context) (string, error)
}

const SerenaPackageNamespaceSchema = "serena-package-namespace-v1"

type serenaPackageNamespaceIdentity struct {
	runtimeRoot, packageRoot SerenaTreeObject
}

type serenaPackageNamespaceBinding struct {
	issuer                                      *SerenaRecordNamespaceObservation
	root, key, operationID, sourceEpoch, digest string
	identity                                    serenaPackageNamespaceIdentity
}
type SerenaRecordNamespaceObservation struct {
	readOnly          bool
	mu                sync.Mutex
	io                PreparedSerenaRecordIO
	key, path         string
	role, operationID string
	platform          serenaRecordNamespacePlatform
	publications      []*StateFilePublicationObservation
}

func (o *SerenaRecordNamespaceObservation) retainPublication(publication *StateFilePublicationObservation) {
	if o == nil || publication == nil || (publication.fresh != nil && publication.fresh.source != o) || publication.path != o.path {
		return
	}
	o.mu.Lock()
	o.publications = append(o.publications, publication)
	o.mu.Unlock()
}

func (io PreparedSerenaRecordIO) AcquireRecordNamespaceObserved(ctx context.Context, key string) (*SerenaRecordNamespaceObservation, error) {
	path, err := io.recordPath(key)
	if err != nil {
		return nil, err
	}
	if !preparedSerenaID(io.operationID) || !preparedSerenaDigest(io.sourceSHA256) {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_OPERATION_UNBOUND")
	}
	return acquireSerenaRecordNamespacePlatform(ctx, io, key, path)
}
func (io PreparedSerenaRecordIO) AcquireOperationNamespaceObserved(ctx context.Context, key, operationID string, createMissing bool) (*SerenaRecordNamespaceObservation, error) {
	if ctx == nil || !preparedSerenaDigest(key) || !preparedSerenaID(operationID) || operationID != io.operationID || !preparedSerenaDigest(io.sourceSHA256) || !filepath.IsAbs(io.Root) || filepath.Clean(io.Root) != io.Root {
		return nil, errors.New("SERENA_OPERATION_NAMESPACE_INPUT_INVALID")
	}
	if io.packageBinding == nil || io.packageBinding.key != key || io.packageBinding.root != io.Root || io.packageBinding.operationID != operationID || io.packageBinding.sourceEpoch != io.sourceSHA256 {
		return nil, errors.New("SERENA_OPERATION_PACKAGE_NAMESPACE_UNBOUND")
	}
	path := filepath.Join(io.Root, key, "operations", operationID+".json")
	return acquireSerenaOperationNamespacePlatform(ctx, io, key, operationID, path, createMissing)
}

func (o *SerenaRecordNamespaceObservation) LeafParentSHA256(ctx context.Context) (string, error) {
	if o == nil || o.platform == nil {
		return "", errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	return o.platform.leafParentDigest(ctx)
}
func (o *SerenaRecordNamespaceObservation) NamespaceSHA256(ctx context.Context) (string, error) {
	if o == nil || o.platform == nil {
		return "", errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	return o.platform.namespaceDigest(ctx)
}
func (o *SerenaRecordNamespaceObservation) validateStateFileFreshNamespace(ctx context.Context, path, role string) error {
	if o != nil && o.readOnly {
		return errors.New("SERENA_RECORD_READ_HAS_NO_FRESH_CREATION_AUTHORITY")
	}
	if o == nil || o.platform == nil || path != o.path {
		return errors.New("SERENA_RECORD_FRESH_LEAF_UNDECLARED")
	}
	if role == "prepared-operation" {
		if o.role != "prepared-operation" || !preparedSerenaID(o.operationID) || filepath.Base(path) != o.operationID+".json" || filepath.Dir(path) != filepath.Join(o.io.Root, o.key, "operations") {
			return errors.New("SERENA_OPERATION_FRESH_LEAF_UNDECLARED")
		}
	} else if role != "prepared-ready" || o.role != "" || filepath.Base(path) != "ready.json" {
		return errors.New("SERENA_RECORD_FRESH_LEAF_UNDECLARED")
	}
	return o.platform.validate(ctx, path)
}
func (o *SerenaRecordNamespaceObservation) borrowStateFileFreshNamespace(ctx context.Context, body func() error) error {
	if o == nil || o.platform == nil {
		return errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	return o.platform.borrow(ctx, body)
}
func (o *SerenaRecordNamespaceObservation) validateStateFileFreshNamespaceClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if o == nil || o.platform == nil {
		return errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	return o.platform.validateClose(proof)
}
func (o *SerenaRecordNamespaceObservation) CloseObserved() (*StateFileFreshNamespaceCloseObservation, error) {
	if o == nil || o.platform == nil {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	proof, closeErr := o.platform.close()
	o.mu.Lock()
	publications := append([]*StateFilePublicationObservation(nil), o.publications...)
	o.mu.Unlock()
	for _, publication := range publications {
		closeErr = errors.Join(closeErr, publication.receiveRecordNamespaceClose(o, proof))
	}
	return proof, closeErr
}
