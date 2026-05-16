package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisorLock_AcquireRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.lock")
	lk, err := AcquireSupervisorLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Pidport sidecar should exist with current PID.
	owner, err := ReadSupervisorLockOwner(path)
	if err != nil {
		t.Fatalf("read owner: %v", err)
	}
	if owner.PID != os.Getpid() {
		t.Fatalf("owner.PID = %d, want %d", owner.PID, os.Getpid())
	}
	lk.Release()
}

func TestSupervisorLock_StaleReclaim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.lock")

	// Plant stale owner sidecar with bogus PID.
	if err := writeStaleOwner(path, 999999, "2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}

	// Acquire should succeed (stale PID detected).
	lk, err := AcquireSupervisorLock(path)
	if err != nil {
		t.Fatalf("expected reclaim of stale lock, got: %v", err)
	}
	lk.Release()
}

func writeStaleOwner(path string, pid int, started string) error {
	raw, _ := json.Marshal(SupervisorLockOwner{PID: pid, StartedAt: started})
	return os.WriteFile(path+".owner.json", raw, 0600)
}
