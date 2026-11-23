//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func (o *WindowsCanonicalEntryDispatchSetV2) retain(h windows.Handle, name string, directory bool) (*canonicalEntryNativeFile, error) {
	f, err := canonicalEntryRetainNative(&o.files, h, name, directory)
	return o.receiveOriginal(f, directory, err)
}

func (o *WindowsCanonicalEntryDispatchSetV2) receiveOriginal(f *canonicalEntryNativeFile, directory bool, err error) (*canonicalEntryNativeFile, error) {
	if f != nil {
		if !directory {
			for _, prior := range o.originals {
				if prior.identity == f.identity {
					return f, errors.New("current canonical entry file roles alias one original object")
				}
			}
		}
		o.originals = append(o.originals, f)
	}
	return f, err
}

func (o *WindowsCanonicalEntryDispatchSetV2) open(ctx context.Context, name string) (*canonicalEntryNativeFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if original := o.acquisition; original != nil {
		open, retain := canonicalEntryObservedOpenCallbacks(ctx, &original.opens)
		return canonicalEntryOpenAbsoluteWithOpen(name, false, windows.FILE_SHARE_READ, windows.FILE_OPEN,
			func(handle windows.Handle, path string, directory bool) (*canonicalEntryNativeFile, error) {
				f, err := retain(handle, path, directory)
				return o.receiveOriginal(f, directory, err)
			}, nil, open)
	}
	return canonicalEntryOpenAbsolute(name, false, windows.FILE_SHARE_READ, windows.FILE_OPEN, o.retain, nil)
}

func currentCanonicalEntryPaths(entryPath string, registry WindowsProductRegistryV4) (productPath, identityPath string, err error) {
	identityRelative, err := registry.IdentityRelativePath()
	if err != nil {
		return "", "", err
	}
	if registry.SchemaVersion != 4 {
		return "", "", errors.New("current canonical entry requires registry4")
	}
	if !filepath.IsAbs(entryPath) || filepath.Clean(entryPath) != entryPath || filepath.Base(entryPath) != registry.Artifacts[1].Filename {
		return "", "", errors.New("current canonical entry requires exact absolute launcher path")
	}
	root := filepath.Dir(entryPath)
	return filepath.Join(root, filepath.FromSlash(registry.Artifacts[0].Filename)), filepath.Join(root, filepath.FromSlash(identityRelative)), nil
}

// Receive the immutable compiled registry before locating an identity. A
// current read failure never probes or retries the historical layout.
func AcquireDispatchWindowsCanonicalEntrySetV2(ctx context.Context, launcherBuildID string) (*WindowsCanonicalEntryDispatchSetV2, error) {
	if !validLowerHex(launcherBuildID, 32, 32) {
		return nil, errors.New("current canonical entry BuildID is missing or invalid")
	}
	registry, err := CompiledWindowsProductRegistry()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return acquireCurrentCanonicalEntryOriginal(ctx, executable, registry, launcherBuildID)
}

// AcquireInstalledWindowsCanonicalEntryDispatchSetV2 uses the executing
// Product's linker binding and the caller's canonical installed launcher path.
// It owns current file objects only, with the same read-only scope as launch.
func AcquireInstalledWindowsCanonicalEntryDispatchSetV2(ctx context.Context, launcherPath string, binding WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntryDispatchSetV2, error) {
	registry, err := installedCanonicalEntryRegistry(binding)
	if err != nil {
		return nil, err
	}
	return acquireBoundCurrentCanonicalEntryOriginal(ctx, launcherPath, registry, binding.BuildID(), &binding)
}

func installedCanonicalEntryRegistry(binding WindowsCanonicalEntryBuildBindingV1) (WindowsProductRegistryV4, error) {
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return WindowsProductRegistryV4{}, err
	}
	return CompiledWindowsProductRegistry()
}

func AcquireInstalledWindowsCanonicalEntryDispatchSetV2Observed(ctx context.Context, launcherPath string,
	binding WindowsCanonicalEntryBuildBindingV1) (owner *WindowsCanonicalEntryDispatchSetV2, observation *InstalledCanonicalEntryAcquisitionObservation, err error) {
	observation = &InstalledCanonicalEntryAcquisitionObservation{}
	defer func() { observation.returned, observation.err = true, err }()
	registry, err := installedCanonicalEntryRegistry(binding)
	if err != nil {
		return nil, observation, err
	}
	owner = &WindowsCanonicalEntryDispatchSetV2{acquisition: observation}
	observation.owner = owner
	err = owner.acquireCurrent(ctx, launcherPath, registry, binding.BuildID(), &binding)
	return owner, observation, err
}

func acquireCurrentCanonicalEntryOriginal(ctx context.Context, entryPath string, registry WindowsProductRegistryV4, dispatchBuildID string) (*WindowsCanonicalEntryDispatchSetV2, error) {
	return acquireBoundCurrentCanonicalEntryOriginal(ctx, entryPath, registry, dispatchBuildID, nil)
}

func acquireBoundCurrentCanonicalEntryOriginal(ctx context.Context, entryPath string, registry WindowsProductRegistryV4, dispatchBuildID string, binding *WindowsCanonicalEntryBuildBindingV1) (*WindowsCanonicalEntryDispatchSetV2, error) {
	o := &WindowsCanonicalEntryDispatchSetV2{}
	return o, o.acquireCurrent(ctx, entryPath, registry, dispatchBuildID, binding)
}

func (o *WindowsCanonicalEntryDispatchSetV2) acquireCurrent(ctx context.Context, entryPath string, registry WindowsProductRegistryV4, dispatchBuildID string, binding *WindowsCanonicalEntryBuildBindingV1) error {
	if ctx == nil {
		return errors.New("nil current canonical entry context")
	}
	if !validLowerHex(dispatchBuildID, 32, 32) {
		return errors.New("current canonical entry BuildID is missing or invalid")
	}
	productPath, identityPath, err := currentCanonicalEntryPaths(entryPath, registry)
	if err != nil {
		return err
	}
	for _, target := range []struct {
		name string
		slot **canonicalEntryNativeFile
	}{{entryPath, &o.entry}, {productPath, &o.product}, {identityPath, &o.identity}} {
		if *target.slot, err = o.open(ctx, target.name); err != nil {
			return err
		}
	}
	if err = o.receive(ctx, registry, dispatchBuildID, binding); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	o.ready = true
	return nil
}

func (o *WindowsCanonicalEntryDispatchSetV2) receive(ctx context.Context, registry WindowsProductRegistryV4, dispatchBuildID string, binding *WindowsCanonicalEntryBuildBindingV1) error {
	base, identity, _, err := receiveCanonicalEntryFilesWithRegistry(ctx, [3]*os.File{o.entry.file, o.product.file, o.identity.file}, WindowsCanonicalEntrySetTargetsV1{o.entry.path, o.product.path, o.identity.path}, binding, dispatchBuildID, registry)
	if err != nil {
		return err
	}
	runtimePath := filepath.Join(filepath.Dir(o.entry.path), filepath.FromSlash(registry.RuntimePayload.Filename))
	if o.runtimeIndex, err = o.open(ctx, runtimePath); err != nil {
		return err
	}
	indexBytes, err := readHeldCanonicalRuntimeIndex(ctx, o.runtimeIndex.file)
	if err != nil {
		return err
	}
	runtimeHash := RuntimeDigest(indexBytes)
	if runtimeHash != identity.RuntimePayloadSHA256 {
		return errors.New("original current runtime index differs from authenticated digest")
	}
	if _, err = ParseRuntimePayload(indexBytes); err != nil {
		return err
	}
	o.snapshot = WindowsCanonicalEntryDispatchSnapshotV2{BuildID: base.BuildID, RegistryProjectionSHA256: base.RegistryProjectionSHA256, SourceEpoch: identity.SourceEpoch, CanonicalEntry: base.CanonicalEntry, Product: base.Product, Identity: base.Identity, RuntimeIndex: WindowsCanonicalEntryFileV1{o.runtimeIndex.path, runtimeHash}, ProductArtifact: base.ProductArtifact}
	return nil
}

// Read the original runtime index through its retained file object. Its
// exact canonical extent is known before any file content is read.
func readHeldCanonicalRuntimeIndex(ctx context.Context, f *os.File) ([]byte, error) {
	if ctx == nil || f == nil {
		return nil, errors.New("held runtime index and context required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	extent, err := canonicalRuntimePayloadExtent()
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != extent {
		return nil, errors.New("original current runtime index extent differs from the canonical schema")
	}
	// A single extra byte detects growth between the native size and read
	// observations. The held original is never reopened or read unboundedly.
	raw, readErr := io.ReadAll(&canonicalEntryBuildContextReader{ctx, io.NewSectionReader(f, 0, extent+1)})
	var extentErr error
	if int64(len(raw)) != extent {
		extentErr = errors.New("original current runtime index extent changed during bounded read")
	}
	return raw, errors.Join(readErr, extentErr, ctx.Err())
}

func (o *WindowsCanonicalEntryDispatchSetV2) DispatchSnapshot() (WindowsCanonicalEntryDispatchSnapshotV2, error) {
	if o == nil {
		return WindowsCanonicalEntryDispatchSnapshotV2{}, errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.ready || o.closed {
		return WindowsCanonicalEntryDispatchSnapshotV2{}, errors.New("current canonical entry owner is not admitted/open")
	}
	return o.snapshot, nil
}

func (o *WindowsCanonicalEntryDispatchSetV2) verifyLocked(ctx context.Context) error {
	if !o.ready || o.closed {
		return errors.New("current canonical entry owner is not admitted/open")
	}
	for _, original := range o.originals {
		info, err := original.file.Stat()
		if err != nil {
			return err
		}
		id, err := canonicalEntryNativeID(windows.Handle(original.file.Fd()), info.IsDir())
		if err != nil || id != original.identity {
			return errors.Join(errors.New("current canonical entry original identity changed"), err)
		}
	}
	for _, pair := range []struct {
		original *canonicalEntryNativeFile
		expected WindowsCanonicalEntryFileV1
	}{{o.entry, o.snapshot.CanonicalEntry}, {o.product, o.snapshot.Product}, {o.identity, o.snapshot.Identity}} {
		digest, err := canonicalEntryHashFile(ctx, pair.original.file)
		if err != nil || digest != pair.expected.SHA256 {
			return errors.Join(fmt.Errorf("current canonical entry original bytes changed: %s", pair.expected.Path), err)
		}
	}
	indexBytes, err := readHeldCanonicalRuntimeIndex(ctx, o.runtimeIndex.file)
	if err != nil || RuntimeDigest(indexBytes) != o.snapshot.RuntimeIndex.SHA256 {
		return errors.Join(fmt.Errorf("current canonical entry original bytes changed: %s", o.snapshot.RuntimeIndex.Path), err)
	}
	return nil
}

func (o *WindowsCanonicalEntryDispatchSetV2) VerifyUnchanged() error {
	if o == nil {
		return errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.verifyLocked(context.Background())
}

// Synchronous borrow only: the caller owns and settles the original process.
// All original file guards remain held until the foreground owner receives Close.
func (o *WindowsCanonicalEntryDispatchSetV2) VerifyOriginalProductProcessHandle(borrowedProcessHandle uintptr) error {
	return o.VerifyCurrentProductProcessPath(context.Background(), borrowedProcessHandle)
}

// VerifyCurrentProductProcessPath compares the retained current file with the
// running handle's reported image path. It does not read the mapped image.
func (o *WindowsCanonicalEntryDispatchSetV2) VerifyCurrentProductProcessPath(ctx context.Context, borrowedProcessHandle uintptr) error {
	if o == nil || ctx == nil {
		return errors.New("missing current canonical entry owner or operation context")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyLocked(ctx); err != nil {
		return err
	}
	return errors.Join(verifyCanonicalEntryOriginalProductProcessHandle(o.product, borrowedProcessHandle, nil), ctx.Err())
}

type ProductProcessImageObservation struct {
	opens     canonicalEntryObservedOpens
	imageErr  error
	finalized bool
}

func (o *ProductProcessImageObservation) ResourcesFinalized() bool {
	return o != nil && o.finalized && o.opens.resourcesFinalized()
}

// VerifyCurrentProductProcessPathObserved compares the same retained Product
// while preserving the temporary image path probe's original handles.
func (o *WindowsCanonicalEntryDispatchSetV2) VerifyCurrentProductProcessPathObserved(ctx context.Context,
	borrowedProcessHandle uintptr) (observation *ProductProcessImageObservation, retErr error) {
	observation = &ProductProcessImageObservation{}
	defer func() { observation.finalized = true }()
	if o == nil || ctx == nil {
		return observation, errors.New("missing current canonical entry owner or operation context")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyLocked(ctx); err != nil {
		return observation, err
	}
	path, err := queryCanonicalEntryProcessImagePath(borrowedProcessHandle)
	observation.imageErr = err
	if err != nil {
		return observation, err
	}
	defer func() { retErr = errors.Join(retErr, observation.opens.close()) }()
	open, retain := canonicalEntryObservedOpenCallbacks(ctx, &observation.opens)
	file, err := canonicalEntryOpenAbsoluteWithOpen(path, false, windows.FILE_SHARE_READ, windows.FILE_OPEN, retain, nil, open)
	if err == nil && file.identity != o.product.identity {
		err = errors.New("actual original process image differs from retained Product")
	}
	return observation, errors.Join(err, ctx.Err())
}

func canonicalEntryObservedOpenCallbacks(ctx context.Context, history *canonicalEntryObservedOpens) (
	func(windows.Handle, string, bool, uint32, uint32, uint32) (windows.Handle, error),
	func(windows.Handle, string, bool) (*canonicalEntryNativeFile, error)) {
	open := observeCanonicalEntryOpen(ctx, history, canonicalEntryOpenNative, windows.CloseHandle)
	retain := func(handle windows.Handle, name string, directory bool) (*canonicalEntryNativeFile, error) {
		slot := history.slots[len(history.slots)-1]
		if slot.native != uintptr(handle) || !slot.present || slot.openErr != nil || slot.file != nil {
			return nil, errors.New("original native file acquisition unavailable")
		}
		slot.file = os.NewFile(uintptr(handle), name)
		if slot.file == nil {
			return nil, errors.New("native file wrapper unavailable")
		}
		identity, err := canonicalEntryNativeID(handle, directory)
		if err != nil {
			return nil, err
		}
		return &canonicalEntryNativeFile{file: slot.file, identity: identity, path: name}, nil
	}
	return open, retain
}

func observeCanonicalEntryOpen(ctx context.Context, history *canonicalEntryObservedOpens,
	open func(windows.Handle, string, bool, uint32, uint32, uint32) (windows.Handle, error), close func(windows.Handle) error,
) func(windows.Handle, string, bool, uint32, uint32, uint32) (windows.Handle, error) {
	return func(parent windows.Handle, name string, directory bool, access, share, disposition uint32) (windows.Handle, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		handle, err := open(parent, name, directory, access, share, disposition)
		history.register(uintptr(handle), handle != 0 && handle != windows.InvalidHandle, err, func() error { return close(handle) })
		return handle, err
	}
}

func (o *WindowsCanonicalEntryDispatchSetV2) Close() error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return o.closeError
	}
	o.closed = true
	if observed := o.acquisition; observed != nil {
		o.closeError = observed.opens.close()
		if !observed.opens.resourcesFinalized() {
			o.closeError = errors.Join(o.closeError, errors.New("original installed Product acquisition closure unavailable"))
		}
		return o.closeError
	}
	for i := len(o.files) - 1; i >= 0; i-- {
		if o.files[i] != nil {
			o.closeError = errors.Join(o.closeError, o.files[i].Close())
		}
	}
	return o.closeError
}
