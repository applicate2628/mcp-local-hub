package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/binaryadmission"
)

type runtimePackVerification struct {
	Schema string `json:"schema"`
	npmPackArchive
	Identity binaryadmission.WindowsProductIdentityV2 `json:"identity"`
}

type npmPackArchive struct {
	ArchiveFilename string `json:"archive_filename"`
	ArchiveSize     int64  `json:"archive_size"`
	ArchiveSHA256   string `json:"archive_sha256"`
}

// Expected metadata comes from the same verified source snapshot whose epoch
// is compiled into Product. Mutable sibling files are not metadata authority.
func verifyRuntimePack(archive, candidate, launcher string, identity binaryadmission.WindowsProductIdentityV2, inspection binaryadmission.RuntimePayloadInspection, registry binaryadmission.WindowsProductRegistryV4, metadata map[string][]byte) (result runtimePackVerification, err error) {
	identityRelative, err := registry.IdentityRelativePath()
	if err != nil {
		return result, err
	}
	if registry.SchemaVersion != 4 || registry.RuntimePayload == nil {
		return result, errors.New("PACK_RUNTIME_REGISTRY_REQUIRED")
	}
	var productPath, launcherPath string
	for _, artifact := range registry.Artifacts {
		if artifact.Role == "product" {
			productPath = artifact.Filename
		} else if artifact.Role == "launcher" {
			launcherPath = artifact.Filename
		}
	}
	productRoot := path.Join("bin", path.Dir(productPath))
	if path.Join(path.Dir(productPath), identity.RuntimePayloadPath) != registry.RuntimePayload.Filename ||
		inspection.PayloadSHA256 != identity.RuntimePayloadSHA256 {
		return result, errors.New("PACK_RUNTIME_BINDING_MISMATCH")
	}
	expected := map[string]binaryadmission.RuntimePayloadFile{}
	for _, f := range inspection.Files {
		f.Path = path.Join(productRoot, f.Path)
		expected[f.Path] = f
	}
	for name, local := range map[string]string{path.Join("bin", productPath): candidate, path.Join("bin", launcherPath): launcher} {
		size, hash, err := hashPackFile(local)
		if err != nil {
			return result, err
		}
		wanted := identity.SHA256
		if name == path.Join("bin", launcherPath) {
			wanted = identity.LauncherSHA256
		}
		if hash != wanted {
			return result, errors.New("PACK_EXECUTABLE_READBACK_DRIFT")
		}
		expected[name] = binaryadmission.RuntimePayloadFile{Path: name, Size: size, SHA256: hash}
	}
	identityRaw, err := binaryadmission.CanonicalRuntimeJSON(identity)
	if err != nil {
		return result, err
	}
	identityMember := path.Join("bin", identityRelative)
	expected[identityMember] = binaryadmission.RuntimePayloadFile{Path: identityMember, Size: int64(len(identityRaw)), SHA256: binaryadmission.RuntimeDigest(identityRaw)}
	for _, name := range []string{"package.json", "README.md"} {
		raw, ok := metadata[name]
		if !ok || len(raw) == 0 {
			return result, errors.New("PACK_QUALIFIED_METADATA_MISSING")
		}
		expected[name] = binaryadmission.RuntimePayloadFile{Path: name, Size: int64(len(raw)), SHA256: binaryadmission.RuntimeDigest(raw)}
	}
	var manifest struct {
		Version string `json:"version"`
		Product string `json:"mcphubProduct"`
	}
	if json.Unmarshal(metadata["package.json"], &manifest) != nil || manifest.Version != identity.Version || manifest.Product != path.Join("bin", productPath) {
		return result, errors.New("PACK_SOURCE_METADATA_CONTRACT_MISMATCH")
	}
	verified, err := verifyNpmPack(archive, expected)
	if err != nil {
		return result, err
	}
	return runtimePackVerification{"windows-runtime-pack-verification-v1", verified, identity}, nil
}

// Verify the members and return the digest from the same compressed stream.
func verifyNpmPack(archive string, expected map[string]binaryadmission.RuntimePayloadFile) (result npmPackArchive, returned error) {
	before, err := os.Lstat(archive)
	if err != nil {
		return result, err
	}
	if !before.Mode().IsRegular() {
		return result, errors.New("PACK_ARCHIVE_NOT_REGULAR")
	}
	f, err := os.Open(archive)
	if err != nil {
		return result, err
	}
	defer func() { returned = errors.Join(returned, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return result, err
	}
	if !os.SameFile(before, opened) {
		return result, errors.New("PACK_ARCHIVE_IDENTITY_DRIFT")
	}
	archiveHash := sha256.New()
	section := io.NewSectionReader(f, 0, before.Size())
	compressed := bufio.NewReader(io.TeeReader(section, archiveHash))
	g, err := gzip.NewReader(compressed)
	if err != nil {
		return result, err
	}
	defer func() { returned = errors.Join(returned, g.Close()) }()
	// A package is one gzip stream. ByteReader positioning lets the owner
	// distinguish legal tar padding inside that stream from another gzip stream.
	g.Multistream(false)
	if err := verifyPackMembers(tar.NewReader(g), expected); err != nil {
		return result, err
	}
	// tar.Next stops at the archive end marker, before any remaining decoded
	// padding. Only zero padding is admissible; read to gzip EOF even after a
	// nonzero byte so its checksum/trailer error is also retained.
	var padding [512]byte // tar's format block size, not an archive-size cap
	var closureErr error
	for {
		n, readErr := g.Read(padding[:])
		if closureErr == nil {
			for _, value := range padding[:n] {
				if value != 0 {
					closureErr = errors.New("PACK_TAR_TRAILING_DATA")
					break
				}
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				closureErr = errors.Join(closureErr, readErr)
			}
			break
		}
	}
	if _, tailErr := compressed.Peek(1); tailErr == nil {
		closureErr = errors.Join(closureErr, errors.New("PACK_GZIP_TRAILING_DATA"))
	} else if !errors.Is(tailErr, io.EOF) {
		closureErr = errors.Join(closureErr, tailErr)
	}
	after, err := os.Lstat(archive)
	if err != nil {
		return result, errors.Join(closureErr, err)
	}
	count, err := section.Seek(0, io.SeekCurrent)
	if err != nil || count != before.Size() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return result, errors.Join(closureErr, err, errors.New("PACK_ARCHIVE_IDENTITY_DRIFT"))
	}
	if closureErr != nil {
		return result, closureErr
	}
	return npmPackArchive{filepath.Base(archive), count, hex.EncodeToString(archiveHash.Sum(nil))}, nil
}

func verifyPackMembers(t *tar.Reader, expected map[string]binaryadmission.RuntimePayloadFile) error {
	seen := map[string]bool{}
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if !strings.HasPrefix(h.Name, "package/") {
			return errors.New("PACK_ROOT_INVALID")
		}
		name := strings.TrimPrefix(h.Name, "package/")
		if !binaryadmission.RuntimeRelativePath(name) || seen[strings.ToLower(name)] {
			return errors.New("PACK_PATH_ALIAS")
		}
		seen[strings.ToLower(name)] = true
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return errors.New("PACK_OBJECT_UNSUPPORTED")
		}
		row, ok := expected[name]
		if !ok {
			return fmt.Errorf("PACK_UNACCOUNTED_FILE: %s", name)
		}
		// Header rejection precedes every body read, including unknown/oversized
		// metadata. The bound is this qualified file's actual size, not a new cap.
		if h.Size != row.Size {
			return fmt.Errorf("PACK_MEMBER_SIZE_MISMATCH: %s", name)
		}
		hash := sha256.New()
		count, err := io.Copy(hash, io.LimitReader(t, row.Size))
		if err != nil {
			return err
		}
		if count != row.Size || hex.EncodeToString(hash.Sum(nil)) != row.SHA256 {
			return fmt.Errorf("PACK_FILE_DIGEST_MISMATCH: %s", name)
		}
		delete(expected, name)
	}
	if len(expected) != 0 {
		return errors.New("PACK_PAYLOAD_INCOMPLETE")
	}
	return nil
}

func hashPackFile(name string) (size int64, digest string, returned error) {
	before, err := os.Lstat(name)
	if err != nil {
		return 0, "", err
	}
	if !before.Mode().IsRegular() {
		return 0, "", errors.New("PACK_SOURCE_NOT_REGULAR")
	}
	f, err := os.Open(name)
	if err != nil {
		return 0, "", err
	}
	defer func() { returned = errors.Join(returned, f.Close()) }()
	opened, err := f.Stat()
	if err != nil {
		return 0, "", err
	}
	if !os.SameFile(before, opened) {
		return 0, "", errors.New("PACK_SOURCE_IDENTITY_DRIFT")
	}
	hash := sha256.New()
	count, err := io.Copy(hash, io.NewSectionReader(f, 0, before.Size()))
	if err != nil {
		return 0, "", err
	}
	after, err := os.Lstat(name)
	if err != nil {
		return 0, "", err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || count != before.Size() || !before.ModTime().Equal(after.ModTime()) {
		return 0, "", errors.New("PACK_SOURCE_IDENTITY_DRIFT")
	}
	return count, hex.EncodeToString(hash.Sum(nil)), nil
}

func runRuntimeFetchPlan(options validatorOptions, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	archives, err := binaryadmission.ReadRuntimeFetchPlan(ctx, options.sourceRoot, options.sourceSnapshot, options.expectedSourceEpoch)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	plan := struct {
		Schema      string                                `json:"schema"`
		SourceEpoch string                                `json:"source_epoch"`
		Archives    []binaryadmission.RuntimeFetchArchive `json:"archives"`
	}{"windows-runtime-fetch-plan-v1", options.expectedSourceEpoch, archives}
	if err := json.NewEncoder(stdout).Encode(plan); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runMetaPackVerification(options validatorOptions, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	archive, err := verifyMetaPack(ctx, options)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	receipt := struct {
		Schema string `json:"schema"`
		npmPackArchive
	}{"npm-meta-pack-verification-v1", archive}
	if err := json.NewEncoder(stdout).Encode(receipt); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func verifyMetaPack(ctx context.Context, options validatorOptions) (npmPackArchive, error) {
	names := []string{"npm/README.md", "npm/bin/cli.js", "npm/lib/platform-binary.js", "npm/package.json", "npm/scripts/postinstall.js"}
	source, files, err := binaryadmission.ReadPublisherSourceSnapshotFiles(ctx, options.packSourceRoot, options.packSourceSnapshot, names)
	if err != nil {
		return npmPackArchive{}, err
	}
	if source.SourceEpoch != options.expectedSourceEpoch {
		return npmPackArchive{}, errors.New("PUBLISHER_SOURCE_EPOCH_MISMATCH")
	}
	if !validMetaPackManifest(files["npm/package.json"], options.expectedVersion) {
		return npmPackArchive{}, errors.New("PACK_META_SOURCE_MANIFEST_MISMATCH")
	}
	expected := make(map[string]binaryadmission.RuntimePayloadFile, len(names))
	for _, sourceName := range names {
		name := strings.TrimPrefix(sourceName, "npm/")
		raw := files[sourceName]
		expected[name] = binaryadmission.RuntimePayloadFile{Path: name, Size: int64(len(raw)), SHA256: binaryadmission.RuntimeDigest(raw)}
	}
	return verifyNpmPack(options.metaPack, expected)
}

func validMetaPackManifest(raw []byte, expectedVersion string) bool {
	input := json.NewDecoder(bytes.NewReader(raw))
	if token, err := input.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	fields := map[string]json.RawMessage{"name": nil, "version": nil, "optionalDependencies": nil}
	for input.More() {
		token, err := input.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return false
		}
		prior, guarded := fields[key]
		if guarded && prior != nil {
			return false
		}
		var value json.RawMessage
		if err := input.Decode(&value); err != nil {
			return false
		}
		if guarded {
			fields[key] = value
		}
	}
	if token, err := input.Token(); err != nil || token != json.Delim('}') {
		return false
	}
	if _, err := input.Token(); err != io.EOF {
		return false
	}
	var name, version string
	if json.Unmarshal(fields["name"], &name) != nil || name != "mcp-local-hub" ||
		json.Unmarshal(fields["version"], &version) != nil || version == "" || version != expectedVersion {
		return false
	}
	deps := json.NewDecoder(bytes.NewReader(fields["optionalDependencies"]))
	if token, err := deps.Token(); err != nil || token != json.Delim('{') {
		return false
	}
	if key, err := deps.Token(); err != nil || key != "@applicate2628/mcp-local-hub-win32-x64" {
		return false
	}
	var dependencyVersion string
	if err := deps.Decode(&dependencyVersion); err != nil || dependencyVersion != version {
		return false
	}
	token, err := deps.Token()
	return err == nil && token == json.Delim('}')
}

func runRuntimeStaging(options validatorOptions, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	inspection, err := binaryadmission.StageRuntimePayload(ctx, binaryadmission.RuntimeStageRequest{
		SourceRoot: options.sourceRoot, SourceSnapshot: options.sourceSnapshot,
		OutputRoot: options.runtimeOutput, ArchiveDirectory: options.runtimeArchives,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	out := struct {
		Schema               string `json:"schema"`
		RuntimePayloadSHA256 string `json:"runtime_payload_sha256"`
		FileCount            int    `json:"file_count"`
	}{"hub-runtime-staging-v1", inspection.PayloadSHA256, len(inspection.Files)}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

func runCurrentProductValidation(options validatorOptions, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	identity, inspection, err := binaryadmission.BuildWindowsCurrentProductIdentity(ctx, options.candidate, options.helper, options.runtimePayload, options.expectedSourceEpoch)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if identity.Version != options.expectedVersion || identity.Commit != options.expectedCommit || identity.BuildDate != options.expectedBuildDate {
		fmt.Fprintln(stderr, "windows product validation failed: current build identity mismatch")
		return 1
	}
	sourceRoot, snapshotPath := options.sourceRoot, options.sourceSnapshot
	var packNames []string
	if options.packArchive != "" {
		sourceRoot, snapshotPath = options.packSourceRoot, options.packSourceSnapshot
		prefix := "npm/packages/" + inspection.Payload.Target + "/"
		packNames = []string{prefix + "package.json", prefix + "README.md"}
	}
	source, registry, files, err := binaryadmission.ReadWindowsProductSourceV4(ctx, sourceRoot, snapshotPath, packNames...)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if source.SourceEpoch != identity.SourceEpoch {
		fmt.Fprintln(stderr, "PUBLISHER_SOURCE_EPOCH_MISMATCH")
		return 1
	}
	if err := binaryadmission.ValidateWindowsProductRuntimeSourceV4(inspection.Payload, registry, files); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var received any = identity
	if options.packArchive != "" {
		metadata := map[string][]byte{"package.json": files[packNames[0]], "README.md": files[packNames[1]]}
		verification, err := verifyRuntimePack(options.packArchive, options.candidate, options.helper, identity, inspection, registry, metadata)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		received = verification
	}
	if err := json.NewEncoder(stdout).Encode(received); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
