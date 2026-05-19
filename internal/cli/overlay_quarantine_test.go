package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// listOverlayCorrupt returns all .corrupt-* siblings of the overlay file
// in stateDir, sorted oldest-first by mtime. Test helper.
func listOverlayCorrupt(t *testing.T, stateDir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	var out []os.DirEntry
	prefix := overlayBaseName + ".corrupt-"
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		ii, _ := out[i].Info()
		jj, _ := out[j].Info()
		return ii.ModTime().Before(jj.ModTime())
	})
	return out
}

// TestOverlayQuarantineRenamesFile seeds the overlay file and asserts
// that runOverlayQuarantine renames it aside under a .corrupt-* suffix
// and leaves no original.
func TestOverlayQuarantineRenamesFile(t *testing.T) {
	stateDir := t.TempDir()
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	if err := os.WriteFile(overlayPath, []byte("garbage: ["), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	var buf strings.Builder
	if err := runOverlayQuarantine(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayQuarantine: %v", err)
	}

	if _, err := os.Stat(overlayPath); !os.IsNotExist(err) {
		t.Fatalf("original overlay still present after quarantine: stat err=%v", err)
	}
	got := listOverlayCorrupt(t, stateDir)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 .corrupt-* file, got %d (%v)", len(got), got)
	}
	out := buf.String()
	if !strings.Contains(out, "renamed to ") {
		t.Fatalf("expected 'renamed to ' message in stdout, got: %q", out)
	}
	if !strings.Contains(out, "mcphub restart") {
		t.Fatalf("expected 'mcphub restart' guidance in stdout, got: %q", out)
	}
}

// TestOverlayQuarantineMissingIsNoop runs quarantine on an empty state
// dir and asserts exit 0 plus a "no overlay to quarantine" message.
func TestOverlayQuarantineMissingIsNoop(t *testing.T) {
	stateDir := t.TempDir()

	var buf strings.Builder
	if err := runOverlayQuarantine(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayQuarantine on empty dir: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "no overlay to quarantine") {
		t.Fatalf("expected 'no overlay to quarantine' message, got: %q", out)
	}
	// And the dir really is empty (no lock leakage, no corrupt-* artifact)
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, e := range entries {
		// A .lock file is acceptable (flock probe), but no .corrupt-* prefix.
		if strings.HasPrefix(e.Name(), overlayBaseName+".corrupt-") {
			t.Fatalf("unexpected .corrupt-* artifact created on no-op path: %s", e.Name())
		}
	}
}

// TestOverlayQuarantineFiveNewestRetention seeds 7 .corrupt-* siblings
// with sequential mtimes, then runs quarantine on a fresh overlay.
// Expectation: exactly 5 newest siblings survive after the run (the
// 2 oldest of the seeded 7 are deleted; the new .corrupt-* freshly
// produced by the run is among the surviving 5 because it has the
// newest mtime).
func TestOverlayQuarantineFiveNewestRetention(t *testing.T) {
	stateDir := t.TempDir()

	// Seed 7 .corrupt-* files with strictly-ascending mtimes far apart
	// so the sort order is deterministic. Use names that include the
	// index so we can identify which ones survive.
	base := time.Now().UTC().Add(-7 * time.Hour)
	seeded := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		name := fmt.Sprintf("%s.corrupt-seed-%d-%s",
			overlayBaseName, i, ts.Format("20060102T150405Z"))
		path := filepath.Join(stateDir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		// Force mtime: index i -> base + i hour
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
		seeded = append(seeded, name)
	}

	// Now seed a fresh overlay and run quarantine. Its rename produces
	// a new .corrupt-* with mtime ~= now, which beats every seeded one.
	overlayPath := filepath.Join(stateDir, overlayBaseName)
	if err := os.WriteFile(overlayPath, []byte("payload"), 0o600); err != nil {
		t.Fatalf("seed overlay: %v", err)
	}

	var buf strings.Builder
	if err := runOverlayQuarantine(stateDir, &buf); err != nil {
		t.Fatalf("runOverlayQuarantine: %v", err)
	}

	got := listOverlayCorrupt(t, stateDir)
	if len(got) != 5 {
		names := make([]string, 0, len(got))
		for _, e := range got {
			names = append(names, e.Name())
		}
		t.Fatalf("expected exactly 5 .corrupt-* survivors, got %d: %v", len(got), names)
	}

	// The two oldest seeded files (index 0 and 1) MUST be gone.
	for _, gone := range []string{seeded[0], seeded[1]} {
		if _, err := os.Stat(filepath.Join(stateDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("oldest %s should be deleted, stat err=%v", gone, err)
		}
	}
	// The five newer (4 seeded indices 2..6 + 1 freshly produced by run)
	// MUST all be present. We check the 4 seeded explicitly.
	for _, keep := range seeded[2:] {
		if _, err := os.Stat(filepath.Join(stateDir, keep)); err != nil {
			t.Fatalf("seeded %s should survive, stat err=%v", keep, err)
		}
	}
}
