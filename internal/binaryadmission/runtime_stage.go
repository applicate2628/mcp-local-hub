package binaryadmission

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type RuntimeStageRequest struct {
	SourceRoot       string
	SourceSnapshot   string
	OutputRoot       string
	ArchiveDirectory string
}

type runtimeStageInput struct {
	descriptor    RuntimeReleaseDescriptorV1
	descriptorRaw []byte
	originals     map[string][]byte
	archive       []byte
}

type runtimeStageSource struct {
	inputs      []runtimeStageInput
	payloadPath string
	epoch       string
}

// Descriptor previews select which notices to capture. Only the matching
// bytes returned by the repository's source snapshot receiver are used.
func readRuntimeStageSource(ctx context.Context, source *os.Root, sourceRoot, snapshotPath string) (runtimeStageSource, error) {
	const registryPath = "windows-product-artifacts.json"
	registryRaw, err := readRuntimeFile(ctx, source, registryPath)
	if err != nil {
		return runtimeStageSource{}, err
	}
	registry, err := ParseWindowsProductRegistryV4(registryRaw)
	if err != nil {
		return runtimeStageSource{}, err
	}
	names := []string{registryPath}
	known := map[string]bool{registryPath: true}
	include := func(name string) {
		if !known[name] {
			names = append(names, name)
			known[name] = true
		}
	}
	ids := runtimePayloadComponentIDsV1()
	inputs := make([]runtimeStageInput, len(ids))
	for i, name := range registry.RuntimePayload.ReleaseDescriptors {
		include(name)
		raw, err := readRuntimeFile(ctx, source, name)
		if err != nil {
			return runtimeStageSource{}, err
		}
		d, err := ParseRuntimeReleaseDescriptor(raw)
		if err != nil {
			return runtimeStageSource{}, err
		}
		if d.Component != ids[i] {
			return runtimeStageSource{}, errors.New("RUNTIME_DESCRIPTOR_ORDER_INVALID")
		}
		inputs[i] = runtimeStageInput{descriptor: d, descriptorRaw: raw, originals: map[string][]byte{}}
		for _, file := range d.OriginalFiles {
			include(file.SourcePath)
		}
	}
	snapshot, sourceFiles, err := ReadPublisherSourceSnapshotFiles(ctx, sourceRoot, snapshotPath, names)
	if err != nil {
		return runtimeStageSource{}, err
	}
	if !bytes.Equal(registryRaw, sourceFiles[registryPath]) {
		return runtimeStageSource{}, errors.New("RUNTIME_SOURCE_REGISTRY_DRIFT")
	}
	for i, name := range registry.RuntimePayload.ReleaseDescriptors {
		input := &inputs[i]
		if !bytes.Equal(input.descriptorRaw, sourceFiles[name]) {
			return runtimeStageSource{}, errors.New("RUNTIME_SOURCE_DESCRIPTOR_DRIFT")
		}
		input.descriptorRaw = sourceFiles[name]
		for _, file := range input.descriptor.OriginalFiles {
			original := sourceFiles[file.SourcePath]
			if RuntimeDigest(original) != file.SHA256 {
				return runtimeStageSource{}, errors.New("RUNTIME_ORIGINAL_INPUT_DIGEST_MISMATCH")
			}
			input.originals[file.Path] = original
		}
	}
	return runtimeStageSource{inputs, path.Base(registry.RuntimePayload.Filename), snapshot.SourceEpoch}, nil
}

// RuntimeFetchArchive is an acquisition row from the qualified staged source.
// Its decimal size preserves the descriptor's int64 range on the wire.
type RuntimeFetchArchive struct {
	Component string `json:"component"`
	Release   string `json:"release"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size,string"`
}

// ReadRuntimeFetchPlan projects the same ordered descriptors and notices used
// for runtime staging. The complete batch is returned after source readback.
func ReadRuntimeFetchPlan(ctx context.Context, sourceRoot, snapshotPath, expectedEpoch string) ([]RuntimeFetchArchive, error) {
	if ctx == nil || !filepath.IsAbs(sourceRoot) || !filepath.IsAbs(snapshotPath) || !validLowerHex(expectedEpoch, 64, 64) {
		return nil, errors.New("RUNTIME_FETCH_PLAN_INPUT_INVALID")
	}
	source, info, err := openPublisherSourceRoot(sourceRoot)
	if err != nil {
		return nil, err
	}
	selected, err := readRuntimeStageSource(ctx, source, sourceRoot, snapshotPath)
	err = errors.Join(err, publisherSourceRootUnchanged(source, sourceRoot, info), source.Close(), ctx.Err())
	if err != nil {
		return nil, err
	}
	if selected.epoch != expectedEpoch {
		return nil, errors.New("PUBLISHER_SOURCE_EPOCH_MISMATCH")
	}
	rows := make([]RuntimeFetchArchive, 0, len(selected.inputs))
	for _, input := range selected.inputs {
		d := input.descriptor
		rows = append(rows, RuntimeFetchArchive{d.Component, d.Release, d.ArchiveURL, d.ArchiveSHA256, d.ArchiveSize})
	}
	return rows, nil
}

// StageRuntimePayload is release staging only. The caller owns the explicit
// archive directory and output root; it never chooses an installed runtime root.
// Content-addressed archives are original acquisition inputs, not an installer
// cache or a substitute for installed native receiving.
func StageRuntimePayload(ctx context.Context, request RuntimeStageRequest) (result RuntimePayloadInspection, returned error) {
	if ctx == nil || !filepath.IsAbs(request.SourceRoot) || !filepath.IsAbs(request.SourceSnapshot) ||
		!filepath.IsAbs(request.OutputRoot) || filepath.Clean(request.OutputRoot) != request.OutputRoot ||
		!filepath.IsAbs(request.ArchiveDirectory) {
		return result, errors.New("RUNTIME_STAGE_INPUT_INVALID")
	}
	source, sourceInfo, err := openPublisherSourceRoot(request.SourceRoot)
	if err != nil {
		return result, err
	}
	defer func() { returned = errors.Join(returned, source.Close()) }()
	selected, err := readRuntimeStageSource(ctx, source, request.SourceRoot, request.SourceSnapshot)
	if err != nil {
		return result, err
	}
	inputs, payloadPath := selected.inputs, selected.payloadPath
	archives, archiveInfo, err := openPublisherSourceRoot(request.ArchiveDirectory)
	if err != nil {
		return result, err
	}
	defer func() { returned = errors.Join(returned, archives.Close()) }()
	for i := range inputs {
		a := inputs[i].descriptor.RuntimeArchiveSource
		raw, err := readRuntimeFile(ctx, archives, a.ArchiveSHA256, RuntimePayloadFile{Path: a.ArchiveSHA256, Size: a.ArchiveSize, SHA256: a.ArchiveSHA256})
		if err != nil {
			return result, err
		}
		inputs[i].archive = raw
	}
	output, err := openRuntimeStageOutput(request, sourceInfo, archiveInfo)
	if err != nil {
		return result, err
	}
	defer func() { returned = errors.Join(returned, output.Close()) }()
	temporary := ".runtime-staging-" + rand.Text()
	if err := output.Mkdir(temporary, 0700); err != nil {
		return result, err
	}
	temp, err := output.OpenRoot(temporary)
	defer func() {
		if temp != nil {
			returned = errors.Join(returned, temp.Close())
		}
		returned = errors.Join(returned, output.RemoveAll(temporary))
	}()
	if err != nil {
		return result, err
	}
	payload := RuntimePayloadV1{Schema: RuntimePayloadSchemaV1, Target: runtimePayloadTargetV1, Components: []RuntimeComponentV1{}}
	for _, input := range inputs {
		d, raw := input.descriptor, input.descriptorRaw
		working := d.Component + "/root"
		if err := runtimeMkdirAll(temp, working); err != nil {
			return result, err
		}
		if err := extractRuntimeArchive(ctx, input.archive, temp, working, d.RuntimeArchiveSource); err != nil {
			return result, err
		}
		for _, f := range d.OriginalFiles {
			if err := writeRuntimeFile(temp, working+"/"+f.Path, input.originals[f.Path], 0644); err != nil {
				return result, err
			}
		}
		inventory, err := inventoryRuntimeTree(ctx, temp, working)
		if err != nil {
			return result, err
		}
		inventoryRaw, err := CanonicalRuntimeJSON(inventory)
		if err != nil {
			return result, err
		}
		if _, err := ParseRuntimeInventory(inventoryRaw); err != nil {
			return result, err
		}
		descriptorSHA, treeSHA := RuntimeDigest(raw), RuntimeDigest(inventoryRaw)
		component, err := runtimePayloadComponentV1(payload.Target, d.Component, descriptorSHA, treeSHA)
		if err != nil {
			return result, err
		}
		if err := copyRuntimeTree(ctx, temp, working, temp, component.Root); err != nil {
			return result, err
		}
		if err := writeRuntimeFile(temp, component.DescriptorPath, raw, 0644); err != nil {
			return result, err
		}
		if err := writeRuntimeFile(temp, component.Inventory, inventoryRaw, 0644); err != nil {
			return result, err
		}
		payload.Components = append(payload.Components, component)
	}
	payloadRaw, err := CanonicalRuntimeJSON(payload)
	if err != nil {
		return result, err
	}
	if err := writeRuntimeFile(temp, payloadPath, payloadRaw, 0644); err != nil {
		return result, err
	}
	// Validate the complete candidate through the same production reader before
	// touching the output. Unknown occupied output is never overwritten.
	candidate, err := InspectRuntimePayload(ctx, temp, payloadPath, RuntimeDigest(payloadRaw))
	if err != nil {
		return result, err
	}
	if prior, err := readRuntimeAuthenticatedFile(ctx, output, payloadPath, RuntimeDigest(payloadRaw)); err == nil {
		if !bytes.Equal(prior, payloadRaw) {
			return result, errors.New("RUNTIME_OUTPUT_OCCUPIED")
		}
		return InspectRuntimePayload(ctx, output, payloadPath, RuntimeDigest(payloadRaw))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	if _, err := output.Lstat("runtimes"); err == nil {
		return result, errors.New("RUNTIME_OUTPUT_OCCUPIED")
	} else if !errors.Is(err, fs.ErrNotExist) {
		return result, err
	}
	// This output is an unpublished build artifact. Partial copy errors remain
	// visible under this exact output; installed transaction rollback is separate.
	if err := output.Mkdir("runtimes", 0755); err != nil {
		return result, fmt.Errorf("RUNTIME_OUTPUT_OCCUPIED: %w", err)
	}
	if err := copyRuntimeTree(ctx, temp, "runtimes", output, "runtimes"); err != nil {
		return result, fmt.Errorf("RUNTIME_STAGE_OUTPUT_PENDING: %w", err)
	}
	if err := writeRuntimeFile(output, payloadPath, payloadRaw, 0644); err != nil {
		return result, fmt.Errorf("RUNTIME_STAGE_OUTPUT_PENDING: %w", err)
	}
	result, err = InspectRuntimePayload(ctx, output, payloadPath, RuntimeDigest(payloadRaw))
	if err != nil {
		return result, err
	}
	if result.PayloadSHA256 != candidate.PayloadSHA256 {
		return result, errors.New("RUNTIME_STAGE_READBACK_DRIFT")
	}
	return result, nil
}

func openRuntimeStageOutput(request RuntimeStageRequest, sourceInfo, archiveInfo os.FileInfo) (*os.Root, error) {
	current := request.OutputRoot
	var missing []string
	for {
		_, err := os.Lstat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(current) == current {
			return nil, err
		}
		missing = append(missing, filepath.Base(current))
		current = filepath.Dir(current)
	}
	parent, parentInfo, err := openPublisherSourceRoot(current)
	if err != nil {
		return nil, err
	}
	for _, info := range []os.FileInfo{sourceInfo, archiveInfo} {
		overlaps, err := publisherSourceHasAncestor(current, info)
		if err != nil || overlaps {
			return nil, errors.Join(err, errors.New("RUNTIME_STAGE_OUTPUT_OVERLAP"), parent.Close())
		}
	}
	if len(missing) == 0 {
		for _, input := range []string{request.SourceRoot, request.ArchiveDirectory, filepath.Dir(request.SourceSnapshot)} {
			overlaps, err := publisherSourceHasAncestor(input, parentInfo)
			if err != nil || overlaps {
				return nil, errors.Join(err, errors.New("RUNTIME_STAGE_OUTPUT_OVERLAP"), parent.Close())
			}
		}
		return parent, nil
	}
	relative := ""
	for i := len(missing) - 1; i >= 0; i-- {
		relative = filepath.Join(relative, missing[i])
		if err := parent.Mkdir(relative, 0755); err != nil {
			return nil, errors.Join(err, parent.Close())
		}
	}
	info, err := publisherSourceRelativeInfo(parent, filepath.ToSlash(relative))
	if err != nil || !info.IsDir() {
		return nil, errors.Join(err, errors.New("RUNTIME_STAGE_OUTPUT_UNQUALIFIED"), parent.Close())
	}
	output, err := parent.OpenRoot(relative)
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	opened, err := output.Stat(".")
	currentErr := publisherSourceRootUnchanged(output, request.OutputRoot, info)
	if err != nil || !os.SameFile(info, opened) || currentErr != nil {
		return nil, errors.Join(err, currentErr, errors.New("RUNTIME_STAGE_OUTPUT_CHANGED"), output.Close(), parent.Close())
	}
	if err := parent.Close(); err != nil {
		return nil, errors.Join(err, output.Close())
	}
	return output, nil
}

func runtimeMkdirAll(root *os.Root, name string) error {
	if !RuntimeRelativePath(name) {
		return errors.New("RUNTIME_DIRECTORY_PATH_INVALID")
	}
	parts := strings.Split(name, "/")
	for n := 1; n <= len(parts); n++ {
		current := strings.Join(parts[:n], "/")
		err := root.Mkdir(current, 0755)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil {
			return err
		}
		if !info.IsDir() || runtimeReparse(info) {
			return errors.New("RUNTIME_DIRECTORY_UNQUALIFIED")
		}
	}
	return nil
}
func writeRuntimeFile(root *os.Root, name string, raw []byte, mode fs.FileMode) (returned error) {
	if !RuntimeRelativePath(name) {
		return errors.New("RUNTIME_FILE_PATH_INVALID")
	}
	if parent := path.Dir(name); parent != "." {
		if err := runtimeMkdirAll(root, parent); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, f.Close()) }()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	return f.Sync()
}
func extractRuntimeArchive(ctx context.Context, archive []byte, root *os.Root, destination string, a RuntimeArchiveSource) error {
	seen := map[string]bool{}
	mapName := func(name string) (string, bool, error) {
		name = strings.TrimSuffix(name, "/")
		if !RuntimeRelativePath(name) {
			return "", false, errors.New("RUNTIME_ARCHIVE_PATH_INVALID")
		}
		var selected string
		matched := false
		for _, m := range a.Layout {
			if name == m.Source || strings.HasPrefix(name, m.Source+"/") {
				if matched {
					return "", false, errors.New("RUNTIME_ARCHIVE_LAYOUT_OVERLAP")
				}
				matched = true
				suffix := strings.TrimPrefix(strings.TrimPrefix(name, m.Source), "/")
				selected = path.Join(m.Destination, suffix)
				if selected == "." {
					selected = ""
				}
			}
		}
		if matched && selected != "" {
			if !RuntimeRelativePath(selected) || seen[strings.ToLower(selected)] {
				return "", false, errors.New("RUNTIME_ARCHIVE_ALIAS")
			}
			seen[strings.ToLower(selected)] = true
		}
		return selected, matched, nil
	}
	consume := func(name string, mode fs.FileMode, size int64, reader io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mapped, selected, err := mapName(name)
		if err != nil {
			return err
		}
		if !selected {
			return nil
		}
		if mode&fs.ModeSymlink != 0 || (!mode.IsRegular() && !mode.IsDir()) {
			return errors.New("RUNTIME_ARCHIVE_OBJECT_UNSUPPORTED")
		}
		if mapped == "" {
			if !mode.IsDir() {
				return errors.New("RUNTIME_ARCHIVE_ROOT_INVALID")
			}
			return nil
		}
		target := destination + "/" + mapped
		if mode.IsDir() {
			return runtimeMkdirAll(root, target)
		}
		if size < 0 {
			return errors.New("RUNTIME_ARCHIVE_MEMBER_SIZE_DRIFT")
		}
		return writeRuntimeStream(ctx, root, target, reader, size, mode.Perm())
	}
	if a.ArchiveFormat == "zip" {
		z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return err
		}
		for _, f := range z.File {
			if f.UncompressedSize64 > uint64(^uint64(0)>>1) {
				return errors.New("RUNTIME_ARCHIVE_MEMBER_SIZE_DRIFT")
			}
			r, err := f.Open()
			if err != nil {
				return err
			}
			e := consume(f.Name, f.Mode(), int64(f.UncompressedSize64), r)
			if err := errors.Join(e, r.Close()); err != nil {
				return err
			}
		}
	} else if a.ArchiveFormat == "tar.gz" {
		g, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return err
		}
		defer g.Close()
		t := tar.NewReader(g)
		for {
			h, err := t.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA && h.Typeflag != tar.TypeDir {
				return errors.New("RUNTIME_ARCHIVE_OBJECT_UNSUPPORTED")
			}
			mode := h.FileInfo().Mode()
			if err := consume(h.Name, mode, h.Size, t); err != nil {
				return err
			}
		}
	} else {
		return errors.New("RUNTIME_ARCHIVE_FORMAT_UNSUPPORTED")
	}
	if len(seen) == 0 {
		return errors.New("RUNTIME_ARCHIVE_LAYOUT_EMPTY")
	}
	return nil
}

func writeRuntimeStream(ctx context.Context, root *os.Root, name string, reader io.Reader, size int64, mode fs.FileMode) (returned error) {
	if !RuntimeRelativePath(name) || size < 0 {
		return errors.New("RUNTIME_FILE_PATH_INVALID")
	}
	if parent := path.Dir(name); parent != "." {
		if err := runtimeMkdirAll(root, parent); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	defer func() { returned = errors.Join(returned, f.Close()) }()
	n, err := io.Copy(f, io.LimitReader(&runtimeContextReader{ctx, reader}, size))
	if err != nil {
		return err
	}
	if n != size {
		return errors.New("RUNTIME_ARCHIVE_MEMBER_SIZE_DRIFT")
	}
	var probe [1]byte
	if n, err := reader.Read(probe[:]); n != 0 || err != io.EOF {
		return errors.Join(err, errors.New("RUNTIME_ARCHIVE_MEMBER_SIZE_DRIFT"))
	}
	return f.Sync()
}

type runtimeContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *runtimeContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func inventoryRuntimeTree(ctx context.Context, root *os.Root, prefix string) (v RuntimeInventoryV1, returned error) {
	v = RuntimeInventoryV1{Schema: RuntimeInventorySchemaV1, Target: runtimePayloadTargetV1, Objects: []RuntimeObjectV1{}}
	err := fs.WalkDir(root.FS(), prefix, func(name string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if name == prefix {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if runtimeReparse(info) {
			return errors.New("RUNTIME_STAGE_REPARSE")
		}
		rel := strings.TrimPrefix(name, prefix+"/")
		o := RuntimeObjectV1{Path: rel, Kind: "directory", Attributes: runtimeAttributes(info)}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return errors.New("RUNTIME_STAGE_OBJECT_UNSUPPORTED")
			}
			fact, err := inspectRuntimeFile(ctx, root, name, nil)
			if err != nil {
				return err
			}
			size := fact.Size
			o.Kind = "file"
			o.Size = &size
			o.SHA256 = fact.SHA256
		}
		v.Objects = append(v.Objects, o)
		return nil
	})
	sort.Slice(v.Objects, func(i, j int) bool { return v.Objects[i].Path < v.Objects[j].Path })
	return v, err
}
func copyRuntimeTree(ctx context.Context, source *os.Root, from string, target *os.Root, to string) error {
	if err := runtimeMkdirAll(target, to); err != nil {
		return err
	}
	return fs.WalkDir(source.FS(), from, func(name string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if name == from {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		destination := to + "/" + strings.TrimPrefix(name, from+"/")
		if entry.IsDir() {
			return runtimeMkdirAll(target, destination)
		}
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		return withRuntimeFile(ctx, source, name, nil, func(f *os.File, size int64) error {
			return writeRuntimeStream(ctx, target, destination, f, size, info.Mode().Perm())
		})
	})
}
