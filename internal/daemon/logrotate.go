package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RotateIfLarge moves logPath to "<logPath>.<timestamp>" if its size exceeds maxSize.
// After rotation, prunes oldest rotated siblings so no more than keepCount remain.
// Returns nil if the file doesn't exist or is smaller than maxSize.
func RotateIfLarge(logPath string, maxSize int64, keepCount int) error {
	info, err := os.Stat(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Size() < maxSize {
		return nil
	}
	ts := time.Now().Format("20060102-150405")
	rotated := logPath + "." + ts
	if err := os.Rename(logPath, rotated); err != nil {
		return fmt.Errorf("rotate: %w", err)
	}
	return pruneOldRotations(logPath, keepCount)
}

// pruneOldRotations deletes rotated siblings beyond keepCount, keeping the newest.
// Rotation files are identified by the prefix `filepath.Base(logPath)+"."`.
func pruneOldRotations(logPath string, keepCount int) error {
	if keepCount < 0 {
		keepCount = 0
	}
	dir := filepath.Dir(logPath)
	base := filepath.Base(logPath) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type rotatedFile struct {
		path string
		mod  time.Time
	}
	var rotations []rotatedFile
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		rotations = append(rotations, rotatedFile{path: filepath.Join(dir, e.Name()), mod: info.ModTime()})
	}
	if len(rotations) <= keepCount {
		return nil
	}
	sort.Slice(rotations, func(i, j int) bool {
		return rotations[i].mod.After(rotations[j].mod) // newest first
	})
	for _, r := range rotations[keepCount:] {
		_ = os.Remove(r.path)
	}
	return nil
}
