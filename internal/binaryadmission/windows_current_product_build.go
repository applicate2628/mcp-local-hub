package binaryadmission

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
)

// WindowsCurrentBundleBuildData is a settled build observation, not installed
// admission or an original native transaction owner.
type WindowsCurrentBundleBuildData struct {
	Complete                 bool
	BuildID                  string
	RegistryProjectionSHA256 string
	CanonicalEntrySHA256     string
	ProductSHA256            string
	IdentitySHA256           string
	SourceEpoch              string
	IdentityRelativePath     string
	ProductArtifact          WindowsArtifact
	Identity                 WindowsProductIdentityV2
	Runtime                  RuntimePayloadInspection
}

// BuildWindowsCurrentProductIdentity receives the settled original launcher,
// Product and staged runtime bytes before the caller serializes its identity.
func BuildWindowsCurrentProductIdentity(ctx context.Context, productPath, entryPath, indexPath, sourceEpoch string) (WindowsProductIdentityV2, RuntimePayloadInspection, error) {
	root, err := windowsCurrentBuildRoot(entryPath)
	if err != nil {
		return WindowsProductIdentityV2{}, RuntimePayloadInspection{}, err
	}
	if productPath != filepath.Join(root, filepath.FromSlash(WindowsCanonicalProductRelativePathV1)) || indexPath != runtimePayloadIndexRelativeV4 {
		return WindowsProductIdentityV2{}, RuntimePayloadInspection{}, errors.New("RUNTIME_BUILD_LOCATOR_MISMATCH")
	}
	data, err := readWindowsCurrentBuild(ctx, root, indexPath, sourceEpoch, "", nil)
	return data.Identity, data.Runtime, err
}

// CheckWindowsCurrentBundleBuild accepts the exact source-bound registry4
// location before opening any output. There is no historical sidecar fallback.
func CheckWindowsCurrentBundleBuild(ctx context.Context, entryPath string, binding WindowsCanonicalEntryBuildBindingV1, source PublisherSourceInput, registry WindowsProductRegistryV4) (WindowsCurrentBundleBuildData, error) {
	if !validLowerHex(source.SourceSnapshotSHA256, 64, 64) || !validLowerHex(source.SourceEpoch, 64, 64) || source.FileCount <= 0 {
		return WindowsCurrentBundleBuildData{}, errors.New("PUBLISHER_SOURCE_CURRENT_BUILD_INPUT_INVALID")
	}
	if _, err := NewWindowsCanonicalEntryBuildBindingV1(binding.BuildID(), binding.ExpectedCanonicalEntrySHA256(), binding.ReverseProjection()); err != nil {
		return WindowsCurrentBundleBuildData{}, err
	}
	identityRelative, err := registry.IdentityRelativePath()
	if err != nil {
		return WindowsCurrentBundleBuildData{}, err
	}
	registrySHA, err := registry.ProjectionSHA256()
	if err != nil {
		return WindowsCurrentBundleBuildData{}, err
	}
	root, err := windowsCurrentBuildRoot(entryPath)
	if err != nil {
		return WindowsCurrentBundleBuildData{}, err
	}
	result, err := readWindowsCurrentBuild(ctx, root, path.Base(registry.RuntimePayload.Filename), source.SourceEpoch, identityRelative, &binding)
	result.RegistryProjectionSHA256 = registrySHA
	return result, err
}

func windowsCurrentBuildRoot(entryPath string) (string, error) {
	if !filepath.IsAbs(entryPath) || filepath.Clean(entryPath) != entryPath || filepath.Base(entryPath) != WindowsCanonicalEntryRelativePathV1 {
		return "", errors.New("current build requires the exact absolute canonical launcher path")
	}
	return filepath.Dir(entryPath), nil
}

func readWindowsCurrentBuild(ctx context.Context, rootPath, indexPath, sourceEpoch, identityRelative string, expectedBinding *WindowsCanonicalEntryBuildBindingV1) (data WindowsCurrentBundleBuildData, retErr error) {
	if ctx == nil || !validLowerHex(sourceEpoch, 64, 64) || indexPath != runtimePayloadIndexRelativeV4 {
		return data, errors.New("RUNTIME_BUILD_INPUT_INVALID")
	}
	if err := ctx.Err(); err != nil {
		return data, err
	}
	root, rootInfo, err := openPublisherSourceRoot(rootPath)
	if err != nil {
		return data, err
	}
	paths := []string{WindowsCanonicalEntryRelativePathV1, WindowsCanonicalProductRelativePathV1}
	if identityRelative != "" {
		paths = append(paths, identityRelative)
	}
	files, err := openCanonicalEntryBuildFiles(ctx, root, paths...)
	defer func() {
		for i := len(files) - 1; i >= 0; i-- {
			retErr = errors.Join(retErr, files[i].file.Close())
		}
		retErr = errors.Join(retErr, root.Close())
	}()
	if err != nil {
		return data, err
	}
	entry, product := files[0], files[1]
	entryHeader, err := readWindowsPEHeader(entry.file, entry.info.Size())
	if err != nil {
		return data, err
	}
	productHeader, err := readWindowsPEHeader(product.file, product.info.Size())
	if err != nil {
		return data, err
	}
	metadata, err := readWindowsBuildIdentityAt(product.file, product.info.Size())
	if err != nil {
		return data, err
	}
	if entryHeader.subsystem != WindowsCUISubsystem || productHeader.subsystem != WindowsGUISubsystem ||
		(entryHeader.characteristics|productHeader.characteristics)&windowsPEDLLCharacteristic != 0 ||
		entryHeader.machine != 0x8664 || productHeader.machine != entryHeader.machine || metadata.Role != WindowsArtifactRoleProduct {
		return data, errors.New("current build PE role/machine disagreement")
	}
	comments, err := parseWindowsBuildCommentsComposition(metadata.Comments)
	if err != nil {
		return data, err
	}
	if comments.backendRunHoldV1 {
		return data, ErrWindowsBackendRunHoldUnverified
	}
	if !comments.hasEntry || (expectedBinding != nil && comments.entry != *expectedBinding) {
		return data, errors.New("current build Product differs from original issued entry binding")
	}
	if comments.sourceEpoch != sourceEpoch {
		return data, errors.New("PUBLISHER_SOURCE_EPOCH_MISMATCH")
	}
	if comments.runtimeSHA256 == "" {
		return data, errors.New("RUNTIME_BUILD_BINDING_REQUIRED")
	}
	entrySHA, err := hashCanonicalEntryBuildFileV1(ctx, entry)
	if err != nil {
		return data, err
	}
	if entrySHA != comments.entry.ExpectedCanonicalEntrySHA256() {
		return data, errors.New("current canonical entry differs from original build digest")
	}
	productSHA, err := hashCanonicalEntryBuildFileV1(ctx, product)
	if err != nil {
		return data, err
	}
	identity := WindowsProductIdentityV2{
		Schema: "windows-product-identity-v2", Role: metadata.Role,
		Filename: WindowsCanonicalEntryRelativePathV1, Subsystem: productHeader.subsystem, Machine: productHeader.machine,
		Version: metadata.Version, Commit: metadata.Commit, BuildDate: metadata.BuildDate, SHA256: productSHA,
		SourceEpoch: sourceEpoch, LauncherSHA256: entrySHA, RuntimePayloadPath: indexPath, RuntimePayloadSHA256: comments.runtimeSHA256,
	}
	var identitySHA string
	if identityRelative != "" {
		sidecar := files[2]
		if err := validateWindowsProductIdentitySize(sidecar.info.Size()); err != nil {
			return data, err
		}
		raw, err := io.ReadAll(&canonicalEntryBuildContextReader{ctx, io.NewSectionReader(sidecar.file, 0, sidecar.info.Size())})
		if err != nil {
			return data, err
		}
		received, err := ParseWindowsProductIdentityV2(raw)
		if err != nil {
			return data, err
		}
		if received != identity {
			return data, errors.New("current build Product/identity/source/runtime tuple disagreement")
		}
		identitySHA = RuntimeDigest(raw)
	}

	productDir := path.Dir(WindowsCanonicalProductRelativePathV1)
	info, err := publisherSourceRelativeInfo(root, productDir)
	if err != nil || !info.IsDir() || publisherSourceReparse(info) {
		return data, errors.Join(err, errors.New("RUNTIME_ROOT_NOT_DIRECTORY"))
	}
	productRoot, err := root.OpenRoot(filepath.FromSlash(productDir))
	if err != nil {
		return data, err
	}
	defer func() { retErr = errors.Join(retErr, productRoot.Close()) }()
	held, err := productRoot.Stat(".")
	if err != nil || !os.SameFile(info, held) {
		return data, errors.Join(err, errors.New("RUNTIME_ROOT_CHANGED"))
	}
	data.Runtime, err = InspectRuntimePayload(ctx, productRoot, indexPath, identity.RuntimePayloadSHA256)
	if err != nil {
		return data, err
	}
	current, err := publisherSourceRelativeInfo(root, productDir)
	if err != nil || !current.IsDir() || publisherSourceReparse(current) || !os.SameFile(held, current) || !held.ModTime().Equal(current.ModTime()) {
		return data, errors.Join(err, errors.New("RUNTIME_ROOT_CHANGED"))
	}
	for _, file := range files {
		if err := currentCanonicalEntryBuildFileV1(root, file); err != nil {
			return data, err
		}
	}
	if err := publisherSourceRootUnchanged(root, rootPath, rootInfo); err != nil {
		return data, err
	}
	if err := ctx.Err(); err != nil {
		return data, err
	}
	data.Complete = true
	data.BuildID, data.SourceEpoch = comments.entry.BuildID(), sourceEpoch
	data.CanonicalEntrySHA256, data.ProductSHA256, data.IdentitySHA256 = entrySHA, productSHA, identitySHA
	data.IdentityRelativePath, data.Identity = identityRelative, identity
	data.ProductArtifact = WindowsArtifact{
		Path: filepath.Join(rootPath, filepath.FromSlash(WindowsCanonicalProductRelativePathV1)), Role: identity.Role,
		Version: identity.Version, Commit: identity.Commit, BuildDate: identity.BuildDate, SHA256: productSHA,
		Machine: identity.Machine, Subsystem: identity.Subsystem,
	}
	return data, nil
}

// CheckWindowsLegacyProductBuildDomain keeps a recognized current Product out
// of the historical identity producer. The original legacy identity receiver
// owns the unchanged version/commit/time and optional source epoch checks.
func CheckWindowsLegacyProductBuildDomain(productPath string) error {
	metadata, err := readWindowsBuildIdentity(productPath)
	if err != nil {
		return err
	}
	_, err = checkWindowsLegacyProductBuildComments(metadata.Comments)
	return err
}

func checkWindowsLegacyProductBuildComments(rawComments string) (windowsBuildCommentsComposition, error) {
	comments, err := parseWindowsBuildCommentsComposition(rawComments)
	if err != nil {
		return windowsBuildCommentsComposition{}, err
	}
	if comments.backendRunHoldV1 {
		return windowsBuildCommentsComposition{}, ErrWindowsBackendRunHoldUnverified
	}
	if comments.runtimeSHA256 != "" {
		return windowsBuildCommentsComposition{}, errors.New("RUNTIME_BUILD_PAYLOAD_INPUT_REQUIRED")
	}
	return comments, nil
}
