package gui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
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

// TestProbe_Healthy verifies a Probe call against a live incumbent
// (here: an httptest server bound to a real port) returns
// VerdictHealthy with PingMatch=true.
func TestProbe_Healthy(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Spin up a fake /api/ping that reports our own PID.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch = false, want true")
	}
	if v.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", v.PID, os.Getpid())
	}
}

// TestProbe_LiveUnreachable: alive PID, no listener.
func TestProbe_LiveUnreachable(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictLiveUnreachable {
		t.Errorf("Class = %v, want VerdictLiveUnreachable. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestProbe_DeadPID: pid is impossible.
func TestProbe_DeadPID(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const impossible = 2147483646
	if err := os.WriteFile(pidport, []byte(formatPidport(impossible, 9125)), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictDeadPID {
		t.Errorf("Class = %v, want VerdictDeadPID. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestProbe_Malformed: garbage in pidport.
func TestProbe_Malformed(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	if err := os.WriteFile(pidport, []byte("not a pidport file"), 0o600); err != nil {
		t.Fatal(err)
	}
	v := Probe(context.Background(), pidport)
	if v.Class != VerdictMalformed {
		t.Errorf("Class = %v, want VerdictMalformed. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_RefusesNonMcphubImage: pidport refers to
// the test process (image is the test binary, NOT mcphub.exe), so
// the three-part identity gate's image-basename check refuses.
// Asserts Class=VerdictKillRefused, no kill attempted.
func TestKillRecordedHolder_RefusesNonMcphubImage(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const port = 9125
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Acquire a flock so KillRecordedHolder reaches the gate path.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err == nil {
		t.Errorf("expected non-nil error on kill-refused; got nil")
	}
	if v.Class != VerdictKillRefused {
		t.Errorf("Class = %v, want VerdictKillRefused. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_HealthyEarlyExit: incumbent is healthy
// (ping matches) — KillRecordedHolder must NOT kill, must report
// VerdictHealthy so the caller can route to handshake.
func TestKillRecordedHolder_HealthyEarlyExit(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	port := portFromURL(t, srv.URL)
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err != nil {
		t.Errorf("expected nil error on healthy early-exit; got %v", err)
	}
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch should be true on healthy")
	}
}

// TestVerdictDiagnoseHintNotInJSON guards the json:"-" tags so
// A4-b's HTTP API doesn't ship pre-formatted strings to the UI.
func TestVerdictDiagnoseHintNotInJSON(t *testing.T) {
	v := Verdict{
		Class:    VerdictDeadPID,
		PID:      123,
		Port:     9125,
		Diagnose: "should not appear in JSON",
		Hint:     "should not appear either",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "should not appear") {
		t.Errorf("Diagnose/Hint leaked into JSON: %s", b)
	}
}

// portFromURL is shared with probe_test.go; kept package-private.
// (Defined in probe_test.go; redefining would conflict.)
