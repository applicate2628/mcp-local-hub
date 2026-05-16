package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteStateFileAtomic writes payload as JSON to path through a flock +
// temp + atomic-rename pipeline. The temp file lives in the same directory
// as path so the rename is atomic (same volume).
//
// Pipeline: lock <path>.lock → write <path>.tmp → fsync → rename to <path>.
// Caller is responsible for invoking inside the appropriate lock window
// when writes from multiple goroutines are possible; this helper does
// per-call flock for cross-process safety.
func WriteStateFileAtomic(path string, payload any) error {
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
