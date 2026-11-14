//go:build windows

package binaryadmission

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// Files are acquired using the pinned x/sys/windows NtCreateFile primitive.
// Directory name guards deny deletion; image/identity guards deny write/delete.
// Every component is opened relative to its retained parent, never followed.
func canonicalEntryNativeID(h windows.Handle, directory bool) (canonicalEntryNativeIdentity, error) {
	var v windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &v); err != nil {
		return canonicalEntryNativeIdentity{}, err
	}
	if v.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (v.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || (!directory && v.NumberOfLinks != 1) {
		return canonicalEntryNativeIdentity{}, errors.New("canonical entry object is linked, reparse, or wrong kind")
	}
	return canonicalEntryNativeIdentity{v.VolumeSerialNumber, v.FileIndexHigh, v.FileIndexLow}, nil
}

func canonicalEntryOpenNative(parent windows.Handle, name string, directory bool, access, share, disposition uint32) (windows.Handle, error) {
	u, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: u, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_NON_DIRECTORY_FILE)
	if directory {
		options = windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_DIRECTORY_FILE
	}
	var h windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	err = windows.NtCreateFile(&h, access|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE, &oa, &iosb, &allocation, windows.FILE_ATTRIBUTE_NORMAL, share, disposition, options, 0, 0)
	return h, err
}

func (o *WindowsCanonicalEntrySetV1) retain(h windows.Handle, name string, directory bool) (*canonicalEntryNativeFile, error) {
	f := os.NewFile(uintptr(h), name)
	if f == nil {
		return nil, errors.Join(errors.New("native file wrapper unavailable"), windows.CloseHandle(h))
	}
	o.files = append(o.files, f)
	id, err := canonicalEntryNativeID(h, directory)
	if err != nil {
		return nil, err
	}
	return &canonicalEntryNativeFile{file: f, identity: id, path: name}, nil
}

func nativeCanonicalEntryPathParts(name string) (string, []string, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, `\\?\`) || strings.HasPrefix(name, `\\.\`) {
		return "", nil, errors.New("canonical entry path must be exact absolute ordinary path")
	}
	volume := filepath.VolumeName(name)
	rest := strings.TrimPrefix(strings.TrimPrefix(name, volume), `\`)
	parts := strings.Split(rest, `\`)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `/:`) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", nil, errors.New("canonical entry invalid path component")
		}
	}
	return volume, parts, nil
}

func (o *WindowsCanonicalEntrySetV1) openAbsolute(name string, directory bool) (*canonicalEntryNativeFile, error) {
	return o.openAbsoluteWithOptions(name, directory, windows.FILE_SHARE_READ, windows.FILE_OPEN)
}

// Only destination staging permits FILE_OPEN_IF for directories. Source and
// recovery admission keep FILE_OPEN and cannot create missing evidence.
func (o *WindowsCanonicalEntrySetV1) openAbsoluteWithOptions(name string, directory bool, leafShare, directoryDisposition uint32) (*canonicalEntryNativeFile, error) {
	volume, parts, err := nativeCanonicalEntryPathParts(name)
	if err != nil {
		return nil, err
	}
	anchor := `\??\` + volume + `\`
	if strings.HasPrefix(volume, `\\`) {
		anchor = `\??\UNC\` + strings.TrimPrefix(volume, `\\`) + `\`
	}
	h, err := canonicalEntryOpenNative(0, anchor, true, windows.FILE_TRAVERSE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_OPEN)
	if err != nil {
		return nil, err
	}
	parent, err := o.retain(h, volume+`\`, true)
	if err != nil {
		return nil, err
	}
	path := volume + `\`
	for i, part := range parts {
		isDir := i < len(parts)-1 || directory
		if isDir {
			borrowed, matched, borrowErr := canonicalEntryBorrowPayloadDirectory(o, parent, part)
			if borrowErr != nil {
				return nil, borrowErr
			}
			if matched {
				path = filepath.Join(path, part)
				parent = borrowed
				continue
			}
		}
		access := uint32(windows.FILE_READ_DATA)
		share := leafShare
		disposition := uint32(windows.FILE_OPEN)
		if isDir {
			access = windows.FILE_TRAVERSE
			share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
			disposition = directoryDisposition
		}
		h, err = canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), part, isDir, access, share, disposition)
		if err != nil {
			return nil, fmt.Errorf("open original component %q: %w", part, err)
		}
		path = filepath.Join(path, part)
		parent, err = o.retain(h, path, isDir)
		if err != nil {
			return nil, err
		}
	}
	return parent, nil
}

func canonicalEntryHashFile(ctx context.Context, f *os.File) (string, error) {
	if ctx == nil {
		return "", errors.New("nil canonical entry context")
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	r := io.NewSectionReader(f, 0, info.Size())
	buf := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := r.Read(buf)
		if n > 0 {
			if _, err := h.Write(buf[:n]); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func acquireCanonicalEntryOriginal(ctx context.Context, targets WindowsCanonicalEntrySetTargetsV1, binding *WindowsCanonicalEntryBuildBindingV1, dispatchBuildID string) (*WindowsCanonicalEntrySetV1, error) {
	o := &WindowsCanonicalEntrySetV1{}
	if ctx == nil {
		return o, errors.New("nil canonical entry context")
	}
	want, err := CanonicalEntryTargetsV1(targets.CanonicalEntryPath)
	if err != nil || want != targets {
		return o, errors.Join(errors.New("canonical entry targets disagree with registry"), err)
	}
	if err = ctx.Err(); err != nil {
		return o, err
	}
	if o.entry, err = o.openAbsolute(targets.CanonicalEntryPath, false); err != nil {
		return o, err
	}
	if o.product, err = o.openAbsolute(targets.ProductPath, false); err != nil {
		return o, err
	}
	if o.identity, err = o.openAbsolute(targets.IdentityPath, false); err != nil {
		return o, err
	}
	if err = o.receive(ctx, binding, dispatchBuildID); err != nil {
		return o, err
	}
	o.ready = true
	return o, nil
}

func (o *WindowsCanonicalEntrySetV1) receive(ctx context.Context, binding *WindowsCanonicalEntryBuildBindingV1, dispatchBuildID string) error {
	pi, err := o.product.file.Stat()
	if err != nil {
		return err
	}
	metadata, err := readWindowsBuildIdentityAt(o.product.file, pi.Size())
	if err != nil {
		return err
	}
	_, actualBinding, err := ParseWindowsCanonicalEntryCommentsV1(metadata.Comments)
	if err != nil {
		return err
	}
	if binding != nil && actualBinding != *binding {
		return errors.New("executing Product build binding disagrees with original Product")
	}
	if dispatchBuildID != "" && actualBinding.BuildID() != dispatchBuildID {
		return errors.New("executing canonical entry generation disagrees with Product")
	}
	eh, err := o.entry.file.Stat()
	if err != nil {
		return err
	}
	entryHeader, err := readWindowsPEHeader(o.entry.file, eh.Size())
	if err != nil {
		return err
	}
	productHeader, err := readWindowsPEHeader(o.product.file, pi.Size())
	if err != nil {
		return err
	}
	if entryHeader.subsystem != WindowsCUISubsystem || productHeader.subsystem != WindowsGUISubsystem || entryHeader.machine != productHeader.machine || (entryHeader.machine != 0x8664 && entryHeader.machine != 0xaa64) || (entryHeader.characteristics|productHeader.characteristics)&windowsPEDLLCharacteristic != 0 || metadata.Role != WindowsArtifactRoleProduct {
		return errors.New("canonical entry PE role/machine disagreement")
	}
	entryHash, err := canonicalEntryHashFile(ctx, o.entry.file)
	if err != nil {
		return err
	}
	if entryHash != actualBinding.ExpectedCanonicalEntrySHA256() {
		return errors.New("original canonical entry differs from build-issued digest")
	}
	productHash, err := canonicalEntryHashFile(ctx, o.product.file)
	if err != nil {
		return err
	}
	ii, err := o.identity.file.Stat()
	if err != nil {
		return err
	}
	if err := validateWindowsProductIdentitySize(ii.Size()); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.NewSectionReader(o.identity.file, 0, ii.Size()))
	if err != nil {
		return err
	}
	identity, err := ParseWindowsProductIdentity(raw)
	if err != nil {
		return err
	}
	artifact := WindowsArtifact{Path: o.product.path, Role: metadata.Role, Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: productHash, Machine: productHeader.machine, Subsystem: productHeader.subsystem}
	if identity.SHA256 != productHash || identity.Version != artifact.Version || identity.Commit != artifact.Commit || identity.BuildDate != artifact.BuildDate || identity.Machine != artifact.Machine {
		return errors.New("original Product and identity-v1 disagree")
	}
	o.snapshot = WindowsCanonicalEntrySetSnapshotV1{
		Schema: WindowsCanonicalEntrySetSchemaV1, BuildID: actualBinding.BuildID(), RegistryProjectionSHA256: WindowsCanonicalEntryRegistryProjectionSHA256V1(),
		CanonicalEntryRelativePath: WindowsCanonicalEntryRelativePathV1, ProductRelativePath: WindowsCanonicalProductRelativePathV1, IdentityRelativePath: WindowsCanonicalIdentityRelativePathV1, ReverseProjection: actualBinding.ReverseProjection(),
		CanonicalEntry: WindowsCanonicalEntryFileV1{o.entry.path, entryHash}, Product: WindowsCanonicalEntryFileV1{o.product.path, productHash}, Identity: WindowsCanonicalEntryFileV1{o.identity.path, canonicalEntrySHA256(raw)}, Machine: entryHeader.machine, ProductArtifact: artifact, IdentityBytes: raw,
	}
	return validateCanonicalEntrySnapshotV1(o.snapshot)
}

func AcquireExecutingWindowsCanonicalEntrySetV1(ctx context.Context, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV1, error) {
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	entry := filepath.Clean(filepath.Join(filepath.Dir(executable), filepath.FromSlash(binding.ReverseProjection())))
	targets, err := CanonicalEntryTargetsV1(entry)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(executable, targets.ProductPath) {
		return nil, errors.New("executing Product is outside exact registry layout")
	}
	return acquireCanonicalEntryOriginal(ctx, targets, &binding, "")
}

// Admit is the build/validator entry point. Its expected binding is supplied by
// the original build issuer, independently of the observed entry bytes.
func AdmitWindowsCanonicalEntrySetV1(ctx context.Context, canonicalEntryPath string, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV1, error) {
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return nil, err
	}
	targets, err := CanonicalEntryTargetsV1(canonicalEntryPath)
	if err != nil {
		return nil, err
	}
	return acquireCanonicalEntryOriginal(ctx, targets, &binding, "")
}

func AcquireDispatchWindowsCanonicalEntrySetV1(ctx context.Context, launcherBuildID string) (*WindowsCanonicalEntrySetV1, error) {
	if !validLowerHex(launcherBuildID, 32, 32) {
		return nil, errors.New("canonical entry BuildID is missing or invalid")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	targets, err := CanonicalEntryTargetsV1(executable)
	if err != nil {
		return nil, err
	}
	return acquireCanonicalEntryOriginal(ctx, targets, nil, launcherBuildID)
}

func AcquireWindowsCanonicalEntrySetV1(ctx context.Context, targets WindowsCanonicalEntrySetTargetsV1, expected WindowsCanonicalEntrySetSnapshotV1) (*WindowsCanonicalEntrySetV1, error) {
	if err := validateCanonicalEntrySnapshotV1(expected); err != nil {
		return nil, err
	}
	binding, err := NewWindowsCanonicalEntryBuildBindingV1(expected.BuildID, expected.CanonicalEntry.SHA256, expected.ReverseProjection)
	if err != nil {
		return nil, err
	}
	o, err := acquireCanonicalEntryOriginal(ctx, targets, &binding, "")
	if err != nil {
		return o, err
	}
	actual := o.snapshot
	if actual.CanonicalEntry.SHA256 != expected.CanonicalEntry.SHA256 || actual.Product.SHA256 != expected.Product.SHA256 || actual.Identity.SHA256 != expected.Identity.SHA256 || !bytes.Equal(actual.IdentityBytes, expected.IdentityBytes) {
		o.ready = false
		return o, errors.New("canonical entry set differs from expected journaled set")
	}
	return o, nil
}

func (o *WindowsCanonicalEntrySetV1) Snapshot() (WindowsCanonicalEntrySetSnapshotV1, error) {
	if o == nil {
		return WindowsCanonicalEntrySetSnapshotV1{}, errors.New("missing canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ready || o.closed {
		return WindowsCanonicalEntrySetSnapshotV1{}, errors.New("canonical entry owner is not admitted/open")
	}
	v := o.snapshot
	v.IdentityBytes = append([]byte(nil), v.IdentityBytes...)
	return v, nil
}

func (o *WindowsCanonicalEntrySetV1) verifyLocked() error {
	if !o.ready || o.closed {
		return errors.New("canonical entry owner is not admitted/open")
	}
	for _, f := range []*canonicalEntryNativeFile{o.entry, o.product, o.identity} {
		id, err := canonicalEntryNativeID(windows.Handle(f.file.Fd()), false)
		if err != nil {
			return err
		}
		if id != f.identity {
			return errors.New("canonical entry original file identity changed")
		}
		hash, err := canonicalEntryHashFile(context.Background(), f.file)
		if err != nil {
			return err
		}
		want := o.snapshot.CanonicalEntry.SHA256
		if f == o.product {
			want = o.snapshot.Product.SHA256
		}
		if f == o.identity {
			want = o.snapshot.Identity.SHA256
		}
		if hash != want {
			return errors.New("canonical entry original bytes changed")
		}
	}
	return nil
}

func (o *WindowsCanonicalEntrySetV1) VerifyUnchanged() error {
	if o == nil {
		return errors.New("missing canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.verifyLocked()
}

// VerifyOriginalProductProcessHandle is a synchronous borrow from the original
// at-create process owner before Resume. It never owns/closes that process
// handle. The creator must have held this set continuously across native create
// with explicit lpApplicationName selecting its Product. This proves the image
// file, not the process's memory or a caller-authored PID/path assertion.
func (o *WindowsCanonicalEntrySetV1) VerifyOriginalProductProcessHandle(borrowedProcessHandle uintptr) error {
	if o == nil {
		return errors.New("missing canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyLocked(); err != nil {
		return err
	}
	return verifyCanonicalEntryOriginalProductProcessHandle(o.product, borrowedProcessHandle, nil)
}

func verifyCanonicalEntryOriginalProductProcessHandle(product *canonicalEntryNativeFile, borrowedProcessHandle uintptr, prior *WindowsCanonicalEntryPriorSetV1) error {
	if product == nil || product.file == nil {
		return errors.New("missing original Product object")
	}
	leafShare := uint32(windows.FILE_SHARE_READ)
	if prior != nil {
		// The caller holds this original prior owner's mutex through the query.
		// Only its live retained DELETE object permits a compatible READ probe.
		if !prior.ready || prior.closed || prior.root == nil || prior.root.closed {
			return errors.New("original prior owner is not live for image verification")
		}
		owned := false
		for _, member := range prior.members {
			if member == product && member.file != nil {
				owned = true
				break
			}
		}
		if !owned {
			return errors.New("image proof object is not retained by this original prior owner")
		}
		leafShare = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE
	}
	var payloadDirectoryLink *canonicalEntryPayloadDirectoryLinkV1
	if prior != nil {
		payloadDirectoryLink = prior.root.payloadDirectoryLink
	}
	return verifyCanonicalEntryProductProcessImage(product, borrowedProcessHandle, leafShare, payloadDirectoryLink)
}

// Callers retain and validate the original Product under its owning set's
// mutex. This compatible name probe only compares the process image to that
// object; it never replaces the original handle or grants disposition rights.
func verifyCanonicalEntryProductProcessImage(product *canonicalEntryNativeFile, borrowedProcessHandle uintptr, leafShare uint32, payloadDirectoryLink *canonicalEntryPayloadDirectoryLinkV1) error {
	if borrowedProcessHandle == 0 || windows.Handle(borrowedProcessHandle) == windows.InvalidHandle {
		return errors.New("missing original process handle")
	}
	var actualImagePath string
	for size := uint32(256); ; {
		buf := make([]uint16, int(size))
		n := size
		err := windows.QueryFullProcessImageName(windows.Handle(borrowedProcessHandle), 0, &buf[0], &n)
		if err == nil {
			actualImagePath = windows.UTF16ToString(buf[:n])
			break
		}
		if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return err
		}
		if uint64(size)*2 > uint64(^uint32(0)) || uint64(size)*2 > uint64(int(^uint(0)>>1)) {
			return err
		}
		size *= 2
	}
	probe := &WindowsCanonicalEntrySetV1{payloadDirectoryLink: payloadDirectoryLink}
	f, err := probe.openAbsoluteWithOptions(actualImagePath, false, leafShare, windows.FILE_OPEN)
	if err == nil && f.identity != product.identity {
		err = errors.New("actual original process image differs from retained Product")
	}
	return errors.Join(err, probe.Close())
}

func (o *WindowsCanonicalEntrySetV1) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.closeError
	}
	o.closed = true
	for i := len(o.files) - 1; i >= 0; i-- {
		if o.files[i] != nil {
			o.closeError = errors.Join(o.closeError, o.files[i].Close())
		}
	}
	if o.closeError == nil {
		o.closeError = o.releasePayloadDirectoryBorrow()
	}
	return o.closeError
}
