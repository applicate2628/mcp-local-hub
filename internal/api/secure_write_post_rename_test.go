package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecureWritePostRenameVerifyFailureLeavesNoFile pins the
// "no file on error" contract (secure_write_client_config.go package
// doc: "On any error, no partial file is left at path") for the
// POST-RENAME failure path on BOTH platform legs.
//
// Before this fix, a failure AFTER the atomic rename (the post-rename
// re-open or the owner/mode/DACL re-verify) returned an error while a
// COMPLETE owner-only file remained published at `path`. The caller
// (client_write_init.go) treats any error as refuse-and-fall-back-to-
// per-daemon-URLs, believing nothing was written — so a published file
// at `path` is a contract violation. The fix best-effort deletes the
// published file via a handle/dirfd-relative delete; this test forces a
// post-rename verify failure via the postRenameVerifyFailHook seam and
// asserts NO file is left at the destination.
func TestSecureWritePostRenameVerifyFailureLeavesNoFile(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")

	// Force the post-rename verify to fail (platform-neutral seam).
	prev := postRenameVerifyFailHook
	t.Cleanup(func() { postRenameVerifyFailHook = prev })
	postRenameVerifyFailHook = func() error {
		return fmt.Errorf("synthetic post-rename verify failure")
	}

	err := SecureWriteClientConfig(target, []byte(`{"v":1}`))
	if err == nil {
		t.Fatalf("SecureWriteClientConfig must return an error when the post-rename verify fails")
	}
	if !strings.Contains(err.Error(), "post-rename verify") {
		t.Errorf("error %q should name the post-rename verify failure", err.Error())
	}

	// The contract: no file left at path after the error. The post-rename
	// cleanup must have deleted the just-published destination.
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("destination %q must NOT exist after a post-rename verify failure (no-file-on-error contract); the published file was left behind", target)
	} else if !os.IsNotExist(statErr) {
		t.Fatalf("unexpected stat error on %q: %v", target, statErr)
	}
}

// TestSecureWritePostRenameVerifyFailureSucceedsOnFreshSlot is the
// companion that proves the cleanup path is reached even when the
// destination did NOT pre-exist (the slot was empty before the write).
// The published file (created by the rename) must still be removed.
func TestSecureWritePostRenameVerifyFailureSucceedsOnFreshSlot(t *testing.T) {
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "fresh-only.json")

	// Sanity: the slot is empty before the write.
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("precondition: %q should not exist yet", target)
	}

	prev := postRenameVerifyFailHook
	t.Cleanup(func() { postRenameVerifyFailHook = prev })
	postRenameVerifyFailHook = func() error {
		return fmt.Errorf("synthetic post-rename verify failure")
	}

	if err := SecureWriteClientConfig(target, []byte(`{"v":1}`)); err == nil {
		t.Fatalf("expected post-rename verify failure error")
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatalf("destination %q must NOT exist after the failed write on a fresh slot", target)
	}
}

// TestSecureWriteNoHookIsCleanRoundTrip is the regression guard that the
// seam is inert in production (nil hook → real verify → normal success).
func TestSecureWriteNoHookIsCleanRoundTrip(t *testing.T) {
	if postRenameVerifyFailHook != nil {
		t.Fatalf("postRenameVerifyFailHook must be nil by default (production), got non-nil")
	}
	dir := hardenedTempDir(t)
	target := filepath.Join(dir, "client-config.json")
	if err := SecureWriteClientConfig(target, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("clean write with nil hook failed: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Errorf("content = %q, want %q", got, `{"ok":true}`)
	}
}
