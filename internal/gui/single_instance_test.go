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
	"time"

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

// TestProbe_RecentPidport_StartupWindow_RetriesAndClassifiesHealthy
// pins the Codex PR #23 P2 #1 fix (iter-2 widened gate). The window
// between AcquireSingleInstanceAt (which writes pidport immediately)
// and Server.Start binding the requested port was kill-vulnerable:
// a second --force --kill --yes that landed in that sub-second
// window saw alive PID + ping fail and classified the holder as
// VerdictLiveUnreachable. Probe now retries when (LiveUnreachable
// AND PIDAlive AND mtime within probeStartupWindow), re-reading the
// pidport on each iteration so a holder finishing its bind during
// the retry window flips the classification to Healthy.
//
// Setup mirrors the production race: pidport starts with our PID +
// port=0, then a goroutine rewrites it to {ourPid, healthyPort} after
// a delay shorter than the 500ms retry budget. Asserts the final
// verdict is VerdictHealthy and that PingMatch is true.
//
// Was named TestProbe_Port0StartupWindow_… in iter-1; the iter-2
// fix widens the gate from `port==0` to `recent mtime`, so the
// `Port0` framing no longer accurately describes the contract.
func TestProbe_RecentPidport_StartupWindow_RetriesAndClassifiesHealthy(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Healthy ping server (mirrors the incumbent post-bind state).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	healthyPort := portFromURL(t, srv.URL)

	// Initial pidport: alive PID (our own) + port=0 (pre-bind).
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), 0)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate the holder finishing its bind: rewrite pidport to
	// {ourPid, healthyPort} after 150ms — well inside the retry
	// budget (5 × 100ms = 500ms) but after the first probe
	// classifies as LiveUnreachable+port=0.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), healthyPort)), 0o600)
	}()

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy (retry should catch the port rewrite). Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch = false, want true after retry caught the rewritten port")
	}
	if v.Port != healthyPort {
		t.Errorf("Port = %d, want %d (the rewritten healthy port)", v.Port, healthyPort)
	}
}

// TestProbe_RecentPidport_StartupWindow_GivesUpAfterDeadline pins
// the upper bound on the retry: if pidport mtime stays recent and
// ping keeps failing for the entire retry budget (the holder
// genuinely never finishes binding within ~500ms), Probe must
// classify as VerdictLiveUnreachable rather than spinning forever.
// The test also pins the rough deadline (500ms upper bound on the
// retry portion, plus per-attempt pingTimeout) so a future
// regression that doubled the retries would surface as a slow test.
//
// Was named TestProbe_Port0StartupWindow_… in iter-1; the iter-2
// fix widens the gate from `port==0` to `recent mtime`, so the
// `Port0` framing no longer accurately describes the contract.
func TestProbe_RecentPidport_StartupWindow_GivesUpAfterDeadline(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// pidport stays {ourPid, 0} for the entire test — no rewrite.
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), 0)), 0o600); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	v := Probe(context.Background(), pidport)
	elapsed := time.Since(start)

	if v.Class != VerdictLiveUnreachable {
		t.Errorf("Class = %v, want VerdictLiveUnreachable after retry budget expired. Diagnose=%q", v.Class, v.Diagnose)
	}
	if v.Port != 0 {
		t.Errorf("Port = %d, want 0 (port stayed 0 throughout)", v.Port)
	}
	// Upper bound: 5 retries × 100ms backoff + per-attempt
	// pingTimeout (500ms × 6 attempts = 3s in the worst case where
	// each ping waits its full timeout). Connection-refused on
	// 127.0.0.1:0 fails fast so the real wall time is dominated by
	// the backoff sleep (~500ms). 5s is generous enough for slow CI
	// while still catching a doubled-budget regression.
	if elapsed > 5*time.Second {
		t.Errorf("Probe took %v, want <5s — retry budget is unbounded?", elapsed)
	}
}

// TestProbe_LiveUnreachable_OldMtimeDoesNotRetry pins the boundary
// condition for the iter-2 retry gate: when the pidport mtime is
// older than probeStartupWindow, the holder is genuinely stuck
// (its startup race window has long since passed) and the retry
// MUST NOT fire. A regression that retried in this case would slow
// down every diagnostic of a real stuck-incumbent by ~500ms.
//
// Replaces TestProbe_LiveUnreachable_NonZeroPortDoesNotRetry, which
// locked in the iter-1 (port==0) gate semantic that was wrong: a
// real stuck incumbent could record port==N while still failing
// ping, and the iter-1 gate gave it the same fast-path classification
// as the startup-race case. The iter-2 gate uses pidport mtime
// (set by AcquireSingleInstanceAt) to distinguish startup-race from
// stuck-incumbent: recent mtime → race-in-progress, retry; old
// mtime → real stuck, no retry.
func TestProbe_LiveUnreachable_OldMtimeDoesNotRetry(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate pidport mtime to 1 hour ago so it falls outside the
	// startup window (5s). A real stuck incumbent's pidport mtime
	// is whatever time it wrote pidport at — typically minutes/hours
	// ago by the time the operator notices and runs --force.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	start := time.Now()
	v := Probe(context.Background(), pidport)
	elapsed := time.Since(start)

	if v.Class != VerdictLiveUnreachable {
		t.Errorf("Class = %v, want VerdictLiveUnreachable. Diagnose=%q", v.Class, v.Diagnose)
	}
	// Without retry the only wait is the ping timeout (500ms). With
	// the retry incorrectly firing on old-mtime entries, total wall
	// time would climb to ~3s (5 × 500ms ping timeout + 5 × 100ms
	// backoff). Threshold of 1.5s catches the regression while
	// leaving slack for slow CI.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("Probe took %v on old-mtime stuck incumbent — retry incorrectly fired (should only retry when mtime is recent)", elapsed)
	}
}

// TestProbe_LiveUnreachable_RecentPidport_PortN_RetriesAndClassifiesHealthy
// pins the iter-2 fix for the --port=N startup race. AcquireSingleInstanceAt
// writes pidport with {pid, N} immediately, but Server.Start may not
// be listening yet when a concurrent --force --kill --yes lands. The
// iter-1 gate (port==0) missed this entirely; the iter-2 gate (recent
// mtime) covers it. Without this fix, a healthy starting GUI invoked
// with --port=N would be killed by a racing kill command.
//
// Setup: write pidport with {ourPid, 8080} and current mtime — note
// 8080 is closed (ping will fail), so probeOnce returns
// LiveUnreachable initially. A goroutine rewrites pidport to
// {ourPid, healthyPort} after 150ms, where healthyPort hosts a
// matching ping server. The retry loop must observe the rewrite and
// flip to VerdictHealthy.
func TestProbe_LiveUnreachable_RecentPidport_PortN_RetriesAndClassifiesHealthy(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Healthy ping server on the eventual port (mirrors the post-bind
	// state of the starting GUI).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "pid": os.Getpid()})
	}))
	defer srv.Close()
	healthyPort := portFromURL(t, srv.URL)

	// Initial pidport: alive PID + an unrelated port (8080) that has
	// no listener — first probe will classify as LiveUnreachable.
	// Mtime is current (just written), so the iter-2 retry gate
	// fires.
	const racePort = 8080
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), racePort)), 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate the holder finishing its bind: rewrite pidport to
	// {ourPid, healthyPort} after 150ms — well inside the retry
	// budget but after the first probe classifies as
	// LiveUnreachable.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), healthyPort)), 0o600)
	}()

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictHealthy {
		t.Errorf("Class = %v, want VerdictHealthy (retry should catch the port=N rewrite). Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch = false, want true after retry caught the rewritten port")
	}
	if v.Port != healthyPort {
		t.Errorf("Port = %d, want %d (the rewritten healthy port)", v.Port, healthyPort)
	}
}

// TestWritePidport_WritesBothPidAndPort pins the Codex PR #23 P2 #2
// helper: WritePidport must overwrite the file with the supplied PID
// AND port together, in the canonical "<pid> <port>\n" format. This
// guards against a future regression that kept only one field
// updated (the original RewritePidportPort bug).
func TestWritePidport_WritesBothPidAndPort(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Pre-write something different to make sure WritePidport
	// actually overwrites it instead of appending.
	if err := os.WriteFile(pidport, []byte("99999 22222\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WritePidport(pidport, 12345, 9100); err != nil {
		t.Fatalf("WritePidport: %v", err)
	}
	got, err := os.ReadFile(pidport)
	if err != nil {
		t.Fatalf("read pidport: %v", err)
	}
	want := []byte("12345 9100\n")
	if string(got) != string(want) {
		t.Errorf("pidport content = %q, want %q", got, want)
	}
	// Also verify ReadPidport parses it back cleanly.
	pid, port, perr := ReadPidport(pidport)
	if perr != nil {
		t.Fatalf("ReadPidport: %v", perr)
	}
	if pid != 12345 || port != 9100 {
		t.Errorf("ReadPidport = pid=%d port=%d, want 12345 9100", pid, port)
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
