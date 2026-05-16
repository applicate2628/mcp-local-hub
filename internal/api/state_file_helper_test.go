package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStateFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test-state.json")
	payload := map[string]string{"hello": "world"}

	if err := WriteStateFileAtomic(path, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["hello"] != "world" {
		t.Fatalf("payload mismatch: %v", got)
	}

	// Verify no temp file leftover.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "test-state.json" {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}
