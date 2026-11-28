//go:build windows

package binaryadmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
)

type RuntimePayloadFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type RuntimeComponentInspection struct {
	Component  RuntimeComponentV1
	Descriptor RuntimeReleaseDescriptorV1
	Inventory  RuntimeInventoryV1
}
type RuntimePayloadInspection struct {
	Payload       RuntimePayloadV1
	PayloadSHA256 string
	Components    []RuntimeComponentInspection
	Files         []RuntimePayloadFile
}

func inspectInstalledRuntimePayload(ctx context.Context, root *os.Root, indexPath, expectedIndexSHA256 string, index []byte) (result RuntimePayloadInspection, err error) {
	if ctx == nil || root == nil || !RuntimeRelativePath(indexPath) || !validLowerHex(expectedIndexSHA256, 64, 64) {
		return result, errors.New("RUNTIME_INSPECTION_INPUT_INVALID")
	}
	if RuntimeDigest(index) != expectedIndexSHA256 {
		return result, errors.New("RUNTIME_INDEX_DIGEST_MISMATCH")
	}
	result.Payload, err = ParseRuntimePayload(index)
	if err != nil {
		return result, err
	}
	result.PayloadSHA256 = RuntimeDigest(index)
	add := func(name string, raw []byte) {
		result.Files = append(result.Files, RuntimePayloadFile{name, int64(len(raw)), RuntimeDigest(raw)})
	}
	add(indexPath, index)
	for _, c := range result.Payload.Components {
		desc, e := readRuntimeAuthenticatedFile(ctx, root, c.DescriptorPath, c.DescriptorSHA256)
		if e != nil {
			return result, e
		}
		if RuntimeDigest(desc) != c.DescriptorSHA256 {
			return result, errors.New("RUNTIME_DESCRIPTOR_DIGEST_MISMATCH")
		}
		d, e := ParseRuntimeReleaseDescriptor(desc)
		if e != nil {
			return result, e
		}
		if d.Component != c.ID || d.Target != result.Payload.Target {
			return result, errors.New("RUNTIME_DESCRIPTOR_TARGET_MISMATCH")
		}
		add(c.DescriptorPath, desc)
		inv, e := readRuntimeAuthenticatedFile(ctx, root, c.Inventory, c.TreeSHA256)
		if e != nil {
			return result, e
		}
		if RuntimeDigest(inv) != c.TreeSHA256 {
			return result, errors.New("RUNTIME_TREE_DIGEST_MISMATCH")
		}
		v, e := ParseRuntimeInventory(inv)
		if e != nil {
			return result, e
		}
		add(c.Inventory, inv)
		rootInfo, e := root.Lstat(c.Root)
		if e != nil || !rootInfo.IsDir() || runtimeReparse(rootInfo) {
			return result, errors.Join(e, errors.New("RUNTIME_ROOT_NOT_DIRECTORY"))
		}
		objects := map[string]RuntimeObjectV1{}
		for _, o := range v.Objects {
			objects[o.Path] = o
		}
		found := map[string]bool{}
		e = fs.WalkDir(root.FS(), c.Root, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if e := ctx.Err(); e != nil {
				return e
			}
			if name == c.Root {
				if !entry.IsDir() {
					return errors.New("RUNTIME_ROOT_NOT_DIRECTORY")
				}
				return nil
			}
			rel := strings.TrimPrefix(name, c.Root+"/")
			o, ok := objects[rel]
			if !ok {
				return fmt.Errorf("RUNTIME_UNACCOUNTED_OBJECT: %s", name)
			}
			found[rel] = true
			info, e := root.Lstat(name)
			if e != nil {
				return e
			}
			if runtimeReparse(info) || entry.IsDir() != (o.Kind == "directory") {
				return errors.New("RUNTIME_OBJECT_KIND_MISMATCH")
			}
			if o.Kind == "file" {
				file, e := inspectRuntimeFile(ctx, root, name, o.Size)
				if e != nil {
					return e
				}
				if file.SHA256 != o.SHA256 {
					return errors.New("RUNTIME_FILE_DIGEST_MISMATCH")
				}
				result.Files = append(result.Files, file)
			}
			if runtimeAttributes(info) != o.Attributes {
				return errors.New("RUNTIME_ATTRIBUTE_MISMATCH")
			}
			return nil
		})
		if e != nil {
			return result, e
		}
		if len(found) != len(objects) {
			return result, errors.New("RUNTIME_MISSING_OBJECT")
		}
		if o, ok := objects[d.Executable]; !ok || o.Kind != "file" {
			return result, errors.New("RUNTIME_EXECUTABLE_MISSING")
		}
		for _, notice := range d.Licenses {
			if o, ok := objects[notice]; !ok || o.Kind != "file" {
				return result, errors.New("RUNTIME_LICENSE_MISSING")
			}
		}
		for _, f := range d.OriginalFiles {
			if o, ok := objects[f.Path]; !ok || o.Kind != "file" || o.SHA256 != f.SHA256 {
				return result, errors.New("RUNTIME_ORIGINAL_FILE_DRIFT")
			}
		}
		result.Components = append(result.Components, RuntimeComponentInspection{c, d, v})
	}
	return result, ctx.Err()
}

func readRuntimeFile(ctx context.Context, root *os.Root, name string, expected ...RuntimePayloadFile) (raw []byte, retErr error) {
	var size *int64
	if len(expected) > 1 {
		return nil, errors.New("RUNTIME_FILE_EXPECTATION_INVALID")
	}
	if len(expected) == 1 {
		size = &expected[0].Size
	}
	retErr = withRuntimeFile(ctx, root, name, size, func(f *os.File, n int64) error {
		if n == int64(^uint64(0)>>1) {
			return errors.New("RUNTIME_FILE_SIZE_OVERFLOW")
		}
		var err error
		raw, err = io.ReadAll(io.LimitReader(&canonicalEntryBuildContextReader{ctx, f}, n+1))
		if err != nil {
			return err
		}
		if int64(len(raw)) != n {
			return errors.New("RUNTIME_FILE_SIZE_DRIFT")
		}
		if len(expected) == 1 && RuntimeDigest(raw) != expected[0].SHA256 {
			return errors.New("RUNTIME_FILE_DIGEST_MISMATCH")
		}
		return nil
	})
	return raw, retErr
}

// Metadata size is learned by streaming bytes which first match an expected
// digest from the compiled index/descriptor chain. Only then may a bounded
// parsing buffer be allocated. Native payload files never need that buffer.
func readRuntimeAuthenticatedFile(ctx context.Context, root *os.Root, name, expectedSHA string) ([]byte, error) {
	fact, err := inspectRuntimeFile(ctx, root, name, nil)
	if err != nil {
		return nil, err
	}
	if fact.SHA256 != expectedSHA {
		return nil, errors.New("RUNTIME_FILE_DIGEST_MISMATCH")
	}
	return readRuntimeFile(ctx, root, name, fact)
}
func inspectRuntimeFile(ctx context.Context, root *os.Root, name string, size *int64) (fact RuntimePayloadFile, returned error) {
	returned = withRuntimeFile(ctx, root, name, size, func(f *os.File, n int64) error {
		h := sha256.New()
		copied, err := io.Copy(h, &canonicalEntryBuildContextReader{ctx, io.LimitReader(f, n)})
		if err != nil {
			return err
		}
		if copied != n {
			return errors.New("RUNTIME_FILE_SIZE_DRIFT")
		}
		fact = RuntimePayloadFile{name, n, hex.EncodeToString(h.Sum(nil))}
		return nil
	})
	return fact, returned
}
func withRuntimeFile(ctx context.Context, root *os.Root, name string, expectedSize *int64, consume func(*os.File, int64) error) (retErr error) {
	if !RuntimeRelativePath(name) {
		return errors.New("RUNTIME_PATH_INVALID")
	}
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, e := root.Lstat(parent)
		if e != nil {
			return e
		}
		if !info.IsDir() || runtimeReparse(info) {
			return errors.New("RUNTIME_ANCESTOR_INVALID")
		}
	}
	before, e := root.Lstat(name)
	if e != nil {
		return e
	}
	if !before.Mode().IsRegular() || runtimeReparse(before) {
		return errors.New("RUNTIME_FILE_NOT_REGULAR")
	}
	if before.Size() < 0 || expectedSize != nil && before.Size() != *expectedSize {
		return errors.New("RUNTIME_FILE_SIZE_MISMATCH")
	}
	f, e := root.Open(name)
	if e != nil {
		return e
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	opened, e := f.Stat()
	if e != nil {
		return e
	}
	if !os.SameFile(before, opened) {
		return errors.New("RUNTIME_FILE_IDENTITY_DRIFT")
	}
	if e := consume(f, before.Size()); e != nil {
		return e
	}
	after, e := root.Lstat(name)
	if e != nil {
		return e
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runtimeReparse(after) || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("RUNTIME_FILE_IDENTITY_DRIFT")
	}
	return nil
}
func runtimeAttributes(info os.FileInfo) uint32 {
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes & 7
	}
	return 0
}
func runtimeReparse(info os.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}
