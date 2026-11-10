package binaryadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// BuildObservation is portable build DATA. It owns no native admission,
// staging or promotion authority. The original issuer's writers must already
// be settled; before/after observations do not provide a hostile-writer lock.
type WindowsCanonicalEntryBuildObservationV1 struct {
	EntrySet    WindowsCanonicalEntrySetSnapshotV1
	SourceEpoch string
}

var ErrWindowsCanonicalEntryBuildObservationUnsupported = errors.New("Windows canonical entry build observation is unsupported on this host")

type canonicalEntryBuildFileObservationV1 struct {
	Device, Index, Links uint64
	Attributes           uint32
}

type canonicalEntryBuildFileV1 struct {
	file     *os.File
	info     os.FileInfo
	native   canonicalEntryBuildFileObservationV1
	relative string
}

type canonicalEntryBuildContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *canonicalEntryBuildContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func hashCanonicalEntryBuildFileV1(ctx context.Context, file canonicalEntryBuildFileV1) (string, error) {
	h := sha256.New()
	r := &canonicalEntryBuildContextReader{ctx, io.NewSectionReader(file.file, 0, file.info.Size())}
	if _, err := io.Copy(h, r); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func currentCanonicalEntryBuildFileV1(root *os.Root, file canonicalEntryBuildFileV1) error {
	held, err := file.file.Stat()
	if err != nil {
		return err
	}
	native, err := observeCanonicalEntryBuildFileV1(file.file)
	if err != nil {
		return err
	}
	current, err := publisherSourceRelativeInfo(root, file.relative)
	if err != nil {
		return err
	}
	if !held.Mode().IsRegular() || !current.Mode().IsRegular() || publisherSourceReparse(current) || !os.SameFile(file.info, held) || !os.SameFile(held, current) || held.Size() != file.info.Size() || current.Size() != file.info.Size() || !held.ModTime().Equal(file.info.ModTime()) || !current.ModTime().Equal(file.info.ModTime()) || native != file.native {
		return fmt.Errorf("build artifact original identity/extent/currentness changed: %s", file.relative)
	}
	return nil
}

// Check reads the complete settled build output through retained ordinary files
// and the same strict PE/VERSIONINFO/Comments/identity/registry codecs used by
// native admission. It never substitutes for a native Windows owner or silently
// falls back from an unsupported native acquisition.
func CheckWindowsCanonicalEntryBuildV1(ctx context.Context, entryPath string, binding WindowsCanonicalEntryBuildBindingV1) (observation WindowsCanonicalEntryBuildObservationV1, retErr error) {
	if ctx == nil {
		return observation, errors.New("nil canonical entry build observation context")
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return observation, err
	}
	targets, err := CanonicalEntryTargetsV1(entryPath)
	if err != nil {
		return observation, err
	}
	rootPath := filepath.Dir(entryPath)
	root, rootInfo, err := openPublisherSourceRoot(rootPath)
	if err != nil {
		return observation, err
	}
	var files []canonicalEntryBuildFileV1
	defer func() {
		for i := len(files) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, files[i].file.Close())
		}
		retErr = errors.Join(retErr, root.Close())
	}()
	for _, relative := range []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1, WindowsCanonicalIdentityRelativePathV1} {
		if err = ctx.Err(); err != nil {
			return observation, err
		}
		before, infoErr := publisherSourceRelativeInfo(root, relative)
		if infoErr != nil {
			return observation, infoErr
		}
		if !before.Mode().IsRegular() || publisherSourceReparse(before) {
			return observation, errors.New("build artifact is not an ordinary regular file")
		}
		f, openErr := root.Open(filepath.FromSlash(relative))
		if openErr != nil {
			return observation, openErr
		}
		files = append(files, canonicalEntryBuildFileV1{file: f, relative: relative})
		held, statErr := f.Stat()
		if statErr != nil {
			return observation, statErr
		}
		if !held.Mode().IsRegular() || !os.SameFile(before, held) || held.Size() != before.Size() {
			return observation, errors.New("build artifact changed during original open")
		}
		native, nativeErr := observeCanonicalEntryBuildFileV1(f)
		if nativeErr != nil {
			return observation, nativeErr
		}
		for _, prior := range files[:len(files)-1] {
			if os.SameFile(prior.info, held) || prior.native.Device == native.Device && prior.native.Index == native.Index {
				return observation, errors.New("build artifact roles alias the same original object")
			}
		}
		files[len(files)-1].info = held
		files[len(files)-1].native = native
		if err = currentCanonicalEntryBuildFileV1(root, files[len(files)-1]); err != nil {
			return observation, err
		}
	}
	if err = publisherSourceRootUnchanged(root, rootPath, rootInfo); err != nil {
		return observation, err
	}
	entry, product, identityFile := files[0], files[1], files[2]
	entryHeader, err := readWindowsPEHeader(entry.file, entry.info.Size())
	if err != nil {
		return observation, err
	}
	productHeader, err := readWindowsPEHeader(product.file, product.info.Size())
	if err != nil {
		return observation, err
	}
	metadata, err := readWindowsBuildIdentityAt(product.file, product.info.Size())
	if err != nil {
		return observation, err
	}
	existingComments, actualBinding, err := ParseWindowsCanonicalEntryCommentsV1(metadata.Comments)
	if err != nil {
		return observation, err
	}
	if actualBinding != binding {
		return observation, errors.New("build Product differs from original issued entry binding")
	}
	// Source epoch is optional build DATA. Namespace presence, rather than an
	// error-message match, distinguishes absence from malformed metadata. The
	// canonical suffix was already strictly decoded once above; present source
	// metadata must pass its existing strict owner without text normalization.
	var epoch string
	if strings.Contains(strings.ToLower(existingComments), "mcphub-source-") {
		epoch, err = parsePublisherSourceComments(existingComments)
		if err != nil {
			return observation, err
		}
	}
	entryHash, err := hashCanonicalEntryBuildFileV1(ctx, entry)
	if err != nil {
		return observation, err
	}
	if entryHash != binding.ExpectedCanonicalEntrySHA256() {
		return observation, errors.New("build canonical entry differs from original issued digest")
	}
	productHash, err := hashCanonicalEntryBuildFileV1(ctx, product)
	if err != nil {
		return observation, err
	}
	raw, err := io.ReadAll(&canonicalEntryBuildContextReader{ctx, io.NewSectionReader(identityFile.file, 0, identityFile.info.Size())})
	if err != nil {
		return observation, err
	}
	identity, err := ParseWindowsProductIdentity(raw)
	if err != nil {
		return observation, err
	}
	if entryHeader.subsystem != WindowsCUISubsystem || productHeader.subsystem != WindowsGUISubsystem || (entryHeader.characteristics|productHeader.characteristics)&windowsPEDLLCharacteristic != 0 || entryHeader.machine != identity.Machine || productHeader.machine != identity.Machine || metadata.Role != WindowsArtifactRoleProduct {
		return observation, errors.New("build canonical entry PE role/machine disagreement")
	}
	artifact := WindowsArtifact{Path: targets.ProductPath, Role: metadata.Role, Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: productHash, Machine: productHeader.machine, Subsystem: productHeader.subsystem}
	snapshot := WindowsCanonicalEntrySetSnapshotV1{
		Schema: WindowsCanonicalEntrySetSchemaV1, BuildID: binding.BuildID(), RegistryProjectionSHA256: WindowsCanonicalEntryRegistryProjectionSHA256V1(),
		CanonicalEntryRelativePath: WindowsCanonicalEntryRelativePathV1, ProductRelativePath: WindowsCanonicalProductRelativePathV1, IdentityRelativePath: WindowsCanonicalIdentityRelativePathV1, ReverseProjection: binding.ReverseProjection(),
		CanonicalEntry: WindowsCanonicalEntryFileV1{targets.CanonicalEntryPath, entryHash}, Product: WindowsCanonicalEntryFileV1{targets.ProductPath, productHash}, Identity: WindowsCanonicalEntryFileV1{targets.IdentityPath, canonicalEntrySHA256(raw)}, Machine: entryHeader.machine, ProductArtifact: artifact, IdentityBytes: raw,
	}
	if err = validateCanonicalEntrySnapshotV1(snapshot); err != nil {
		return observation, err
	}
	for _, file := range files {
		if err = currentCanonicalEntryBuildFileV1(root, file); err != nil {
			return observation, err
		}
	}
	if err = publisherSourceRootUnchanged(root, rootPath, rootInfo); err != nil {
		return observation, err
	}
	if err = ctx.Err(); err != nil {
		return observation, err
	}
	return WindowsCanonicalEntryBuildObservationV1{snapshot, epoch}, nil
}
