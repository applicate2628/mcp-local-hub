package api

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestRenameAside_TwoStepReplace verifies the rename-aside contract:
// after a successful call, the target byte-content equals newSrc's
// prior content AND exactly one `.old-*` aside file exists at the
// expected sibling path.
func TestRenameAside_TwoStepReplace(t *testing.T) {
	dir := t.TempDir()
	// Use the platform-appropriate basename so the glob in
	// SweepOldBinaries matches what RenameAsideReplace produces.
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("old-binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	newSrc := filepath.Join(dir, targetName+".new")
	if err := os.WriteFile(newSrc, []byte("new-binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := RenameAsideReplace(target, newSrc); err != nil {
		t.Fatalf("rename-aside: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new-binary" {
		t.Fatalf("target not replaced: got %q want %q", got, "new-binary")
	}
	matches, err := filepath.Glob(target + ".old-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected one .old-<ts> file, got %d: %v", len(matches), matches)
	}
	// The aside should hold the original "old-binary" bytes.
	asideBytes, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read aside: %v", err)
	}
	if string(asideBytes) != "old-binary" {
		t.Fatalf("aside content unexpected: got %q want %q", asideBytes, "old-binary")
	}
	// newSrc must no longer exist — Rename moved it.
	if _, err := os.Stat(newSrc); !os.IsNotExist(err) {
		t.Fatalf("newSrc still exists post-rename (err=%v)", err)
	}
}

// TestSweepOldBinaries_RemovesAgedFiles plants a stale aside whose
// mtime is 10 days in the past and verifies SweepOldBinaries removes
// it while leaving the current binary intact.
func TestSweepOldBinaries_RemovesAgedFiles(t *testing.T) {
	dir := t.TempDir()
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant an old .old- file with mtime > 7 days. Use a
	// Windows-filename-safe suffix (no colons).
	old := target + ".old-20260501T000000Z"
	if err := os.WriteFile(old, []byte("ancient"), 0o700); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old binary not swept: %v", err)
	}
	// Current binary must be untouched (glob is `.old-*`, not `*`).
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("current binary swept: %v", err)
	}
}

// TestSweepOldBinaries_KeepsRecentFiles verifies that aside files
// whose mtime is within the 7-day retention window are NOT removed.
func TestSweepOldBinaries_KeepsRecentFiles(t *testing.T) {
	dir := t.TempDir()
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Plant a recent .old- file (mtime 1 day ago — within retention).
	recent := target + ".old-recent"
	if err := os.WriteFile(recent, []byte("recent"), 0o700); err != nil {
		t.Fatal(err)
	}
	recentTime := time.Now().Add(-1 * 24 * time.Hour)
	if err := os.Chtimes(recent, recentTime, recentTime); err != nil {
		t.Fatal(err)
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent binary was swept (should retain <7d): %v", err)
	}
}

// TestSweepOldBinaries_CountCapTrimsRecentSurplus plants more recent (<7-day)
// asides than renameAsideMaxKeep and verifies the count cap trims the OLDEST
// surplus even though every file is within the age window — the burst-of-
// same-day-deploys case the 7-day age rule alone never pruned.
func TestSweepOldBinaries_CountCapTrimsRecentSurplus(t *testing.T) {
	dir := t.TempDir()
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}

	// Plant renameAsideMaxKeep+2 recent asides with strictly-decreasing mtimes
	// (index 0 newest). All are < 7 days old, so only the count cap can prune.
	const planted = renameAsideMaxKeep + 2
	paths := make([]string, planted)
	for i := 0; i < planted; i++ {
		p := target + ".old-recent" + string(rune('a'+i))
		if err := os.WriteFile(p, []byte("aside"), 0o700); err != nil {
			t.Fatal(err)
		}
		// i hours ago → index 0 is newest, last index is oldest (still <7d).
		mt := time.Now().Add(-time.Duration(i) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Newest renameAsideMaxKeep must remain; the oldest 2 surplus removed.
	for i := 0; i < renameAsideMaxKeep; i++ {
		if _, err := os.Stat(paths[i]); err != nil {
			t.Fatalf("newest aside #%d was trimmed (should be kept): %v", i, err)
		}
	}
	for i := renameAsideMaxKeep; i < planted; i++ {
		if _, err := os.Stat(paths[i]); !os.IsNotExist(err) {
			t.Fatalf("surplus aside #%d not trimmed by count cap: %v", i, err)
		}
	}
	// The live binary is never an aside; it must survive.
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("current binary swept: %v", err)
	}
}
