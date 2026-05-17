package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSupervisorLock_AcquireRelease(t *testing.T) {
	// v0.5.0 Fix Group 5: AcquireSupervisorLock writes the owner
	// sidecar via WriteStateFileAtomic, which now flows through
	// the hardened secure-write pipeline. See the matching note in
	// supervisor_intent_test.go for why hardenedTempDir is required.
	dir := hardenedTempDir(t)
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
	dir := hardenedTempDir(t)
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

func TestSupervisorLockOwnerSidecarNotDeletedOnContention(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor.lock")

	lk, err := AcquireSupervisorLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lk.Release()

	corrupt := []byte(`{"pid":`)
	if err := os.WriteFile(path+".owner.json", corrupt, 0600); err != nil {
		t.Fatalf("corrupt owner sidecar: %v", err)
	}

	second, err := AcquireSupervisorLock(path)
	if err == nil {
		second.Release()
		t.Fatal("second acquire unexpectedly succeeded while first lock is held")
	}
	raw, readErr := os.ReadFile(path + ".owner.json")
	if readErr != nil {
		t.Fatalf("owner sidecar was deleted while lock was still held: %v", readErr)
	}
	if string(raw) != string(corrupt) {
		t.Fatalf("owner sidecar changed under contention: got %q want %q", string(raw), string(corrupt))
	}
}

func writeStaleOwner(path string, pid int, started string) error {
	raw, _ := json.Marshal(SupervisorLockOwner{PID: pid, StartedAt: started})
	return os.WriteFile(path+".owner.json", raw, 0600)
}
