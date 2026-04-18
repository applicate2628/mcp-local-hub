package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFindLatestBackupSkipsSentinel locks in the fix for the lexicographic
// sort pitfall: the pristine `-original` sentinel shares the
// `.bak-mcp-local-hub-` prefix with timestamped backups but must never be
// returned by findLatestBackup. "original" sorts AFTER any digit-prefixed
// timestamp (letters > digits in ASCII), so without the explicit skip the
// function would hand back the pristine copy and a `rollback` would
// silently revert all the way to the first-ever install instead of the
// most recent change.
func TestFindLatestBackupSkipsSentinel(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.json")

	// The live file isn't read by findLatestBackup, but the test mirrors
	// how the caller constructs the base prefix (filepath.Base of live).
	if err := os.WriteFile(live, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	write := func(suffix string) string {
		p := live + ".bak-mcp-local-hub-" + suffix
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	_ = write("original")                     // pristine sentinel
	_ = write("20260101-000000")              // first timestamped
	newest := write("20260418-030645")        // most recent — expected result
	_ = write("20260215-120000")              // middle

	got, err := findLatestBackup(live)
	if err != nil {
		t.Fatalf("findLatestBackup: %v", err)
	}
	if got != newest {
		t.Errorf("got %q, want %q (lexicographic sort picked %q instead — sentinel leaked?)",
			filepath.Base(got), filepath.Base(newest), filepath.Base(got))
	}
}

// TestFindLatestBackupNoBackups exercises the empty-result path (no
// sentinel, no timestamped backups) so the function returns "" not error.
func TestFindLatestBackupNoBackups(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.json")
	if err := os.WriteFile(live, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := findLatestBackup(live)
	if err != nil {
		t.Fatalf("findLatestBackup: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string (no backups), got %q", got)
	}
}

// TestFindLatestBackupSentinelOnly covers the case where only the pristine
// sentinel exists (user has installed but never changed anything). Since
// we skip the sentinel, findLatestBackup must return "" — `rollback`
// without --original then prints "no backup found, skipping" and leaves
// the pristine copy untouched.
func TestFindLatestBackupSentinelOnly(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "config.json")
	if err := os.WriteFile(live, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live+".bak-mcp-local-hub-original", []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := findLatestBackup(live)
	if err != nil {
		t.Fatalf("findLatestBackup: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty (only sentinel present), got %q", got)
	}
}
