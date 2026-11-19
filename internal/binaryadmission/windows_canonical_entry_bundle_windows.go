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

func acquireCurrentCanonicalEntryOriginal(ctx context.Context, entryPath string, registry WindowsProductRegistryV4, dispatchBuildID string) (*WindowsCanonicalEntryDispatchSetV2, error) {
	o := &WindowsCanonicalEntryDispatchSetV2{}
	if ctx == nil {
		return o, errors.New("nil current canonical entry context")
	}
	if !validLowerHex(dispatchBuildID, 32, 32) {
		return o, errors.New("current canonical entry BuildID is missing or invalid")
	}
	productPath, identityPath, err := currentCanonicalEntryPaths(entryPath, registry)
	if err != nil {
		return o, err
	}
	for _, target := range []struct {
		name string
		slot **canonicalEntryNativeFile
	}{{entryPath, &o.entry}, {productPath, &o.product}, {identityPath, &o.identity}} {
		if *target.slot, err = o.open(ctx, target.name); err != nil {
			return o, err
		}
	}
	if err = o.receive(ctx, registry, dispatchBuildID); err != nil {
		return o, err
	}
	if err = ctx.Err(); err != nil {
		return o, err
	}
	o.ready = true
	return o, nil
}

func (o *WindowsCanonicalEntryDispatchSetV2) receive(ctx context.Context, registry WindowsProductRegistryV4, dispatchBuildID string) error {
	base, identity, _, err := receiveCanonicalEntryFilesWithRegistry(ctx, [3]*os.File{o.entry.file, o.product.file, o.identity.file}, WindowsCanonicalEntrySetTargetsV1{o.entry.path, o.product.path, o.identity.path}, nil, dispatchBuildID, registry)
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

func (o *WindowsCanonicalEntryDispatchSetV2) verifyLocked() error {
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
		digest, err := canonicalEntryHashFile(context.Background(), pair.original.file)
		if err != nil || digest != pair.expected.SHA256 {
			return errors.Join(fmt.Errorf("current canonical entry original bytes changed: %s", pair.expected.Path), err)
		}
	}
	indexBytes, err := readHeldCanonicalRuntimeIndex(context.Background(), o.runtimeIndex.file)
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
	return o.verifyLocked()
}

// Synchronous borrow only: the caller owns and settles the original process.
// All original file guards remain held until the foreground owner receives Close.
func (o *WindowsCanonicalEntryDispatchSetV2) VerifyOriginalProductProcessHandle(borrowedProcessHandle uintptr) error {
	if o == nil {
		return errors.New("missing current canonical entry owner")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err := o.verifyLocked(); err != nil {
		return err
	}
	return verifyCanonicalEntryOriginalProductProcessHandle(o.product, borrowedProcessHandle, nil)
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
	for i := len(o.files) - 1; i >= 0; i-- {
		if o.files[i] != nil {
			o.closeError = errors.Join(o.closeError, o.files[i].Close())
		}
	}
	return o.closeError
}
