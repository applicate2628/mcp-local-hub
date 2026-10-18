//go:build windows

package api

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"path/filepath"
)

func restoreSupervisorIntentAbsencePlatform(ctx context.Context, o *SupervisorIntentAbsenceRestorationObservation) (retErr error) {
	namespace := o.input.NamespaceSource
	if err := namespace.ValidateNamespace(ctx, o.path, SupervisorIntentLeaf, o.input.NamespaceSHA256); err != nil {
		return err
	}
	w, ok := namespace.platform.(*windowsSerenaRecordNamespace)
	if !ok || w == nil || w.root == nil {
		return errors.New("intent restoration anchored parent unavailable")
	}
	handle, openErr := ntOpenRelativeWithShareAccess(w.root.handle, filepath.Base(o.path), windows.DELETE|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ)
	if openErr != nil {
		if errors.Is(openErr, statusObjectNameMissing) || errors.Is(openErr, statusNoSuchFile) || errors.Is(openErr, windows.ERROR_FILE_NOT_FOUND) {
			o.noOwnedObject = true
			o.view.Disposition = "already_absent"
			return o.receiveAbsence(ctx)
		}
		return openErr
	}
	held := &serenaTreeHandle{handle: handle}
	o.capture.acquired("intent-restoration-leaf")
	b := newStateFileReadBuilder(ctx, o.path, stateFileReadCapBytes(o.path))
	b.policy = namespace.policy
	b.acquire("intent-restoration-leaf", func() error {
		close, err := held.closeOnce()
		o.view.CloseAttempted = close.Attempted
		o.view.Closed = close.Closed
		o.capture.closed("intent-restoration-leaf", err)
		return err
	})
	defer func() { _, o.localPolicy, retErr = b.finish(nil, retErr) }()
	object, err := serenaTreeHandleIdentity(handle, nil)
	if err != nil {
		return err
	}
	if object.Directory || object.Links != 1 || !sameSerenaTreeObject(object, o.object.object) {
		return errors.New("intent restoration original object identity drift")
	}
	if err = verifyWindowsDACLFromHandleMaskedObserved(handle, windowsDACLSignificantBits, b, "intent-restoration-policy"); err != nil {
		return err
	}
	hash, err := serenaTreeContentHash(ctx, handle, object)
	if err != nil || hash != o.object.sha256 {
		return errors.Join(err, errors.New("intent restoration original bytes differ"))
	}
	if err = namespace.ValidateNamespace(ctx, o.path, SupervisorIntentLeaf, o.input.NamespaceSHA256); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	o.view.MarkAttempted = true
	if err = setFileDeleteOnClose(handle); err != nil {
		return err
	}
	o.view.Marked = true
	// Close the original object once before receiving name absence. The builder's
	// later finalizer consumes its cached close and cannot retry an uncertain FD.
	if err = b.closeResource("intent-restoration-leaf"); err != nil {
		return err
	}
	o.view.Disposition = "removed"
	return o.receiveAbsence(ctx)
}
func (o *SupervisorIntentAbsenceRestorationObservation) receiveAbsence(ctx context.Context) error {
	_, reader, readErr := ReadStateFileInodeAnchoredObserved(ctx, o.path, stateFileReadCapBytes(o.path))
	o.reader = reader
	if reader == nil {
		return errors.Join(readErr, errors.New("intent restoration absence reader missing"))
	}
	if err := reader.ValidateAbsence(ctx, o.path, StateFileReadLeafNotFound); err != nil {
		return errors.Join(readErr, err)
	}
	o.view.AbsenceQualified = true
	return nil
}
