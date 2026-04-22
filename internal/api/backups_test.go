package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBackupsListFiltersByKind verifies BackupsList classifies entries as
// "original" (sentinel) vs "timestamped".
func TestBackupsListFiltersByKind(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(live, []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-original", []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-20260417-150000", []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-20260417-160000", []byte("{}"), 0600)

	a := NewAPI()
	list, err := a.BackupsListIn(tmp, ".claude.json")
	if err != nil {
		t.Fatal(err)
	}
	var origs, ts int
	for _, b := range list {
		if b.Kind == "original" {
			origs++
		}
		if b.Kind == "timestamped" {
			ts++
		}
	}
	if origs != 1 {
		t.Errorf("expected 1 original, got %d", origs)
	}
	if ts != 2 {
		t.Errorf("expected 2 timestamped, got %d", ts)
	}
}

// TestBackupsCleanKeepsN prunes timestamped backups down to keepN, never
// touches the original sentinel.
func TestBackupsCleanKeepsN(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(live, []byte("{}"), 0600)
	_ = os.WriteFile(live+".bak-mcp-local-hub-original", []byte("orig"), 0600)

	// Create 6 timestamped backups with distinct mtimes.
	base := time.Now().Add(-10 * time.Hour)
	for i := 0; i < 6; i++ {
		p := live + ".bak-mcp-local-hub-" + base.Add(time.Duration(i)*time.Hour).Format("20060102-150405")
		_ = os.WriteFile(p, []byte("bak"), 0600)
		_ = os.Chtimes(p, base.Add(time.Duration(i)*time.Hour), base.Add(time.Duration(i)*time.Hour))
	}

	a := NewAPI()
	removed, err := a.BackupsCleanIn(tmp, ".claude.json", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 3 {
		t.Errorf("expected 3 removed (6 - keepN=3), got %d", len(removed))
	}

	// Original sentinel intact.
	if _, err := os.Stat(live + ".bak-mcp-local-hub-original"); err != nil {
		t.Error("sentinel removed")
	}
}

func TestBackupsCleanInRejectsNegativeKeepN(t *testing.T) {
	tmp := t.TempDir()
	live := filepath.Join(tmp, ".claude.json")
	_ = os.WriteFile(live, []byte("{}"), 0600)

	a := NewAPI()
	_, err := a.BackupsCleanIn(tmp, ".claude.json", -1)
	if err == nil {
		t.Fatal("expected error for negative keepN, got nil")
	}
}

func TestBackupsCleanPreviewRejectsNegativeKeepN(t *testing.T) {
	a := NewAPI()
	_, err := a.BackupsCleanPreview(-1)
	if err == nil {
		t.Fatal("expected error for negative keepN, got nil")
	}
}
