//go:build windows

package api

import (
	"context"
	"errors"
	"io"
	"path"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// All originals, including a failed CloseHandle, remain in the acquisition.
// No path-based cleanup, retrying closer or synthetic no-child proof exists.
type privateSerenaCacheSeedMaterialization struct {
	owner                         *windowsSerenaOwnedTree
	handles                       []*serenaTreeHandle
	creation                      *StateFileReadObservation
	workDeadline, cleanupDeadline time.Time
	live                          int
	closed, success               bool
	creationAttempted             bool
	closeErr                      error
}

func (m *privateSerenaCacheSeedMaterialization) closeHandle(h *serenaTreeHandle) error {
	if h == nil {
		return nil
	}
	if h.close.Attempted {
		return h.close.Error
	}
	closed, err := h.closeOnce()
	if closed.Closed {
		m.live--
	}
	if !closed.Attempted || !closed.Closed || closed.Error != nil {
		err = errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_CLOSE_PENDING"))
	}
	if err != nil {
		m.closeErr = errors.Join(m.closeErr, err)
	}
	return err
}

func (m *privateSerenaCacheSeedMaterialization) closeOriginal() error {
	if m == nil {
		return nil
	}
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	for i := len(m.handles) - 1; i >= 0; i-- {
		_ = m.closeHandle(m.handles[i])
	}
	if m.creationAttempted && (m.creation == nil || !m.creation.finalized || !m.creation.view.OwnedResourcesComplete || !m.creation.view.BorrowedResourcesComplete) || m.live != 0 {
		m.closeErr = errors.Join(m.closeErr, errors.New("SERENA_PRIVATE_CACHE_SEED_RESOURCES_PENDING"))
	}
	if m.cleanupDeadline.IsZero() || !time.Now().Before(m.cleanupDeadline) {
		m.closeErr = errors.Join(m.closeErr, errors.New("SERENA_PRIVATE_CACHE_SEED_CLOSE_OUT_OF_BOUND"))
	}
	return m.closeErr
}

func (m *privateSerenaCacheSeedMaterialization) retain(handle windows.Handle) *serenaTreeHandle {
	h := &serenaTreeHandle{handle: handle}
	m.handles = append(m.handles, h)
	m.live++
	return h
}

func (m *privateSerenaCacheSeedMaterialization) openRelative(ctx context.Context, parent windows.Handle, name string, directory bool, access, disposition uint32, descriptor *windows.SECURITY_DESCRIPTOR) (*serenaTreeHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.live+1 > m.owner.request.Limits.OpenHandles || !serenaTreeName(name) {
		return nil, errors.New("SERENA_PRIVATE_CACHE_SEED_OPEN_LIMIT")
	}
	share := uint32(windows.FILE_SHARE_READ)
	if directory {
		share |= windows.FILE_SHARE_WRITE
	}
	h, err := ntOpenStateReadRelativeMode(parent, name, access, &directory, disposition, share, descriptor)
	if err != nil {
		return nil, err
	}
	return m.retain(h), nil
}

type privateSerenaCacheSeedHandleReader struct {
	handle windows.Handle
}

func (r privateSerenaCacheSeedHandleReader) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	var count uint32
	err := windows.ReadFile(r.handle, buffer, &count, nil)
	if errors.Is(err, windows.ERROR_HANDLE_EOF) || err == nil && count == 0 {
		err = io.EOF
	}
	return int(count), err
}

type privateSerenaCacheSeedDirectory struct {
	path     string
	mtime    string
	handle   *serenaTreeHandle
	identity SerenaTreeObject
}

func (m *privateSerenaCacheSeedMaterialization) setTime(ctx context.Context, h *serenaTreeHandle, before SerenaTreeObject, nanoseconds string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ns, err := privateSerenaCacheSeedNanoseconds(nanoseconds)
	if err != nil {
		return err
	}
	current, err := serenaTreeHandleIdentity(h.handle, before.Relative)
	if err != nil || current.Device != before.Device || current.FileID != before.FileID ||
		current.Directory != before.Directory || (!current.Directory && (current.Bytes != before.Bytes || current.Links != 1)) {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_DESTINATION_DRIFT"))
	}
	ticks := uint64(ns/100) + 116444736000000000
	modified := windows.Filetime{LowDateTime: uint32(ticks), HighDateTime: uint32(ticks >> 32)}
	if err = windows.SetFileTime(h.handle, nil, nil, &modified); err != nil {
		return err
	}
	after, err := serenaTreeHandleIdentity(h.handle, before.Relative)
	if err != nil || after.Device != before.Device || after.FileID != before.FileID || after.Directory != before.Directory ||
		after.Modified != ticks || (!after.Directory && (after.Bytes != before.Bytes || after.Links != 1)) {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_MTIME_DRIFT"))
	}
	streams, err := m.owner.observeStreams(ctx, h.handle)
	if err != nil || (after.Directory && len(streams) != 0) || (!after.Directory && (len(streams) != 1 || streams[0].Bytes != after.Bytes)) {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_STREAM_DRIFT"))
	}
	return ctx.Err()
}

func (m *privateSerenaCacheSeedMaterialization) createDirectory(ctx context.Context, parent windows.Handle, name, relative, mtime string, descriptor *windows.SECURITY_DESCRIPTOR) (privateSerenaCacheSeedDirectory, error) {
	var result privateSerenaCacheSeedDirectory
	h, err := m.openRelative(ctx, parent, name, true,
		windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_CREATE, descriptor)
	if err != nil {
		return result, err
	}
	identity, err := serenaTreeHandleIdentity(h.handle, strings.Split(relative, "/"))
	if err != nil || !identity.Directory {
		return result, errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_DIRECTORY_CREATE"))
	}
	return privateSerenaCacheSeedDirectory{path: relative, mtime: mtime, handle: h, identity: identity}, nil
}

func (m *privateSerenaCacheSeedMaterialization) copyFile(ctx context.Context, parent windows.Handle, entry privateSerenaCacheSeedEntry, source io.Reader, descriptor *windows.SECURITY_DESCRIPTOR) error {
	name := path.Base(entry.Path)
	writer, err := m.openRelative(ctx, parent, name, false,
		windows.FILE_WRITE_DATA|windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_CREATE, descriptor)
	if err != nil {
		return err
	}
	var buffer [64 << 10]byte
	var written int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buffer[:])
		if int64(n) > entry.Bytes-written {
			return errors.New("SERENA_PRIVATE_CACHE_SEED_WRITE_SIZE")
		}
		if n != 0 {
			if err = windowsWriteAll(writer.handle, buffer[:n]); err != nil {
				return err
			}
			written += int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	identity, err := serenaTreeHandleIdentity(writer.handle, strings.Split("cache/"+entry.Path, "/"))
	if err != nil || written != entry.Bytes || identity.Directory || identity.Bytes != entry.Bytes || identity.Links != 1 {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_WRITTEN_FILE_INVALID"))
	}
	if _, err = windows.Seek(writer.handle, 0, 0); err != nil {
		return err
	}
	actual, err := serenaTreeContentHash(ctx, writer.handle, identity)
	if err != nil || actual != entry.SHA256 {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_READBACK_DRIFT"))
	}
	// This is disposable data: completed synchronous writes and readback, not
	// a promise of survival after power loss. Durable records keep their Sync.
	if err = m.closeHandle(writer); err != nil {
		return err
	}
	// Set and verify the timestamp only after the final data writer closed.
	metadata, err := m.openRelative(ctx, parent, name, false,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.READ_CONTROL,
		windows.FILE_OPEN, nil)
	if err != nil {
		return err
	}
	if err = m.setTime(ctx, metadata, identity, entry.MtimeNS); err != nil {
		return err
	}
	return m.closeHandle(metadata)
}

func materializePrivateSerenaCacheSeed(ctx context.Context, acquisition *preparedSerenaAcquisition) (retErr error) {
	if acquisition == nil || ctx == nil {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_ACQUISITION_REQUIRED")
	}
	seed, request := acquisition.inputs.cacheSeed, acquisition.inputs.Request
	if seed == nil {
		return nil
	}
	if err := validatePrivateSerenaCacheSeedBinding(acquisition.inputs, seed.owner); err != nil {
		return err
	}
	if err := seed.owner.validateOrigin(ctx); err != nil {
		return err
	}
	seed.owner.activityMu.Lock()
	if seed.materializationUsed {
		seed.owner.activityMu.Unlock()
		return errors.New("SERENA_PRIVATE_CACHE_SEED_MATERIALIZATION_REENTRY")
	}
	seed.materializationUsed = true
	seed.owner.activityMu.Unlock()
	if ctx == nil || acquisition.attempt == nil || acquisition.cacheMaterialization != nil {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_ATTEMPT_REQUIRED")
	}
	end, finite := ctx.Deadline()
	if !finite || end.After(request.WorkDeadline) {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_WORK_DEADLINE")
	}
	owner, ok := acquisition.attempt.platform.(*windowsSerenaOwnedTree)
	key, err := request.Identity()
	if err != nil || !ok || owner == nil || owner.readExisting || owner.closeResult.Attempted || owner.removeAttempted ||
		owner.request.Kind != "attempt" || owner.request.Namespace != request.Namespace ||
		owner.request.PackageIdentitySHA256 != key || owner.request.OperationID != request.OperationID ||
		owner.request.GenerationID != request.GenerationID || owner.request.Limits != request.OwnedTreeLimits ||
		owner.request.AbsoluteCleanupDeadline != request.AbsoluteCleanupDeadline {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_ATTEMPT_BINDING"))
	}
	m := &privateSerenaCacheSeedMaterialization{owner: owner, workDeadline: request.WorkDeadline, cleanupDeadline: request.AbsoluteCleanupDeadline}
	acquisition.cacheMaterialization = m // Retain originals before the first open.
	return m.materialize(ctx, seed)
}

func (m *privateSerenaCacheSeedMaterialization) materialize(ctx context.Context, seed *privateSerenaCacheSeed) (retErr error) {
	if m == nil || m.owner == nil || seed == nil || ctx == nil || m.closed || m.creationAttempted || len(m.handles) != 0 {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_MATERIALIZATION_UNAVAILABLE")
	}
	end, finite := ctx.Deadline()
	if !finite || m.workDeadline.IsZero() || end.After(m.workDeadline) || m.cleanupDeadline.Before(m.workDeadline) {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_WORK_DEADLINE")
	}
	if err := m.owner.checkRoot(ctx, m.owner.parentPolicy, false); err != nil {
		return err
	}
	root, err := m.owner.workingDirectory(ctx)
	if err != nil {
		return err
	}
	m.creationAttempted = true
	builder := newStateFileReadBuilder(ctx, filepath.Join(root, "cache"), seed.descriptor.Manifest.TotalBytes+1)
	defer func() {
		_, m.creation, retErr = builder.finish(nil, retErr)
		retErr = errors.Join(retErr, m.closeOriginal())
		m.success = retErr == nil
	}()
	descriptor, _, err := buildAllowlistSDObserved(builder, "private-cache-seed-create-descriptor")
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	parentHandle, err := ntOpenStateReadRootMode(filepath.Dir(seed.config.DescriptorPath),
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		return err
	}
	parent := m.retain(parentHandle)
	var parentInfo windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(parent.handle, &parentInfo); err != nil {
		return err
	}
	if parentInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || parentInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		observedStateReadWindowsIdentity(parentInfo) != seed.parentIdentitySHA256 {
		return errors.New("SERENA_PRIVATE_CACHE_SEED_PARENT_DRIFT")
	}
	archive, err := m.openRelative(ctx, parent.handle, seed.descriptor.Archive.Path, false,
		windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_OPEN, nil)
	if err != nil {
		return err
	}
	before, err := serenaTreeHandleIdentity(archive.handle, nil)
	if err != nil || before.Directory || before.Bytes != seed.descriptor.Archive.Bytes || before.Links != 1 {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_ARCHIVE_IDENTITY"))
	}
	streams, err := m.owner.observeStreams(ctx, archive.handle)
	if err != nil || len(streams) != 1 || streams[0].Bytes != before.Bytes {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_ARCHIVE_STREAMS"))
	}
	cache, err := m.createDirectory(ctx, m.owner.root.handle, "cache", ".", seed.inventory.RootMtimeNS, descriptor)
	if err != nil {
		return err
	}
	stack := []privateSerenaCacheSeedDirectory{cache}
	finishTop := func() error {
		last := stack[len(stack)-1]
		if err := m.setTime(ctx, last.handle, last.identity, last.mtime); err != nil {
			return err
		}
		if err := m.closeHandle(last.handle); err != nil {
			return err
		}
		stack = stack[:len(stack)-1]
		return nil
	}
	err = readPrivateSerenaCacheTar(ctx, privateSerenaCacheSeedHandleReader{archive.handle}, seed.descriptor, seed.inventory,
		func(entry privateSerenaCacheSeedEntry, reader io.Reader) error {
			parentPath := path.Dir(entry.Path)
			for len(stack) > 0 && stack[len(stack)-1].path != parentPath {
				if err := finishTop(); err != nil {
					return err
				}
			}
			if len(stack) == 0 {
				return errors.New("SERENA_PRIVATE_CACHE_SEED_PARENT_ORDER")
			}
			parent := stack[len(stack)-1].handle.handle
			if entry.Kind == "directory" {
				directory, err := m.createDirectory(ctx, parent, path.Base(entry.Path), entry.Path, entry.MtimeNS, descriptor)
				if err == nil {
					stack = append(stack, directory)
				}
				return err
			}
			return m.copyFile(ctx, parent, entry, reader, descriptor)
		})
	if err != nil {
		return err
	}
	for len(stack) != 0 {
		if err = finishTop(); err != nil {
			return err
		}
	}
	after, err := serenaTreeHandleIdentity(archive.handle, nil)
	if err != nil || !sameSerenaTreeObject(before, after) {
		return errors.Join(err, errors.New("SERENA_PRIVATE_CACHE_SEED_ARCHIVE_IDENTITY_DRIFT"))
	}
	if err = m.owner.checkRoot(ctx, m.owner.parentPolicy, false); err != nil {
		return err
	}
	return ctx.Err()
}
