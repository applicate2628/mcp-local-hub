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
