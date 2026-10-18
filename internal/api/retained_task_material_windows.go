//go:build windows

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
)

// RetainedTaskXMLMaterialStore owns physical retained Task XML only. It has no
// Product receipt, runtime-adoption, Task mutation or initialization authority.
// Native parent/child handles are held only during each physical operation.
type RetainedTaskXMLMaterialStore struct {
	mu                           sync.Mutex
	root                         string
	hashes                       map[string]string
	attempts                     map[string]struct{}
	origin                       retainedTaskMaterialOrigin
	closed                       bool
	closeErr                     error
	io                           retainedTaskMaterialIO
	parentIdentity, rootIdentity SerenaTreeObject
	identityReceived, absent     bool
	// Failed native closes retain their original owners and outcomes. No new
	// operation may hide an uncertain close by reopening or deleting material.
	unsettled      []*retainedTaskMaterialRoot
	nativeCloseErr error
}

type retainedTaskMaterialOrigin uint8

const (
	retainedTaskMaterialBorrowed retainedTaskMaterialOrigin = iota
	retainedTaskMaterialCreated
	retainedTaskMaterialRecovered
)

type retainedTaskMaterialIO struct {
	write  func(string, []byte) error
	read   func(context.Context, string, []string, string) ([]byte, error)
	remove func(string) error
}

// RetainedTaskXMLStoreAcquisitionError retains the original physical store for
// acquisition and operation failures, including final native-close outcomes.
type RetainedTaskXMLStoreAcquisitionError struct {
	store *RetainedTaskXMLMaterialStore
	cause error
}

func (e *RetainedTaskXMLStoreAcquisitionError) Error() string { return e.cause.Error() }
func (e *RetainedTaskXMLStoreAcquisitionError) Unwrap() error { return e.cause }
func (e *RetainedTaskXMLStoreAcquisitionError) RetainedStore() *RetainedTaskXMLMaterialStore {
	return e.store
}

func retainedTaskMaterialOwnerError(s *RetainedTaskXMLMaterialStore, cause error) error {
	if s == nil || cause == nil {
		return cause
	}
	var original *RetainedTaskXMLStoreAcquisitionError
	if errors.As(cause, &original) && original != nil && original.store == s {
		return cause
	}
	return &RetainedTaskXMLStoreAcquisitionError{store: s, cause: cause}
}

func productionRetainedTaskMaterialIO() retainedTaskMaterialIO {
	return retainedTaskMaterialIO{write: WriteStateFileBytesAtomic, read: ReadStateFileBeneathRootNoFollow, remove: os.Remove}
}

func (d retainedTaskMaterialIO) validate() error {
	if d.write == nil || d.read == nil || d.remove == nil {
		return errors.New("Windows product pair task rollback store I/O dependencies are incomplete")
	}
	return nil
}

// CreateRetainedTaskXMLStore preserves the existing V3 directory and ref format.
// A partial owner returned with an error still owns its attempted material.
func CreateRetainedTaskXMLStore(stateDir string) (*RetainedTaskXMLMaterialStore, error) {
	return createRetainedTaskXMLStoreWithIO(stateDir, productionRetainedTaskMaterialIO())
}

func createRetainedTaskXMLStoreWithIO(stateDir string, io retainedTaskMaterialIO) (*RetainedTaskXMLMaterialStore, error) {
	if err := io.validate(); err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp(stateDir, "windows-product-pair-task-rollback-")
	if err != nil {
		return nil, fmt.Errorf("create Windows product pair task rollback store: %w", err)
	}
	s := &RetainedTaskXMLMaterialStore{root: root, hashes: make(map[string]string), attempts: make(map[string]struct{}), origin: retainedTaskMaterialCreated, io: io}
	// Receive the actually created child before attempting any path-based I/O.
	guard, captureErr := openRetainedTaskMaterialRoot(root, false)
	if captureErr == nil {
		s.parentIdentity, s.rootIdentity = guard.parentIdentity, guard.rootIdentity
		s.identityReceived = true
	}
	var protectErr error
	if captureErr == nil {
		// The original child and parent stay held through this existing effect.
		protectErr = os.Chmod(root, 0o700)
	}
	closeErr := s.receiveNativeClose(guard)
	if err := errors.Join(captureErr, closeErr); err != nil {
		return s, retainedTaskMaterialOwnerError(s, errors.Join(fmt.Errorf("receive created task rollback root %q: %w", root, err), protectErr))
	}
	if protectErr != nil {
		cleanupErr := s.Close()
		return s, retainedTaskMaterialOwnerError(s, errors.Join(fmt.Errorf("protect Windows product pair task rollback store: %w", protectErr), cleanupErr))
	}
	return s, nil
}

// ReopenRetainedTaskXMLStore receives physical I/O inputs from the existing V3
// recovery adapter. The ref map is copied; it grants no other domain authority.
func ReopenRetainedTaskXMLStore(stateDir, root string, refs map[string]string, requireRoot bool) (*RetainedTaskXMLMaterialStore, error) {
	root = filepath.Clean(root)
	if !strings.EqualFold(filepath.Clean(filepath.Dir(root)), filepath.Clean(stateDir)) || !strings.HasPrefix(filepath.Base(root), "windows-product-pair-task-rollback-") {
		return nil, errors.New("Windows product pair task rollback store is outside the exact state-directory owner")
	}
	hashes := make(map[string]string, len(refs))
	for ref, digest := range refs {
		if !serenaTreeName(ref) || filepath.Base(ref) != ref || digest == "" {
			return nil, errors.New("retained task rollback descriptor contains an invalid opaque ref")
		}
		hashes[ref] = digest
	}
	s := &RetainedTaskXMLMaterialStore{root: root, hashes: hashes, attempts: make(map[string]struct{}, len(hashes)), origin: retainedTaskMaterialRecovered, io: productionRetainedTaskMaterialIO()}
	for ref := range hashes {
		s.attempts[ref] = struct{}{}
	}
	guard, err := openRetainedTaskMaterialRoot(root, false)
	if err != nil {
		closeErr := s.receiveNativeClose(guard)
		if !requireRoot && stateReadOpenCauseIsNotExist(err) && closeErr == nil {
			s.absent = true
			return s, nil
		}
		return s, retainedTaskMaterialOwnerError(s, errors.Join(fmt.Errorf("inspect retained task rollback store: %w", err), closeErr))
	}
	s.parentIdentity, s.rootIdentity = guard.parentIdentity, guard.rootIdentity
	s.identityReceived = true
	if err := s.receiveNativeClose(guard); err != nil {
		return s, retainedTaskMaterialOwnerError(s, err)
	}
	return s, nil
}

// RootPath is a DATA projection, never a native identity or cleanup permit.
func (s *RetainedTaskXMLMaterialStore) RootPath() string {
	if s == nil {
		return ""
	}
	return s.root
}

func (s *RetainedTaskXMLMaterialStore) receiveNativeClose(guard *retainedTaskMaterialRoot) error {
	if guard == nil {
		return nil
	}
	err := guard.close()
	if err != nil {
		s.unsettled = append(s.unsettled, guard)
		s.nativeCloseErr = errors.Join(s.nativeCloseErr, err)
	}
	return err
}

func (s *RetainedTaskXMLMaterialStore) openOriginalRoot(discard bool) (*retainedTaskMaterialRoot, error) {
	if s.nativeCloseErr != nil {
		return nil, errors.Join(errors.New("retained task rollback native close is unconfirmed"), s.nativeCloseErr)
	}
	if !s.identityReceived {
		return nil, errors.New("retained task rollback original root identity is unavailable")
	}
	guard, err := openRetainedTaskMaterialRoot(s.root, discard)
	if err != nil {
		return guard, err
	}
	if !sameRetainedTaskDirectory(guard.parentIdentity, s.parentIdentity) || !sameRetainedTaskDirectory(guard.rootIdentity, s.rootIdentity) {
		return guard, errors.New("retained task rollback original parent/child identity differs")
	}
	return guard, nil
}

func (s *RetainedTaskXMLMaterialStore) Retain(ctx context.Context, xml []byte) (ref string, retErr error) {
	defer func() { retErr = retainedTaskMaterialOwnerError(s, retErr) }()
	if ctx == nil {
		return "", errors.New("retained task rollback context is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(xml))
	ref = "task-" + digest + ".xml"
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return "", errors.New("Windows product pair task rollback store is closed")
	}
	if s.origin != retainedTaskMaterialCreated && s.origin != retainedTaskMaterialRecovered {
		return "", errors.New("Windows product pair task rollback root has no retention owner")
	}
	guard, err := s.openOriginalRoot(false)
	defer func() {
		retErr = errors.Join(retErr, s.receiveNativeClose(guard))
		if retErr != nil {
			ref = ""
		} else {
			s.hashes[ref] = digest
		}
	}()
	if err != nil {
		return "", err
	}
	if s.origin == retainedTaskMaterialRecovered {
		if expected, ok := s.hashes[ref]; !ok || expected != digest {
			return "", errors.New("recovered task rollback store cannot admit an unadopted ref")
		}
	} else {
		if _, owned := s.attempts[ref]; !owned {
			for _, leaf := range []string{ref, ref + ".lock"} {
				if _, err := os.Lstat(filepath.Join(s.root, leaf)); err == nil {
					return "", fmt.Errorf("task rollback retention conflicts with unadopted leaf %q", leaf)
				} else if !errors.Is(err, os.ErrNotExist) {
					return "", fmt.Errorf("inspect task rollback retention leaf %q: %w", leaf, err)
				}
			}
			s.attempts[ref] = struct{}{}
		}
		if err := s.io.write(filepath.Join(s.root, ref), xml); err != nil {
			return "", fmt.Errorf("retain task XML write: %w", err)
		}
	}
	readBack, err := s.io.read(ctx, s.root, []string{ref}, digest)
	if err != nil {
		return "", fmt.Errorf("retain task XML readback: %w", err)
	}
	if !bytes.Equal(readBack, xml) {
		return "", errors.New("retained task XML readback differs")
	}
	return ref, nil
}

func (s *RetainedTaskXMLMaterialStore) Load(ctx context.Context, ref string) (raw []byte, retErr error) {
	defer func() { retErr = retainedTaskMaterialOwnerError(s, retErr) }()
	if ctx == nil {
		return nil, errors.New("retained task rollback context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	digest, ok := s.hashes[ref]
	if s.closed || !ok {
		return nil, errors.New("Windows product pair task rollback ref is unavailable")
	}
	guard, err := s.openOriginalRoot(false)
	defer func() { retErr = errors.Join(retErr, s.receiveNativeClose(guard)) }()
	if err != nil {
		return nil, err
	}
	return s.io.read(ctx, s.root, []string{ref}, digest)
}

// Close retains the existing selected discard meaning. Preserving a pending
// journal simply does not select this operation; there are no idle handles.
func (s *RetainedTaskXMLMaterialStore) Close() (retErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		retErr = retainedTaskMaterialOwnerError(s, retErr)
		s.closeErr = retErr
	}()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.origin == retainedTaskMaterialBorrowed {
		return errors.New("Windows product pair task rollback root is not cleanup-owned")
	}
	if s.absent {
		// Possibly absent V3 recovery may settle only continued absence, never
		// adopt a newly appearing directory for deletion.
		if _, err := os.Lstat(s.root); errors.Is(err, os.ErrNotExist) {
			return nil
		} else {
			return errors.Join(errors.New("retained task rollback absent root changed"), err)
		}
	}
	guard, err := s.openOriginalRoot(true)
	defer func() { retErr = errors.Join(retErr, s.receiveNativeClose(guard)) }()
	if err != nil {
		if stateReadOpenCauseIsNotExist(err) {
			return nil // Preserve selected V3 discard's existing absence no-op.
		}
		return err
	}
	refs := make([]string, 0, len(s.attempts))
	for ref := range s.attempts {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	var errs []error
	for _, ref := range refs {
		for _, leaf := range []string{ref, ref + ".lock"} {
			if err := s.io.remove(filepath.Join(s.root, leaf)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove owned retained task leaf %q: %w", leaf, err))
			}
		}
	}
	if len(errs) == 0 {
		// Delete the SAME retained native child, not a re-resolved pathname.
		// Nonempty/foreign material still refuses, like the original Remove.
		if err := setFileDeleteOnClose(guard.root.handle); err != nil {
			errs = append(errs, fmt.Errorf("remove retained task root: %w", err))
		}
	}
	return errors.Join(errs...)
}

type retainedTaskMaterialRoot struct {
	parent, root                 *serenaTreeHandle
	parentIdentity, rootIdentity SerenaTreeObject
}

func sameRetainedTaskDirectory(actual, expected SerenaTreeObject) bool {
	return actual.Directory && expected.Directory && actual.Device != "" && actual.FileID != "" && actual.Device == expected.Device && actual.FileID == expected.FileID
}

func openRetainedTaskMaterialRoot(root string, discard bool) (guard *retainedTaskMaterialRoot, retErr error) {
	guard = &retainedTaskMaterialRoot{}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !serenaTreeName(filepath.Base(root)) {
		return guard, errors.New("retained task rollback root is not an ordinary absolute child")
	}
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	h, err := ntOpenStateReadRootMode(filepath.Dir(root), access, share)
	if err != nil {
		return guard, err
	}
	guard.parent = &serenaTreeHandle{handle: h}
	guard.parentIdentity, err = serenaTreeHandleIdentity(h, nil)
	if err != nil || !guard.parentIdentity.Directory {
		return guard, errors.Join(err, errors.New("retained task rollback parent is not an ordinary directory"))
	}
	if discard {
		access |= windows.DELETE
	}
	directory := true
	h, err = ntOpenStateReadRelativeMode(guard.parent.handle, filepath.Base(root), access, &directory, windows.FILE_OPEN, share, nil)
	if err != nil {
		return guard, err
	}
	guard.root = &serenaTreeHandle{handle: h}
	guard.rootIdentity, err = serenaTreeHandleIdentity(h, nil)
	if err != nil || !guard.rootIdentity.Directory {
		return guard, errors.Join(err, errors.New("retained task rollback root is not an ordinary directory"))
	}
	return guard, nil
}

func (g *retainedTaskMaterialRoot) close() error {
	if g == nil {
		return nil
	}
	_, childErr := g.root.closeOnce()
	_, parentErr := g.parent.closeOnce()
	return errors.Join(childErr, parentErr)
}
