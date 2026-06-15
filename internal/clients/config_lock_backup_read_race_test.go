package clients

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConfigLock_BackupReadDuringWrite_NeverSelectsTornFile is the b1
// regression guard for the backup-READ-during-WRITE race. The backup-write
// path (Backup/BackupKeep -> writeBackup -> copyFile) is serialized by the
// lockingClient decorator, but BEFORE this fix the backup-READ selection and
// per-file inspection (LatestBackupPath to pick a path, then
// BackupContainsEntry / BackupEntryIsHubManaged to classify it — the exact
// demigrate sequence) read the same backup directory/files with NO lock. A
// reader could therefore inspect a timestamped backup that a concurrent
// writer was mid-truncate-writing and observe it as empty — silently
// reporting the entry ABSENT when it is actually present. That is the
// "demigrate reading a polluted backup" / silent-wrong-answer failure in
// work-items/bugs/b1-backup-file-race.md §Summary (latestBackup does no JSON
// validation; an empty selection reads as "no entry").
//
// The torn window is made DETERMINISTICALLY large via copyFileTornWindowHook
// (a test-only seam): while a backup file is truncate-opened but not yet
// rewritten, the hook blocks so the file exists on disk as zero bytes for a
// fixed duration. With both the path SELECTION (LatestBackupPath) and the
// content INSPECTION (BackupContainsEntry) now under the SAME per-path lock as
// the writer, a concurrent reader blocks until the write completes and can
// never observe the zero-byte intermediate. Run with -race.
func TestConfigLock_BackupReadDuringWrite_NeverSelectsTornFile(t *testing.T) {
	// Seed the live config with entry "a" so every backup taken below contains
	// it; the reader asserts BackupContainsEntry("a") is ALWAYS true.
	c, path := newLockingCursorForTest(t, `{"other":"keep-me","mcpServers":{}}`)
	if err := c.AddEntry(MCPEntry{Name: "a", URL: "http://localhost:9121/mcp"}); err != nil {
		t.Fatalf("seed AddEntry: %v", err)
	}
	if _, err := c.Backup(); err != nil {
		t.Fatalf("seed Backup: %v", err)
	}

	// Install the torn-write seam: while a timestamped backup is half-written,
	// block briefly so a non-serialized reader would catch the zero-byte
	// window. Restore on cleanup so this never leaks into other tests.
	copyFileTornWindowHook = func(dst string) {
		// Only stall the rolling timestamped backups, never the one-shot
		// pristine sentinel (irrelevant to this race).
		if filepath.Base(dst) == filepath.Base(path)+originalSentinelSuffix {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Cleanup(func() { copyFileTornWindowHook = nil })

	const iters = 80
	var wg sync.WaitGroup
	wg.Add(2)

	// Writer: hammer Backup so the torn window is open a large fraction of the
	// time. Second-resolution timestamps mean these repeatedly truncate-rewrite
	// the SAME timestamped file the reader selects.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			if _, err := c.Backup(); err != nil {
				t.Errorf("Backup: %v", err)
				return
			}
		}
	}()

	// Reader: run the demigrate read sequence — select via LatestBackupPath,
	// then inspect the selected file's content via BackupContainsEntry. Both
	// are lock-serialized after the fix. If the content read were unserialized
	// it could inspect the mid-truncate (empty) backup and report entry "a"
	// ABSENT — a silent wrong answer the demigrate flow would act on.
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			p, ok, err := c.LatestBackupPath()
			if err != nil {
				t.Errorf("LatestBackupPath: %v", err)
				return
			}
			if !ok {
				continue
			}
			has, err := c.BackupContainsEntry(p, "a")
			if err != nil {
				t.Errorf("BackupContainsEntry(%s): %v", p, err)
				return
			}
			if !has {
				t.Errorf("selected backup %s reports entry \"a\" ABSENT — read a torn/empty file mid-write (b1 read-during-write race)", p)
				return
			}
		}
	}()

	wg.Wait()
}
