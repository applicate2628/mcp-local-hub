package clients

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBackupSentinelWrittenOnlyFirstTime verifies the pristine-original
// sentinel (.bak-mcp-local-hub-original) is written exactly once on the
// first Backup call and never overwritten afterwards, even if the live
// config has since been modified.
func TestBackupSentinelWrittenOnlyFirstTime(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"initial":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	// jsonMCPClient is the shared base adapter used by both Gemini and
	// Antigravity; its Backup path exercises the same writeBackup helper
	// that all four adapters now delegate to, so one adapter is enough to
	// lock in the sentinel contract.
	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// First backup — should create the sentinel.
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("first backup: %v", err)
	}
	sentinel := livePath + ".bak-mcp-local-hub-original"
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel not created: %v", err)
	}
	if string(data) != `{"initial":true}` {
		t.Errorf("sentinel content wrong: %s", data)
	}

	// Modify the live file, then back up again. The sentinel must remain
	// pinned to the ORIGINAL content — that's the whole point of the
	// one-shot pristine strategy.
	if err := os.WriteFile(livePath, []byte(`{"modified":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Backup(); err != nil {
		t.Fatalf("second backup: %v", err)
	}
	data, err = os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel disappeared after second backup: %v", err)
	}
	if string(data) != `{"initial":true}` {
		t.Errorf("sentinel got overwritten on second backup: %s", data)
	}
}

// TestBackupKeepsNLatestTimestamped verifies that after N+3 BackupKeep
// calls, only the N most recent timestamped backups remain on disk,
// plus the one pristine sentinel.
func TestBackupKeepsNLatestTimestamped(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"v":0}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// 8 backups with sleep between them so each lands on a distinct
	// timestamp-second (Windows FS resolves mtime only to the second).
	// keepN = 5, so after the 8th call 3 older backups should be pruned.
	for i := 1; i <= 8; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatalf("rewrite live %d: %v", i, err)
		}
		if _, err := adapter.BackupKeep(5); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var timestamped, original int
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".bak-mcp-local-hub-original"):
			original++
		case strings.Contains(name, ".bak-mcp-local-hub-"):
			timestamped++
		}
	}
	if original != 1 {
		t.Errorf("expected 1 sentinel, got %d", original)
	}
	if timestamped != 5 {
		t.Errorf("expected 5 timestamped backups after keep=5, got %d", timestamped)
	}
}

// TestBackupKeepN_DoesNotRemoveSentinel verifies that even when keepN is
// small and there are many timestamped backups, the pristine sentinel is
// never pruned — it must survive arbitrary rotation.
func TestBackupKeepN_DoesNotRemoveSentinel(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"pristine":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	// Seed the sentinel via a first Backup call with pristine content, then
	// overwrite the live config so subsequent backups differ.
	if _, err := adapter.BackupKeep(1); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	sentinel := livePath + ".bak-mcp-local-hub-original"
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != `{"pristine":true}` {
		t.Fatalf("sentinel seeded wrong: data=%q err=%v", data, err)
	}

	// Churn the rolling window. keepN=1 is aggressive — at each call only one
	// timestamped backup should survive, plus the pristine sentinel.
	for i := 1; i <= 4; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.BackupKeep(1); err != nil {
			t.Fatalf("churn backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("sentinel was removed by pruning: %v", err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != `{"pristine":true}` {
		t.Errorf("sentinel content mutated: %s", data)
	}
}

// TestBackupKeepZero_DoesNotPrune verifies that BackupKeep(0) and
// Backup() are equivalent: they leave every existing timestamped backup
// in place. This preserves the pre-rotation contract for install.go
// and migrate.go, which still call Backup() without a keep cap.
func TestBackupKeepZero_DoesNotPrune(t *testing.T) {
	tmp := t.TempDir()
	livePath := filepath.Join(tmp, ".claude.json")
	if err := os.WriteFile(livePath, []byte(`{"v":0}`), 0600); err != nil {
		t.Fatal(err)
	}

	adapter := &jsonMCPClient{path: livePath, clientName: "claude-code", urlField: "url"}

	for i := 1; i <= 3; i++ {
		if err := os.WriteFile(livePath, []byte(fmt.Sprintf(`{"v":%d}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Backup(); err != nil {
			t.Fatalf("backup %d: %v", i, err)
		}
		time.Sleep(1100 * time.Millisecond)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var timestamped int
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".bak-mcp-local-hub-original") {
			continue
		}
		if strings.Contains(name, ".bak-mcp-local-hub-") {
			timestamped++
		}
	}
	if timestamped != 3 {
		t.Errorf("Backup() (keepN=0) must not prune; want 3 timestamped, got %d", timestamped)
	}
}
