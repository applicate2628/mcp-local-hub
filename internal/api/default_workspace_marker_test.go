package api

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestClearDefaultWorkspaceIfMatchesReadsUnderStateFileFlock(t *testing.T) {
	stateDir := t.TempDir()
	oldDefault := filepath.Join(stateDir, "old")
	newDefault := filepath.Join(stateDir, "new")
	if err := WriteDefaultWorkspace(stateDir, oldDefault); err != nil {
		t.Fatalf("seed default workspace: %v", err)
	}

	path := filepath.Join(stateDir, DefaultWorkspaceFilename)
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatalf("lock default workspace marker: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- ClearDefaultWorkspaceIfMatches(stateDir, oldDefault)
	}()

	select {
	case err := <-done:
		_ = lock.Unlock()
		t.Fatalf("ClearDefaultWorkspaceIfMatches returned before the flock holder published the new default; err=%v", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := WriteStateFileBytesLockHeld(path, []byte(newDefault)); err != nil {
		_ = lock.Unlock()
		t.Fatalf("publish new default under held flock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock default workspace marker: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("ClearDefaultWorkspaceIfMatches after flock release: %v", err)
	}
	got, err := ReadDefaultWorkspace(stateDir)
	if err != nil {
		t.Fatalf("read default workspace: %v", err)
	}
	if got != newDefault {
		t.Fatalf("default workspace = %q, want concurrent writer value %q", got, newDefault)
	}
}
