package gui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireSingleInstance_FirstCallerSucceeds(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	got, err := os.ReadFile(pidport)
	if err != nil {
		t.Fatalf("read pidport: %v", err)
	}
	want := []byte(formatPidport(os.Getpid(), 9100))
	if string(got) != string(want) {
		t.Errorf("pidport content = %q, want %q", got, want)
	}
}

func TestAcquireSingleInstance_SecondCallerFails(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	first, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()
	_, err = acquireSingleInstanceAt(pidport, 9101)
	if err == nil {
		t.Fatal("second acquire should fail but succeeded")
	}
	if err != ErrSingleInstanceBusy {
		t.Errorf("err = %v, want ErrSingleInstanceBusy", err)
	}
}

func TestReadPidport_ParsesPidAndPort(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("12345 9100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, port, err := ReadPidport(pidport)
	if err != nil {
		t.Fatalf("ReadPidport: %v", err)
	}
	if pid != 12345 || port != 9100 {
		t.Errorf("got pid=%d port=%d, want 12345 9100", pid, port)
	}
}

// TestRelease_UnlocksBeforeRemovingPidport pins the post-Release
// invariants that matter for the shutdown race: after Release,
// (1) the pidport file is gone and (2) the flock is released so a
// racing second instance can immediately re-acquire. The unlock-first
// order is documented in Release() itself; this test is the behavioral
// regression guard — it would pass with the old order too, but
// together with the comment it pins the shutdown-race contract.
func TestRelease_UnlocksBeforeRemovingPidport(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lock.Release()
	if _, err := os.Stat(pidport); !os.IsNotExist(err) {
		t.Errorf("pidport file should be gone after Release; got err=%v", err)
	}
	// Re-acquire to confirm the flock is released too.
	lock2, err := acquireSingleInstanceAt(pidport, 9101)
	if err != nil {
		t.Fatalf("re-acquire after Release should succeed: %v", err)
	}
	lock2.Release()
}

// TestRelease_DoesNotClobberSuccessorPidport pins the round-8 fix for
// the successor-clobber race introduced in round 7. The round-7 change
// moved flock.Unlock() before os.Remove(pidport) to close the
// shutdown-race window where a second instance could see ErrBusy but
// fail to ReadPidport. That fix opened a new window: between Unlock
// and Remove, a racing successor can acquire the flock and rewrite
// pidport with its own PID; a blind Remove would then delete the
// successor's metadata file. The round-8 fix checks the recorded PID
// before removing. This test simulates the successor-write by
// overwriting pidport with a different PID after lock acquisition,
// then calling Release() and asserting the file survives unchanged.
func TestRelease_DoesNotClobberSuccessorPidport(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Simulate the lock + write phase (our process becomes incumbent).
	lock1, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("lock1: %v", err)
	}

	// Simulate the post-Unlock window: a racing successor acquires the
	// flock (we can't take it inside the test because lock1 still
	// holds it — Release's Unlock runs first in production, which is
	// what lets the successor in) and writes ITS pidport. Using a
	// fake PID that cannot equal os.Getpid() keeps the assertion
	// deterministic across platforms and test runners.
	successorPID := os.Getpid() + 1
	successorPidport := []byte(formatPidport(successorPID, 9101))
	if err := os.WriteFile(pidport, successorPidport, 0o600); err != nil {
		t.Fatal(err)
	}

	// Release must NOT remove the successor's pidport because the
	// recorded PID no longer names our process.
	lock1.Release()

	got, err := os.ReadFile(pidport)
	if err != nil {
		t.Fatalf("pidport should still exist (successor wrote it): %v", err)
	}
	if string(got) != string(successorPidport) {
		t.Errorf("pidport contents = %q, want successor's %q", got, successorPidport)
	}
}
