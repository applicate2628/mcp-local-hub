//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"golang.org/x/sys/windows"
)

// Enumeration uses a duplicate of the retained original directory handle. Each
// os.File starts a fresh native directory scan; no pathname is followed/reopened.
func canonicalEntryReadOriginalDirectory(guard *WindowsCanonicalEntrySetV1, directory *canonicalEntryNativeFile) (entries []os.DirEntry, retErr error) {
	var borrowed windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(process, windows.Handle(directory.file.Fd()), process, &borrowed, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(borrowed), directory.path)
	if file == nil {
		return nil, errors.Join(errors.New("original directory enumeration wrapper unavailable"), windows.CloseHandle(borrowed))
	}
	// Keep each issued duplicate with the same original guard until successful
	// close. A failed close remains retained and visible to the caller's Close.
	guard.files = append(guard.files, file)
	defer func() {
		closeErr := file.Close()
		retErr = errors.Join(retErr, closeErr)
		if closeErr == nil {
			guard.files[len(guard.files)-1] = nil
			guard.files = guard.files[:len(guard.files)-1]
		} else {
			guard.closeError = errors.Join(guard.closeError, closeErr)
		}
	}()
	return file.ReadDir(-1)
}

func (o *WindowsCanonicalEntrySetV2) openChild(parent *canonicalEntryNativeFile, name string, directory bool) (*canonicalEntryNativeFile, error) {
	if !RuntimeRelativePath(name) || strings.Contains(name, "/") {
		return nil, errors.New("invalid direct current bundle child")
	}
	access := uint32(windows.FILE_READ_DATA)
	share := uint32(windows.FILE_SHARE_READ)
	if directory {
		access = windows.FILE_TRAVERSE | windows.FILE_LIST_DIRECTORY
		share |= windows.FILE_SHARE_WRITE
	}
	h, err := canonicalEntryOpenNative(windows.Handle(parent.file.Fd()), name, directory, access, share, windows.FILE_OPEN)
	if err != nil {
		return nil, err
	}
	return o.guard.retain(h, filepath.Join(parent.path, name), directory)
}

func acquireCanonicalEntryBundleOriginal(ctx context.Context, targets WindowsCanonicalEntrySetTargetsV2, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV2, error) {
	registry, registryErr := CompiledWindowsProductRegistry()
	o := &WindowsCanonicalEntrySetV2{guard: &WindowsCanonicalEntrySetV1{}, objects: map[string]*canonicalEntryNativeFile{}, ctx: ctx}
	if registryErr != nil {
		return o, registryErr
	}
	if ctx == nil {
		return o, errors.New("nil current canonical entry context")
	}
	want, err := CanonicalEntryTargetsV2(targets.CanonicalEntryPath)
	if err != nil || want != targets {
		return o, errors.Join(errors.New("current canonical entry targets disagree with registry"), err)
	}
	if err = ctx.Err(); err != nil {
		return o, err
	}
	o.parent, err = o.guard.openAbsolute(filepath.Dir(targets.CanonicalEntryPath), true)
	if err != nil {
		return o, err
	}
	o.guard.entry, err = o.openChild(o.parent, filepath.Base(targets.CanonicalEntryPath), false)
	if err != nil {
		return o, err
	}
	o.bundle, err = o.openChild(o.parent, filepath.Base(targets.BundlePath), true)
	if err != nil {
		return o, err
	}
	o.guard.product, err = o.openChild(o.bundle, path.Base(WindowsCanonicalProductRelativePathV1), false)
	if err != nil {
		return o, err
	}
	o.objects[path.Base(WindowsCanonicalProductRelativePathV1)] = o.guard.product
	o.guard.identity, err = o.openChild(o.bundle, registry.IdentityManifest.Filename, false)
	if err != nil {
		return o, err
	}
	o.objects[registry.IdentityManifest.Filename] = o.guard.identity
	base, identity, comments, err := receiveCanonicalEntryFilesWithRegistry(ctx, [3]*os.File{o.guard.entry.file, o.guard.product.file, o.guard.identity.file}, targets.entryTargets(), &binding, "", registry)
	if err != nil {
		return o, err
	}
	if identity.Schema != "windows-product-identity-v2" || comments.runtimeSHA256 == "" || comments.sourceEpoch == "" {
		return o, errors.New("current native bundle requires complete identity/runtime/source binding")
	}
	runtimeIndex, err := o.openChild(o.bundle, identity.RuntimePayloadPath, false)
	if err != nil {
		return o, err
	}
	o.objects[identity.RuntimePayloadPath] = runtimeIndex
	manifestRaw, err := readHeldCanonicalRuntimeIndex(ctx, runtimeIndex.file)
	if err != nil {
		return o, err
	}
	inventory, err := canonicalDeclaredBundleInventoryV2(ctx, targets.BundlePath, identity, manifestRaw, o.guard.product, o.guard.identity)
	if err != nil {
		return o, err
	}
	for _, object := range inventory.Objects {
		if o.objects[object.Path] != nil {
			continue
		}
		parent := o.bundle
		if name := path.Dir(object.Path); name != "." {
			parent = o.objects[name]
		}
		if parent == nil {
			return o, errors.New("declared current member has no retained original parent")
		}
		file, openErr := o.openChild(parent, path.Base(object.Path), object.Kind == "directory")
		if openErr != nil {
			return o, openErr
		}
		o.objects[object.Path] = file
	}
	o.snapshot, err = currentCanonicalEntrySnapshot(base, comments.entry, manifestRaw, inventory)
	if err != nil {
		return o, err
	}
	parentID, err := canonicalPayloadDirectoryIdentity(o.parent)
	if err != nil {
		return o, err
	}
	bundleID, err := canonicalPayloadDirectoryIdentity(o.bundle)
	if err != nil {
		return o, err
	}
	o.directorySnapshot = WindowsCanonicalEntryPayloadDirectorySnapshotV1{Schema: WindowsCanonicalEntryPayloadDirectorySchemaV1, Root: filepath.Dir(targets.CanonicalEntryPath), RelativePath: o.snapshot.BundleRelativePath, ParentIdentity: parentID, DirectoryIdentity: bundleID}
	o.ready = true
	if err = o.verifyLocked(); err != nil {
		o.ready = false
		return o, err
	}
	return o, nil
}

func currentCanonicalEntrySnapshot(base canonicalEntryFilesObservation, binding WindowsCanonicalEntryBuildBindingV1, manifest []byte, inventory RuntimeInventoryV1) (WindowsCanonicalEntrySetSnapshotV2, error) {
	r, err := CompiledWindowsProductRegistry()
	if err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	identity, err := ParseWindowsProductIdentityV2(base.IdentityBytes)
	if err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	identityRelative, err := r.IdentityRelativePath()
	if err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	raw, err := CanonicalRuntimeJSON(inventory)
	if err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	if _, err = ParseRuntimeInventory(raw); err != nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, err
	}
	v := WindowsCanonicalEntrySetSnapshotV2{Schema: WindowsCanonicalEntrySetSchemaV2, BuildID: base.BuildID, RegistryProjectionSHA256: base.RegistryProjectionSHA256, CanonicalEntryRelativePath: r.Artifacts[1].Filename, ProductRelativePath: r.Artifacts[0].Filename, IdentityRelativePath: identityRelative, ReverseProjection: binding.ReverseProjection(), CanonicalEntry: base.CanonicalEntry, Product: base.Product, Identity: base.Identity, Machine: base.ProductArtifact.Machine, ProductArtifact: base.ProductArtifact, IdentityBytes: slices.Clone(base.IdentityBytes), BundleRelativePath: path.Dir(r.Artifacts[0].Filename), BundleInventory: inventory.Objects, BundleTreeSHA256: RuntimeDigest(raw), RuntimePayload: WindowsCanonicalEntryFileV1{Path: r.RuntimePayload.Filename, SHA256: identity.RuntimePayloadSHA256}, RuntimePayloadBytes: slices.Clone(manifest)}
	if err = ValidateWindowsCanonicalEntrySetSnapshotV2(v); err != nil {
		return v, err
	}
	return cloneCanonicalEntrySnapshotV2(v), nil
}

func AdmitWindowsCanonicalEntrySetV2(ctx context.Context, entryPath string, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV2, error) {
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return nil, err
	}
	targets, err := CanonicalEntryTargetsV2(entryPath)
	if err != nil {
		return nil, err
	}
	return acquireCanonicalEntryBundleOriginal(ctx, targets, binding)
}

func AcquireExecutingWindowsCanonicalEntrySetV2(ctx context.Context, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntrySetV2, error) {
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	entry := filepath.Clean(filepath.Join(filepath.Dir(executable), filepath.FromSlash(binding.ReverseProjection())))
	targets, err := CanonicalEntryTargetsV2(entry)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(executable, targets.ProductPath) {
		return nil, errors.New("executing Product is outside current registry layout")
	}
	return acquireCanonicalEntryBundleOriginal(ctx, targets, binding)
}

func AcquireWindowsCanonicalEntrySetV2(ctx context.Context, targets WindowsCanonicalEntrySetTargetsV2, expected WindowsCanonicalEntrySetSnapshotV2) (*WindowsCanonicalEntrySetV2, error) {
	if err := ValidateWindowsCanonicalEntrySetSnapshotV2(expected); err != nil {
		return nil, err
	}
	binding, err := NewWindowsCanonicalEntryBuildBindingV1(expected.BuildID, expected.CanonicalEntry.SHA256, expected.ReverseProjection)
	if err != nil {
		return nil, err
	}
	o, err := acquireCanonicalEntryBundleOriginal(ctx, targets, binding)
	if err != nil {
		return o, err
	}
	if !compareSnapshotV2(o.snapshot, expected) {
		o.ready = false
		return o, errors.New("current native bundle differs from complete expected journaled set")
	}
	return o, nil
}

func (o *WindowsCanonicalEntrySetV2) verifyLocked() error {
	if !o.ready || o.closed || o.guard == nil {
		return errors.New("current canonical entry owner is not admitted/open")
	}
	if err := o.ctx.Err(); err != nil {
		return err
	}
	parentID, err := canonicalPayloadDirectoryIdentity(o.parent)
	if err != nil {
		return err
	}
	bundleID, err := canonicalPayloadDirectoryIdentity(o.bundle)
	if err != nil {
		return err
	}
	if parentID != o.directorySnapshot.ParentIdentity || bundleID != o.directorySnapshot.DirectoryIdentity {
		return errors.New("original current bundle directory identity changed")
	}
	seenObjects := make(map[canonicalEntryNativeIdentity]bool, len(o.objects)+1)
	for name, file := range o.objects {
		objectIndex := sort.Search(len(o.snapshot.BundleInventory), func(i int) bool { return o.snapshot.BundleInventory[i].Path >= name })
		if objectIndex == len(o.snapshot.BundleInventory) || o.snapshot.BundleInventory[objectIndex].Path != name {
			return errors.New("unaccounted original current bundle object")
		}
		object := o.snapshot.BundleInventory[objectIndex]
		id, err := canonicalEntryNativeID(windows.Handle(file.file.Fd()), object.Kind == "directory")
		if err != nil {
			return err
		}
		if id != file.identity {
			return errors.New("original current bundle object identity changed")
		}
		if seenObjects[id] {
			return errors.New("declared current bundle objects alias one original")
		}
		seenObjects[id] = true
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(windows.Handle(file.file.Fd()), &info); err != nil {
			return err
		}
		if info.FileAttributes&7 != object.Attributes {
			return errors.New("original current bundle attributes changed")
		}
		if object.Kind == "file" {
			stat, err := file.file.Stat()
			if err != nil {
				return err
			}
			if object.Size == nil || stat.Size() != *object.Size {
				return errors.New("original current bundle extent changed")
			}
			hash, err := canonicalEntryHashFile(o.ctx, file.file)
			if err != nil {
				return err
			}
			if hash != object.SHA256 {
				return errors.New("original current bundle bytes changed")
			}
		}
	}
	payload, err := ParseRuntimePayload(o.snapshot.RuntimePayloadBytes)
	if err != nil {
		return err
	}
	for _, component := range payload.Components {
		if err := o.verifyDirectoryMembers(o.objects[component.Root], component.Root); err != nil {
			return err
		}
	}
	entry := o.guard.entry
	id, err := canonicalEntryNativeID(windows.Handle(entry.file.Fd()), false)
	if err != nil {
		return err
	}
	if id != entry.identity {
		return errors.New("original launcher identity changed")
	}
	if seenObjects[id] {
		return errors.New("original launcher aliases a declared bundle member")
	}
	digest, err := canonicalEntryHashFile(o.ctx, entry.file)
	if err != nil {
		return err
	}
	if digest != o.snapshot.CanonicalEntry.SHA256 {
		return errors.New("original launcher bytes changed")
	}
	return nil
}

func (o *WindowsCanonicalEntrySetV2) verifyDirectoryMembers(directory *canonicalEntryNativeFile, prefix string) error {
	return canonicalEntryVerifyDirectoryMembers(o.ctx, o.guard, o.objects, directory, prefix)
}

func canonicalEntryVerifyDirectoryMembers(ctx context.Context, guard *WindowsCanonicalEntrySetV1, objects map[string]*canonicalEntryNativeFile, directory *canonicalEntryNativeFile, prefix string) error {
	entries, err := canonicalEntryReadOriginalDirectory(guard, directory)
	if err != nil {
		return err
	}
	count := 0
	for name := range objects {
		if path.Dir(name) == prefix || prefix == "" && path.Dir(name) == "." {
			count++
		}
	}
	if len(entries) != count {
		return errors.New("original current bundle directory membership changed")
	}
	for _, entry := range entries {
		name := path.Join(prefix, entry.Name())
		file := objects[name]
		if file == nil {
			return errors.New("unknown current bundle child appeared")
		}
		if entry.IsDir() {
			if err := canonicalEntryVerifyDirectoryMembers(ctx, guard, objects, file, name); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (o *WindowsCanonicalEntrySetV2) Snapshot() (WindowsCanonicalEntrySetSnapshotV2, error) {
	if o == nil {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ready || o.closed {
		return WindowsCanonicalEntrySetSnapshotV2{}, errors.New("current canonical entry owner is not admitted/open")
	}
	return cloneCanonicalEntrySnapshotV2(o.snapshot), nil
}
func (o *WindowsCanonicalEntrySetV2) BundleDirectorySnapshot() (WindowsCanonicalEntryPayloadDirectorySnapshotV1, error) {
	if o == nil {
		return WindowsCanonicalEntryPayloadDirectorySnapshotV1{}, errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ready || o.closed {
		return WindowsCanonicalEntryPayloadDirectorySnapshotV1{}, errors.New("current canonical entry owner is not admitted/open")
	}
	return o.directorySnapshot, nil
}
func (o *WindowsCanonicalEntrySetV2) VerifyUnchanged() error {
	if o == nil {
		return errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.verifyLocked()
}

func (o *WindowsCanonicalEntrySetV2) VerifyOriginalProductProcessHandle(handle uintptr) error {
	if o == nil {
		return errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyLocked(); err != nil {
		return err
	}
	return verifyCanonicalEntryOriginalProductProcessHandle(o.guard.product, handle, nil)
}
func (o *WindowsCanonicalEntrySetV2) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.closed = true
		if o.guard != nil {
			o.closeError = o.guard.Close()
		}
	}
	return o.closeError
}
