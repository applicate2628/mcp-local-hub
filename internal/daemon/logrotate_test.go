package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotateIfLarge_RotatesWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	big := make([]byte, 11*1024*1024) // 11 MB
	os.WriteFile(logPath, big, 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatalf("RotateIfLarge: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	var rotated int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test.log.") {
			rotated++
		}
	}
	if rotated != 1 {
		t.Errorf("expected 1 rotated file, got %d", rotated)
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("original log should be removed after rotation")
	}
}

func TestRotateIfLarge_SkipsSmall(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	os.WriteFile(logPath, []byte("small"), 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatalf("RotateIfLarge: %v", err)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Error("small log should not be rotated")
	}
}

func TestRotateIfLarge_PrunesOldestWhenOverCount(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	// Create 6 pre-existing rotated files
	for i := 0; i < 6; i++ {
		p := filepath.Join(dir, "test.log.2026010"+string(rune('0'+i))+"-000000")
		os.WriteFile(p, []byte("x"), 0600)
	}
	big := make([]byte, 11*1024*1024)
	os.WriteFile(logPath, big, 0600)

	if err := RotateIfLarge(logPath, 10*1024*1024, 5); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var rotated int
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test.log.") {
			rotated++
		}
	}
	if rotated != 5 {
		t.Errorf("expected 5 rotated files after prune, got %d", rotated)
	}
}

func TestRotateIfLarge_NegativeKeepCountDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")
	big := make([]byte, 11*1024*1024)
	if err := os.WriteFile(logPath, big, 0600); err != nil {
		t.Fatal(err)
	}

	if err := RotateIfLarge(logPath, 10*1024*1024, -1); err != nil {
		t.Fatalf("RotateIfLarge: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "test.log.") {
			t.Fatalf("expected no rotated files when keepCount is negative, found %s", e.Name())
		}
	}
}
