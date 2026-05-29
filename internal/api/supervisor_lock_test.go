package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
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

// TestSupervisorRunningUnderStateDir verifies the §7.1 spec-bearing write gate's
// liveness probe (bot PR #246 r2). The live-self-PID assertion is the
// cross-platform check the gate depends on: on Windows it must exercise
// isOwnerLive's OpenProcess/GetExitCodeProcess path (NOT degrade to a
// Signal(0)-unsupported no-op that would make the gate dormant on the GA
// platform).
func TestSupervisorRunningUnderStateDir(t *testing.T) {
	dir := hardenedTempDir(t)
	lockPath := filepath.Join(dir, "supervisor.lock")

	// (1) No supervisor holds the lock → not running, no error.
	if running, pid, err := SupervisorRunningUnderStateDir(dir); running || pid != 0 || err != nil {
		t.Fatalf("no lock held: got (running=%v, pid=%d, err=%v), want (false, 0, nil)", running, pid, err)
	}

	// (2) A live "supervisor" holds the flock (+ wrote the sidecar) → running.
	// The probe must see the HELD flock (the authoritative cross-platform
	// signal) and report running. This is the Windows-correctness assertion: a
	// sidecar-only isOwnerLive probe would wrongly report not-running here
	// because Go's Windows Process.Signal(0) errors on a live PID.
	lk, err := AcquireSupervisorLock(lockPath)
	if err != nil {
		t.Fatalf("acquire supervisor lock: %v", err)
	}
	if running, pid, perr := SupervisorRunningUnderStateDir(dir); !running || pid != os.Getpid() || perr != nil {
		t.Fatalf("supervisor lock held: got (running=%v, pid=%d, err=%v), want (true, %d, nil)", running, pid, perr, os.Getpid())
	}
	lk.Release()

	// (3) After release the kernel frees the flock → not running (a crashed or
	// exited supervisor frees the lock; the NEXT start is a fresh process on the
	// current binary, which is the safe state for a spec-bearing write).
	if running, _, err := SupervisorRunningUnderStateDir(dir); running || err != nil {
		t.Fatalf("after release: got (running=%v, err=%v), want (false, nil)", running, err)
	}
}

// TestSupervisorRunningUnderStateDir_FailsClosedOnProbeError verifies the
// fail-closed contract (consultant PR #246 r2 #1): when the lock probe itself
// cannot run (here a state dir under a nonexistent parent chain, so the flock
// file cannot be opened/created), liveness is UNDETERMINABLE and the function
// returns a non-nil error — so a safety gate refuses rather than silently
// assuming not-running (which would disable the gate on hardened hosts).
func TestSupervisorRunningUnderStateDir_FailsClosedOnProbeError(t *testing.T) {
	bogus := filepath.Join(hardenedTempDir(t), "no-such-parent", "deeper", "state")
	running, _, err := SupervisorRunningUnderStateDir(bogus)
	if err == nil {
		t.Fatalf("probe against a nonexistent parent must return a non-nil error (undeterminable); got running=%v, err=nil", running)
	}
	if running {
		t.Fatalf("undeterminable probe must not report running=true; got running=true, err=%v", err)
	}
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

func TestSupervisorLock_RetriesMissingOwnerDuringReleaseWindow(t *testing.T) {
	dir := hardenedTempDir(t)
	path := filepath.Join(dir, "supervisor.lock")

	raw := flock.New(path + ".lock")
	got, err := raw.TryLock()
	if err != nil {
		t.Fatalf("raw TryLock: %v", err)
	}
	if !got {
		t.Fatal("raw TryLock did not acquire test lock")
	}
	defer raw.Unlock()

	prevWindow := supervisorLockOwnerMissingRetryWindow
	prevDelay := supervisorLockOwnerMissingRetryDelay
	supervisorLockOwnerMissingRetryWindow = 500 * time.Millisecond
	supervisorLockOwnerMissingRetryDelay = 10 * time.Millisecond
	t.Cleanup(func() {
		supervisorLockOwnerMissingRetryWindow = prevWindow
		supervisorLockOwnerMissingRetryDelay = prevDelay
	})

	released := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = raw.Unlock()
		close(released)
	}()

	lk, err := AcquireSupervisorLock(path)
	if err != nil {
		t.Fatalf("AcquireSupervisorLock should retry missing owner sidecar until flock releases: %v", err)
	}
	defer lk.Release()
	<-released
	if _, err := ReadSupervisorLockOwner(path); err != nil {
		t.Fatalf("owner sidecar was not written after retry acquire: %v", err)
	}
}

func writeStaleOwner(path string, pid int, started string) error {
	raw, _ := json.Marshal(SupervisorLockOwner{PID: pid, StartedAt: started})
	return os.WriteFile(path+".owner.json", raw, 0600)
}
