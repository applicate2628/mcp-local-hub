//go:build windows

package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type serenaTreeHandle struct {
	handle windows.Handle
	close  SerenaTreeClose
}

func (h *serenaTreeHandle) closeOnce() (SerenaTreeClose, error) {
	if h == nil {
		return SerenaTreeClose{}, nil
	}
	if !h.close.Attempted {
		h.close.Attempted = true
		h.close.Error = windows.CloseHandle(h.handle)
		h.close.Closed = h.close.Error == nil
	}
	return h.close, h.close.Error
}

type windowsSerenaOwnedTree struct {
	readExisting                 bool
	inventoryCloseError          error
	request                      SerenaOwnedTreeRequest
	parent, root                 *serenaTreeHandle
	parentIdentity, rootIdentity SerenaTreeObject
	rootName                     string
	closeResult                  SerenaTreeClose
	removeAttempted              bool
	removeResult                 SerenaTreeCleanupReceipt
	parentPolicy                 *ConfigParentPolicyObservation
	creation                     *StateFileReadObservation
}

func serenaTreeHandleIdentity(h windows.Handle, relative []string) (SerenaTreeObject, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return SerenaTreeObject{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return SerenaTreeObject{}, errors.New("SERENA_TREE_REPARSE")
	}
	// Reuse the existing providerFileIDInfo ABI/layout assertions, retaining
	// the actual 128-bit identity rather than truncating it to FileIndex64.
	var id providerFileIDInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileIdInfo, (*byte)(unsafe.Pointer(&id)), uint32(unsafe.Sizeof(id))); err != nil {
		return SerenaTreeObject{}, err
	}
	return SerenaTreeObject{Relative: append([]string(nil), relative...), Device: fmt.Sprintf("%016x", id.volumeSerialNumber), FileID: fmt.Sprintf("%x", id.fileID), Directory: info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0, Bytes: int64(info.FileSizeHigh)<<32 | int64(info.FileSizeLow), Links: info.NumberOfLinks, Modified: uint64(info.LastWriteTime.HighDateTime)<<32 | uint64(info.LastWriteTime.LowDateTime)}, nil
}
func sameSerenaTreeObject(a, b SerenaTreeObject) bool {
	return a.Device == b.Device && a.FileID == b.FileID && a.Directory == b.Directory && a.Bytes == b.Bytes && a.Links == b.Links && a.Modified == b.Modified
}
func serenaTreeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "\\/:\x00")
}

func acquireSerenaTreePlatform(ctx context.Context, r SerenaOwnedTreeRequest) (result *SerenaOwnedTree, err error) {
	return acquireSerenaTreePlatformMode(ctx, r, false)
}
func acquireSerenaPublishedGenerationPlatform(ctx context.Context, r SerenaOwnedTreeRequest) (serenaTreePlatform, error) {
	tree, err := acquireSerenaTreePlatformMode(ctx, r, true)
	if tree == nil {
		return nil, err
	}
	return tree.platform, err
}
func acquireSerenaTreePlatformMode(ctx context.Context, r SerenaOwnedTreeRequest, readExisting bool) (result *SerenaOwnedTree, err error) {
	if !filepath.IsAbs(r.Namespace) || filepath.Clean(r.Namespace) != r.Namespace {
		return nil, errors.New("SERENA_TREE_NAMESPACE_INVALID")
	}
	branch := "generations"
	leaf := r.GenerationID
	if r.Kind == "attempt" {
		branch = "attempts"
		leaf = r.OperationID
	}
	if r.Kind == "qualification" {
		branch = "qualifications"
		leaf = r.QualificationID
	}
	parentPath := filepath.Join(r.Namespace, r.PackageIdentitySHA256, branch)
	exists, parentPolicy, parentErr := SecureConfigLockParentPolicyObserved(ctx, parentPath, !readExisting)
	if validationErr := parentPolicy.Validate(ctx, parentPath, !readExisting); parentErr != nil || validationErr != nil || !exists {
		return nil, errors.Join(parentErr, validationErr, errors.New("SERENA_TREE_PARENT_POLICY_UNQUALIFIED"))
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	parent, err := ntOpenStateReadRootMode(parentPath, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return nil, err
	}
	w := &windowsSerenaOwnedTree{request: r, parent: &serenaTreeHandle{handle: parent}, rootName: leaf, parentPolicy: parentPolicy, readExisting: readExisting}
	result = &SerenaOwnedTree{request: r, platform: w}
	b := newStateFileReadBuilder(ctx, filepath.Join(parentPath, leaf), 1)
	defer func() {
		_, w.creation, err = b.finish(nil, err)
		if err != nil {
			_, closeErr := w.close()
			err = errors.Join(err, closeErr)
		}
	}()
	if w.parentIdentity, err = serenaTreeHandleIdentity(parent, nil); err != nil {
		return result, err
	}
	if !namespaceIdentityMatches(w.parentIdentity, parentPolicy.Snapshot().FinalIdentity) {
		return result, errors.New("SERENA_TREE_PARENT_PREFLIGHT_DRIFT")
	}
	violations, policyErr := windowsDACLAllowlistViolationsObserved(parent, windowsDACLSignificantBits, b, "owned-tree-parent")
	if policyErr != nil || len(violations) != 0 {
		err = errors.Join(policyErr, errors.New("SERENA_TREE_PARENT_POLICY_REFUSED"))
		return result, err
	}
	var sd *windows.SECURITY_DESCRIPTOR
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.READ_CONTROL)
	disposition := uint32(windows.FILE_OPEN)
	if !readExisting {
		sd, _, err = buildAllowlistSDObserved(b, "owned-tree-create-descriptor")
		if err != nil {
			return result, err
		}
		access |= windows.DELETE
		disposition = windows.FILE_CREATE
	}
	directory := true
	root, err := ntOpenStateReadRelativeMode(parent, leaf, access, &directory, disposition, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sd)
	if err != nil {
		return result, err
	}
	w.root = &serenaTreeHandle{handle: root}
	w.rootIdentity, err = serenaTreeHandleIdentity(root, nil)
	return result, err
}
func (w *windowsSerenaOwnedTree) checkRoot(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.root == nil || w.parent == nil || w.root.close.Attempted || w.parent.close.Attempted {
		return errors.New("SERENA_TREE_ROOT_UNAVAILABLE")
	}
	if w.creation == nil || !w.creation.finalized || !w.creation.view.OwnedResourcesComplete || !w.creation.view.BorrowedResourcesComplete {
		return errors.New("SERENA_TREE_ACQUISITION_RESOURCES_PENDING")
	}
	branch := "generations"
	if w.request.Kind == "attempt" {
		branch = "attempts"
	}
	if w.request.Kind == "qualification" {
		branch = "qualifications"
	}
	if err := w.parentPolicy.Validate(ctx, filepath.Join(w.request.Namespace, w.request.PackageIdentitySHA256, branch), !w.readExisting); err != nil {
		return err
	}
	p, err := serenaTreeHandleIdentity(w.parent.handle, nil)
	if err != nil {
		return err
	}
	if p.Device != w.parentIdentity.Device || p.FileID != w.parentIdentity.FileID {
		return errors.New("SERENA_TREE_PARENT_DRIFT")
	}
	r, err := serenaTreeHandleIdentity(w.root.handle, nil)
	if err != nil {
		return err
	}
	if r.Device != w.rootIdentity.Device || r.FileID != w.rootIdentity.FileID {
		return errors.New("SERENA_TREE_ROOT_DRIFT")
	}
	return nil
}
func (w *windowsSerenaOwnedTree) workingDirectory(ctx context.Context) (string, error) {
	if err := w.checkRoot(ctx); err != nil {
		return "", err
	}
	branch := "generations"
	if w.request.Kind == "attempt" {
		branch = "attempts"
	}
	if w.request.Kind == "qualification" {
		branch = "qualifications"
	}
	return filepath.Join(w.request.Namespace, w.request.PackageIdentitySHA256, branch, w.rootName), nil
}

// Enumeration uses the installed Go1.27 handle-based Readdirnames. The new
// duplicate owns only enumeration state; no name is reopened from an absolute
// path. Every actual entry is then opened beneath the retained directory.
func (w *windowsSerenaOwnedTree) directoryNames(ctx context.Context, h windows.Handle) (names []string, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(windows.CurrentProcess(), h, windows.CurrentProcess(), &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(duplicate), "serena-owned-directory")
	if f == nil {
		return nil, errors.Join(errors.New("SERENA_TREE_ENUMERATION_FILE"), windows.CloseHandle(duplicate))
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		batch, err := f.Readdirnames(64)
		for _, name := range batch {
			if !serenaTreeName(name) {
				return nil, errors.New("SERENA_TREE_NAME_INVALID")
			}
			names = append(names, name)
			if len(names) > w.request.Limits.Entries {
				return nil, errors.New("SERENA_TREE_ENTRY_LIMIT")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(names)
	return names, nil
}
func (w *windowsSerenaOwnedTree) openEntry(parent windows.Handle, name string) (*serenaTreeHandle, error) {
	if !serenaTreeName(name) {
		return nil, errors.New("SERENA_TREE_NAME_INVALID")
	}
	access := uint32(windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES)
	if !w.readExisting {
		access |= windows.DELETE
	}
	h, err := ntOpenStateReadRelativeMode(parent, name, access, nil, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
	if err != nil {
		return nil, err
	}
	return &serenaTreeHandle{handle: h}, nil
}

func (w *windowsSerenaOwnedTree) observeStreams(ctx context.Context, handle windows.Handle) ([]SerenaTreeStream, error) {
	for capacity := 256; ; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if int64(capacity) > w.request.Limits.EncodedPathBytes || uint64(capacity) > uint64(^uint32(0)) {
			return nil, errors.New("SERENA_TREE_STREAM_BUFFER_LIMIT")
		}
		raw := make([]byte, capacity)
		err := windows.GetFileInformationByHandleEx(handle, windows.FileStreamInfo, &raw[0], uint32(capacity))
		if err == nil {
			if err = ctx.Err(); err != nil {
				return nil, err
			}
			return decodeSerenaTreeStreams(raw, w.request.Limits)
		}
		if !errors.Is(err, windows.ERROR_MORE_DATA) && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return nil, err
		}
		if capacity > int(^uint(0)>>1)/2 {
			return nil, errors.New("SERENA_TREE_STREAM_BUFFER_LIMIT")
		}
		capacity *= 2
	}
}

func serenaTreeContentHash(ctx context.Context, handle windows.Handle, before SerenaTreeObject) (string, error) {
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var read int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var count uint32
		err := windows.ReadFile(handle, buffer, &count, nil)
		if int64(count) > before.Bytes-read {
			return "", errors.New("SERENA_TREE_CONTENT_SIZE_DRIFT")
		}
		_, _ = hash.Write(buffer[:count])
		read += int64(count)
		if errors.Is(err, windows.ERROR_HANDLE_EOF) || err == nil && count == 0 {
			break
		}
		if err != nil {
			return "", err
		}
	}
	after, err := serenaTreeHandleIdentity(handle, before.Relative)
	if err != nil || read != before.Bytes || !sameSerenaTreeObject(before, after) {
		return "", errors.Join(err, errors.New("SERENA_TREE_CONTENT_IDENTITY_DRIFT"))
	}
	return hex.EncodeToString(hash.Sum(nil)), ctx.Err()
}
func (w *windowsSerenaOwnedTree) inventory(ctx context.Context) (v SerenaTreeInventory, retErr error) {
	if err := w.checkRoot(ctx); err != nil {
		return v, err
	}
	currentRoot, err := serenaTreeHandleIdentity(w.root.handle, nil)
	if err != nil {
		return v, err
	}
	v = SerenaTreeInventory{Version: 1, OperationID: w.request.OperationID, RootIdentity: currentRoot}
	v.RootIdentity.Streams, err = w.observeStreams(ctx, w.root.handle)
	if err != nil {
		return v, err
	}
	var bytes, pathBytes int64
	streamCount := len(v.RootIdentity.Streams)
	rootRaw, encodeErr := json.Marshal(v)
	if encodeErr != nil {
		return v, encodeErr
	}
	recordBytes := int64(len(rootRaw))
	var walk func(windows.Handle, []string) error
	walk = func(parent windows.Handle, path []string) error {
		if len(path) > w.request.Limits.Depth || len(path)+4 > w.request.Limits.OpenHandles {
			return errors.New("SERENA_TREE_DEPTH_HANDLE_LIMIT")
		}
		names, err := w.directoryNames(ctx, parent)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			h, err := w.openEntry(parent, name)
			if err != nil {
				return err
			}
			relative := append(append([]string(nil), path...), name)
			object, readErr := serenaTreeHandleIdentity(h.handle, relative)
			if readErr == nil {
				object.Streams, readErr = w.observeStreams(ctx, h.handle)
			}
			if readErr == nil && !object.Directory {
				if len(object.Streams) != 1 || object.Streams[0].Bytes != object.Bytes {
					readErr = errors.New("SERENA_TREE_STREAM_SIZE_DRIFT")
				} else {
					object.ContentSHA256, readErr = serenaTreeContentHash(ctx, h.handle, object)
				}
			}
			if readErr == nil {
				objectRaw, objectErr := json.Marshal(object)
				var utf16Bytes int64
				for _, part := range object.Relative {
					utf16Bytes += int64(len(utf16.Encode([]rune(part))) * 2)
				}
				for _, stream := range object.Streams {
					utf16Bytes += int64(len(utf16.Encode([]rune(stream.Name))) * 2)
				}
				if objectErr != nil {
					readErr = objectErr
				} else if object.Bytes < 0 || (!object.Directory && object.Bytes > w.request.Limits.LogicalBytes-bytes) || int64(len(objectRaw))+2 > w.request.Limits.RecordBytes-recordBytes || utf16Bytes > w.request.Limits.EncodedPathBytes-pathBytes || len(v.Objects)+1+streamCount+len(object.Streams) >= w.request.Limits.Entries {
					readErr = errors.New("SERENA_TREE_INVENTORY_LIMIT")
				} else {
					if !object.Directory {
						bytes += object.Bytes
					}
					recordBytes += int64(len(objectRaw)) + 2
					pathBytes += utf16Bytes
					streamCount += len(object.Streams)
					v.Objects = append(v.Objects, object)
					if object.Directory {
						readErr = walk(h.handle, relative)
						if readErr == nil && w.readExisting {
							after, identityErr := serenaTreeHandleIdentity(h.handle, relative)
							if identityErr != nil || !sameSerenaTreeObject(object, after) {
								readErr = errors.Join(identityErr, errors.New("SERENA_PUBLISHED_DIRECTORY_DRIFT"))
							}
						}
					}
				}
			}
			_, closeErr := h.closeOnce()
			if w.readExisting {
				w.inventoryCloseError = errors.Join(w.inventoryCloseError, closeErr)
			}
			if err = errors.Join(readErr, closeErr); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(w.root.handle, nil); err != nil {
		return v, err
	}
	if w.readExisting {
		if err := w.checkRoot(ctx); err != nil {
			return v, err
		}
		after, identityErr := serenaTreeHandleIdentity(w.root.handle, nil)
		if identityErr != nil || !sameSerenaTreeObject(currentRoot, after) {
			return v, errors.Join(identityErr, errors.New("SERENA_PUBLISHED_ROOT_DRIFT"))
		}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return v, err
	}
	var nameBytes int64
	for _, entry := range v.Objects {
		for _, part := range entry.Relative {
			nameBytes += int64(len(utf16.Encode([]rune(part))) * 2)
		}
	}
	if int64(len(raw)) > w.request.Limits.RecordBytes || nameBytes > w.request.Limits.EncodedPathBytes {
		return v, errors.New("SERENA_TREE_RECORD_LIMIT")
	}
	v.SHA256 = preparedSerenaHash(raw)
	return v, nil
}

func (w *windowsSerenaOwnedTree) remove(ctx context.Context, closure PreparedSerenaExecutionClosure) (receipt SerenaTreeCleanupReceipt, retErr error) {
	if w.readExisting {
		return receipt, errors.New("SERENA_PUBLISHED_READ_HAS_NO_DELETE_AUTHORITY")
	}
	if w.removeAttempted {
		return w.removeResult, w.removeResult.Error
	}
	w.removeAttempted = true
	defer func() {
		for _, h := range []*serenaTreeHandle{w.root, w.parent} {
			if h != nil && !h.close.Attempted {
				c, e := h.closeOnce()
				receipt.Closes = append(receipt.Closes, c)
				retErr = errors.Join(retErr, e)
			}
		}
		receipt.Error = retErr
		w.removeResult = receipt
	}()
	if err := w.checkRoot(ctx); err != nil {
		return receipt, err
	}
	before, err := w.inventory(ctx)
	receipt.Before = before
	if err != nil {
		return receipt, err
	}
	counts := map[string]uint32{}
	for _, entry := range before.Objects {
		if !entry.Directory {
			counts[entry.Device+"/"+entry.FileID]++
		}
	}
	for _, survivor := range w.request.survivors {
		if survivor == nil || survivor.request.OperationID != w.request.OperationID || survivor.request.PackageIdentitySHA256 != w.request.PackageIdentitySHA256 {
			return receipt, errors.New("SERENA_TREE_SURVIVOR_BINDING")
		}
		inventory, err := survivor.Inventory(ctx)
		if err != nil {
			return receipt, err
		}
		for _, entry := range inventory.Objects {
			if !entry.Directory {
				counts[entry.Device+"/"+entry.FileID]++
			}
		}
	}
	for _, entry := range before.Objects {
		if !entry.Directory && entry.Links != counts[entry.Device+"/"+entry.FileID] {
			return receipt, errors.New("SERENA_TREE_FOREIGN_HARDLINK")
		}
	}
	expected := map[string]SerenaTreeObject{}
	for _, entry := range before.Objects {
		expected[strings.Join(entry.Relative, "/")] = entry
	}
	removedLinks := map[string]uint32{}
	var walk func(windows.Handle, []string) error
	walk = func(parent windows.Handle, path []string) error {
		if len(path) > w.request.Limits.Depth || len(path)+4 > w.request.Limits.OpenHandles {
			return errors.New("SERENA_TREE_DEPTH_HANDLE_LIMIT")
		}
		names, err := w.directoryNames(ctx, parent)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			relative := append(append([]string(nil), path...), name)
			want, exists := expected[strings.Join(relative, "/")]
			if !exists {
				return errors.New("SERENA_TREE_ADDED_ENTRY")
			}
			if !want.Directory {
				removed := removedLinks[want.Device+"/"+want.FileID]
				if removed >= want.Links {
					return errors.New("SERENA_TREE_LINK_ACCOUNTING")
				}
				want.Links -= removed
			}
			h, err := w.openEntry(parent, name)
			if err != nil {
				return err
			}
			actual, err := serenaTreeHandleIdentity(h.handle, relative)
			if err == nil && !sameSerenaTreeObject(actual, want) {
				err = errors.New("SERENA_TREE_ENTRY_DRIFT")
			}
			if err == nil && actual.Directory {
				err = walk(h.handle, relative)
			}
			if err == nil {
				err = ctx.Err()
			}
			if err == nil {
				receipt.DeleteAttempts++
				err = setFileDeleteOnClose(h.handle)
			}
			closed, closeErr := h.closeOnce()
			receipt.Closes = append(receipt.Closes, closed)
			if err = errors.Join(err, closeErr); err != nil {
				return err
			}
			probe, probeErr := w.openEntry(parent, name)
			if probeErr == nil {
				c, e := probe.closeOnce()
				receipt.Closes = append(receipt.Closes, c)
				return errors.Join(e, errors.New("SERENA_TREE_DELETE_NOT_ABSENT"))
			}
			if !errors.Is(probeErr, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(probeErr, windows.ERROR_PATH_NOT_FOUND) && !errors.Is(probeErr, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
				return probeErr
			}
			delete(expected, strings.Join(relative, "/"))
			if !want.Directory {
				removedLinks[want.Device+"/"+want.FileID]++
			}
		}
		return nil
	}
	if err = walk(w.root.handle, nil); err != nil {
		return receipt, err
	}
	if len(expected) != 0 {
		return receipt, errors.New("SERENA_TREE_MISSING_ENTRY")
	}
	if err = w.checkRoot(ctx); err != nil {
		return receipt, err
	}
	receipt.DeleteAttempts++
	if err = setFileDeleteOnClose(w.root.handle); err != nil {
		return receipt, err
	}
	closed, err := w.root.closeOnce()
	receipt.Closes = append(receipt.Closes, closed)
	if err != nil {
		return receipt, err
	}
	receipt.Removed = true
	probe, probeErr := w.openEntry(w.parent.handle, w.rootName)
	if probeErr == nil {
		c, e := probe.closeOnce()
		receipt.Closes = append(receipt.Closes, c)
		return receipt, errors.Join(e, errors.New("SERENA_TREE_ROOT_REPLACED_OR_PRESENT"))
	}
	if !errors.Is(probeErr, windows.ERROR_FILE_NOT_FOUND) && !errors.Is(probeErr, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return receipt, probeErr
	}
	receipt.Absent = true
	closed, err = w.parent.closeOnce()
	receipt.Closes = append(receipt.Closes, closed)
	return receipt, err
}
func (w *windowsSerenaOwnedTree) close() (SerenaTreeClose, error) {
	if w.closeResult.Attempted {
		return w.closeResult, w.closeResult.Error
	}
	w.closeResult.Attempted = true
	_, a := w.root.closeOnce()
	_, b := w.parent.closeOnce()
	w.closeResult.Error = errors.Join(a, b, w.inventoryCloseError)
	if w.creation == nil || !w.creation.finalized || !w.creation.view.OwnedResourcesComplete || !w.creation.view.BorrowedResourcesComplete {
		w.closeResult.Error = errors.Join(w.closeResult.Error, errors.New("SERENA_TREE_ACQUISITION_RESOURCES_PENDING"))
	}
	if w.parentPolicy == nil || !w.parentPolicy.Snapshot().ResourcesComplete {
		w.closeResult.Error = errors.Join(w.closeResult.Error, errors.New("SERENA_TREE_PARENT_RESOURCES_PENDING"))
	}
	w.closeResult.Closed = w.closeResult.Error == nil
	return w.closeResult, w.closeResult.Error
}
