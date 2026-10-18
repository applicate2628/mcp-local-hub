package binaryadmission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

var ErrPublisherSourceTableInvalid = errors.New("PUBLISHER_SOURCE_TABLE_INVALID")

type PublisherSourceInput struct {
	SourceSnapshotSHA256 string `json:"source_snapshot_sha256"`
	SourceEpoch          string `json:"source_epoch"`
	FileCount            int    `json:"file_count"`
}

type publisherSourceFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

func marshalPublisherSourceFiles(files []publisherSourceFile) ([]byte, error) {
	return json.Marshal(struct {
		Schema  string                `json:"schema"`
		Version int                   `json:"version"`
		Files   []publisherSourceFile `json:"files"`
	}{"mcphub-publisher-source-closure-v1", 1, files})
}

// uniqueSourceJSON validates duplicate keys at every object boundary before
// decoding the existing snapshot. It selects no files and mints no epoch.
func uniqueSourceJSON(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, composite := t.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrPublisherSourceTableInvalid
			}
			seen[name] = true
			if err := uniqueSourceJSON(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := uniqueSourceJSON(d); err != nil {
				return err
			}
		}
	default:
		return ErrPublisherSourceTableInvalid
	}
	_, err = d.Token()
	return err
}

func publisherSourcePathValid(p string) bool {
	if p == "" || !utf8.ValidString(p) || strings.ContainsRune(p, '\ufffd') || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\:\x00\r\n") {
		return false
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
			return false
		}
	}
	return true
}

func readPublisherSourceFile(ctx context.Context, filename string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lstat, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	return readPublisherSourceOpenedFile(ctx, lstat, func() (*os.File, error) { return os.Open(filename) })
}

// Preparation uses the same reader through a retained, traversal-bound os.Root.
func readPublisherSourceOpenedFile(ctx context.Context, lstat os.FileInfo, open func() (*os.File, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !lstat.Mode().IsRegular() || publisherSourceReparse(lstat) {
		return nil, ErrPublisherSourceTableInvalid
	}
	f, err := open()
	if err != nil {
		return nil, err
	}
	info, statErr := f.Stat()
	var raw []byte
	primary := statErr
	if primary == nil && (!info.Mode().IsRegular() || !os.SameFile(lstat, info)) {
		primary = ErrPublisherSourceTableInvalid
	}
	if primary == nil {
		raw, primary = io.ReadAll(f)
	}
	closeErr := f.Close()
	if err := errors.Join(primary, closeErr, ctx.Err()); err != nil {
		return nil, err
	}
	return raw, nil
}

func VerifyPublisherSourceSnapshot(ctx context.Context, sourceRoot string, rawSnapshot []byte) (PublisherSourceInput, error) {
	fail := func(err error) (PublisherSourceInput, error) {
		return PublisherSourceInput{}, errors.Join(ErrPublisherSourceTableInvalid, err)
	}
	if ctx == nil || !filepath.IsAbs(sourceRoot) || !utf8.Valid(rawSnapshot) {
		return fail(errors.New("invalid source input"))
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	d := json.NewDecoder(bytes.NewReader(rawSnapshot))
	if err := uniqueSourceJSON(d); err != nil {
		return fail(err)
	}
	if _, err := d.Token(); err != io.EOF {
		return fail(errors.New("trailing snapshot data"))
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(rawSnapshot, &envelope); err != nil {
		return fail(err)
	}
	var schema string
	if err := json.Unmarshal(envelope["schema"], &schema); err != nil {
		return fail(err)
	}
	var rawFiles map[string]json.RawMessage
	if err := json.Unmarshal(envelope["files"], &rawFiles); err != nil {
		return fail(err)
	}
	if schema != "reused-graphify-source-snapshot-v1" || len(rawFiles) == 0 {
		return fail(errors.New("missing snapshot files"))
	}
	filesTable := map[string]string{}
	for name, rawRow := range rawFiles {
		var row map[string]json.RawMessage
		if err := json.Unmarshal(rawRow, &row); err != nil {
			return fail(err)
		}
		var hash string
		if err := json.Unmarshal(row["sha256"], &hash); err != nil {
			return fail(err)
		}
		filesTable[name] = hash
	}
	names := make([]string, 0, len(filesTable))
	folded := map[string]bool{}
	for name, hash := range filesTable {
		fold := strings.ToLower(name)
		generatedJSON := strings.HasPrefix(name, ".scratch/windows-versioninfo-") && strings.Count(name, "/") == 2 && strings.HasSuffix(name, "/product.json")
		if !publisherSourcePathValid(name) || !validLowerHex(hash, 64, 64) || folded[fold] || name == "cmd/mcphub/zz_product_versioninfo.syso" || generatedJSON {
			return fail(errors.New("invalid or generated source row"))
		}
		folded[fold] = true
		names = append(names, name)
	}
	sort.Strings(names)
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil {
		return fail(err)
	}
	if !rootInfo.IsDir() || publisherSourceReparse(rootInfo) {
		return fail(errors.New("source root is not ordinary"))
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(sourceRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if publisherSourceReparse(info) {
			return ErrPublisherSourceTableInvalid
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return ErrPublisherSourceTableInvalid
		}
		rel, err := filepath.Rel(sourceRoot, filename)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		hash, exists := filesTable[rel]
		if !exists {
			return fmt.Errorf("undeclared source file")
		}
		raw, err := readPublisherSourceFile(ctx, filename)
		if err != nil {
			return err
		}
		h := sha256.Sum256(raw)
		if hex.EncodeToString(h[:]) != hash {
			return fmt.Errorf("changed source file")
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return fail(err)
	}
	if len(seen) != len(filesTable) {
		return fail(errors.New("missing source file"))
	}
	// Resolver/embed closure admission belongs to Root's original input owner
	// before helper compilation. This verifier proves only the SAME selected
	// table's ordinary physical coverage; it does not select or resolve modules.
	files := make([]publisherSourceFile, 0, len(names))
	for _, name := range names {
		files = append(files, publisherSourceFile{name, filesTable[name]})
	}
	body, err := marshalPublisherSourceFiles(files)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	epoch := sha256.Sum256(body)
	input := sha256.Sum256(rawSnapshot)
	return PublisherSourceInput{hex.EncodeToString(input[:]), hex.EncodeToString(epoch[:]), len(files)}, nil
}
