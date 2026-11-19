//go:build windows

package binaryadmission

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
)

// Receive only the explicit V4 layout over already-held original files.
// V1 continues through its existing native receiver and exact codecs.
func receiveCanonicalEntryFilesWithRegistry(ctx context.Context, files [3]*os.File, targets WindowsCanonicalEntrySetTargetsV1, binding *WindowsCanonicalEntryBuildBindingV1, dispatchBuildID string, r WindowsProductRegistryV4) (base canonicalEntryFilesObservation, identity WindowsProductIdentityV2, comments windowsBuildCommentsComposition, err error) {
	if ctx == nil {
		return base, identity, comments, errors.New("nil canonical entry file context")
	}
	if err = ctx.Err(); err != nil {
		return
	}
	for _, f := range files {
		if f == nil {
			return base, identity, comments, errors.New("complete held canonical entry files required")
		}
	}
	productPath, identityPath, err := currentCanonicalEntryPaths(targets.CanonicalEntryPath, r)
	if err != nil {
		return base, identity, comments, err
	}
	current := WindowsCanonicalEntrySetTargetsV1{targets.CanonicalEntryPath, productPath, identityPath}
	if targets != current {
		return base, identity, comments, errors.New("canonical entry file targets disagree with the current registry layout")
	}
	pi, err := files[1].Stat()
	if err != nil {
		return base, identity, comments, err
	}
	metadata, err := readWindowsBuildIdentityAt(files[1], pi.Size())
	if err != nil {
		return base, identity, comments, err
	}
	comments, err = parseWindowsBuildCommentsComposition(metadata.Comments)
	if err != nil {
		return base, identity, comments, err
	}
	if !comments.hasEntry {
		return base, identity, comments, ErrWindowsCanonicalEntryBindingMissing
	}
	if binding != nil && comments.entry != *binding {
		return base, identity, comments, errors.New("executing Product build binding disagrees with original Product")
	}
	if dispatchBuildID != "" && comments.entry.BuildID() != dispatchBuildID {
		return base, identity, comments, errors.New("executing current generation disagrees with original Product binding")
	}
	ei, err := files[0].Stat()
	if err != nil {
		return base, identity, comments, err
	}
	eh, err := readWindowsPEHeader(files[0], ei.Size())
	if err != nil {
		return base, identity, comments, err
	}
	ph, err := readWindowsPEHeader(files[1], pi.Size())
	if err != nil {
		return base, identity, comments, err
	}
	if eh.subsystem != WindowsCUISubsystem || ph.subsystem != WindowsGUISubsystem || eh.machine != ph.machine || (eh.machine != 0x8664 && eh.machine != 0xaa64) || (eh.characteristics|ph.characteristics)&windowsPEDLLCharacteristic != 0 || metadata.Role != WindowsArtifactRoleProduct {
		return base, identity, comments, errors.New("canonical entry PE role/machine disagreement")
	}
	entryHash, err := canonicalEntryHashFile(ctx, files[0])
	if err != nil {
		return base, identity, comments, err
	}
	if entryHash != comments.entry.ExpectedCanonicalEntrySHA256() {
		return base, identity, comments, errors.New("original canonical entry differs from build-issued digest")
	}
	productHash, err := canonicalEntryHashFile(ctx, files[1])
	if err != nil {
		return base, identity, comments, err
	}
	ii, err := files[2].Stat()
	if err != nil {
		return base, identity, comments, err
	}
	if err = validateWindowsProductIdentitySize(ii.Size()); err != nil {
		return base, identity, comments, err
	}
	raw, err := canonicalEntryHeldBytes(ctx, files[2])
	if err != nil {
		return base, identity, comments, err
	}
	identity, err = ParseWindowsProductIdentityV2(raw)
	if err != nil {
		return base, identity, comments, err
	}
	if comments.sourceEpoch == "" || comments.runtimeSHA256 == "" || identity.LauncherSHA256 != entryHash ||
		identity.SourceEpoch != comments.sourceEpoch || identity.RuntimePayloadSHA256 != comments.runtimeSHA256 || eh.machine != 0x8664 {
		return base, identity, comments, errors.New("original current source/runtime/launcher tuple disagreement")
	}
	if path.Join(path.Dir(r.Artifacts[0].Filename), identity.RuntimePayloadPath) != r.RuntimePayload.Filename {
		return base, identity, comments, errors.New("current runtime index locator disagrees with registry")
	}
	if identity.SHA256 != productHash || identity.Version != metadata.Version || identity.Commit != metadata.Commit || identity.BuildDate != metadata.BuildDate || identity.Machine != ph.machine {
		return base, identity, comments, errors.New("original Product and identity tuple disagree")
	}
	projection, err := r.ProjectionSHA256()
	if err != nil {
		return base, identity, comments, err
	}
	if filepath.Base(targets.ProductPath) != identity.Filename {
		return base, identity, comments, errors.New("identity filename differs from original Product")
	}
	base = canonicalEntryFilesObservation{
		BuildID: comments.entry.BuildID(), RegistryProjectionSHA256: projection,
		CanonicalEntry: WindowsCanonicalEntryFileV1{targets.CanonicalEntryPath, entryHash},
		Product: WindowsCanonicalEntryFileV1{targets.ProductPath, productHash},
		Identity: WindowsCanonicalEntryFileV1{targets.IdentityPath, canonicalEntrySHA256(raw)},
		ProductArtifact: WindowsArtifact{Path: targets.ProductPath, Role: metadata.Role, Version: metadata.Version,
			Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: productHash, Machine: ph.machine, Subsystem: ph.subsystem},
	}
	return base, identity, comments, ctx.Err()
}

type canonicalEntryFilesObservation struct {
	BuildID, RegistryProjectionSHA256 string
	CanonicalEntry, Product, Identity WindowsCanonicalEntryFileV1
	ProductArtifact                   WindowsArtifact
}

// Read only from the held original, retaining an acquired prefix alongside
// an error if the read or final context check refuses qualification.
func canonicalEntryHeldBytes(ctx context.Context, f *os.File) ([]byte, error) {
	if ctx == nil || f == nil {
		return nil, errors.New("held file and context required")
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(&canonicalEntryBuildContextReader{ctx, io.NewSectionReader(f, 0, info.Size())})
	var extentErr error
	if int64(len(raw)) != info.Size() {
		extentErr = errors.New("held file extent changed")
	}
	// Bytes already acquired remain available to the owning receiver even when
	// the read, extent or final context check refuses qualification.
	return raw, errors.Join(readErr, extentErr, ctx.Err())
}
