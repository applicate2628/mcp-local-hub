package gui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
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

// TestKillRecordedHolder_LongArgv0_DoesNotPassGate pins Codex iter-3
// P2 #1. Pre-fix code truncated argv to 1024 bytes BEFORE running
// cmdlineIsGui, so a process whose argv[0] (binary path) exceeded
// 1KB had argv[1] dropped from the truncation buffer; cmdlineIsGui
// then read len(argv)==1 and returned true (the Explorer no-arg
// auto-gui branch). A `--force --kill --yes` against a non-GUI
// mcphub subcommand whose launch path was long enough could pass
// the argv gate even though argv[1] != "gui".
//
// Post-fix: the gate reads Verdict.pidCmdlineRaw (untruncated),
// observes argv[1] == "daemon", and refuses with
// "argv subcommand is not 'gui'". Verdict.PIDCmdline (the public
// JSON/display field) remains truncated.
//
// The test injects ProcessIdentity via processIDOverride. Image
// path is set to the platform-correct mcphub basename so the FIRST
// gate (matchBasename) passes — the bug being tested is in the
// SECOND (argv) gate.
func TestKillRecordedHolder_LongArgv0_DoesNotPassGate(t *testing.T) {
	// Build the platform-appropriate "mcphub" image path so the
	// matchBasename gate passes. Linux/darwin use "mcphub";
	// Windows uses "mcphub.exe".
	mcphubBinary := mcphubBinaryNameForTest()

	// Construct argv whose argv[0] alone exceeds the 1KB
	// truncation budget — pre-fix truncation would drop argv[1].
	const oversize = 1500
	longArgv0 := strings.Repeat("a", oversize)
	rawArgv := []string{longArgv0, "daemon"}

	prev := ProcessIDForTest()
	defer RestoreProcessID(prev)
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{
			Alive:     true,
			Denied:    false,
			ImagePath: mcphubBinary,
			Cmdline:   rawArgv,
			// StartTime well before pidport mtime so the
			// start-time gate would also pass (we're isolating
			// the argv-gate bug).
			StartTime: time.Now().Add(-1 * time.Hour),
		}, nil
	})

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate pidport mtime so probe doesn't retry.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Pre-acquire flock so KillRecordedHolder reaches the gate path.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err == nil {
		t.Fatalf("expected non-nil error on kill-refused; got nil")
	}
	if v.Class != VerdictKillRefused {
		t.Fatalf("Class = %v, want VerdictKillRefused. Diagnose=%q", v.Class, v.Diagnose)
	}
	// Diagnose must mention the argv gate, NOT image basename or
	// start-time, to prove the truncation bug is what we caught.
	// Codex iter-10 P2 #2 redacted the diagnostic to print only the
	// offending subcommand token (here "daemon"), not the full argv,
	// to avoid leaking secrets from `mcphub secrets set --value …`.
	if !strings.Contains(v.Diagnose, `argv subcommand is "daemon", not 'gui'`) {
		t.Errorf("Diagnose = %q; want argv-gate message proving the gate read raw argv (post-iter-10 redacted format)", v.Diagnose)
	}
	// PIDCmdline (public/display field) should be truncated to ~1KB
	// for safe display; pidCmdlineRaw on the verdict should still
	// hold both elements. This pins the truncation move.
	if len(v.pidCmdlineRaw) != 2 {
		t.Errorf("pidCmdlineRaw len = %d, want 2 (raw argv preserved for the gate)", len(v.pidCmdlineRaw))
	}
	if len(v.pidCmdlineRaw) >= 2 && v.pidCmdlineRaw[1] != "daemon" {
		t.Errorf("pidCmdlineRaw[1] = %q, want %q", v.pidCmdlineRaw[1], "daemon")
	}
	// PIDCmdline truncated: the long argv[0] is clipped at 1024
	// bytes and there is no room for argv[1] in the bounded buffer.
	totalDisplayBytes := 0
	for _, a := range v.PIDCmdline {
		totalDisplayBytes += len(a)
	}
	if totalDisplayBytes > 1024 {
		t.Errorf("PIDCmdline total bytes = %d, want <= 1024 (display truncation)", totalDisplayBytes)
	}
}

// TestProbe_MacOSUnsupported_HealthyPing_StillClassifiesHealthy pins
// Codex iter-3 P2 #2: on macOS (or any platform where processIDImpl
// returns errMacOSProbeUnsupported) a healthy incumbent must still
// classify as VerdictHealthy if /api/ping matches the recorded PID.
// Pre-fix code returned VerdictMalformed before reaching the ping,
// breaking bare `mcphub gui --force` activate-window on macOS.
//
// The test uses processIDOverride to simulate the darwin sentinel
// on every platform so the regression coverage is portable.
func TestProbe_MacOSUnsupported_HealthyPing_StillClassifiesHealthy(t *testing.T) {
	prev := ProcessIDForTest()
	defer RestoreProcessID(prev)
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, ErrMacOSProbeUnsupportedForTest()
	})

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Healthy ping server reporting our own PID.
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
	// Backdate so retry doesn't fire (Healthy short-circuits anyway,
	// but explicit is safer).
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictHealthy {
		t.Fatalf("Class = %v, want VerdictHealthy. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.PingMatch {
		t.Errorf("PingMatch = false, want true (ping-only Healthy verdict)")
	}
	if v.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", v.PID, os.Getpid())
	}
}

// TestProbe_MacOSUnsupported_NoPing_ClassifiesLiveUnreachable pins
// Codex iter-3 P2 #2 part 2: when ping fails AND processIDImpl
// returns errMacOSProbeUnsupported, the verdict must be
// VerdictLiveUnreachable (with macOSUnsupported flagged), NOT
// VerdictMalformed. Pre-fix code returned Malformed in this case,
// which made the bare `--force` diagnostic block emit "Stuck-instance
// kill recovery is platform-specific..." but skipped the LiveUnreachable
// path that KillRecordedHolder needs to surface a clear macOS-specific
// KillRefused.
func TestProbe_MacOSUnsupported_NoPing_ClassifiesLiveUnreachable(t *testing.T) {
	prev := ProcessIDForTest()
	defer RestoreProcessID(prev)
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		return ProcessIdentity{}, ErrMacOSProbeUnsupportedForTest()
	})

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Port 1 has no listener — ping will fail.
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate so the retry loop doesn't fire (LiveUnreachable on a
	// fresh pidport would otherwise burn the retry budget).
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	v := Probe(context.Background(), pidport)
	if v.Class != VerdictLiveUnreachable {
		t.Fatalf("Class = %v, want VerdictLiveUnreachable (NOT Malformed). Diagnose=%q", v.Class, v.Diagnose)
	}
	if !v.macOSUnsupported {
		t.Errorf("macOSUnsupported = false, want true (probe should flag the platform-unsupported case)")
	}
	if !strings.Contains(v.Hint, "macOS") {
		t.Errorf("Hint = %q; want macOS-specific message", v.Hint)
	}

	// Bonus: KillRecordedHolder against this verdict must refuse
	// with the macOS-specific diagnose, NOT cascade through the
	// image gate.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	_, killV, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err == nil {
		t.Errorf("KillRecordedHolder on macOS-unsupported verdict returned nil error; expected refused")
	}
	if killV.Class != VerdictKillRefused {
		t.Errorf("Class = %v, want VerdictKillRefused on macOS shortcut", killV.Class)
	}
	if !strings.Contains(killV.Diagnose, "macOS") {
		t.Errorf("Diagnose = %q; want macOS-specific message (not the image-gate cascade)", killV.Diagnose)
	}
}

// TestKillRecordedHolder_PidportChangedMidPrompt_ReturnsRaceLost pins
// the Codex iter-5 P1 TOCTOU fix. Before the fix, the cli would
// Probe the pidport, print PID X to the user, prompt for confirmation,
// then call KillRecordedHolder which Probes a SECOND time. A
// competitor that rewrote the pidport between the two probes could
// flip the recorded PID; the gate would then admit the new PID and
// SIGKILL/TerminateProcess would fire on a different process than the
// user confirmed.
//
// Post-fix: KillOpts.Expected carries the (PID, Port, Mtime) the
// caller already showed to the user. The internal re-probe asserts
// the recorded fields are unchanged; otherwise it returns
// VerdictRaceLost without sending any signal.
//
// This test deterministically simulates the race by populating the
// pidport with one identity and calling KillRecordedHolder with an
// Expected tuple that disagrees on PID. No real second-prober is
// needed — the production code path is the same:
// 1. probe reads the current pidport,
// 2. the Expected guard compares against the caller-supplied tuple,
// 3. mismatch → VerdictRaceLost.
//
// The full race (with a goroutine racing the probe) cannot be made
// deterministic without an additional probe-level seam, and the
// boundary semantics being tested here are identical: when probe's
// observation differs from Expected, the function refuses with
// VerdictRaceLost. (Codex iter-5 P1.)
func TestKillRecordedHolder_PidportChangedMidPrompt_ReturnsRaceLost(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// The probe must classify as VerdictLiveUnreachable for the
	// Expected guard to fire — Malformed/DeadPID/Healthy short-
	// circuit before the guard. To reach LiveUnreachable we need:
	//   - alive PID (use os.Getpid() — always alive)
	//   - failing ping (use port 1 — closed on test runners)
	//
	// The pidport thus records the "after competitor rewrote it"
	// state with a real PID we can probe.
	observedPID := os.Getpid()
	const observedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(observedPID, observedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(pidport)
	if err != nil {
		t.Fatal(err)
	}
	observedMtime := st.ModTime()

	// Pre-acquire the flock so the verdict reaches LiveUnreachable
	// rather than a flock-clear path.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Expected disagrees on PID: simulates the cli's first Probe
	// having captured a different PID before the competitor rewrote
	// the file. os.Getppid() is also alive (the test runner) but
	// distinct from os.Getpid().
	confirmedPID := os.Getppid()
	if confirmedPID == observedPID {
		t.Skip("os.Getpid() == os.Getppid(); cannot distinguish confirmed from observed identity")
	}
	confirmed := ExpectedIdentity{
		PID:   confirmedPID,
		Port:  observedPort,
		Mtime: observedMtime,
	}

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{
		Expected: confirmed,
	})
	if err == nil {
		t.Fatalf("expected error from Expected mismatch; got nil (verdict=%+v)", v)
	}
	if v.Class != VerdictRaceLost {
		t.Errorf("Class = %v, want VerdictRaceLost. Diagnose=%q", v.Class, v.Diagnose)
	}
	if !strings.Contains(v.Diagnose, "pidport changed between user confirmation and kill") {
		t.Errorf("Diagnose = %q; want 'pidport changed between user confirmation and kill'", v.Diagnose)
	}
	// Confirmed PID and observed PID must both appear in the
	// diagnose so an operator can see what changed.
	confirmedStr := fmt.Sprintf("%d", confirmedPID)
	observedStr := fmt.Sprintf("%d", observedPID)
	if !strings.Contains(v.Diagnose, confirmedStr) || !strings.Contains(v.Diagnose, observedStr) {
		t.Errorf("Diagnose = %q; want both confirmed PID %d and observed PID %d", v.Diagnose, confirmedPID, observedPID)
	}
}

// TestKillRecordedHolder_PidportPortChanged_ReturnsRaceLost covers
// the Port axis of the Expected guard: an attacker that kept the
// recorded PID stable but flipped the recorded port (e.g. to redirect
// a follow-up probe at a different listener) must also be rejected
// with VerdictRaceLost. (Codex iter-5 P1.)
func TestKillRecordedHolder_PidportPortChanged_ReturnsRaceLost(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	pid := os.Getpid()
	const observedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(pid, observedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(pidport)
	if err != nil {
		t.Fatal(err)
	}
	observedMtime := st.ModTime()

	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Expected disagrees on Port.
	const confirmedPort = 1234
	confirmed := ExpectedIdentity{
		PID:   pid,
		Port:  confirmedPort,
		Mtime: observedMtime,
	}

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{
		Expected: confirmed,
	})
	if err == nil {
		t.Fatalf("expected error from Expected mismatch; got nil (verdict=%+v)", v)
	}
	if v.Class != VerdictRaceLost {
		t.Errorf("Class = %v, want VerdictRaceLost. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_PidportMtimeChanged_ReturnsRaceLost covers
// the Mtime axis of the Expected guard. (Codex iter-5 P1.)
func TestKillRecordedHolder_PidportMtimeChanged_ReturnsRaceLost(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	pid := os.Getpid()
	const port = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(pid, port)), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(pidport)
	if err != nil {
		t.Fatal(err)
	}

	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Expected disagrees on Mtime — even by 1 nanosecond.
	confirmed := ExpectedIdentity{
		PID:   pid,
		Port:  port,
		Mtime: st.ModTime().Add(1 * time.Nanosecond),
	}

	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{
		Expected: confirmed,
	})
	if err == nil {
		t.Fatalf("expected error from Expected mismatch; got nil (verdict=%+v)", v)
	}
	if v.Class != VerdictRaceLost {
		t.Errorf("Class = %v, want VerdictRaceLost. Diagnose=%q", v.Class, v.Diagnose)
	}
}

// TestKillRecordedHolder_ExpectedZeroValueDisablesCheck verifies
// back-compat: callers that don't populate KillOpts.Expected (e.g.
// older tests) still see the original behavior — the Expected guard
// is gated on Expected.PID != 0. (Codex iter-5 P1.)
func TestKillRecordedHolder_ExpectedZeroValueDisablesCheck(t *testing.T) {
	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")

	// Use a non-mcphub image so the production identity gate refuses
	// — that's the verdict we expect when the Expected guard is
	// disabled (rather than VerdictRaceLost). os.Getppid() is the
	// shell or test runner, which fails matchBasename.
	if err := os.WriteFile(pidport, []byte(fmt.Sprintf("%d 1\n", os.Getppid())), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Empty Expected (zero PID) disables the guard — KillRecordedHolder
	// should behave exactly as before iter-5: identity gate runs and
	// refuses on the non-mcphub image.
	_, v, err := KillRecordedHolder(context.Background(), pidport, KillOpts{})
	if err == nil {
		t.Fatalf("expected refusal verdict; got nil error (verdict=%+v)", v)
	}
	// Specifically must NOT be VerdictRaceLost — that would mean the
	// guard fired despite Expected being zero-valued.
	if v.Class == VerdictRaceLost {
		t.Errorf("Class = VerdictRaceLost; want non-race refusal (Expected zero-value should disable the TOCTOU guard). Diagnose=%q", v.Diagnose)
	}
}

// TestKillRecordedHolder_WaitsForKillBeforeAcquirePoll pins the
// Codex iter-9 P2 #2 fix (memo §"Take-over protocol" step 5f).
// Before the fix, KillRecordedHolder went straight from killProcess
// to the acquire-poll loop. On Windows TerminateProcess is async
// and on Unix the kernel needs time to reap zombies / release the
// flock; without an explicit wait the 2-second AcquireDeadline
// could elapse before the kernel released the flock, producing a
// spurious VerdictRaceLost on a successful kill.
//
// Setup: the processID override returns Alive=true on the first
// few calls and Alive=false thereafter. KillRecordedHolder must:
//  1. probe (call 1, Alive=true → reaches LiveUnreachable)
//  2. killProcess (no processID call — but in this test we have
//     no real PID; killProcess on os.Getppid is real and would
//     either succeed or fail. We avoid that by using a fake PID
//     ONLY safe inside this seam-driven test: the gate is
//     overridden, so identity validation is bypassed; killProcess
//     itself is the only real interaction. We use a guaranteed-
//     dead PID that killProcess will return an error on; the
//     test asserts that BEFORE the wait loop runs the call site
//     never observed killSucceeded.) Actually a cleaner approach:
//     use os.Getpid() — alive but the kill seam below replaces
//     killProcess.
//
// The test instead overrides processID to count invocations and
// returns alive=true the first 2 times, alive=false afterward.
// killProcess targets os.Getpid() — but we DO NOT actually want
// to kill the test runner. Solution: use the IdentityGateForTest
// seam to pass the gate, the postKillHook to record state, and
// trust that os.Getpid() is alive enough that killProcess returns
// nil; the override on processID makes the wait-loop scoreboard
// independent of whether kernel telemetry agrees. This is a unit
// test of the wait loop's control flow, not an end-to-end kill.
//
// Key invariants:
//   - When postKillHook fires, processIDOverride must have been
//     called at least 2 times (probe + at least one wait-loop
//     iteration). One call would mean the wait loop never ran.
//   - The wait loop must observe at least one alive=true sample
//     before exiting (proven by the controlled false-after-N seq).
//   - postKillHook fires exactly once.
//
// We don't actually run killProcess in this test: that would risk
// killing the test runner. Instead we rely on a kill helper override
// to no-op the kill — see killProcessOverride below.
func TestKillRecordedHolder_WaitsForKillBeforeAcquirePoll(t *testing.T) {
	// Bypass the three-part identity gate so we reach the kill +
	// wait-for-exit path even though our injected ProcessIdentity
	// is synthetic.
	prevGate := IdentityGateForTest()
	defer RestoreIdentityGate(prevGate)
	SetIdentityGate(func(v Verdict) (refused bool, reason string) { return false, "" })

	// Track processID call counts and whether the wait loop saw
	// alive=true at least once. The first call (from probeOnce)
	// returns alive=true because the verdict must reach
	// LiveUnreachable. The second call (wait-loop iteration 1) also
	// returns alive=true so the loop iterates at least once.
	// Subsequent calls return alive=false so the wait loop exits
	// promptly and the test stays bounded.
	var (
		processIDCallCount atomic.Int32
		sawAliveInWait     atomic.Bool
	)
	prevProcessID := ProcessIDForTest()
	defer RestoreProcessID(prevProcessID)
	SetProcessIDOverride(func(pid int) (ProcessIdentity, error) {
		n := processIDCallCount.Add(1)
		// Calls 1 (probeOnce) and 2 (first wait-loop iteration):
		// alive=true. The second alive=true sample proves the wait
		// loop ran at least one iteration before observing a dead
		// process.
		if n <= 2 {
			if n == 2 {
				sawAliveInWait.Store(true)
			}
			return ProcessIdentity{
				Alive:     true,
				ImagePath: mcphubBinaryNameForTest(),
				// argv passes the gate but the override above
				// short-circuits anyway; populate for realism.
				Cmdline:   []string{mcphubBinaryNameForTest(), "gui"},
				StartTime: time.Now().Add(-1 * time.Hour),
			}, nil
		}
		// Subsequent calls (further wait-loop iterations OR
		// late post-kill probes that don't exist): alive=false.
		return ProcessIdentity{Alive: false}, nil
	})

	// Replace the kill helper with a no-op so we don't actually
	// SIGKILL/TerminateProcess the test runner. The wait-loop
	// behavior we're testing is independent of whether killProcess
	// did anything real — the loop polls processID, which is
	// already overridden above.
	prevKill := killProcessOverride
	defer func() { killProcessOverride = prevKill }()
	killProcessOverride = func(pid int) error { return nil }

	// Capture state at the moment postKillHook fires. The hook is
	// invoked by KillRecordedHolder AFTER the wait-for-exit loop
	// completes and BEFORE the acquire-poll loop, so this is the
	// natural seam to observe wait-loop completion.
	var hookFiredCount atomic.Int32
	var processIDCountAtHook atomic.Int32
	prevHook := PostKillHookForTest()
	defer RestorePostKillHook(prevHook)
	SetPostKillHook(func() {
		hookFiredCount.Add(1)
		processIDCountAtHook.Store(processIDCallCount.Load())
	})

	dir := t.TempDir()
	pidport := filepath.Join(dir, "gui.pidport")
	// Use os.Getpid() so probeOnce treats the PID as alive when
	// the override seam is bypassed. (The override above replaces
	// the result regardless of PID.) Port 1 makes ping fail so the
	// verdict reaches VerdictLiveUnreachable.
	const probablyClosedPort = 1
	if err := os.WriteFile(pidport, []byte(formatPidport(os.Getpid(), probablyClosedPort)), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate mtime so probe doesn't retry — we want a single
	// LiveUnreachable observation for predictable counts.
	oldMtime := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(pidport, oldMtime, oldMtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	// Pre-acquire the flock so KillRecordedHolder reaches the gate
	// path. The acquire-poll loop after the wait will see this
	// flock busy and return VerdictRaceLost — which is fine for
	// this test: the assertion is on wait-loop behavior, not on
	// the recovered verdict.
	fl := flock.New(pidport + ".lock")
	if ok, _ := fl.TryLock(); !ok {
		t.Fatal("could not pre-lock")
	}
	defer fl.Unlock()

	// Use short backoff so the test doesn't sit through the
	// default 50ms inter-poll delay multiple times. KillExitBackoff
	// of 5ms keeps total runtime well under the test timeout.
	opts := KillOpts{
		KillExitBackoff:  5 * time.Millisecond,
		KillExitDeadline: 1 * time.Second,
		AcquireBackoff:   5 * time.Millisecond,
		AcquireDeadline:  100 * time.Millisecond, // Race-lost expected.
	}
	_, _, _ = KillRecordedHolder(context.Background(), pidport, opts)

	// Assertions:
	// - postKillHook fired exactly once (kill happened).
	if got := hookFiredCount.Load(); got != 1 {
		t.Errorf("postKillHook fire count = %d, want 1", got)
	}
	// - At hook time, processID was called >= 2 (probe + at
	//   least one wait-loop iteration). Without the wait loop
	//   this would be exactly 1 (probe only).
	if got := processIDCountAtHook.Load(); got < 2 {
		t.Errorf("processID call count at postKillHook = %d, want >= 2 (probe + wait-loop iteration); wait-for-exit step 5f did not run", got)
	}
	// - The wait loop observed alive=true at least once (the
	//   second sample) before exiting on the first alive=false.
	//   This pins the polling shape: call processID, observe alive,
	//   sleep backoff, call again.
	if !sawAliveInWait.Load() {
		t.Errorf("wait loop never observed alive=true; control flow may have skipped the loop body")
	}
}

// mcphubBinaryNameForTest returns the platform-appropriate mcphub
// binary name so cross-platform tests can build an ImagePath that
// passes matchBasename. Mirrors the per-platform matchBasename
// logic in probe_windows.go vs probe_linux.go/probe_darwin.go.
func mcphubBinaryNameForTest() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\mcphub\mcphub.exe`
	}
	return "/usr/local/bin/mcphub"
}

// portFromURL is shared with probe_test.go; kept package-private.
// (Defined in probe_test.go; redefining would conflict.)
