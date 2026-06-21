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
// suffix timestamp is 10 days in the past and verifies SweepOldBinaries removes
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
	// Plant an old .old- file with suffix timestamp > 7 days. Use a
	// Windows-filename-safe suffix (no colons).
	oldSuffix := time.Now().UTC().Add(-10 * 24 * time.Hour).Format(renameAsideTimestampLayout)
	old := target + ".old-" + oldSuffix
	if err := os.WriteFile(old, []byte("ancient"), 0o700); err != nil {
		t.Fatal(err)
	}
	freshMtime := time.Now()
	if err := os.Chtimes(old, freshMtime, freshMtime); err != nil {
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

func platformBinaryName() string {
	if runtime.GOOS == "windows" {
		return "mcphub.exe"
	}
	return "mcphub"
}

// TestSweepOldBinaries_KeepsRecentFiles verifies that aside files whose suffix
// timestamp is within the 7-day retention window are NOT removed.
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
	// Plant a recent generated .old- file (suffix 1 day ago — within retention).
	recentSuffix := time.Now().UTC().Add(-24 * time.Hour).Format(renameAsideTimestampLayout)
	recent := target + ".old-" + recentSuffix
	if err := os.WriteFile(recent, []byte("recent"), 0o700); err != nil {
		t.Fatal(err)
	}
	recentMtime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(recent, recentMtime, recentMtime); err != nil {
		t.Fatal(err)
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatalf("recent binary was swept (should retain <7d): %v", err)
	}
}

// TestSweepOldBinaries_KeepsFreshSuffixDespiteStaleMtime verifies that the
// retention age is based on the generated .old-<timestamp> suffix, not on the
// preserved file mtime from the prior installed binary.
func TestSweepOldBinaries_KeepsFreshSuffixDespiteStaleMtime(t *testing.T) {
	dir := t.TempDir()
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	freshSuffix := time.Now().UTC().Format(renameAsideTimestampLayout)
	freshAside := target + ".old-" + freshSuffix
	if err := os.WriteFile(freshAside, []byte("fresh-aside"), 0o700); err != nil {
		t.Fatal(err)
	}
	staleMtime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(freshAside, staleMtime, staleMtime); err != nil {
		t.Fatal(err)
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(freshAside); err != nil {
		t.Fatalf("fresh-suffix aside was swept because its file mtime was stale: %v", err)
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

	// Plant renameAsideMaxKeep+2 recent asides with strictly-decreasing suffix
	// timestamps (index 0 newest). All are < 7 days old, so only the count cap can prune.
	const planted = renameAsideMaxKeep + 2
	paths := make([]string, planted)
	baseSuffixTime := time.Now().UTC().Add(-time.Minute)
	for i := 0; i < planted; i++ {
		suffix := baseSuffixTime.Add(-time.Duration(i) * time.Minute).Format(renameAsideTimestampLayout)
		p := target + ".old-" + suffix
		if err := os.WriteFile(p, []byte("aside"), 0o700); err != nil {
			t.Fatal(err)
		}
		// Keep file mtimes equal; suffix time is the ranking authority.
		mt := time.Now()
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

// TestSweepOldBinaries_SkipsInvalidTimestampSuffix verifies that a same-prefix
// file not generated by RenameAsideReplace is never deleted, even when its mtime
// is beyond retention. The suffix must parse with renameAsideTimestampLayout
// before any age or count-cap pruning can apply.
func TestSweepOldBinaries_SkipsInvalidTimestampSuffix(t *testing.T) {
	dir := t.TempDir()
	targetName := "mcphub.exe"
	if runtime.GOOS != "windows" {
		targetName = "mcphub"
	}
	target := filepath.Join(dir, targetName)
	if err := os.WriteFile(target, []byte("current"), 0o700); err != nil {
		t.Fatal(err)
	}
	invalid := target + ".old-not-a-generated-timestamp"
	if err := os.WriteFile(invalid, []byte("operator-note"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(invalid, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	if err := SweepOldBinaries(dir); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if _, err := os.Stat(invalid); err != nil {
		t.Fatalf("invalid-suffix file was swept; want preserved: %v", err)
	}
}
