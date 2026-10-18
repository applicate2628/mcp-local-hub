//go:build windows

package api

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"path/filepath"
	"sync"
	"time"
)

type windowsSerenaRecordNamespace struct {
	mu                                    sync.Mutex
	source                                StateFileFreshNamespaceSource
	parentPath, rootPath                  string
	validationPath                        string
	deadline                              time.Time
	parent, root                          *serenaTreeHandle
	runtimeRoot, packageRoot              *serenaTreeHandle
	runtimeIdentity, packageIdentityValue SerenaTreeObject
	runtimePolicy                         *ConfigParentPolicyObservation
	parentIdentity, rootIdentity          SerenaTreeObject
	policy                                *StateFileReadObservation
	parentPolicy, rootPolicy              *ConfigParentPolicyObservation
	createMissing                         bool
	borrows                               int
	closeRequested, closeAttempted        bool
	closeProof                            *StateFileFreshNamespaceCloseObservation
}

func acquireSerenaRecordNamespacePlatform(ctx context.Context, io PreparedSerenaRecordIO, key, path string) (result *SerenaRecordNamespaceObservation, retErr error) {
	return acquireSerenaRecordNamespacePlatformMode(ctx, io, key, path, true)
}
func acquireSerenaRecordNamespaceReadPlatform(ctx context.Context, io PreparedSerenaRecordIO, key, path string) (*SerenaRecordNamespaceObservation, error) {
	return acquireSerenaRecordNamespacePlatformMode(ctx, io, key, path, false)
}
func acquireSerenaRecordNamespacePlatformMode(ctx context.Context, io PreparedSerenaRecordIO, key, path string, create bool) (result *SerenaRecordNamespaceObservation, retErr error) {
	result = &SerenaRecordNamespaceObservation{io: io, key: key, path: path, readOnly: !create}
	result.platform, retErr = acquireStateFreshPhysicalNamespace(ctx, result, io.Root, key, create, path, windowsDACLSignificantBits)
	if retErr == nil {
		w := result.platform.(*windowsSerenaRecordNamespace)
		w.runtimeRoot, w.packageRoot = w.parent, w.root
		w.runtimeIdentity, w.packageIdentityValue = w.parentIdentity, w.rootIdentity
		w.runtimePolicy = w.parentPolicy
		_, retErr = w.namespaceDigest(ctx)
		if retErr != nil {
			_, closeErr := w.close()
			retErr = errors.Join(retErr, closeErr)
		}
	}
	return result, retErr
}
func acquireSerenaOperationNamespacePlatform(ctx context.Context, io PreparedSerenaRecordIO, key, operationID, path string, create bool) (result *SerenaRecordNamespaceObservation, retErr error) {
	result = &SerenaRecordNamespaceObservation{io: io, key: key, path: path, role: "prepared-operation", operationID: operationID}
	result.platform, retErr = acquireStateFreshPhysicalNamespace(ctx, result, filepath.Join(io.Root, key), "operations", create, path, windowsDACLSignificantBits)
	if retErr == nil {
		w := result.platform.(*windowsSerenaRecordNamespace)
		defer func() {
			if retErr != nil {
				_, closeErr := w.close()
				retErr = errors.Join(retErr, closeErr)
			}
		}()
		exists, policy, policyErr := SecureConfigLockParentPolicyObserved(ctx, io.Root, false)
		w.runtimePolicy = policy
		if validation := policy.Validate(ctx, io.Root, false); policyErr != nil || validation != nil || !exists {
			return result, errors.Join(policyErr, validation, errors.New("SERENA_OPERATION_RUNTIME_ROOT_UNQUALIFIED"))
		}
		runtimeHandle, err := ntOpenStateReadRootMode(io.Root, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
		if err != nil {
			return result, err
		}
		w.runtimeRoot = &serenaTreeHandle{handle: runtimeHandle}
		w.runtimeIdentity, retErr = serenaTreeHandleIdentity(runtimeHandle, nil)
		if retErr != nil {
			return result, retErr
		}
		if !namespaceIdentityMatches(w.runtimeIdentity, policy.Snapshot().FinalIdentity) {
			return result, errors.New("SERENA_OPERATION_RUNTIME_ROOT_DRIFT")
		}
		directory := true
		packageHandle, err := ntOpenStateReadRelativeMode(runtimeHandle, key, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, &directory, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
		if err != nil {
			return result, err
		}
		w.packageRoot = &serenaTreeHandle{handle: packageHandle}
		w.packageIdentityValue, retErr = serenaTreeHandleIdentity(packageHandle, nil)
		if retErr != nil {
			return result, retErr
		}
		if !sameSerenaNamespaceID(w.packageIdentityValue, w.parentIdentity) {
			return result, errors.New("SERENA_OPERATION_PACKAGE_PARENT_DRIFT")
		}
		_, retErr = w.namespaceDigest(ctx)
	}
	return result, retErr
}
func (w *windowsSerenaRecordNamespace) namespaceDigest(ctx context.Context) (string, error) {
	identity, err := w.packageIdentity(ctx)
	if err != nil {
		return "", err
	}
	source, ok := w.source.(*SerenaRecordNamespaceObservation)
	if !ok {
		return "", errors.New("SERENA_PACKAGE_NAMESPACE_SOURCE_MISMATCH")
	}
	digest, err := serenaPackageNamespaceDigest(source.key, identity)
	if err != nil {
		return "", err
	}
	if bound := source.io.packageBinding; bound != nil && (bound.root != source.io.Root || bound.key != source.key || bound.operationID != source.io.operationID || bound.sourceEpoch != source.io.sourceSHA256 || bound.digest != digest || !sameSerenaNamespaceID(bound.identity.runtimeRoot, identity.runtimeRoot) || !sameSerenaNamespaceID(bound.identity.packageRoot, identity.packageRoot)) {
		return "", errors.New("SERENA_PACKAGE_NAMESPACE_IDENTITY_DRIFT")
	}
	return digest, nil
}

func (w *windowsSerenaRecordNamespace) packageIdentity(ctx context.Context) (serenaPackageNamespaceIdentity, error) {
	if err := w.validate(ctx, w.validationPath); err != nil {
		return serenaPackageNamespaceIdentity{}, err
	}
	source, ok := w.source.(*SerenaRecordNamespaceObservation)
	if !ok || w.runtimeRoot == nil || w.packageRoot == nil || w.runtimeRoot.close.Attempted || w.packageRoot.close.Attempted {
		return serenaPackageNamespaceIdentity{}, errors.New("SERENA_PACKAGE_NAMESPACE_UNAVAILABLE")
	}
	create := source.role != "prepared-operation" && !source.readOnly
	if err := w.runtimePolicy.Validate(ctx, source.io.Root, create); err != nil {
		return serenaPackageNamespaceIdentity{}, err
	}
	runtimeID, err := serenaTreeHandleIdentity(w.runtimeRoot.handle, nil)
	if err != nil {
		return serenaPackageNamespaceIdentity{}, err
	}
	packageID, err := serenaTreeHandleIdentity(w.packageRoot.handle, nil)
	if err != nil {
		return serenaPackageNamespaceIdentity{}, err
	}
	if !sameSerenaNamespaceID(runtimeID, w.runtimeIdentity) || !sameSerenaNamespaceID(packageID, w.packageIdentityValue) {
		return serenaPackageNamespaceIdentity{}, errors.New("SERENA_PACKAGE_NAMESPACE_IDENTITY_DRIFT")
	}
	return serenaPackageNamespaceIdentity{runtimeRoot: runtimeID, packageRoot: packageID}, nil
}

func (w *windowsSerenaRecordNamespace) leafParentDigest(ctx context.Context) (string, error) {
	common, err := w.namespaceDigest(ctx)
	if err != nil {
		return "", err
	}
	source := w.source.(*SerenaRecordNamespaceObservation)
	role := "prepared-ready"
	if source.role == "prepared-operation" {
		role = source.role
	}
	return serenaRecordLeafParentDigest(common, role, filepath.Base(source.path), w.rootIdentity)
}
func acquireSupervisorStateNamespacePlatform(ctx context.Context, source *SupervisorStateNamespaceObservation) (serenaRecordNamespacePlatform, error) {
	mask := windowsDACLWriteOrAdminBits
	if source.policy.Snapshot().EffectiveStrict {
		mask = windowsDACLSignificantBits
	}
	w, err := acquireStateFreshPhysicalNamespace(ctx, source, filepath.Dir(source.root), filepath.Base(source.root), source.request.CreateMissing, filepath.Join(source.root, "supervisor-state.json"), mask)
	if err == nil {
		source.namespaceSHA256, err = supervisorNamespaceDigest(source.root, source.request.SourceEpoch, w.rootIdentity)
	}
	return w, err
}
func acquireStateFreshPhysicalNamespace(ctx context.Context, source StateFileFreshNamespaceSource, parentPath, key string, create bool, path string, mask uint32) (w *windowsSerenaRecordNamespace, retErr error) {
	if ctx == nil {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_CONTEXT_MISSING")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_DEADLINE_MISSING")
	}
	parentExists, parentPolicy, parentErr := SecureConfigLockParentPolicyObserved(ctx, parentPath, create)
	parentValidation := parentPolicy.Validate(ctx, parentPath, create)
	if parentErr != nil || parentValidation != nil || !parentExists {
		return nil, errors.Join(parentErr, parentValidation, errors.New("STATE_FRESH_NAMESPACE_PARENT_UNAVAILABLE"))
	}
	w = &windowsSerenaRecordNamespace{source: source, parentPath: parentPath, rootPath: filepath.Join(parentPath, key), validationPath: path, parentPolicy: parentPolicy, createMissing: create}
	w.deadline, _ = ctx.Deadline()
	defer func() {
		if retErr != nil {
			_, closeErr := w.close()
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	parent, err := ntOpenStateReadRootMode(parentPath, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return w, err
	}
	w.parent = &serenaTreeHandle{handle: parent}
	w.parentIdentity, err = serenaTreeHandleIdentity(parent, nil)
	if err != nil {
		return w, err
	}
	if !namespaceIdentityMatches(w.parentIdentity, parentPolicy.Snapshot().FinalIdentity) {
		return w, errors.New("STATE_FRESH_NAMESPACE_PARENT_PREFLIGHT_DRIFT")
	}
	rootExists, rootPolicy, rootErr := SecureConfigLockParentPolicyObserved(ctx, w.rootPath, create)
	w.rootPolicy = rootPolicy
	rootValidation := rootPolicy.Validate(ctx, w.rootPath, create)
	if rootErr != nil || rootValidation != nil || !rootExists {
		return w, errors.Join(rootErr, rootValidation, errors.New("STATE_FRESH_NAMESPACE_ROOT_PREFLIGHT_UNAVAILABLE"))
	}
	directory := true
	root, err := ntOpenStateReadRelativeMode(parent, key, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, &directory, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
	if err != nil {
		return w, err
	}
	w.root = &serenaTreeHandle{handle: root}
	w.rootIdentity, err = serenaTreeHandleIdentity(root, nil)
	if err != nil {
		return w, err
	}
	if !namespaceIdentityMatches(w.rootIdentity, rootPolicy.Snapshot().FinalIdentity) {
		return w, errors.New("STATE_FRESH_NAMESPACE_ROOT_PREFLIGHT_DRIFT")
	}
	// Receive the SAME descriptor/ACE owner's actual subordinate close results.
	b := newStateFileReadBuilder(ctx, path, 1)
	violations, policyErr := windowsDACLAllowlistViolationsObserved(root, mask, b, "record-namespace")
	if len(violations) != 0 {
		policyErr = errors.Join(policyErr, errors.New("SERENA_RECORD_NAMESPACE_POLICY_REFUSED"))
	}
	_, w.policy, err = b.finish(nil, policyErr)
	return w, errors.Join(policyErr, err)
}
func namespaceIdentityMatches(owned SerenaTreeObject, expected *ConfigParentDirectoryIdentityV1) bool {
	return expected != nil && owned.Device == fmt.Sprintf("%016x", expected.VolumeSerialNumber) && owned.FileID == fmt.Sprintf("%x", expected.FileID)
}
func (w *windowsSerenaRecordNamespace) validate(ctx context.Context, path string) error {
	if w == nil || w.source == nil || ctx == nil || filepath.Dir(path) != w.rootPath {
		return errors.New("SERENA_RECORD_NAMESPACE_BINDING_MISMATCH")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.parentPolicy.Validate(ctx, w.parentPath, w.createMissing); err != nil {
		return err
	}
	if err := w.rootPolicy.Validate(ctx, w.rootPath, w.createMissing); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); !ok || deadline.After(w.deadline) {
		return errors.New("STATE_FRESH_NAMESPACE_DEADLINE_RENEWED")
	}
	w.mu.Lock()
	closed := w.closeRequested || w.closeAttempted
	w.mu.Unlock()
	if closed || w.parent == nil || w.root == nil || w.policy == nil || !w.policy.finalized || !w.policy.view.OwnedResourcesComplete || !w.policy.view.BorrowedResourcesComplete {
		return errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	p, err := serenaTreeHandleIdentity(w.parent.handle, nil)
	if err != nil {
		return err
	}
	r, err := serenaTreeHandleIdentity(w.root.handle, nil)
	if err != nil {
		return err
	}
	if p.Device != w.parentIdentity.Device || p.FileID != w.parentIdentity.FileID || r.Device != w.rootIdentity.Device || r.FileID != w.rootIdentity.FileID {
		return errors.New("SERENA_RECORD_NAMESPACE_IDENTITY_DRIFT")
	}
	return nil
}
func (w *windowsSerenaRecordNamespace) borrow(ctx context.Context, body func() error) (retErr error) {
	if body == nil {
		return errors.New("SERENA_RECORD_NAMESPACE_BODY_MISSING")
	}
	w.mu.Lock()
	if w.closeRequested || w.closeAttempted {
		w.mu.Unlock()
		return errors.New("SERENA_RECORD_NAMESPACE_CLOSING")
	}
	w.borrows++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.borrows--
		closePending := w.borrows == 0 && w.closeRequested
		w.mu.Unlock()
		if closePending {
			_, closeErr := w.close()
			retErr = errors.Join(retErr, closeErr)
		}
	}()
	if err := w.validate(ctx, w.validationPath); err != nil {
		return err
	}
	return body()
}
func (w *windowsSerenaRecordNamespace) close() (*StateFileFreshNamespaceCloseObservation, error) {
	if w == nil {
		return nil, errors.New("SERENA_RECORD_NAMESPACE_UNAVAILABLE")
	}
	w.mu.Lock()
	if w.closeProof != nil {
		proof := w.closeProof
		w.mu.Unlock()
		return proof, proof.err
	}
	w.closeRequested = true
	if w.borrows != 0 {
		w.mu.Unlock()
		return nil, errors.New("SERENA_RECORD_NAMESPACE_BORROW_ACTIVE")
	}
	if w.closeAttempted {
		w.mu.Unlock()
		return nil, errors.New("SERENA_RECORD_NAMESPACE_CLOSE_IN_FLIGHT")
	}
	w.closeAttempted = true
	w.mu.Unlock()
	var closeErr error
	for _, handle := range []*serenaTreeHandle{w.root, w.parent, w.packageRoot, w.runtimeRoot} {
		if handle != nil {
			_, err := handle.closeOnce()
			closeErr = errors.Join(closeErr, err)
		}
	}
	if w.runtimePolicy != nil && !w.runtimePolicy.Snapshot().ResourcesComplete {
		closeErr = errors.Join(closeErr, errors.New("SERENA_PACKAGE_RUNTIME_POLICY_RESOURCES_PENDING"))
	}
	if w.policy != nil && (!w.policy.view.OwnedResourcesComplete || !w.policy.view.BorrowedResourcesComplete) {
		closeErr = errors.Join(closeErr, errors.New("SERENA_RECORD_NAMESPACE_POLICY_RESOURCE_PENDING"))
	}
	proof := &StateFileFreshNamespaceCloseObservation{source: w.source, closed: closeErr == nil, err: closeErr}
	w.mu.Lock()
	w.closeProof = proof
	w.mu.Unlock()
	return proof, closeErr
}
func (w *windowsSerenaRecordNamespace) validateClose(proof *StateFileFreshNamespaceCloseObservation) error {
	if w == nil || proof == nil {
		return errors.New("STATE_FRESH_NAMESPACE_CLOSE_UNSETTLED")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closeProof != proof || !w.closeAttempted || !proof.closed || proof.err != nil {
		return errors.New("STATE_FRESH_NAMESPACE_CLOSE_UNSETTLED")
	}
	return nil
}
