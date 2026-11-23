//go:build windows

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type preparedSerenaFileCloseError struct{ cause error }

func (e *preparedSerenaFileCloseError) Error() string { return "SERENA_PREPARED_FILE_CLOSE_UNSETTLED" }
func (e *preparedSerenaFileCloseError) Unwrap() error { return e.cause }
func preparedSerenaQualifiedFileMissing(err error) bool {
	var closeErr *preparedSerenaFileCloseError
	return err != nil && !errors.As(err, &closeErr) && windowsAnchoredReadErrIsNotExist(err)
}
func preparedSerenaCloseFileHandle(handle windows.Handle) error {
	if err := windows.CloseHandle(handle); err != nil {
		return &preparedSerenaFileCloseError{cause: err}
	}
	return nil
}

// Qualification reads the bytes and identity from one no-follow leaf handle.
// Each ancestor is opened relative to its retained predecessor. This does not
// apply the application-state DACL policy to an installed third-party image.
func qualifyPreparedSerenaFile(ctx context.Context, path string, capBytes int64) (result PreparedSerenaFile, raw []byte, err error) {
	return qualifyPreparedSerenaPath(ctx, path, capBytes, false)
}
func qualifyPreparedSerenaDirectory(ctx context.Context, path string) (PreparedSerenaFile, error) {
	identity, _, err := qualifyPreparedSerenaPath(ctx, path, 1, true)
	return identity, err
}
func qualifyPreparedSerenaPath(ctx context.Context, path string, capBytes int64, directory bool) (result PreparedSerenaFile, raw []byte, err error) {
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || capBytes <= 0 {
		return result, nil, errors.New("SERENA_PREPARED_FILE_INPUT_INVALID")
	}
	volume := filepath.VolumeName(path)
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(path, volume), string(filepath.Separator)), string(filepath.Separator))
	if len(parts) == 0 {
		return result, nil, errors.New("SERENA_PREPARED_FILE_LEAF_MISSING")
	}
	parent, err := ntOpenStateReadRootMode(volume+string(filepath.Separator), windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return result, nil, err
	}
	parentOwned := true
	defer func() {
		if parentOwned {
			err = errors.Join(err, preparedSerenaCloseFileHandle(parent))
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		if !serenaTreeName(name) {
			return result, nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
		}
		if err = ctx.Err(); err != nil {
			return result, nil, err
		}
		dir := true
		next, openErr := ntOpenStateReadRelativeMode(parent, name, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, &dir, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
		if openErr != nil {
			return result, nil, openErr
		}
		closeErr := preparedSerenaCloseFileHandle(parent)
		parent = next
		if closeErr != nil {
			return result, nil, closeErr
		}
	}
	name := parts[len(parts)-1]
	if !serenaTreeName(name) {
		return result, nil, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
	}
	fileKind := directory
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES)
	leaf, err := ntOpenStateReadRelativeMode(parent, name, access, &fileKind, windows.FILE_OPEN, windows.FILE_SHARE_READ, nil)
	if err != nil {
		return result, nil, err
	}
	defer func() { err = errors.Join(err, preparedSerenaCloseFileHandle(leaf)) }()
	return qualifyPreparedSerenaOpenedLeaf(ctx, path, capBytes, directory, leaf)
}

func qualifyPreparedSerenaOpenedLeaf(ctx context.Context, path string, capBytes int64, directory bool, leaf windows.Handle) (result PreparedSerenaFile, raw []byte, err error) {
	before, err := serenaTreeHandleIdentity(leaf, nil)
	if err != nil {
		return result, nil, err
	}
	if before.Directory != directory || before.Bytes < 0 || (!directory && before.Bytes > capBytes) {
		return result, nil, errors.New("SERENA_PREPARED_FILE_SIZE_INVALID")
	}
	if directory {
		after, checkErr := serenaTreeHandleIdentity(leaf, nil)
		if checkErr != nil || !sameSerenaTreeObject(before, after) {
			return result, nil, errors.Join(checkErr, errors.New("SERENA_PREPARED_DIRECTORY_IDENTITY_DRIFT"))
		}
		binding := []byte(before.Device + ":" + before.FileID)
		result = PreparedSerenaFile{Path: path, SHA256: preparedSerenaHash(binding), Device: before.Device, FileID: before.FileID}
		return result, nil, ctx.Err()
	}
	raw = make([]byte, 0, int(before.Bytes))
	buffer := make([]byte, 32768)
	for {
		if err = ctx.Err(); err != nil {
			return result, nil, err
		}
		var n uint32
		readErr := windows.ReadFile(leaf, buffer, &n, nil)
		if readErr != nil && !errors.Is(readErr, windows.ERROR_HANDLE_EOF) {
			return result, nil, readErr
		}
		if int64(len(raw))+int64(n) > capBytes {
			return result, nil, errors.New("SERENA_PREPARED_FILE_SIZE_INVALID")
		}
		raw = append(raw, buffer[:n]...)
		if n == 0 || errors.Is(readErr, windows.ERROR_HANDLE_EOF) {
			break
		}
	}
	after, err := serenaTreeHandleIdentity(leaf, nil)
	if err != nil {
		return result, nil, err
	}
	if !sameSerenaTreeObject(before, after) || int64(len(raw)) != before.Bytes {
		return result, nil, errors.New("SERENA_PREPARED_FILE_IDENTITY_DRIFT")
	}
	h := sha256.Sum256(raw)
	result = PreparedSerenaFile{Path: path, SHA256: hex.EncodeToString(h[:]), Device: before.Device, FileID: before.FileID}
	return result, raw, nil
}

type preparedSerenaExecutableSlot struct {
	native                        stateReadNativeOpenOutcome
	closeAttempted, closeReturned bool
	closeErr                      error
}

func (s *preparedSerenaExecutableSlot) hasHandle() bool {
	return s.native.handle != 0 && s.native.handle != windows.InvalidHandle
}

func (s *preparedSerenaExecutableSlot) close(closeNative func(windows.Handle) error) error {
	if !s.hasHandle() || s.native.err != nil || !s.native.entered || !s.native.returned {
		return nil // a failed NT call's raw output is not authorized custody
	}
	if !s.closeAttempted {
		s.closeAttempted = true
		s.closeErr = closeNative(s.native.handle)
		s.closeReturned = true
	}
	return s.closeErr
}

type preparedSerenaExecutableObservation struct {
	slots     []*preparedSerenaExecutableSlot
	finalized bool
}

func (o *preparedSerenaExecutableObservation) ResourcesFinalized() bool {
	if o == nil || !o.finalized {
		return false
	}
	for _, slot := range o.slots {
		if slot.native.entered && !slot.native.returned || slot.hasHandle() &&
			(slot.native.err != nil || !slot.closeAttempted || !slot.closeReturned || slot.closeErr != nil) {
			return false
		}
	}
	return true
}

type preparedSerenaExecutableOps struct {
	root  func(string) stateReadNativeOpenOutcome
	child func(windows.Handle, string, bool) stateReadNativeOpenOutcome
	close func(windows.Handle) error
}

func qualifyPreparedSerenaExecutableObserved(ctx context.Context, path string, capBytes int64) (PreparedSerenaFile, *preparedSerenaExecutableObservation, error) {
	return qualifyPreparedSerenaExecutableWithOps(ctx, path, capBytes, preparedSerenaExecutableOps{
		root: func(root string) stateReadNativeOpenOutcome {
			return ntOpenStateReadRootModeObserved(root, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
		},
		child: func(parent windows.Handle, name string, directory bool) stateReadNativeOpenOutcome {
			access, share := uint32(windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES), uint32(windows.FILE_SHARE_READ)
			if directory {
				access, share = windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE
			}
			return ntOpenStateReadRelativeModeObserved(parent, name, access, &directory, windows.FILE_OPEN, share, nil)
		},
		close: preparedSerenaCloseFileHandle,
	})
}

func qualifyPreparedSerenaExecutableWithOps(ctx context.Context, path string, capBytes int64, ops preparedSerenaExecutableOps) (result PreparedSerenaFile, evidence *preparedSerenaExecutableObservation, retErr error) {
	evidence = &preparedSerenaExecutableObservation{}
	defer func() {
		for i := len(evidence.slots) - 1; i >= 0; i-- {
			slot := evidence.slots[i]
			if !slot.closeAttempted {
				retErr = errors.Join(retErr, slot.close(ops.close))
			}
		}
		evidence.finalized = true
	}()
	if ctx == nil || !filepath.IsAbs(path) || filepath.Clean(path) != path || capBytes <= 0 {
		return result, evidence, errors.New("SERENA_PREPARED_FILE_INPUT_INVALID")
	}
	if err := ctx.Err(); err != nil {
		return result, evidence, err
	}
	receive := func(out stateReadNativeOpenOutcome) (*preparedSerenaExecutableSlot, error) {
		slot := &preparedSerenaExecutableSlot{native: out}
		evidence.slots = append(evidence.slots, slot)
		if out.err != nil {
			return slot, out.err
		}
		if !out.entered || !out.returned || !slot.hasHandle() {
			return slot, errors.New("original executable native handle unavailable")
		}
		return slot, nil
	}
	volume := filepath.VolumeName(path)
	parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(path, volume), string(filepath.Separator)), string(filepath.Separator))
	parent, err := receive(ops.root(volume + string(filepath.Separator)))
	if err != nil {
		return result, evidence, err
	}
	for i, name := range parts {
		if !serenaTreeName(name) {
			return result, evidence, errors.New("SERENA_PREPARED_FILE_COMPONENT_INVALID")
		}
		if err := ctx.Err(); err != nil {
			return result, evidence, err
		}
		child, err := receive(ops.child(parent.native.handle, name, i < len(parts)-1))
		if err != nil {
			return result, evidence, err
		}
		if err := parent.close(ops.close); err != nil {
			// The child has already been retained for its own final close.
			return result, evidence, err
		}
		parent = child
	}
	result, _, err = qualifyPreparedSerenaOpenedLeaf(ctx, path, capBytes, false, parent.native.handle)
	return result, evidence, err
}

func revalidatePreparedSerenaFile(ctx context.Context, expected PreparedSerenaFile) error {
	actual, _, err := qualifyPreparedSerenaFile(ctx, expected.Path, 64<<20)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("SERENA_PREPARED_FILE_IDENTITY_DRIFT")
	}
	return nil
}
