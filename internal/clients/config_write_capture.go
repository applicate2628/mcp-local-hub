package clients

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// This transaction extends the existing physical source/snapshot owner. It
// owns its file and ancestor handles; a writer borrows only the snapshot's
// parent and can never close it. No parser/client/API dependency is needed.
type ConfigWriteCaptureTransaction struct {
	mu                            sync.Mutex
	ctx                           context.Context
	path                          string
	snapshot                      *ConfigWriteSnapshot
	closers                       []func() error
	active                        int
	closeAttempted                bool
	closeRequested                bool
	closeObservation              *ConfigWriteParentCloseObservation
	primary, validationCloseError error
}
type ConfigWriteParentCloseObservation struct {
	owner    *ConfigWriteCaptureTransaction
	snapshot *ConfigWriteSnapshot
	closed   bool
	err      error
}

func (o *ConfigWriteParentCloseObservation) Validate(snapshot *ConfigWriteSnapshot) error {
	if o == nil || o.owner == nil || snapshot == nil || o.snapshot != snapshot || !o.closed || o.err != nil {
		return errors.New("CONFIG_WRITE_PARENT_CLOSE_UNSETTLED")
	}
	return nil
}
func (t *ConfigWriteCaptureTransaction) Snapshot() *ConfigWriteSnapshot {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closeAttempted {
		return nil
	}
	return t.snapshot
}
func (t *ConfigWriteCaptureTransaction) CloseObserved() (*ConfigWriteParentCloseObservation, error) {
	if t == nil {
		return nil, errors.New("CONFIG_WRITE_CAPTURE_UNAVAILABLE")
	}
	t.mu.Lock()
	if t.closeObservation != nil {
		o := t.closeObservation
		t.mu.Unlock()
		return o, o.err
	}
	if t.active != 0 {
		t.closeRequested = true
		t.mu.Unlock()
		return nil, errors.New("CONFIG_WRITE_CAPTURE_VALIDATION_STILL_ACTIVE")
	}
	if t.closeAttempted {
		t.mu.Unlock()
		return nil, errors.New("CONFIG_WRITE_CAPTURE_CLOSE_STILL_IN_FLIGHT")
	}
	t.closeAttempted = true
	err := t.validationCloseError
	closers := append([]func() error(nil), t.closers...)
	t.mu.Unlock()
	for i := len(closers) - 1; i >= 0; i-- {
		err = errors.Join(err, closers[i]())
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closeObservation = &ConfigWriteParentCloseObservation{owner: t, snapshot: t.snapshot, closed: err == nil, err: err}
	return t.closeObservation, err
}
func (t *ConfigWriteCaptureTransaction) validateAncestors(expected string) (retErr error) {
	t.mu.Lock()
	if t.closeAttempted || t.closeRequested {
		t.mu.Unlock()
		return ErrConfigWriteSnapshotConflict
	}
	t.active++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.active--
		closePending := t.active == 0 && t.closeRequested
		t.mu.Unlock()
		if closePending {
			_, closeErr := t.CloseObserved()
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if err := t.ctx.Err(); err != nil {
		return err
	}
	ids, _, closers, openErr := captureConfigWriteAncestors(t.ctx, t.path)
	var closeErr error
	for i := len(closers) - 1; i >= 0; i-- {
		closeErr = errors.Join(closeErr, closers[i]())
	}
	t.mu.Lock()
	t.validationCloseError = errors.Join(t.validationCloseError, closeErr)
	t.mu.Unlock()
	if openErr != nil || closeErr != nil {
		return errors.Join(openErr, closeErr)
	}
	if ids != expected {
		return ErrConfigWriteSnapshotConflict
	}
	return t.ctx.Err()
}
func captureConfigWriteAncestors(ctx context.Context, path string) (string, uintptr, []func() error, error) {
	var components []string
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		components = append(components, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	var ids []string
	var closers []func() error
	var parent uintptr
	for i := len(components) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return "", 0, closers, err
		}
		id, h, closeHandle, err := openRemovalDirectoryIdentityObserved(components[i])
		if err != nil {
			return "", 0, closers, err
		}
		ids = append(ids, id)
		closers = append(closers, closeHandle)
		parent = h
	}
	return strings.Join(ids, "/"), parent, closers, nil
}
func CaptureStateFileWriteSnapshotObserved(ctx context.Context, path string, maxBytes int64, expectedSHA256 string) (transaction *ConfigWriteCaptureTransaction, err error) {
	decoded, decodeErr := hex.DecodeString(expectedSHA256)
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || maxBytes <= 0 || maxBytes == int64(^uint64(0)>>1) || decodeErr != nil || len(decoded) != sha256.Size || expectedSHA256 != strings.ToLower(expectedSHA256) {
		return nil, errors.New("CONFIG_WRITE_CAPTURE_INPUT_INVALID")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("CONFIG_WRITE_CAPTURE_DEADLINE_REQUIRED")
	}
	transaction = &ConfigWriteCaptureTransaction{ctx: ctx, path: path}
	defer func() {
		if err != nil {
			transaction.primary = err
			_, closeErr := transaction.CloseObserved()
			err = errors.Join(err, closeErr)
		}
	}()
	ids, parent, closers, err := captureConfigWriteAncestors(ctx, path)
	transaction.closers = append(transaction.closers, closers...)
	if err != nil {
		return transaction, err
	}
	file, err := openRemovalSourceFileObserved(path)
	if err != nil {
		return transaction, err
	}
	var once sync.Once
	var fileCloseErr error
	transaction.closers = append(transaction.closers, func() error { once.Do(func() { fileCloseErr = file.Close() }); return fileCloseErr })
	before, err := file.Stat()
	if err != nil {
		return transaction, err
	}
	if !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > maxBytes {
		return transaction, ErrConfigWriteSnapshotConflict
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(contents)) > maxBytes {
		return transaction, errors.Join(err, ErrConfigWriteSnapshotConflict)
	}
	hash := sha256.Sum256(contents)
	if hex.EncodeToString(hash[:]) != expectedSHA256 {
		return transaction, ErrConfigWriteSnapshotConflict
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return transaction, errors.Join(err, ErrConfigWriteSnapshotConflict)
	}
	if err = ctx.Err(); err != nil {
		return transaction, err
	}
	transaction.snapshot = &ConfigWriteSnapshot{path: path, contents: append([]byte(nil), contents...), fileInfo: before, parentHandle: parent, ancestors: ids, captureTransaction: transaction, checkBinding: transaction.validateAncestors}
	return transaction, nil
}
