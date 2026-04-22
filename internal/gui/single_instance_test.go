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
// invariant that matters for the shutdown race: after Release, the
// flock is released so a racing second instance can immediately
// re-acquire. Per the round-9 redesign, the pidport file is
// intentionally NOT removed on Release (the flock is the source of
// truth, and the next acquirer's os.WriteFile overwrites the stale
// file atomically — see Release godoc).
func TestRelease_UnlocksBeforeRemovingPidport(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lock.Release()
	// Pidport file may linger after Release (intentional — flock is
	// the source of truth, see Release godoc). Verify the lock is
	// re-acquirable; the new acquirer's os.WriteFile overwrites the
	// stale file atomically.
	lock2, err := acquireSingleInstanceAt(pidport, 9101)
	if err != nil {
		t.Fatalf("re-acquire after Release should succeed: %v", err)
	}
	lock2.Release()
}

// TestRelease_LeavesPidportFileAlone pins the round-9 redesign of
// Release's pidport handling. Rounds 7 (unlock-first) and 8
// (ownership PID check) both left a TOCTOU window between reading
// the recorded PID and removing the file: a successor that acquired
// the flock and wrote its own pidport in that window could still
// have its file deleted. Round 9 closes the race by not touching
// the pidport file at all on Release — the flock is the source of
// truth for ownership, and the next acquirer overwrites the file
// atomically via os.WriteFile in acquireSingleInstanceAt.
//
// This test simulates an external rewrite (what a successor would
// do between our Unlock and any cleanup we might do) and asserts
// Release leaves whatever pidport is on disk alone.
func TestRelease_LeavesPidportFileAlone(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	lock, err := acquireSingleInstanceAt(pidport, 9100)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Externally rewrite pidport (simulates a successor write between
	// our Unlock and any cleanup we might do).
	rewritten := []byte(formatPidport(99999, 9999))
	if err := os.WriteFile(pidport, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	lock.Release()
	got, err := os.ReadFile(pidport)
	if err != nil {
		t.Fatalf("pidport should be untouched after Release: %v", err)
	}
	if string(got) != string(rewritten) {
		t.Errorf("Release modified pidport: got %q want %q", got, rewritten)
	}
}
