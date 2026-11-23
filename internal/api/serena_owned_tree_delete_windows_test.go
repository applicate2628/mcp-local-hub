//go:build windows

package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSerenaOwnedTreeReadonlyHardlinkDeletion(t *testing.T) {
	// Retain the disposable native fixture for external review and trash-first
	// retirement. No synthetic tree or namespace observation grants authority.
	fixture, err := os.MkdirTemp(os.TempDir(), "serena-readonly-unlink-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("retained native readonly-link fixture: %s", filepath.Base(fixture))
	payload := []byte("owned readonly hardlink fixture\n")
	first, survivor := filepath.Join(fixture, "remove.bin"), filepath.Join(fixture, "survivor.bin")
	if err := os.WriteFile(first, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, survivor); err != nil {
		t.Fatal(err)
	}
	firstW, err := windows.UTF16PtrFromString(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(firstW, windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_ARCHIVE); err != nil {
		t.Fatal(err)
	}

	own := func(handle windows.Handle, label string) *serenaTreeHandle {
		h := &serenaTreeHandle{handle: handle}
		t.Cleanup(func() {
			closed, err := h.closeOnce()
			if err != nil || !closed.Closed {
				t.Errorf("%s close incomplete: %+v, %v", label, closed, err)
			}
		})
		return h
	}
	closeChecked := func(h *serenaTreeHandle) {
		t.Helper()
		if closed, err := h.closeOnce(); err != nil || !closed.Closed {
			t.Fatalf("native fixture close incomplete: %+v, %v", closed, err)
		}
	}
	parentHandle, err := ntOpenStateReadRootMode(fixture, windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)
	if err != nil {
		t.Fatal(err)
	}
	parent := own(parentHandle, "parent")
	file := false
	access := uint32(windows.FILE_READ_DATA | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.DELETE)
	handle, err := ntOpenStateReadRelativeMode(parent.handle, "remove.bin", access, &file, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := own(handle, "nominated link")
	before, err := serenaTreeHandleIdentity(handle, nil)
	if err != nil {
		t.Fatal(err)
	}
	var beforeInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &beforeInfo); err != nil {
		t.Fatal(err)
	}
	if before.Directory || before.Links != 2 || beforeInfo.FileAttributes != windows.FILE_ATTRIBUTE_READONLY|windows.FILE_ATTRIBUTE_ARCHIVE {
		t.Fatalf("fixture is not exactly two readonly file links: %+v, attributes=%#x", before, beforeInfo.FileAttributes)
	}
	beforeHash, err := serenaTreeContentHash(context.Background(), handle, before)
	if err != nil || beforeHash != preparedSerenaHash(payload) {
		t.Fatalf("original bytes differ: %s, %v", beforeHash, err)
	}
	survivorHandle, err := ntOpenStateReadRelativeMode(parent.handle, "survivor.bin", windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES, &file, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil)
	if err != nil {
		t.Fatal(err)
	}
	second := own(survivorHandle, "original survivor")
	survivorBefore, err := serenaTreeHandleIdentity(survivorHandle, nil)
	if err != nil || !sameSerenaTreeObject(before, survivorBefore) {
		t.Fatalf("the two owned names do not account for every link: %+v, %v", survivorBefore, err)
	}
	closeChecked(second)

	legacyErr := setFileDeleteOnClose(handle)
	if !errors.Is(legacyErr, windows.STATUS_CANNOT_DELETE) {
		t.Fatalf("legacy disposition did not refuse readonly file: %T %v", legacyErr, legacyErr)
	}
	t.Logf("legacy disposition refused with STATUS_CANNOT_DELETE; attrs=%#x links=%d file_id=%s sha256=%s", beforeInfo.FileAttributes, before.Links, before.FileID, beforeHash)
	if err := setSerenaOwnedTreeEntryDeleteOnClose(handle); err != nil {
		t.Fatalf("owned readonly unlink failed: %T %v", err, err)
	}
	closeChecked(target)

	probe, err := ntOpenStateReadRelativeMode(parent.handle, "remove.bin", windows.FILE_READ_ATTRIBUTES, &file, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil)
	if err == nil {
		closeChecked(own(probe, "unexpected surviving nominated link"))
		t.Fatal("nominated link remains after checked close")
	}
	if !errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) {
		t.Fatalf("nominated absence was not observed: %T %v", err, err)
	}
	survivorHandle, err = ntOpenStateReadRelativeMode(parent.handle, "survivor.bin", windows.FILE_READ_DATA|windows.FILE_READ_ATTRIBUTES, &file, windows.FILE_OPEN, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil)
	if err != nil {
		t.Fatal(err)
	}
	remaining := own(survivorHandle, "remaining survivor")
	after, err := serenaTreeHandleIdentity(survivorHandle, nil)
	if err != nil {
		t.Fatal(err)
	}
	var afterInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(survivorHandle, &afterInfo); err != nil {
		t.Fatal(err)
	}
	afterHash, err := serenaTreeContentHash(context.Background(), survivorHandle, after)
	if err != nil || after.Device != before.Device || after.FileID != before.FileID || after.Links != 1 ||
		after.Bytes != before.Bytes || after.Modified != before.Modified || afterHash != beforeHash ||
		afterInfo.FileAttributes != beforeInfo.FileAttributes {
		t.Fatalf("survivor identity/content/attributes changed: %+v attrs=%#x sha256=%s err=%v", after, afterInfo.FileAttributes, afterHash, err)
	}
	closeChecked(remaining)
	t.Logf("survivor retained unchanged: attrs=%#x links=%d file_id=%s sha256=%s; nominated link absent", afterInfo.FileAttributes, after.Links, after.FileID, afterHash)
}

func TestSerenaOwnedTreeRemovalOpenRejectsPublishedReader(t *testing.T) {
	// A data-only refusal fixture: the invalid handle must never reach an open.
	owner := &windowsSerenaOwnedTree{readExisting: true}
	handle, err := owner.openEntry(windows.InvalidHandle, "unopened.bin", true)
	if handle != nil || err == nil || err.Error() != "SERENA_PUBLISHED_READ_HAS_NO_DELETE_AUTHORITY" {
		t.Fatalf("published reader reached removal open: handle=%v err=%v", handle, err)
	}
}
