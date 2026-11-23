//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// This writes an inert receipt beneath an issued generation. It starts no
// process and claims neither installation nor process-settlement evidence.
func TestPreparedSerenaOwnedGenerationReadsReceiptWithOriginalCustody(t *testing.T) {
	r := preparedRequest()
	r.Namespace = hardenedTempDir(t)
	r.WorkLimit, r.CleanupLimit = time.Minute, time.Minute
	r.WorkDeadline = time.Now().Add(r.WorkLimit)
	r.AbsoluteCleanupDeadline = r.WorkDeadline.Add(r.CleanupLimit)
	work, cancel := context.WithDeadline(context.Background(), r.WorkDeadline)
	defer cancel()
	key, err := r.Identity()
	if err != nil {
		t.Fatal(err)
	}
	a := &preparedSerenaAcquisition{inputs: SerenaPreparationInputs{Request: r}}
	defer func() {
		if err := a.closeOriginalRoots(); err != nil {
			t.Error(err)
		}
	}()
	roots, err := a.acquireRoots(work, r, key)
	if err != nil {
		t.Fatal(err)
	}
	owner := a.generation.platform.(*windowsSerenaOwnedTree)
	retained := owner.root
	path := filepath.Join(roots.Generation.Path, "tools", r.Package, "uv-receipt.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw := []byte("[tool]\nname = \"original-generation-fixture\"\n")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := qualifyPreparedSerenaFile(work, path, 4096); !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		t.Fatalf("independent reader lost its ancestor policy: %v", err)
	}
	if err := validatePreparedSerenaGeneration(work, a.generation, r, roots.Generation); err != nil {
		t.Fatal(err)
	}
	actual, got, err := qualifyPreparedSerenaOwnedFile(work, a.generation, path, 4096)
	if err != nil || actual.Path != path || actual.SHA256 != preparedSerenaHash(raw) || actual.Device == "" || actual.FileID == "" || string(got) != string(raw) {
		t.Fatalf("issued generation receipt: file=%+v bytes=%d error=%v", actual, len(got), err)
	}
	files, err := qualifyPreparedSerenaOwnedFiles(work, a.generation, []string{path, path}, 4096)
	if err != nil || len(files) != 1 || files[path] != actual {
		t.Fatalf("issued generation batch: files=%v error=%v", files, err)
	}
	if owner.root != retained || retained.close.Attempted {
		t.Fatal("reader replaced or closed the original generation handle")
	}
	identity, err := serenaTreeHandleIdentity(retained.handle, nil)
	if err != nil || identity.Device != roots.Generation.Device || identity.FileID != roots.Generation.FileID {
		t.Fatalf("original generation identity changed: %v", err)
	}
	other, err := ntOpenStateReadRootMode(roots.Generation.Path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)
	if err == nil {
		t.Error("owned reader released the original delete/rename exclusion")
		if closeErr := windows.CloseHandle(other); closeErr != nil {
			t.Error(closeErr)
		}
	} else if !errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		t.Fatal(err)
	}
	closed, err := a.generation.Close()
	if err != nil || !closed.Attempted || !closed.Closed || closed.Error != nil {
		t.Fatalf("original generation close: %+v, %v", closed, err)
	}
	if file, data, err := qualifyPreparedSerenaOwnedFile(work, a.generation, path, 4096); err == nil || file != (PreparedSerenaFile{}) || len(data) != 0 {
		t.Fatal("closed original generation supplied a receipt")
	}
}
