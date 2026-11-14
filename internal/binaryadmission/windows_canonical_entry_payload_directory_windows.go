//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

func canonicalPayloadDirectoryRelativeV1() string {
	return path.Dir(WindowsCanonicalProductRelativePathV1)
}

func canonicalPayloadDirectoryIdentity(file *canonicalEntryNativeFile) (WindowsCanonicalEntryDirectoryIdentityV1, error) {
	if file == nil || file.file == nil {
		return WindowsCanonicalEntryDirectoryIdentityV1{}, errors.New("original directory reference unavailable")
	}
	return canonicalPayloadDirectoryHandleIdentity(windows.Handle(file.file.Fd()))
}

func canonicalPayloadDirectoryHandleIdentity(handle windows.Handle) (WindowsCanonicalEntryDirectoryIdentityV1, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return WindowsCanonicalEntryDirectoryIdentityV1{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return WindowsCanonicalEntryDirectoryIdentityV1{}, errors.New("original directory is reparse or wrong-kind")
	}
	return WindowsCanonicalEntryDirectoryIdentityV1{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow, uint64(info.CreationTime.HighDateTime)<<32 | uint64(info.CreationTime.LowDateTime)}, nil
}

func sameCanonicalDirectoryObject(a, b WindowsCanonicalEntryDirectoryIdentityV1) bool {
	return a.VolumeSerialNumber == b.VolumeSerialNumber && a.FileIndexHigh == b.FileIndexHigh && a.FileIndexLow == b.FileIndexLow
}

func validateCanonicalPayloadExpected(snapshot WindowsCanonicalEntryPayloadDirectorySnapshotV1) error {
	if snapshot.Schema != WindowsCanonicalEntryPayloadDirectorySchemaV1 || !filepath.IsAbs(snapshot.Root) || filepath.Clean(snapshot.Root) != snapshot.Root || snapshot.RelativePath != canonicalPayloadDirectoryRelativeV1() {
		return errors.New("original durable payload identity required")
	}
	return nil
}

// payloadParentPreimageLocked returns nil only for captured parent absence.
// A missing Product leaf has a retained parent and is a different preimage.
func (p *WindowsCanonicalEntryPriorSetV1) payloadParentPreimageLocked(ctx context.Context) (*canonicalEntryNativeFile, error) {
	if ctx == nil {
		return nil, errors.New("payload parent observation context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !p.ready || p.closed || p.parent == nil || p.parent.file == nil {
		return nil, errors.New("original prior owner is not ready/open")
	}
	index, err := canonicalEntryMemberIndex("product")
	if err != nil {
		return nil, err
	}
	missing := p.missingComponents[index]
	if !p.present[index] && missing == canonicalPayloadDirectoryRelativeV1() && p.payloadParent == nil {
		return nil, nil
	}
	if p.payloadParent != nil && p.payloadParent.file != nil &&
		((p.present[index] && missing == "") || (!p.present[index] && missing == WindowsCanonicalProductRelativePathV1)) {
		return p.payloadParent, nil
	}
	return nil, errors.New("original payload parent preimage is unavailable")
}

// ObservePayloadParentAbsent receives the present preimage under the caller's
// existing fence. A true result describes this lookup, not a reservation.
func (p *WindowsCanonicalEntryPriorSetV1) ObservePayloadParentAbsent(ctx context.Context) (absent bool, retErr error) {
	if p == nil {
		return false, errors.New("original prior owner required for payload parent observation")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	expected, err := p.payloadParentPreimageLocked(ctx)
	if err != nil {
		return false, err
	}
	rootID, err := canonicalPayloadDirectoryIdentity(p.parent)
	if err != nil {
		return false, err
	}
	rootProbe := &WindowsCanonicalEntrySetV1{}
	defer func() {
		retErr = errors.Join(retErr, rootProbe.Close())
		if retErr != nil {
			absent = false
		}
	}()
	currentRoot, err := rootProbe.openAbsolute(p.parent.path, true)
	if err != nil {
		return false, err
	}
	currentRootID, err := canonicalPayloadDirectoryIdentity(currentRoot)
	if err != nil {
		return false, err
	}
	if currentRoot.identity != p.parent.identity || currentRootID != rootID {
		return false, errors.New("original payload root identity changed")
	}
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE)
	if expected != nil {
		// The held original keeps name custody while the read probe coexists.
		share |= windows.FILE_SHARE_DELETE
	}
	handle, err := canonicalEntryOpenNative(windows.Handle(p.parent.file.Fd()), canonicalPayloadDirectoryRelativeV1(), true, windows.FILE_TRAVERSE, share, windows.FILE_OPEN)
	if expected == nil {
		if err == nil {
			return false, errors.Join(errors.New("payload parent appeared after original absence"), windows.CloseHandle(handle))
		}
		if !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
			return false, err
		}
	} else {
		if err != nil {
			return false, err
		}
		actualID, queryErr := canonicalPayloadDirectoryHandleIdentity(handle)
		if err = errors.Join(queryErr, windows.CloseHandle(handle)); err != nil {
			return false, err
		}
		expectedID, err := canonicalPayloadDirectoryIdentity(expected)
		if err != nil {
			return false, err
		}
		if actualID != expectedID || actualID.VolumeSerialNumber != rootID.VolumeSerialNumber {
			return false, errors.New("original payload parent identity changed")
		}
	}
	afterRootID, err := canonicalPayloadDirectoryIdentity(p.parent)
	if err != nil {
		return false, err
	}
	if afterRootID != rootID {
		return false, errors.New("original payload root changed during observation")
	}
	return expected == nil, ctx.Err()
}

func (s *StagedWindowsCanonicalEntrySetV1) EnsureOriginalPayloadDirectory(ctx context.Context) (owner *WindowsCanonicalEntryPayloadDirectoryV1, retErr error) {
	owner = &WindowsCanonicalEntryPayloadDirectoryV1{guard: &WindowsCanonicalEntrySetV1{}}
	if s == nil {
		return owner, errors.New("original staged owner required for payload directory")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.root.payloadDirectoryOwner(); existing != nil {
		owner = existing
		_, err := owner.Snapshot()
		return owner, err
	}
	if !s.ready || s.closed || s.root == nil {
		return owner, errors.New("original staged owner is not ready/open")
	}
	if s.root.payloadDirectoryLink == nil {
		s.root.payloadDirectoryLink = &canonicalEntryPayloadDirectoryLinkV1{}
	}
	s.root.payloadDirectoryLink.mu.Lock()
	prior := s.root.payloadDirectoryLink.prior
	s.root.payloadDirectoryLink.mu.Unlock()
	var expectedParent *canonicalEntryNativeFile
	if prior != nil {
		prior.mu.Lock()
		defer prior.mu.Unlock()
		var err error
		expectedParent, err = prior.payloadParentPreimageLocked(ctx)
		if err != nil {
			return owner, err
		}
		if prior.parent.identity != s.parent.identity {
			return owner, errors.New("original prior payload root differs from staging root")
		}
	}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	s.root.payloadDirectoryLink.mu.Lock()
	s.root.payloadDirectoryLink.owner = owner
	s.root.payloadDirectoryLink.mu.Unlock()
	defer func() { owner.acquisitionError = retErr }()
	if ctx == nil {
		return owner, errors.New("payload directory context required")
	}
	if err := ctx.Err(); err != nil {
		return owner, err
	}
	owner.snapshot = WindowsCanonicalEntryPayloadDirectorySnapshotV1{Schema: WindowsCanonicalEntryPayloadDirectorySchemaV1, Root: s.parent.path, RelativePath: canonicalPayloadDirectoryRelativeV1()}
	var err error
	owner.parent, err = owner.guard.openAbsolute(s.parent.path, true)
	if err != nil {
		return owner, err
	}
	owner.snapshot.ParentIdentity, err = canonicalPayloadDirectoryIdentity(owner.parent)
	if err != nil {
		return owner, err
	}
	stageParent, err := canonicalPayloadDirectoryIdentity(s.parent)
	if err != nil {
		return owner, err
	}
	if stageParent != owner.snapshot.ParentIdentity {
		return owner, errors.New("payload directory parent differs from original staging root")
	}
	// A captured absent parent only authorizes FILE_CREATE. A later occupant
	// makes that operation fail instead of being adopted as prior presence.
	create := prior != nil && expectedParent == nil
	var h windows.Handle
	if !create {
		h, err = canonicalEntryOpenNative(windows.Handle(owner.parent.file.Fd()), owner.snapshot.RelativePath, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
		create = prior == nil && errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND)
		if err != nil && !create {
			return owner, err
		}
	}
	if create {
		if err = ctx.Err(); err != nil {
			return owner, err
		}
		h, err = canonicalEntryOpenNative(windows.Handle(owner.parent.file.Fd()), owner.snapshot.RelativePath, true, windows.FILE_TRAVERSE|windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_CREATE)
		if err != nil {
			return owner, err
		}
		owner.snapshot.Created = true
	}
	owner.directory, err = owner.guard.retain(h, filepath.Join(owner.snapshot.Root, owner.snapshot.RelativePath), true)
	if err != nil {
		return owner, err
	}
	owner.snapshot.DirectoryIdentity, err = canonicalPayloadDirectoryIdentity(owner.directory)
	if err != nil {
		return owner, err
	}
	if owner.snapshot.DirectoryIdentity.VolumeSerialNumber != owner.snapshot.ParentIdentity.VolumeSerialNumber {
		return owner, errors.New("payload directory crosses original root volume")
	}
	if expectedParent != nil {
		expectedID, err := canonicalPayloadDirectoryIdentity(expectedParent)
		if err != nil {
			return owner, err
		}
		if owner.snapshot.DirectoryIdentity != expectedID {
			return owner, errors.New("payload directory differs from the retained prior parent")
		}
	}
	owner.ready = true
	return owner, nil
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) Snapshot() (WindowsCanonicalEntryPayloadDirectorySnapshotV1, error) {
	if d == nil {
		return WindowsCanonicalEntryPayloadDirectorySnapshotV1{}, errors.New("payload directory owner unavailable")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.ready || d.closed {
		return d.snapshot, errors.Join(d.acquisitionError, d.closeError, errors.New("payload directory owner is not ready/open"))
	}
	return d.snapshot, nil
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) validateLocked(ctx context.Context) error {
	if !d.ready || d.closed || d.directory == nil || d.directory.file == nil {
		return errors.Join(d.acquisitionError, errors.New("original payload directory is not ready/open"))
	}
	if ctx == nil {
		return errors.New("original payload directory context required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.source != nil {
		if err := d.source.ValidateOriginalRecovery(ctx); err != nil {
			return err
		}
	}
	parent, err := canonicalPayloadDirectoryIdentity(d.parent)
	if err != nil {
		return err
	}
	directory, err := canonicalPayloadDirectoryIdentity(d.directory)
	if err != nil {
		return err
	}
	if parent != d.snapshot.ParentIdentity || directory != d.snapshot.DirectoryIdentity {
		return errors.New("original payload directory/parent identity or creation identity changed")
	}
	return nil
}

// Borrow only an actual directory alias of this live original owner. The
// compatible probe never weakens its DELETE-denying guard; all other directory
// opens retain the ordinary restrictive sharing contract.
func canonicalEntryBorrowPayloadDirectory(root *WindowsCanonicalEntrySetV1, parent *canonicalEntryNativeFile, component string) (*canonicalEntryNativeFile, bool, error) {
	d := root.payloadDirectoryOwner()
	if d == nil {
		return nil, false, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.validateLocked(context.Background()); err != nil {
		return nil, false, err
	}
	parentID, err := canonicalPayloadDirectoryIdentity(parent)
	if err != nil {
		return nil, false, err
	}
	if !sameCanonicalDirectoryObject(parentID, d.snapshot.ParentIdentity) {
		return nil, false, nil
	}
	if parentID != d.snapshot.ParentIdentity {
		return nil, false, errors.New("original payload parent creation identity changed")
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), component, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	identity, queryErr := canonicalPayloadDirectoryHandleIdentity(h)
	closeErr := windows.CloseHandle(h)
	if err = errors.Join(queryErr, closeErr); err != nil {
		return nil, false, err
	}
	if !sameCanonicalDirectoryObject(identity, d.snapshot.DirectoryIdentity) {
		return nil, false, nil
	}
	if identity != d.snapshot.DirectoryIdentity {
		return nil, false, errors.New("original payload creation identity changed")
	}
	if !root.payloadDirectoryBorrowed {
		if d.borrowers == ^uint64(0) {
			return nil, false, errors.New("payload borrower representation overflow")
		}
		d.borrowers++
		root.payloadDirectoryBorrowed = true
	}
	return d.directory, true, nil
}

func (o *WindowsCanonicalEntrySetV1) releasePayloadDirectoryBorrow() error {
	if !o.payloadDirectoryBorrowed {
		return nil
	}
	d := o.payloadDirectoryOwner()
	if d == nil {
		return errors.New("original payload borrow owner unavailable")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.borrowers == 0 {
		return errors.New("original payload borrow settlement unavailable")
	}
	d.borrowers--
	o.payloadDirectoryBorrowed = false
	return nil
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) DisposeOriginalIfEmpty(ctx context.Context) (result WindowsCanonicalEntryMoveResultV1, retErr error) {
	result.Member = "payload-directory"
	result.SourceRelative = canonicalPayloadDirectoryRelativeV1()
	if d == nil {
		result.Error = errors.New("original payload directory owner required")
		return result, result.Error
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.disposition != nil {
		return *d.disposition, d.disposition.Error
	}
	result.OriginalOwnerPending = !d.closed
	defer func() {
		result.Error = errors.Join(retErr, result.CloseError)
		retErr = result.Error
		if result.Applied {
			copy := result
			d.disposition = &copy
		}
	}()
	if err := d.validateLocked(ctx); err != nil {
		return result, err
	}
	if !d.snapshot.Created {
		return result, errors.New("prior-present payload directory has no original-created deletion authority")
	}
	if d.borrowers != 0 {
		return result, errors.New("original payload child borrowers are not settled")
	}
	h, err := canonicalEntryOpenNative(windows.Handle(d.parent.file.Fd()), d.snapshot.RelativePath, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if err != nil {
		return result, err
	}
	current, queryErr := canonicalPayloadDirectoryHandleIdentity(h)
	result.CloseError = windows.CloseHandle(h)
	if queryErr != nil || result.CloseError != nil {
		return result, queryErr
	}
	if current != d.snapshot.DirectoryIdentity {
		return result, errors.New("original payload name no longer selects its created object")
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	remove := byte(1)
	var iosb windows.IO_STATUS_BLOCK
	result.Attempted = true
	err = windows.NtSetInformationFile(windows.Handle(d.directory.file.Fd()), &iosb, &remove, uint32(unsafe.Sizeof(remove)), windows.FileDispositionInformation)
	canonicalEntryReceiveNativeStatus(&result, err)
	if err != nil {
		return result, err
	} // Native directory emptiness is authoritative.
	result.Applied = true
	result.CloseError = canonicalEntryCloseOriginalMember(d.guard, d.directory)
	probe, absenceErr := canonicalEntryOpenNative(windows.Handle(d.parent.file.Fd()), d.snapshot.RelativePath, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
	if absenceErr == nil {
		result.CloseError = errors.Join(result.CloseError, windows.CloseHandle(probe))
		absenceErr = errors.New("payload directory name is occupied after original close")
	} else if errors.Is(absenceErr, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		absenceErr = nil
	}
	result.Readback = absenceErr == nil && result.CloseError == nil
	d.closeError = errors.Join(result.CloseError, d.guard.Close())
	d.closed = true
	result.CloseError = d.closeError
	result.OriginalOwnerPending = d.closeError != nil
	return result, errors.Join(absenceErr, ctx.Err())
}

func (d *WindowsCanonicalEntryPayloadDirectoryV1) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return d.closeError
	}
	if d.borrowers != 0 {
		return errors.New("payload directory close requires settled child borrowers")
	}
	d.closed = true
	if d.guard != nil {
		d.closeError = d.guard.Close()
	}
	return d.closeError
}

func AcquireWindowsCanonicalEntryPayloadDirectoryForRecoveryV1(ctx context.Context, source WindowsCanonicalEntryPayloadDirectoryRecoverySourceV1) (owner *WindowsCanonicalEntryPayloadDirectoryV1, retErr error) {
	owner = &WindowsCanonicalEntryPayloadDirectoryV1{guard: &WindowsCanonicalEntrySetV1{}, source: source}
	owner.mu.Lock()
	defer owner.mu.Unlock()
	defer func() { owner.acquisitionError = retErr }()
	if ctx == nil || source == nil {
		return owner, errors.New("original payload recovery Source/context required")
	}
	if err := source.ValidateOriginalRecovery(ctx); err != nil {
		return owner, err
	}
	calls := 0
	var receivingErr error
	err := source.WithOriginalPayloadDirectoryRecoveryInput(ctx, func(expected WindowsCanonicalEntryPayloadDirectorySnapshotV1) error {
		calls++
		if calls != 1 {
			receivingErr = errors.Join(receivingErr, errors.New("original payload recovery input received more than once"))
			return receivingErr
		}
		receivingErr = func() error {
			if err := validateCanonicalPayloadExpected(expected); err != nil {
				return err
			}
			owner.snapshot = expected
			owner.snapshot.Created = false
			owner.snapshot.DirectoryIdentity = WindowsCanonicalEntryDirectoryIdentityV1{}
			var err error
			owner.parent, err = owner.guard.openAbsolute(expected.Root, true)
			if err != nil {
				return err
			}
			parentID, err := canonicalPayloadDirectoryIdentity(owner.parent)
			if err != nil {
				return err
			}
			owner.snapshot.ParentIdentity = parentID
			if parentID != expected.ParentIdentity {
				return errors.New("payload recovery original parent identity mismatch")
			}
			access := uint32(windows.FILE_TRAVERSE)
			if expected.Created {
				access |= windows.DELETE
			}
			h, err := canonicalEntryOpenNative(windows.Handle(owner.parent.file.Fd()), expected.RelativePath, true, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
			if err != nil {
				return err
			}
			owner.directory, err = owner.guard.retain(h, filepath.Join(expected.Root, expected.RelativePath), true)
			if err != nil {
				return err
			}
			actual, err := canonicalPayloadDirectoryIdentity(owner.directory)
			if err != nil {
				return err
			}
			owner.snapshot.DirectoryIdentity = actual
			if actual != expected.DirectoryIdentity {
				return errors.New("payload recovery occupant is foreign or has another creation identity")
			}
			owner.snapshot.Created = expected.Created
			return nil
		}()
		return receivingErr
	})
	if calls != 1 {
		return owner, errors.Join(err, receivingErr, errors.New("original payload recovery input not uniquely received"))
	}
	if err = errors.Join(err, receivingErr); err != nil {
		return owner, err
	}
	if err = source.ValidateOriginalRecovery(ctx); err != nil {
		return owner, err
	}
	owner.ready = true
	return owner, nil
}

func ObserveWindowsCanonicalEntryPayloadDirectoryAbsenceV1(ctx context.Context, source WindowsCanonicalEntryPayloadDirectoryAbsenceSourceV1) (observation WindowsCanonicalEntryPayloadDirectoryAbsenceObservationV1, retErr error) {
	if ctx == nil || source == nil {
		return observation, errors.New("original payload absence Source/context required")
	}
	if err := source.ValidateOriginalRecovery(ctx); err != nil {
		return observation, err
	}
	guard := &WindowsCanonicalEntrySetV1{}
	closeReceived := false
	defer func() {
		if !closeReceived {
			retErr = errors.Join(retErr, guard.Close())
		}
	}()
	calls := 0
	var receivingErr error
	err := source.WithOriginalPayloadDirectoryAbsenceInput(ctx, func(expected WindowsCanonicalEntryPayloadDirectorySnapshotV1) error {
		calls++
		if calls != 1 {
			receivingErr = errors.Join(receivingErr, errors.New("original payload absence input received more than once"))
			return receivingErr
		}
		receivingErr = func() error {
			if err := validateCanonicalPayloadExpected(expected); err != nil {
				return err
			}
			if !expected.Created {
				return errors.New("original-created durable payload identity required for absence observation")
			}
			parent, err := guard.openAbsolute(expected.Root, true)
			if err != nil {
				return err
			}
			identity, err := canonicalPayloadDirectoryIdentity(parent)
			if err != nil {
				return err
			}
			observation.Root = expected.Root
			observation.RelativePath = expected.RelativePath
			observation.ParentIdentity = identity
			if identity != expected.ParentIdentity {
				return errors.New("original payload absence parent identity/creation mismatch")
			}
			h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), expected.RelativePath, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN)
			if errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
				return nil
			}
			if err != nil {
				return err
			}
			return errors.Join(errors.New("original payload directory name is occupied"), windows.CloseHandle(h))
		}()
		return receivingErr
	})
	if calls != 1 {
		err = errors.Join(err, errors.New("original payload absence input not uniquely received"))
	}
	err = errors.Join(err, receivingErr, guard.Close())
	closeReceived = true
	// The Source/fence must remain valid through native parent release. No old
	// deletion, directory identity or close outcome is reconstructed here.
	err = errors.Join(err, source.ValidateOriginalRecovery(ctx))
	if err != nil {
		return observation, err
	}
	observation.Absent = true
	return observation, nil
}
