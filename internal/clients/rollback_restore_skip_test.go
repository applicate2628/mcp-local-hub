package clients

// Tests for the design round-4 atomic, entry-scoped skip-if-unchanged folded into
// the rollback restore body (allowHubEntry=true path). Two properties are locked in:
//
//  1. Entry-scoped compare (NOT whole-file): when the write-target's copy of the
//     restored entry already equals the backup's, the restore returns nil WITHOUT
//     writing — even if a SIBLING entry in the same config differs. A whole-file
//     compare would see the sibling differ and force a redundant (potentially
//     damaging) restore (round-3 Sol P2). The write is observed via a WriteConfigFile
//     counter, so the assertion is robust regardless of an adapter's re-serialization
//     byte behavior.
//
//  2. The compare + the restore write are one atomic critical section under the
//     lockingClient's single withConfigLock hold — a concurrent lock holder blocks a
//     rollback restore until it releases (round-3 TOCTOU, Sol + Terra).

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeFileForSkipTest(t *testing.T, path, contents string) {
	t.Helper()
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileForSkipTest(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// countingWriteOverrideForSkipTest swaps WriteConfigFile for a plain os.WriteFile
// wrapper that counts the calls, restoring the original in Cleanup. The returned
// pointer is read only from the single test goroutine (these sub-tests do not run
// concurrently), so no synchronization is needed.
func countingWriteOverrideForSkipTest(t *testing.T) *int {
	t.Helper()
	orig := WriteConfigFile
	var n int
	WriteConfigFile = func(path string, contents []byte) error {
		n++
		if dir := filepath.Dir(path); dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
		}
		return os.WriteFile(path, contents, 0o600)
	}
	t.Cleanup(func() { WriteConfigFile = orig })
	return &n
}

// TestRollbackRestore_EntryScopedSkipPreservesSibling exercises the folded skip on
// three of the six distinct restore bodies — codex (TOML, already reads live), the
// jsonMCPClient family (cursor/gemini/qwen/antigravity/bob/amp), and mimo (top
// write-target layer). In each: the target entry E is byte-identical to the backup,
// a SIBLING entry S differs. The rollback restore must return nil, write NOTHING,
// and leave the file (and thus the differing sibling) byte-unchanged.
func TestRollbackRestore_EntryScopedSkipPreservesSibling(t *testing.T) {
	t.Run("codex", func(t *testing.T) {
		dir := t.TempDir()
		livePath := filepath.Join(dir, "config.toml")
		backupPath := filepath.Join(dir, "config.toml.bak")
		backupTOML := "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n\n" +
			"[mcp_servers.S]\nurl = \"http://127.0.0.1:9299/mcp\"\n"
		// Target E byte-identical to backup's E; sibling S differs (9201 vs 9299).
		liveTOML := "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n\n" +
			"[mcp_servers.S]\nurl = \"http://127.0.0.1:9201/mcp\"\n"
		writeFileForSkipTest(t, backupPath, backupTOML)
		writeFileForSkipTest(t, livePath, liveTOML)

		n := countingWriteOverrideForSkipTest(t)
		before := readFileForSkipTest(t, livePath)
		c := &codexCLI{path: livePath}
		if err := c.RestoreEntryFromBackupForRollback(backupPath, "E"); err != nil {
			t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
		}
		assertSkipped(t, n, before, readFileForSkipTest(t, livePath))
	})

	t.Run("jsonMCPClient", func(t *testing.T) {
		dir := t.TempDir()
		livePath := filepath.Join(dir, "mcp.json")
		backupPath := filepath.Join(dir, "mcp.json.bak")
		backupJSON := "{\n  \"mcpServers\": {\n" +
			"    \"E\": {\"url\": \"http://127.0.0.1:9200/mcp\", \"disabled\": false},\n" +
			"    \"S\": {\"url\": \"http://127.0.0.1:9299/mcp\", \"disabled\": false}\n  }\n}\n"
		liveJSON := "{\n  \"mcpServers\": {\n" +
			"    \"E\": {\"url\": \"http://127.0.0.1:9200/mcp\", \"disabled\": false},\n" +
			"    \"S\": {\"url\": \"http://127.0.0.1:9201/mcp\", \"disabled\": false}\n  }\n}\n"
		writeFileForSkipTest(t, backupPath, backupJSON)
		writeFileForSkipTest(t, livePath, liveJSON)

		n := countingWriteOverrideForSkipTest(t)
		before := readFileForSkipTest(t, livePath)
		j := &jsonMCPClient{path: livePath, clientName: "cursor", urlField: "url"}
		if err := j.RestoreEntryFromBackupForRollback(backupPath, "E"); err != nil {
			t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
		}
		assertSkipped(t, n, before, readFileForSkipTest(t, livePath))
	})

	t.Run("mimo", func(t *testing.T) {
		dir := t.TempDir()
		livePath := filepath.Join(dir, "mimocode.json")
		backupPath := filepath.Join(dir, "mimocode.json.bak")
		backupJSON := "{\n  \"mcp\": {\n" +
			"    \"E\": {\"type\": \"remote\", \"url\": \"http://127.0.0.1:9200/mcp\", \"enabled\": true},\n" +
			"    \"S\": {\"type\": \"remote\", \"url\": \"http://127.0.0.1:9299/mcp\", \"enabled\": true}\n  }\n}\n"
		liveJSON := "{\n  \"mcp\": {\n" +
			"    \"E\": {\"type\": \"remote\", \"url\": \"http://127.0.0.1:9200/mcp\", \"enabled\": true},\n" +
			"    \"S\": {\"type\": \"remote\", \"url\": \"http://127.0.0.1:9201/mcp\", \"enabled\": true}\n  }\n}\n"
		writeFileForSkipTest(t, backupPath, backupJSON)
		writeFileForSkipTest(t, livePath, liveJSON)

		n := countingWriteOverrideForSkipTest(t)
		before := readFileForSkipTest(t, livePath)
		// Single-file mimocode.json ⇒ o.path IS the only (top) read+write layer, so
		// the skip's readRawConfig(o.path) compare sees exactly this file.
		o := &mimoCodeClient{path: livePath}
		if err := o.RestoreEntryFromBackupForRollback(backupPath, "E"); err != nil {
			t.Fatalf("RestoreEntryFromBackupForRollback: %v", err)
		}
		assertSkipped(t, n, before, readFileForSkipTest(t, livePath))
	})
}

// assertSkipped asserts the entry-scoped skip fired: NO write happened and the
// write-target file is byte-unchanged (so the differing sibling S — port 9201, not
// the backup's 9299 — is preserved).
func assertSkipped(t *testing.T, writes *int, before, after []byte) {
	t.Helper()
	if *writes != 0 {
		t.Fatalf("rollback restore WROTE the config (writes=%d) though the target entry was unchanged; the entry-scoped skip did not fire (a whole-file compare would restore because the sibling differs)", *writes)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("write-target bytes changed by a restore that should have been skipped:\n got: %q\nwant: %q", after, before)
	}
	if !bytes.Contains(after, []byte("9201")) || bytes.Contains(after, []byte("9299")) {
		t.Fatalf("sibling entry S was not preserved (want live 9201, not backup 9299):\n%s", after)
	}
}

// TestRollbackRestore_BarrierBlocksUntilLockReleased proves the folded compare+restore
// is one atomic critical section under the lockingClient's withConfigLock: while
// another holder owns the per-path lock, a rollback restore cannot proceed.
func TestRollbackRestore_BarrierBlocksUntilLockReleased(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "config.toml")
	backupPath := filepath.Join(dir, "config.toml.bak")
	// MUTATED live (E differs from backup) so the restore actually WRITES once it
	// acquires the lock — proving the whole compare+restore section is lock-guarded,
	// not merely a fast skip.
	writeFileForSkipTest(t, backupPath, "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n")
	writeFileForSkipTest(t, livePath, "[mcp_servers.E]\nurl = \"http://127.0.0.1:9999/mcp\"\n")
	lc := newLockingClient(&codexCLI{path: livePath})

	acquired := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withConfigLock(livePath, func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired // A now holds the per-path lock.

	bDone := make(chan error, 1)
	go func() {
		bDone <- lc.RestoreEntryFromBackupForRollback(backupPath, "E")
	}()

	// While A holds the lock, B must NOT complete.
	select {
	case err := <-bDone:
		t.Fatalf("rollback restore completed while another holder had the config lock (compare+restore NOT lock-guarded): err=%v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // A releases the lock.
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("rollback restore failed after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rollback restore did not complete after the lock was released")
	}

	// The restore ran under the lock and reverted the mutated entry to the backup.
	after := readFileForSkipTest(t, livePath)
	if !bytes.Contains(after, []byte("9200")) || bytes.Contains(after, []byte("9999")) {
		t.Fatalf("restore did not revert the mutated entry after acquiring the lock:\n%s", after)
	}
}

// TestRollbackRestore_BarrierBlocksSkipCompareUntilLockReleased proves the
// entry-scoped SKIP-compare itself runs UNDER the withConfigLock hold (design
// round-5, Terra P2). Here live == backup (byte-identical unmutated entry), so the
// restore's terminal action is a SKIP, not a write. If the skip pre-check ran
// UNLOCKED it would return nil immediately while A still holds the lock — so this
// test's "B must NOT complete while A holds the lock" assertion detects a
// reintroduced unlocked pre-check that the live≠backup write-exclusion test above
// (which always blocks on the write lock) structurally cannot.
func TestRollbackRestore_BarrierBlocksSkipCompareUntilLockReleased(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "config.toml")
	backupPath := filepath.Join(dir, "config.toml.bak")
	// live == backup (identical unmutated E) ⇒ the restore SKIPS (no write). The file is
	// PRESENT, so the whole-file-gone recovery is inert and the entry-scoped skip decides.
	identical := "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n"
	writeFileForSkipTest(t, backupPath, identical)
	writeFileForSkipTest(t, livePath, identical)
	lc := newLockingClient(&codexCLI{path: livePath})
	before := readFileForSkipTest(t, livePath)

	acquired := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = withConfigLock(livePath, func() error {
			close(acquired)
			<-release
			return nil
		})
	}()
	<-acquired // A now holds the per-path lock.

	bDone := make(chan error, 1)
	go func() {
		bDone <- lc.RestoreEntryFromBackupForRollback(backupPath, "E")
	}()

	// While A holds the lock, B must NOT complete — an UNLOCKED skip pre-check would
	// isNoop==true and return immediately here.
	select {
	case err := <-bDone:
		t.Fatalf("rollback restore completed while another holder had the config lock (skip-compare NOT lock-guarded): err=%v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // A releases the lock.
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("rollback restore failed after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rollback restore did not complete after the lock was released")
	}

	// The skip left the live config byte-unchanged.
	if after := readFileForSkipTest(t, livePath); !bytes.Equal(before, after) {
		t.Fatalf("live config changed by a restore that should have skipped:\n got: %q\nwant: %q", after, before)
	}
}

// TestRollbackRestore_LockContentionNoRace hammers the same config path with
// concurrent rollback restores + AddEntry through the lockingClient. Run under
// `-race`, it proves the lock serializes every read-modify-write (no data race) and
// the config stays parseable through the churn.
func TestRollbackRestore_LockContentionNoRace(t *testing.T) {
	dir := t.TempDir()
	livePath := filepath.Join(dir, "config.toml")
	backupPath := filepath.Join(dir, "config.toml.bak")
	writeFileForSkipTest(t, backupPath, "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n")
	writeFileForSkipTest(t, livePath, "[mcp_servers.E]\nurl = \"http://127.0.0.1:9200/mcp\"\n")
	lc := newLockingClient(&codexCLI{path: livePath})
	entry := MCPEntry{Name: "E", URL: "http://127.0.0.1:9300/mcp"}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = lc.RestoreEntryFromBackupForRollback(backupPath, "E")
		}()
		go func() {
			defer wg.Done()
			_ = lc.AddEntry(entry)
		}()
	}
	wg.Wait()

	// Sanity: the config is still parseable after concurrent RMW churn.
	if _, err := (&codexCLI{path: livePath}).readTOML(); err != nil {
		t.Fatalf("config corrupted after concurrent restore/AddEntry churn: %v", err)
	}
}
