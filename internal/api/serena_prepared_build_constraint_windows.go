//go:build windows

package api

import (
	"context"
	"errors"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func acquirePreparedSerenaBuildConstraint(ctx context.Context, tree *SerenaOwnedTree) (result *preparedSerenaBuildConstraint, retErr error) {
	if ctx == nil || tree == nil || tree.request.Kind != "attempt" {
		return nil, errors.New("SERENA_BUILD_CONSTRAINT_ATTEMPT_REQUIRED")
	}
	owner, ok := tree.platform.(*windowsSerenaOwnedTree)
	if !ok || owner == nil || owner.request.Kind != "attempt" || owner.readExisting || owner.closeResult.Attempted || owner.buildConstraintAttempted {
		return nil, errors.New("SERENA_BUILD_CONSTRAINT_ATTEMPT_UNAVAILABLE")
	}
	root, err := owner.workingDirectory(ctx)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, preparedSerenaBuildConstraintName)
	owner.buildConstraintAttempted = true
	result = &preparedSerenaBuildConstraint{tree: tree}
	owner.buildConstraint = result
	builder := newStateFileReadBuilder(ctx, path, int64(len(preparedSerenaBuildConstraintContent)))
	defer func() {
		_, owner.buildConstraintCreation, retErr = builder.finish(nil, retErr)
	}()
	descriptor, _, err := buildAllowlistSDObserved(builder, "build-constraint-create-descriptor")
	if err != nil {
		return result, err
	}
	directory := false
	writer, err := ntOpenStateReadRelativeMode(owner.root.handle, preparedSerenaBuildConstraintName,
		windows.FILE_WRITE_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, &directory,
		windows.FILE_CREATE, windows.FILE_SHARE_READ, descriptor)
	if err != nil {
		return result, err
	}
	owner.buildConstraintWriter = &serenaTreeHandle{handle: writer}
	if err = windowsWriteAll(writer, []byte(preparedSerenaBuildConstraintContent)); err != nil {
		return result, err
	}
	if err = windows.FlushFileBuffers(writer); err != nil {
		return result, err
	}
	created, err := serenaTreeHandleIdentity(writer, nil)
	if err != nil || created.Directory || created.Bytes != int64(len(preparedSerenaBuildConstraintContent)) || created.Links != 1 {
		return result, errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_CREATED_FILE_INVALID"))
	}
	if _, err = owner.buildConstraintWriter.closeOnce(); err != nil {
		return result, err
	}
	// The new read hold denies writes and replacement until UV's original
	// native and I/O owners settle. It is opened beneath the same attempt.
	reader, err := ntOpenStateReadRelativeMode(owner.root.handle, preparedSerenaBuildConstraintName,
		windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, &directory,
		windows.FILE_OPEN, windows.FILE_SHARE_READ, nil)
	if err != nil {
		return result, err
	}
	owner.buildConstraintReader = &serenaTreeHandle{handle: reader}
	actual, err := serenaTreeHandleIdentity(reader, nil)
	if err != nil || actual.Device != created.Device || actual.FileID != created.FileID || actual.Directory || actual.Bytes != created.Bytes || actual.Links != 1 {
		return result, errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_REOPEN_DRIFT"))
	}
	violations, err := windowsDACLAllowlistViolationsObserved(reader, windowsDACLSignificantBits, builder, "build-constraint-leaf")
	if err != nil || len(violations) != 0 {
		return result, errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_POLICY_REFUSED"))
	}
	digest, err := owner.buildConstraintHash(ctx, actual)
	if err != nil {
		return result, err
	}
	result.file = PreparedSerenaFile{Path: path, SHA256: digest, Device: actual.Device, FileID: actual.FileID}
	return result, nil
}

func (owner *windowsSerenaOwnedTree) buildConstraintHash(ctx context.Context, identity SerenaTreeObject) (string, error) {
	if identity.Directory || identity.Links != 1 || identity.Bytes != int64(len(preparedSerenaBuildConstraintContent)) {
		return "", errors.New("SERENA_BUILD_CONSTRAINT_FILE_INVALID")
	}
	streams, err := owner.observeStreams(ctx, owner.buildConstraintReader.handle)
	if err != nil || len(streams) != 1 || streams[0].Bytes != identity.Bytes {
		return "", errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_STREAM_INVALID"))
	}
	if _, err = windows.Seek(owner.buildConstraintReader.handle, 0, 0); err != nil {
		return "", err
	}
	digest, err := serenaTreeContentHash(ctx, owner.buildConstraintReader.handle, identity)
	if err != nil || digest != preparedSerenaHash([]byte(preparedSerenaBuildConstraintContent)) {
		return "", errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_CONTENT_DRIFT"))
	}
	return digest, nil
}

func preparedSerenaBuildConstraintOwner(constraint *preparedSerenaBuildConstraint) (*windowsSerenaOwnedTree, error) {
	if constraint == nil || constraint.tree == nil {
		return nil, errors.New("SERENA_BUILD_CONSTRAINT_CAPABILITY_MISSING")
	}
	owner, ok := constraint.tree.platform.(*windowsSerenaOwnedTree)
	if !ok || owner == nil || owner.buildConstraint != constraint {
		return nil, errors.New("SERENA_BUILD_CONSTRAINT_SOURCE_DIFFERS")
	}
	return owner, nil
}

func beginPreparedSerenaBuildConstraintUse(ctx context.Context, constraint *preparedSerenaBuildConstraint) error {
	owner, err := preparedSerenaBuildConstraintOwner(constraint)
	if err != nil {
		return err
	}
	if ctx == nil || owner.buildConstraintInUse || owner.buildConstraintReader == nil ||
		owner.buildConstraintReader.close.Attempted || owner.buildConstraintWriter == nil ||
		!owner.buildConstraintWriter.close.Closed || owner.buildConstraintWriter.close.Error != nil ||
		owner.buildConstraintCreation == nil || !owner.buildConstraintCreation.finalized ||
		!owner.buildConstraintCreation.view.OwnedResourcesComplete || !owner.buildConstraintCreation.view.BorrowedResourcesComplete {
		return errors.New("SERENA_BUILD_CONSTRAINT_CUSTODY_UNAVAILABLE")
	}
	root, err := owner.workingDirectory(ctx)
	if err != nil {
		return err
	}
	actual, err := serenaTreeHandleIdentity(owner.buildConstraintReader.handle, nil)
	if err != nil || constraint.file.Path != filepath.Join(root, preparedSerenaBuildConstraintName) ||
		actual.Device != constraint.file.Device || actual.FileID != constraint.file.FileID {
		return errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_IDENTITY_DRIFT"))
	}
	digest, err := owner.buildConstraintHash(ctx, actual)
	if err != nil || digest != constraint.file.SHA256 {
		return errors.Join(err, errors.New("SERENA_BUILD_CONSTRAINT_CONTENT_DRIFT"))
	}
	owner.buildConstraintInUse = true
	return nil
}

func finishPreparedSerenaBuildConstraintUse(constraint *preparedSerenaBuildConstraint, result preparedSerenaCommandResult) error {
	owner, err := preparedSerenaBuildConstraintOwner(constraint)
	if err != nil {
		return err
	}
	if !owner.buildConstraintInUse {
		return errors.New("SERENA_BUILD_CONSTRAINT_USE_MISSING")
	}
	if result.containedAttempted && (!result.Receipt.NativeSettlement.Complete() || !result.Receipt.IOSettlement.Complete()) {
		return errors.New("SERENA_BUILD_CONSTRAINT_USE_PENDING")
	}
	// A known local preflight return releases only this pin. It does not
	// manufacture a no-child receipt or permit deletion of attempt roots.
	owner.buildConstraintInUse = false
	return owner.closeBuildConstraint()
}

func closePreparedSerenaBuildConstraint(constraint *preparedSerenaBuildConstraint) error {
	if constraint == nil {
		return nil
	}
	owner, err := preparedSerenaBuildConstraintOwner(constraint)
	if err != nil {
		return err
	}
	return owner.closeBuildConstraint()
}

func (owner *windowsSerenaOwnedTree) closeBuildConstraint() error {
	_, writerErr := owner.buildConstraintWriter.closeOnce()
	var readerErr error
	if owner.buildConstraintInUse {
		// Keep the read hold alive when the original child outcome is still
		// unknown. Owner.Close retains this debt instead of reporting closed.
		readerErr = errors.New("SERENA_BUILD_CONSTRAINT_USE_PENDING")
	} else {
		_, readerErr = owner.buildConstraintReader.closeOnce()
	}
	var observationErr error
	if owner.buildConstraintAttempted && (owner.buildConstraintCreation == nil ||
		!owner.buildConstraintCreation.finalized || !owner.buildConstraintCreation.view.OwnedResourcesComplete ||
		!owner.buildConstraintCreation.view.BorrowedResourcesComplete) {
		observationErr = errors.New("SERENA_BUILD_CONSTRAINT_CREATE_RESOURCES_PENDING")
	}
	return errors.Join(writerErr, readerErr, observationErr)
}
