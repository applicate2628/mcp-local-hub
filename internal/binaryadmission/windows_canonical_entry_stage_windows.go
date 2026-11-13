//go:build windows

package binaryadmission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Stage creates a missing canonical root through retained, no-reparse directory
// handles, then stages operation-owned exclusive leaves in that root. The
// transaction alone creates/owns the Product subdirectory.
// Even on failure, the returned owner retains every acquired disposition.
func (o *WindowsCanonicalEntrySetV1) Stage(ctx context.Context, canonicalEntryPath string) (*StagedWindowsCanonicalEntrySetV1, error) {
	s := &StagedWindowsCanonicalEntrySetV1{root: &WindowsCanonicalEntrySetV1{payloadDirectoryLink: &canonicalEntryPayloadDirectoryLinkV1{}}}
	if o == nil {
		return s, errors.New("missing canonical entry source owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if ctx == nil {
		return s, errors.New("nil canonical entry staging context")
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if err := o.verifyLocked(); err != nil {
		return s, err
	}
	var err error
	s.destinations, err = CanonicalEntryTargetsV1(canonicalEntryPath)
	if err != nil {
		return s, err
	}
	s.parent, err = s.root.openAbsoluteWithOptions(filepath.Dir(canonicalEntryPath), true, windows.FILE_SHARE_READ, windows.FILE_OPEN_IF)
	if err != nil {
		return s, err
	}
	s.snapshot = o.snapshot
	s.snapshot.IdentityBytes = append([]byte(nil), o.snapshot.IdentityBytes...)
	var token [16]byte
	if _, err = rand.Read(token[:]); err != nil {
		return s, err
	}
	stem := ".mcphub-entry-set-" + hex.EncodeToString(token[:])
	for i, source := range []*canonicalEntryNativeFile{o.entry, o.product, o.identity} {
		if err = ctx.Err(); err != nil {
			return s, err
		}
		leaf := fmt.Sprintf("%s-%d.stage", stem, i)
		path := filepath.Join(filepath.Dir(canonicalEntryPath), leaf)
		// The original disposition FileObject continuously denies foreign
		// delete/name changes, but never acquires WRITE_DATA: even a reduced-
		// access duplicate of a writable FileObject prevents image execution.
		h, createErr := canonicalEntryOpenNative(windows.Handle(s.parent.file.Fd()), leaf, false, windows.FILE_READ_DATA|windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_CREATE)
		if createErr != nil {
			return s, createErr
		}
		s.members[i] = &canonicalEntryNativeFile{path: path}
		original, retainErr := s.root.retain(h, path, false)
		if retainErr != nil {
			return s, retainErr
		}
		s.members[i] = original
		// FILE_CREATE establishes this empty preimage even if acquiring the
		// separate writer fails. Cleanup must seal and compare it before delete.
		s.cleanupSHA256[i] = canonicalEntrySHA256(nil)
		h, openErr := canonicalEntryOpenNative(windows.Handle(s.parent.file.Fd()), leaf, false, windows.FILE_WRITE_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
		if openErr != nil {
			return s, openErr
		}
		out := os.NewFile(uintptr(h), path)
		if out == nil {
			return s, errors.Join(errors.New("stage writer wrapper unavailable"), windows.CloseHandle(h))
		}
		id, idErr := canonicalEntryNativeID(h, false)
		if idErr != nil || id != original.identity {
			return s, errors.Join(errors.New("stage writer differs from original creation object"), idErr, out.Close())
		}
		initial, initialErr := original.file.Stat()
		if initialErr != nil {
			return s, errors.Join(initialErr, out.Close())
		}
		if initial.Size() != 0 {
			return s, errors.Join(errors.New("original stage changed before writer acquisition"), out.Close())
		}
		info, statErr := source.file.Stat()
		var copyErr error
		if statErr == nil {
			_, copyErr = io.Copy(out, &canonicalEntryContextReader{ctx: ctx, reader: io.NewSectionReader(source.file, 0, info.Size())})
		}
		syncErr := out.Sync()
		// Capture the bytes actually produced even on copy/Sync failure, before
		// releasing the original writer. Cleanup may remove only that preimage.
		cleanupHash, cleanupHashErr := canonicalEntryHashFile(context.Background(), original.file)
		s.cleanupSHA256[i] = cleanupHash
		closeErr := out.Close()
		// No writable FileObject survives readiness. A writer arriving during
		// the transition makes sealing fail; changed bytes fail the hash below.
		sealErr := s.sealOriginalStage(original)
		if err = errors.Join(statErr, copyErr, syncErr, cleanupHashErr, closeErr, sealErr); err != nil {
			return s, err
		}
		// Receive identity/extent/hash from the retained creation object, never
		// replacing ownership with a source-path reopen after writer Close.
		actualID, idErr := canonicalEntryNativeID(windows.Handle(original.file.Fd()), false)
		if idErr != nil {
			return s, idErr
		}
		actualInfo, extentErr := original.file.Stat()
		if extentErr != nil {
			return s, extentErr
		}
		if actualID != id || actualInfo.Size() != info.Size() {
			return s, errors.New("stage original object identity or extent differs after Close")
		}
		hash, hashErr := canonicalEntryHashFile(ctx, original.file)
		if hashErr != nil {
			return s, hashErr
		}
		want := o.snapshot.CanonicalEntry.SHA256
		switch i {
		case 0:
			s.snapshot.CanonicalEntry.Path = path
		case 1:
			want = o.snapshot.Product.SHA256
			s.snapshot.Product.Path = path
			s.snapshot.ProductArtifact.Path = path
		case 2:
			want = o.snapshot.Identity.SHA256
			s.snapshot.Identity.Path = path
		}
		if hash != want {
			return s, errors.New("stage copied bytes disagree with original set")
		}
	}
	if err = o.verifyLocked(); err != nil {
		return s, err
	}
	if err = validateCanonicalEntrySnapshotV1(s.snapshot); err != nil {
		return s, err
	}
	s.ready = true
	return s, nil
}

type canonicalEntryContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *canonicalEntryContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (s *StagedWindowsCanonicalEntrySetV1) targetsLocked() WindowsCanonicalEntrySetTargetsV1 {
	return WindowsCanonicalEntrySetTargetsV1{s.snapshot.CanonicalEntry.Path, s.snapshot.Product.Path, s.snapshot.Identity.Path}
}

func (s *StagedWindowsCanonicalEntrySetV1) Targets() (WindowsCanonicalEntrySetTargetsV1, error) {
	if s == nil {
		return WindowsCanonicalEntrySetTargetsV1{}, errors.New("missing staged canonical entry owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.closed {
		return WindowsCanonicalEntrySetTargetsV1{}, errors.New("staged canonical entry owner is not ready/open")
	}
	return s.targetsLocked(), nil
}

func (s *StagedWindowsCanonicalEntrySetV1) DestinationTargets() (WindowsCanonicalEntrySetTargetsV1, error) {
	if s == nil {
		return WindowsCanonicalEntrySetTargetsV1{}, errors.New("missing staged canonical entry owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.closed {
		return WindowsCanonicalEntrySetTargetsV1{}, errors.New("staged canonical entry owner is not ready/open")
	}
	return s.destinations, nil
}

func (s *StagedWindowsCanonicalEntrySetV1) Snapshot() (WindowsCanonicalEntrySetSnapshotV1, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, errors.New("missing staged canonical entry owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.closed {
		return WindowsCanonicalEntrySetSnapshotV1{}, errors.New("staged canonical entry owner is not ready/open")
	}
	v := s.snapshot
	v.IdentityBytes = append([]byte(nil), v.IdentityBytes...)
	return v, nil
}

func (s *StagedWindowsCanonicalEntrySetV1) verifyLocked() error {
	if !s.ready || s.closed {
		return errors.New("staged canonical entry owner is not ready/open")
	}
	for i, member := range s.members {
		want := s.snapshot.CanonicalEntry.SHA256
		if i == 1 {
			want = s.snapshot.Product.SHA256
		}
		if i == 2 {
			want = s.snapshot.Identity.SHA256
		}
		if err := canonicalEntryVerifyOriginalMember(member, want); err != nil {
			return err
		}
		_, _, closeErr, err := canonicalEntryCurrentMember(s.root, s.parent, member)
		if err = errors.Join(err, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func (s *StagedWindowsCanonicalEntrySetV1) VerifyUnchanged() error {
	if s == nil {
		return errors.New("missing staged canonical entry owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.verifyLocked()
}

// TakeForTransaction is one-shot. Success transfers remaining leaf disposition
// to the existing transaction/recovery owner; Close can never delete it again.
func (s *StagedWindowsCanonicalEntrySetV1) TakeForTransaction(ctx context.Context, canonicalEntryPath string) (WindowsCanonicalEntrySetSnapshotV1, WindowsCanonicalEntrySetTargetsV1, error) {
	if s == nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, errors.New("missing staged canonical entry owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, errors.New("nil transaction context")
	}
	if err := ctx.Err(); err != nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, err
	}
	if s.taken {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, errors.New("staged canonical entry disposition already transferred")
	}
	want, err := CanonicalEntryTargetsV1(canonicalEntryPath)
	if err != nil || want != s.destinations {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, errors.Join(errors.New("transaction canonical targets disagree with original staging"), err)
	}
	if err = s.verifyLocked(); err != nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, WindowsCanonicalEntrySetTargetsV1{}, err
	}
	s.taken = true
	v := s.snapshot
	v.IdentityBytes = append([]byte(nil), v.IdentityBytes...)
	return v, s.targetsLocked(), nil
}

func (s *StagedWindowsCanonicalEntrySetV1) removeOriginalStage(member *canonicalEntryNativeFile, expectedHash string) error {
	if member == nil {
		return nil
	}
	if !validLowerHex(expectedHash, 64, 64) {
		return errors.New("owned stage cleanup holds: produced byte preimage unavailable")
	}
	// Partial staging may have failed before write exclusion was acquired.
	// Preserve the name if a writer is still live or changed the owned preimage.
	if err := s.sealOriginalStage(member); err != nil {
		return err
	}
	if err := canonicalEntryVerifyOriginalMember(member, expectedHash); err != nil {
		return err
	}
	parent, leaf, closeErr, err := canonicalEntryCurrentMember(s.root, s.parent, member)
	if err = errors.Join(err, closeErr); err != nil {
		return err
	}
	var iosb windows.IO_STATUS_BLOCK
	remove := byte(1)
	err = windows.NtSetInformationFile(windows.Handle(member.file.Fd()), &iosb, &remove, uint32(unsafe.Sizeof(remove)), windows.FileDispositionInformation)
	if err != nil {
		return err
	}
	closeErr = canonicalEntryCloseOriginalMember(s.root, member)
	absenceErr := canonicalEntryRequireAbsent(parent, leaf)
	return errors.Join(closeErr, absenceErr)
}

// A creation object must share WRITE for its transient writer. A separate read
// FileObject denies WRITE after that writer closes, without releasing DELETE.
func (s *StagedWindowsCanonicalEntrySetV1) sealOriginalStage(member *canonicalEntryNativeFile) error {
	if member == nil || member.file == nil || s.root == nil || s.root.closed {
		return errors.New("original stage disposition unavailable for read-only guard")
	}
	if member.readOnlyGuard != nil {
		return nil
	}
	relative, err := canonicalEntryMemberRelative(s.parent, member)
	if err != nil {
		return err
	}
	parent, leaf, err := canonicalEntryRelativeParent(s.root, s.parent, relative)
	if err != nil {
		return err
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if err != nil {
		return err
	}
	id, idErr := canonicalEntryNativeID(h, false)
	if idErr != nil || id != member.identity {
		return errors.Join(errors.New("stage read-only guard differs from original creation object"), idErr, windows.CloseHandle(h))
	}
	guard := os.NewFile(uintptr(h), member.path)
	if guard == nil {
		return errors.Join(errors.New("stage read-only guard wrapper unavailable"), windows.CloseHandle(h))
	}
	member.readOnlyGuard = guard
	s.root.files = append(s.root.files, guard)
	return nil
}

func (s *StagedWindowsCanonicalEntrySetV1) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeError
	}
	s.closed = true
	if !s.taken && s.parent != nil {
		for i, member := range s.members {
			s.closeError = errors.Join(s.closeError, s.removeOriginalStage(member, s.cleanupSHA256[i]))
		}
	}
	s.closeError = errors.Join(s.closeError, s.root.Close())
	return s.closeError
}

func canonicalEntryMemberIndex(member string) (int, error) {
	switch member {
	case "launcher":
		return 0, nil
	case "product":
		return 1, nil
	case "identity":
		return 2, nil
	}
	return 0, errors.New("canonical entry member must be launcher, product, or identity")
}

func canonicalEntryVerifyOriginalMember(member *canonicalEntryNativeFile, expectedHash string) error {
	if member == nil || member.file == nil {
		return errors.New("original canonical entry member handle unavailable")
	}
	id, err := canonicalEntryNativeID(windows.Handle(member.file.Fd()), false)
	if err != nil {
		return err
	}
	if id != member.identity {
		return errors.New("original canonical entry member identity changed")
	}
	if member.readOnlyGuard != nil {
		guardID, guardErr := canonicalEntryNativeID(windows.Handle(member.readOnlyGuard.Fd()), false)
		if guardErr != nil || guardID != member.identity {
			return errors.Join(errors.New("original canonical entry read-only guard changed"), guardErr)
		}
	}
	hash, err := canonicalEntryHashFile(context.Background(), member.file)
	if err != nil {
		return err
	}
	if !validLowerHex(expectedHash, 64, 64) || hash != expectedHash {
		return errors.New("original canonical entry member bytes changed")
	}
	return nil
}

func canonicalEntryRelativeParent(root *WindowsCanonicalEntrySetV1, parent *canonicalEntryNativeFile, relative string) (*canonicalEntryNativeFile, string, error) {
	if root == nil || root.closed || parent == nil || !publisherSourcePathValid(relative) {
		return nil, "", errors.New("invalid confined original canonical entry relative path")
	}
	parts := strings.Split(relative, "/")
	current := parent
	for _, part := range parts[:len(parts)-1] {
		borrowed, matched, borrowErr := canonicalEntryBorrowPayloadDirectory(root, current, part)
		if borrowErr != nil {
			return nil, "", borrowErr
		}
		if matched {
			current = borrowed
			continue
		}
		h, err := canonicalEntryOpenNative(windows.Handle(current.file.Fd()), part, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
		if err != nil {
			return nil, "", err
		}
		next, err := root.retain(h, filepath.Join(current.path, part), true)
		if err != nil {
			return nil, "", err
		}
		if next.identity.volume != parent.identity.volume {
			return nil, "", errors.New("canonical entry destination parent is on another volume")
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func canonicalEntryMemberRelative(parent, member *canonicalEntryNativeFile) (string, error) {
	if parent == nil || member == nil {
		return "", errors.New("missing original member location")
	}
	rel, err := filepath.Rel(parent.path, member.path)
	if err != nil {
		return "", err
	}
	rel = filepath.ToSlash(rel)
	if !publisherSourcePathValid(rel) {
		return "", errors.New("original member is outside its retained canonical root")
	}
	return rel, nil
}

// A name probe checks currentness only. Promotion authority remains the original
// retained DELETE handle; a successful probe never replaces that handle.
func canonicalEntryCurrentMember(root *WindowsCanonicalEntrySetV1, parent, member *canonicalEntryNativeFile) (*canonicalEntryNativeFile, string, error, error) {
	rel, err := canonicalEntryMemberRelative(parent, member)
	if err != nil {
		return nil, "", nil, err
	}
	dir, leaf, err := canonicalEntryRelativeParent(root, parent, rel)
	if err != nil {
		return nil, "", nil, err
	}
	h, err := canonicalEntryOpenNative(windows.Handle(dir.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if err != nil {
		return dir, leaf, nil, err
	}
	id, idErr := canonicalEntryNativeID(h, false)
	if idErr == nil && id != member.identity {
		idErr = errors.New("original member name no longer selects its retained object")
	}
	return dir, leaf, windows.CloseHandle(h), idErr
}

func canonicalEntryProbeAbsent(parent *canonicalEntryNativeFile, leaf string) (error, error) {
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return windows.CloseHandle(h), errors.New("canonical entry destination leaf is present")
}

func canonicalEntryRequireAbsent(parent *canonicalEntryNativeFile, leaf string) error {
	closeErr, err := canonicalEntryProbeAbsent(parent, leaf)
	return errors.Join(err, closeErr)
}

func canonicalEntryCloseOriginalMember(root *WindowsCanonicalEntrySetV1, member *canonicalEntryNativeFile) error {
	if member == nil || member.file == nil {
		return errors.New("original member close already received or unavailable")
	}
	f, guard := member.file, member.readOnlyGuard
	member.file, member.readOnlyGuard = nil, nil
	for i, owned := range root.files {
		if owned == f || owned == guard {
			root.files[i] = nil
		}
	}
	// A read-only guard is a distinct FileObject. Receive both closes before
	// probing absence, otherwise the guard keeps a disposed file pending.
	var guardErr error
	if guard != nil {
		guardErr = guard.Close()
	}
	return errors.Join(guardErr, f.Close())
}

// This is the pinned x/sys v0.47.0 syscall test's native FileRenameInformation
// layout. Zero ReplaceIfExists selects the no-overwrite contract; no POSIX or
// replace flags are used. Variable-length UTF-16 is sized from the actual leaf.
type canonicalEntryRenameInformation struct {
	ReplaceIfExists uint32
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
}

func canonicalEntryRenameBuffer(parent windows.Handle, leaf string) ([]byte, uint32, error) {
	name, err := windows.UTF16FromString(leaf)
	if err != nil {
		return nil, 0, err
	}
	name = name[:len(name)-1]
	var header canonicalEntryRenameInformation
	nameOffset := unsafe.Offsetof(header.FileName)
	nativeSize := uint64(nameOffset) + uint64(len(name))*2
	if nativeSize > uint64(^uint32(0)) || nativeSize > uint64(int(^uint(0)>>1)) {
		return nil, 0, errors.New("native canonical entry rename representation overflow")
	}
	allocation := int(nativeSize)
	if allocation < int(unsafe.Sizeof(header)) {
		allocation = int(unsafe.Sizeof(header))
	}
	buffer := make([]byte, allocation)
	typed := (*canonicalEntryRenameInformation)(unsafe.Pointer(&buffer[0]))
	typed.RootDirectory = parent
	typed.FileNameLength = uint32(len(name) * 2)
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(&buffer[nameOffset])), len(name)), name)
	return buffer, uint32(nativeSize), nil
}

func canonicalEntryReceiveNativeStatus(result *WindowsCanonicalEntryMoveResultV1, err error) {
	if err == nil {
		result.NativeStatusKnown = true
		result.NativeStatus = 0
		return
	}
	var status windows.NTStatus
	if errors.As(err, &status) {
		result.NativeStatusKnown = true
		result.NativeStatus = uint32(status)
	}
}

func canonicalEntryMoveOriginal(ctx context.Context, root *WindowsCanonicalEntrySetV1, parent, member *canonicalEntryNativeFile, memberName, destinationRelative, expectedHash string) (result WindowsCanonicalEntryMoveResultV1, retErr error) {
	result.Member = memberName
	result.DestinationRelative = destinationRelative
	result.OriginalOwnerPending = root != nil && !root.closed
	defer func() { result.Error = errors.Join(retErr, result.CloseError); retErr = result.Error }()
	if ctx == nil {
		return result, errors.New("nil original canonical entry move context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := canonicalEntryVerifyOriginalMember(member, expectedHash); err != nil {
		return result, err
	}
	rel, err := canonicalEntryMemberRelative(parent, member)
	if err != nil {
		return result, err
	}
	result.SourceRelative = rel
	if rel == destinationRelative {
		return result, errors.New("original canonical entry move requires a distinct absent destination")
	}
	_, _, closeErr, err := canonicalEntryCurrentMember(root, parent, member)
	result.CloseError = errors.Join(result.CloseError, closeErr)
	if err != nil || closeErr != nil {
		return result, err
	}
	destinationParent, leaf, err := canonicalEntryRelativeParent(root, parent, destinationRelative)
	if err != nil {
		return result, err
	}
	if member.identity.volume != destinationParent.identity.volume {
		return result, errors.New("original canonical entry move crosses volumes")
	}
	closeErr, err = canonicalEntryProbeAbsent(destinationParent, leaf)
	result.CloseError = errors.Join(result.CloseError, closeErr)
	if err != nil || closeErr != nil {
		return result, err
	}
	buffer, nativeSize, err := canonicalEntryRenameBuffer(windows.Handle(destinationParent.file.Fd()), leaf)
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	var iosb windows.IO_STATUS_BLOCK
	result.Attempted = true
	err = windows.NtSetInformationFile(windows.Handle(member.file.Fd()), &iosb, &buffer[0], nativeSize, windows.FileRenameInformation)
	canonicalEntryReceiveNativeStatus(&result, err)
	if err != nil {
		return result, err
	}
	result.Applied = true
	member.path = filepath.Join(destinationParent.path, leaf)
	_, _, closeErr, err = canonicalEntryCurrentMember(root, parent, member)
	result.CloseError = errors.Join(result.CloseError, closeErr)
	if err == nil {
		err = canonicalEntryVerifyOriginalMember(member, expectedHash)
	}
	result.Readback = err == nil
	return result, errors.Join(err, ctx.Err())
}

func (s *StagedWindowsCanonicalEntrySetV1) MoveOriginal(ctx context.Context, member, destinationRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if s == nil {
		err := errors.New("missing staged original owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err == nil && (!s.ready || !s.taken || s.closed) {
		err = errors.New("original staged move requires the taken open transaction owner")
	}
	if err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err, OriginalOwnerPending: !s.closed}, err
	}
	result, err := canonicalEntryMoveOriginal(ctx, s.root, s.parent, s.members[index], member, destinationRelative, s.cleanupSHA256[index])
	if result.Applied {
		path := s.members[index].path
		switch index {
		case 0:
			s.snapshot.CanonicalEntry.Path = path
		case 1:
			s.snapshot.Product.Path = path
			s.snapshot.ProductArtifact.Path = path
		case 2:
			s.snapshot.Identity.Path = path
		}
	}
	return result, err
}

func (s *StagedWindowsCanonicalEntrySetV1) AcquireOriginalPriorSet(ctx context.Context) (*WindowsCanonicalEntryPriorSetV1, error) {
	p := &WindowsCanonicalEntryPriorSetV1{root: &WindowsCanonicalEntrySetV1{}}
	if s == nil {
		return p, errors.New("missing original staged transaction owner")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.closed {
		return p, errors.New("original staged transaction owner is not ready/open")
	}
	if ctx == nil {
		return p, errors.New("nil original prior acquisition context")
	}
	if err := ctx.Err(); err != nil {
		return p, err
	}
	p.destinations = s.destinations
	if s.root.payloadDirectoryLink == nil {
		s.root.payloadDirectoryLink = &canonicalEntryPayloadDirectoryLinkV1{}
	}
	p.root.payloadDirectoryLink = s.root.payloadDirectoryLink
	s.root.payloadDirectoryLink.mu.Lock()
	existingPrior := s.root.payloadDirectoryLink.prior
	s.root.payloadDirectoryLink.mu.Unlock()
	if existingPrior != nil {
		return p, errors.New("original prior owner already acquired for this stage")
	}
	var err error
	p.parent, err = p.root.openAbsolute(s.parent.path, true)
	if err != nil {
		return p, err
	}
	if p.parent.identity != s.parent.identity {
		return p, errors.New("prior acquisition root differs from retained staging destination")
	}
	for i, relative := range []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, WindowsCanonicalIdentityRelativePathV1} {
		if err = ctx.Err(); err != nil {
			return p, err
		}
		member, missing, acquireErr := p.acquireOriginalMember(relative)
		if acquireErr != nil {
			return p, acquireErr
		}
		p.members[i] = member
		p.missingComponents[i] = missing
		if member != nil {
			p.present[i] = true
			p.sha256[i], err = canonicalEntryHashFile(ctx, member.file)
			if err != nil {
				return p, err
			}
		}
	}
	p.ready = true
	s.root.payloadDirectoryLink.mu.Lock()
	s.root.payloadDirectoryLink.prior = p
	s.root.payloadDirectoryLink.mu.Unlock()
	return p, nil
}

func (p *WindowsCanonicalEntryPriorSetV1) acquireOriginalMember(relative string) (*canonicalEntryNativeFile, string, error) {
	parts := strings.Split(relative, "/")
	parent := p.parent
	for i, part := range parts {
		directory := i < len(parts)-1
		if directory {
			borrowed, matched, borrowErr := canonicalEntryBorrowPayloadDirectory(p.root, parent, part)
			if borrowErr != nil {
				return nil, "", borrowErr
			}
			if matched {
				parent = borrowed
				if relative == WindowsCanonicalProductRelativePathV1 {
					p.payloadParent = parent
				}
				continue
			}
		}
		access := uint32(windows.FILE_READ_DATA | windows.DELETE)
		share := uint32(windows.FILE_SHARE_READ)
		if directory {
			access = windows.FILE_TRAVERSE
			share |= windows.FILE_SHARE_WRITE
		}
		h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), part, directory, access, share, windows.FILE_OPEN)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			return nil, strings.Join(parts[:i+1], "/"), nil
		}
		if err != nil {
			return nil, "", err
		}
		member, err := p.root.retain(h, filepath.Join(parent.path, part), directory)
		if err != nil {
			return nil, "", err
		}
		if member.identity.volume != p.parent.identity.volume {
			return nil, "", errors.New("prior member crosses the retained canonical volume")
		}
		parent = member
		if directory && relative == WindowsCanonicalProductRelativePathV1 {
			p.payloadParent = parent
		}
	}
	return parent, "", nil
}

func (s *StagedWindowsCanonicalEntrySetV1) ReadOriginal(member string) (bool, []byte, string, error) {
	if s == nil {
		return false, nil, "", errors.New("original staged owner required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err != nil {
		return false, nil, "", err
	}
	if !s.ready || s.closed {
		return false, nil, "", errors.New("original staged owner is not ready/open")
	}
	original := s.members[index]
	_, _, closeErr, err := canonicalEntryCurrentMember(s.root, s.parent, original)
	if err = errors.Join(err, closeErr); err != nil {
		return true, nil, "", err
	}
	body, hash, err := readCanonicalEntryOriginalBody(original, s.cleanupSHA256[index], member, "staged")
	return true, body, hash, err
}

func readCanonicalEntryOriginalBody(original *canonicalEntryNativeFile, expectedHash, member, ownerName string) ([]byte, string, error) {
	if err := canonicalEntryVerifyOriginalMember(original, expectedHash); err != nil {
		return nil, "", err
	}
	info, err := original.file.Stat()
	if err != nil {
		return nil, "", err
	}
	if member == "identity" {
		if err := validateWindowsProductIdentitySize(info.Size()); err != nil {
			return nil, "", err
		}
	}
	body, err := io.ReadAll(io.NewSectionReader(original.file, 0, info.Size()))
	if err != nil {
		return body, "", err
	}
	if canonicalEntrySHA256(body) != expectedHash {
		return body, "", fmt.Errorf("original %s bytes changed during receiving", ownerName)
	}
	return body, expectedHash, nil
}

func (p *WindowsCanonicalEntryPriorSetV1) ReadOriginal(member string) (bool, []byte, string, error) {
	if p == nil {
		return false, nil, "", errors.New("missing original prior owner")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err != nil {
		return false, nil, "", err
	}
	if p.closed {
		return p.present[index], nil, "", errors.New("original prior owner is closed")
	}
	if !p.present[index] {
		if p.missingComponents[index] == "" {
			return false, nil, "", errors.New("original prior presence/absence was not acquired")
		}
		return false, nil, "", nil
	}
	body, hash, err := readCanonicalEntryOriginalBody(p.members[index], p.sha256[index], member, "prior")
	return true, body, hash, err
}

func (p *WindowsCanonicalEntryPriorSetV1) MoveOriginal(ctx context.Context, member, destinationRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if p == nil {
		err := errors.New("missing original prior owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err == nil && (!p.ready || p.closed || !p.present[index]) {
		err = errors.New("original prior move requires an acquired present open member")
	}
	if err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, DestinationRelative: destinationRelative, Error: err, OriginalOwnerPending: !p.closed}, err
	}
	return canonicalEntryMoveOriginal(ctx, p.root, p.parent, p.members[index], member, destinationRelative, p.sha256[index])
}

func canonicalEntryIsFinalRelative(relative string) bool {
	for _, final := range []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, WindowsCanonicalIdentityRelativePathV1} {
		if strings.EqualFold(relative, final) {
			return true
		}
	}
	return false
}

// Windows can expose several names for one ordinary object (including short
// names). Textual relative-path comparisons are only diagnostics. A positive
// discard must additionally refuse the original native object selected by ANY
// final canonical target, with original parent/file guards held continuously.
func canonicalEntryRefuseFinalObject(root *WindowsCanonicalEntrySetV1, parent, member *canonicalEntryNativeFile) (error, error) {
	var closeErrors error
	for _, relative := range []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, WindowsCanonicalIdentityRelativePathV1} {
		directory, leaf, err := canonicalEntryRelativeParent(root, parent, relative)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			continue
		}
		if err != nil {
			return closeErrors, err
		}
		h, err := canonicalEntryOpenNative(windows.Handle(directory.file.Fd()), leaf, false, windows.FILE_READ_DATA, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
		if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			continue
		}
		if err != nil {
			return closeErrors, err
		}
		id, idErr := canonicalEntryNativeID(h, false)
		closeErr := windows.CloseHandle(h)
		closeErrors = errors.Join(closeErrors, closeErr)
		if idErr != nil {
			return closeErrors, idErr
		}
		if id == member.identity {
			return closeErrors, errors.New("refuse discard of original object selected by a canonical final target")
		}
		if closeErr != nil {
			return closeErrors, nil
		}
	}
	return closeErrors, nil
}

// The caller owns committed transaction intent. This native port merely
// disposes its exact original non-final object and receives actual absence.
func canonicalEntryDiscardOriginal(ctx context.Context, root *WindowsCanonicalEntrySetV1, parent, member *canonicalEntryNativeFile, memberName, expectedRelative, expectedHash string) (result WindowsCanonicalEntryMoveResultV1, retErr error) {
	result.Member = memberName
	result.SourceRelative = expectedRelative
	result.OriginalOwnerPending = root != nil && !root.closed
	defer func() { result.Error = errors.Join(retErr, result.CloseError); retErr = result.Error }()
	if ctx == nil {
		return result, errors.New("nil original discard context")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !publisherSourcePathValid(expectedRelative) || canonicalEntryIsFinalRelative(expectedRelative) {
		return result, errors.New("original discard requires the exact non-final owned stage/aside")
	}
	relative, err := canonicalEntryMemberRelative(parent, member)
	if err != nil {
		return result, err
	}
	if relative != expectedRelative {
		return result, errors.New("original discard expected relative path disagrees with current owned object")
	}
	if err = canonicalEntryVerifyOriginalMember(member, expectedHash); err != nil {
		return result, err
	}
	finalCloseErr, finalErr := canonicalEntryRefuseFinalObject(root, parent, member)
	result.CloseError = errors.Join(result.CloseError, finalCloseErr)
	if finalErr != nil || finalCloseErr != nil {
		return result, finalErr
	}
	directory, leaf, closeErr, err := canonicalEntryCurrentMember(root, parent, member)
	result.CloseError = errors.Join(result.CloseError, closeErr)
	if err != nil || closeErr != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	remove := byte(1)
	var iosb windows.IO_STATUS_BLOCK
	result.Attempted = true
	err = windows.NtSetInformationFile(windows.Handle(member.file.Fd()), &iosb, &remove, uint32(unsafe.Sizeof(remove)), windows.FileDispositionInformation)
	canonicalEntryReceiveNativeStatus(&result, err)
	if err != nil {
		return result, err
	}
	result.Applied = true
	result.CloseError = errors.Join(result.CloseError, canonicalEntryCloseOriginalMember(root, member))
	absenceCloseErr, absenceErr := canonicalEntryProbeAbsent(directory, leaf)
	result.CloseError = errors.Join(result.CloseError, absenceCloseErr)
	result.Readback = absenceErr == nil && result.CloseError == nil
	return result, errors.Join(absenceErr, ctx.Err())
}

func (p *WindowsCanonicalEntryPriorSetV1) DiscardOriginal(ctx context.Context, member, expectedRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if p == nil {
		err := errors.New("missing original prior owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err == nil && p.discardResults[index] != nil {
		result := *p.discardResults[index]
		if result.SourceRelative == expectedRelative {
			return result, result.Error
		}
		err = errors.New("prior discard was already received for another expected path")
	}
	if err == nil && (!p.ready || p.closed || !p.present[index]) {
		err = errors.New("prior discard requires an acquired present open member")
	}
	if err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err, OriginalOwnerPending: !p.closed}, err
	}
	result, err := canonicalEntryDiscardOriginal(ctx, p.root, p.parent, p.members[index], member, expectedRelative, p.sha256[index])
	if result.Applied {
		p.discardResults[index] = &result
	}
	return result, err
}

func (s *StagedWindowsCanonicalEntrySetV1) DiscardOriginal(ctx context.Context, member, expectedRelative string) (WindowsCanonicalEntryMoveResultV1, error) {
	if s == nil {
		err := errors.New("missing staged original owner")
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	index, err := canonicalEntryMemberIndex(member)
	if err == nil && s.discardResults[index] != nil {
		result := *s.discardResults[index]
		if result.SourceRelative == expectedRelative {
			return result, result.Error
		}
		err = errors.New("staged discard was already received for another expected path")
	}
	if err == nil && (!s.ready || !s.taken || s.closed) {
		err = errors.New("staged discard requires the taken open transaction owner")
	}
	if err != nil {
		return WindowsCanonicalEntryMoveResultV1{Member: member, SourceRelative: expectedRelative, Error: err, OriginalOwnerPending: !s.closed}, err
	}
	result, err := canonicalEntryDiscardOriginal(ctx, s.root, s.parent, s.members[index], member, expectedRelative, s.cleanupSHA256[index])
	if result.Applied {
		s.discardResults[index] = &result
	}
	return result, err
}

func (p *WindowsCanonicalEntryPriorSetV1) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return p.closeError
	}
	p.closed = true
	p.closeError = p.root.Close()
	return p.closeError
}
