package api

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// A read acquisition never exposes the fresh tree's removal or creation API.
type SerenaPublishedGenerationObservation struct {
	mu                 sync.Mutex
	record             PreparedSerenaRuntimeRecord
	recordSHA256       string
	deadline           time.Time
	platform           serenaTreePlatform
	rootPath           string
	entered            int
	sealed, finalizing bool
	primary            error
	closed             *SerenaPublishedGenerationCloseObservation
}
type SerenaPublishedGenerationCloseSnapshot struct {
	Attempted, Closed, Pending bool
	Error                      error
}
type SerenaPublishedGenerationCloseObservation struct {
	owner *SerenaPublishedGenerationObservation
	view  SerenaPublishedGenerationCloseSnapshot
}

func AcquireSerenaPublishedGenerationObserved(ctx context.Context, record PreparedSerenaRuntimeRecord, limits SerenaTreeLimits) (*SerenaPublishedGenerationObservation, error) {
	if ctx == nil || ctx.Err() != nil || !limits.valid() || !preparedSerenaID(record.OperationID) || !preparedSerenaID(record.GenerationID) {
		return nil, errors.New("SERENA_PUBLISHED_READ_INPUT_INVALID")
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if _, err = record.Binding(raw); err != nil {
		return nil, err
	}
	namespace, resolver, err := ResolveDaemonStateDirObserved(ctx)
	if err != nil {
		return nil, err
	}
	if err = resolver.Validate(ctx, namespace); err != nil {
		return nil, err
	}
	deadline, finite := ctx.Deadline()
	if !finite {
		return nil, errors.New("SERENA_PUBLISHED_READ_DEADLINE_REQUIRED")
	}
	o := &SerenaPublishedGenerationObservation{record: record, recordSHA256: preparedSerenaHash(raw), deadline: deadline, rootPath: filepath.Join(namespace, record.RequestIdentitySHA256, "generations", record.GenerationID)}
	r := SerenaOwnedTreeRequest{Namespace: namespace, PackageIdentitySHA256: record.RequestIdentitySHA256, OperationID: record.OperationID, GenerationID: record.GenerationID, Kind: "generation", Limits: limits}
	o.platform, err = acquireSerenaPublishedGenerationPlatform(ctx, r)
	o.primary = err
	return o, err
}
func (o *SerenaPublishedGenerationObservation) enter(ctx context.Context) error {
	if o == nil || ctx == nil || ctx.Err() != nil {
		return errors.New("SERENA_PUBLISHED_READ_CONTEXT_INVALID")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed || o.entered != 0 || o.platform == nil || o.primary != nil {
		return errors.Join(errors.New("SERENA_PUBLISHED_READ_UNAVAILABLE"), o.primary)
	}
	o.entered++
	return nil
}
func (o *SerenaPublishedGenerationObservation) leave() {
	o.mu.Lock()
	o.entered--
	run := o.sealed && o.entered == 0 && !o.finalizing && o.closed == nil
	if run {
		o.finalizing = true
	}
	o.mu.Unlock()
	if run {
		o.finalize()
	}
}
func (o *SerenaPublishedGenerationObservation) Inventory(ctx context.Context) (SerenaTreeInventory, error) {
	if err := o.enter(ctx); err != nil {
		return SerenaTreeInventory{}, err
	}
	defer o.leave()
	v, err := o.platform.inventory(ctx)
	if err != nil {
		o.mu.Lock()
		o.primary = errors.Join(o.primary, err)
		o.mu.Unlock()
	}
	return v, err
}
func (o *SerenaPublishedGenerationObservation) ValidatePublished(ctx context.Context, record PreparedSerenaRuntimeRecord) error {
	raw, err := json.Marshal(record)
	if o == nil || err != nil || preparedSerenaHash(raw) != o.recordSHA256 {
		return errors.New("SERENA_PUBLISHED_RECORD_DRIFT")
	}
	v, err := o.Inventory(ctx)
	if err != nil {
		return err
	}
	if v.OperationID != record.OperationID || v.SHA256 != record.EnvironmentInventorySHA256 {
		return errors.New("SERENA_PUBLISHED_INVENTORY_DRIFT")
	}
	relative, err := filepath.Rel(o.rootPath, record.EntrypointIdentity.Path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("SERENA_PUBLISHED_ENTRYPOINT_OUTSIDE_GENERATION")
	}
	for _, object := range v.Objects {
		if filepath.Join(object.Relative...) == relative {
			if object.Directory || object.Device != record.EntrypointIdentity.Device || object.FileID != record.EntrypointIdentity.FileID || object.ContentSHA256 != record.EntrypointIdentity.SHA256 {
				return errors.New("SERENA_PUBLISHED_ENTRYPOINT_DRIFT")
			}
			return ctx.Err()
		}
	}
	return errors.New("SERENA_PUBLISHED_ENTRYPOINT_ABSENT")
}
func (o *SerenaPublishedGenerationObservation) finalize() {
	var view SerenaTreeClose
	var err error
	if o.platform != nil {
		view, err = o.platform.close()
	} else {
		err = errors.New("SERENA_PUBLISHED_PARTIAL_ACQUISITION")
	}
	if !time.Now().Before(o.deadline) {
		err = errors.Join(err, context.DeadlineExceeded)
	}
	o.mu.Lock()
	o.closed = &SerenaPublishedGenerationCloseObservation{owner: o, view: SerenaPublishedGenerationCloseSnapshot{Attempted: view.Attempted, Closed: view.Closed, Error: errors.Join(err, view.Error)}}
	o.mu.Unlock()
}
func (o *SerenaPublishedGenerationObservation) CloseObserved() (*SerenaPublishedGenerationCloseObservation, error) {
	if o == nil {
		return nil, errors.New("SERENA_PUBLISHED_READ_MISSING")
	}
	o.mu.Lock()
	o.sealed = true
	if o.closed != nil {
		p := o.closed
		o.mu.Unlock()
		return p, p.view.Error
	}
	run := o.entered == 0 && !o.finalizing
	if run {
		o.finalizing = true
	}
	o.mu.Unlock()
	if run {
		o.finalize()
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed == nil {
		return &SerenaPublishedGenerationCloseObservation{owner: o, view: SerenaPublishedGenerationCloseSnapshot{Pending: true}}, errors.New("SERENA_PUBLISHED_READ_CLOSE_PENDING")
	}
	return o.closed, o.closed.view.Error
}
func (p *SerenaPublishedGenerationCloseObservation) Snapshot() SerenaPublishedGenerationCloseSnapshot {
	if p == nil {
		return SerenaPublishedGenerationCloseSnapshot{}
	}
	return p.view
}
func (p *SerenaPublishedGenerationCloseObservation) Validate(original *SerenaPublishedGenerationObservation) error {
	if p == nil || original == nil || p.owner != original {
		return errors.New("SERENA_PUBLISHED_CLOSE_FOREIGN")
	}
	original.mu.Lock()
	defer original.mu.Unlock()
	if original.closed != p || p.view.Pending || !p.view.Attempted || !p.view.Closed || p.view.Error != nil {
		return errors.Join(errors.New("SERENA_PUBLISHED_CLOSE_UNQUALIFIED"), p.view.Error)
	}
	return nil
}
