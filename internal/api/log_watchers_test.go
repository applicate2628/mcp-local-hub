package api

import (
	"runtime"
	"strings"
	"testing"
)

// TestParsePosixPSRows_PaddedColumns is the regression test for Codex
// bot P1 on PR #131: ps -e -o pid=,ppid=,comm=,etime=,args= prints
// padded columns with runs of spaces, and the prior strings.SplitN
// implementation skipped legitimate rows because SplitN spent its
// budget on consecutive separators.
func TestParsePosixPSRows_PaddedColumns(t *testing.T) {
	// Realistic ps output sample with right-padded columns:
	//   "  1234   500 bash           00:01:23 /bin/bash -c \"tail -F ...\""
	// Note multiple spaces between columns AND inside the args string.
	sample := strings.Join([]string{
		" 1234   500 bash       00:01:23 /bin/bash -c \"tail -F /tmp/x.log\"",
		"  555     1 systemd 1-02:03:04 /lib/systemd/systemd --user",
		"99999 12345 grep         0:05 grep -E --line-buffered  pattern1|pattern2",
	}, "\n")

	rows := parsePosixPSRows(strings.NewReader(sample))
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (each row should parse despite padding)", len(rows))
	}

	// Row 0: pid=1234, ppid=500, comm=bash, args contains "tail -F"
	if rows[0].PID != 1234 || rows[0].ParentPID != 500 {
		t.Errorf("row 0 PID/ParentPID = %d/%d, want 1234/500", rows[0].PID, rows[0].ParentPID)
	}
	if rows[0].Name != "bash" {
		t.Errorf("row 0 Name = %q, want bash", rows[0].Name)
	}
	if !strings.Contains(rows[0].Cmdline, "tail -F /tmp/x.log") {
		t.Errorf("row 0 Cmdline = %q, want substring 'tail -F /tmp/x.log'", rows[0].Cmdline)
	}

	// Row 1: with day-prefix etime "1-02:03:04" — must parse.
	if rows[1].PID != 555 {
		t.Errorf("row 1 PID = %d, want 555", rows[1].PID)
	}
	if rows[1].StartTime == 0 {
		t.Errorf("row 1 StartTime should be non-zero (etime parsed = 1-02:03:04)")
	}

	// Row 2: args has internal multi-space — must be preserved verbatim.
	if !strings.Contains(rows[2].Cmdline, "pattern1|pattern2") {
		t.Errorf("row 2 Cmdline = %q, want pattern1|pattern2 substring", rows[2].Cmdline)
	}
}

// TestParsePosixPSRows_NoArgs verifies that rows with exactly 4 tokens
// (a process with no command-line args, e.g. a kernel thread) parse
// with empty Cmdline rather than getting silently skipped.
func TestParsePosixPSRows_NoArgs(t *testing.T) {
	sample := "100 50 kthread 1:00\n200 50 kworker 2:00 /usr/bin/something"
	rows := parsePosixPSRows(strings.NewReader(sample))
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (4-token kernel thread + 5-token user proc)", len(rows))
	}
	if rows[0].PID != 100 || rows[0].Cmdline != "" {
		t.Errorf("row 0 = %+v, want PID=100, Cmdline=''", rows[0])
	}
	if rows[1].PID != 200 || rows[1].Cmdline != "/usr/bin/something" {
		t.Errorf("row 1 = %+v, want PID=200, Cmdline=/usr/bin/something", rows[1])
	}
}

// TestParsePosixPSRows_FewerThan4Tokens verifies that truly malformed
// rows (3 or fewer tokens, missing required pid/ppid/comm/etime
// columns) are skipped rather than producing garbage.
func TestParsePosixPSRows_FewerThan4Tokens(t *testing.T) {
	sample := "100\n200 50\n300 50 onlycomm"
	rows := parsePosixPSRows(strings.NewReader(sample))
	if len(rows) != 0 {
		t.Errorf("rows = %d, want 0 (all malformed); got %+v", len(rows), rows)
	}
}

// TestEffectiveParentAlive_PosixInitReparenting is the regression
// test for Codex Cloud bot P1 on PR #131 (kosyak
// 2026-05-07-posix-reparenting-defeats-parent-alive-orphan-heuristic.md):
// on POSIX, an orphan reparented to PID 1 (init/systemd) must be
// classified as effectively orphan even though PID 1 is alive in the
// snapshot. Windows behavior (no auto-reparent) is preserved.
//
// We can't truly switch runtime.GOOS in a unit test, so this test
// validates the behavior at the function's contract level: pidSet
// containing PID 1, ppid=1. On POSIX (non-windows) this returns false
// (orphan). On windows it returns true. We assert based on actual
// runtime.GOOS.
func TestEffectiveParentAlive_PosixInitReparenting(t *testing.T) {
	pidSet := map[int]bool{1: true, 100: true}

	if got := effectiveParentAlive(0, pidSet); got {
		t.Errorf("ppid=0 → got %v, want false (no parent recorded)", got)
	}
	if got := effectiveParentAlive(99999, pidSet); got {
		t.Errorf("ppid=99999 missing from snapshot → got %v, want false (orphan)", got)
	}
	if got := effectiveParentAlive(100, pidSet); !got {
		t.Errorf("ppid=100 in snapshot → got %v, want true (real parent)", got)
	}

	// PID 1 special-case on POSIX:
	got := effectiveParentAlive(1, pidSet)
	if runtime.GOOS == "windows" {
		if !got {
			t.Errorf("Windows: ppid=1 in snapshot → got %v, want true (Windows does not auto-reparent)", got)
		}
	} else {
		if got {
			t.Errorf("POSIX: ppid=1 (init) → got %v, want false (orphan reparented to init)", got)
		}
	}
}

// TestCleanupLogWatchers_ApplyRevalidatesPIDIdentity is the regression
// test for Codex Cloud bot P2 advisory on PR #131 (kosyak
// 2026-05-07-pid-reuse-race-in-watcher-kill-loop.md): between the
// initial snapshot and the kill syscall, a watcher can exit and the
// OS can recycle the PID. Apply must revalidate identity (Name +
// StartTime) before killing.
//
// Test: inject snapshotProcessesFn that returns DIFFERENT rosters on
// the first vs second call. First call (filter) sees PID 1234 as a
// matching watcher with StartTime=1000. Second call (revalidate) sees
// PID 1234 as a different process (Name and StartTime changed) — that
// PID got recycled. CleanupLogWatchers must skip the kill and record
// the identity-mismatch reason.
func TestCleanupLogWatchers_ApplyRevalidatesPIDIdentity(t *testing.T) {
	originalSnap := snapshotProcessesFn
	defer func() { snapshotProcessesFn = originalSnap }()

	calls := 0
	snapshotProcessesFn = func() ([]processRow, error) {
		calls++
		if calls == 1 {
			// First call: filter step sees a real watcher candidate.
			return []processRow{
				{PID: 1234, ParentPID: 99999, Name: "tail.exe",
					Cmdline: `tail -F /d/dev/.scratch/x.log`,
					StartTime: 1000},
			}, nil
		}
		// Second call: PID 1234 is now an unrelated recycled process.
		return []processRow{
			{PID: 1234, ParentPID: 1, Name: "explorer.exe",
				Cmdline: "C:\\Windows\\explorer.exe",
				StartTime: 9999},
		}, nil
	}

	a := NewAPI()
	got, err := a.CleanupLogWatchers(LogWatcherCleanupOpts{
		IncludeLive: true, // skip the parent-alive gate to ensure kill loop runs
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("CleanupLogWatchers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(got), got)
	}
	if got[0].KillErr != "skipped: PID reused (identity mismatch)" {
		t.Errorf("expected identity-mismatch skip, got KillErr=%q", got[0].KillErr)
	}
	if calls != 2 {
		t.Errorf("expected 2 snapshots (filter + revalidate); got %d", calls)
	}
}

// TestCleanupLogWatchers_ApplySkipsExitedPID verifies the fail-safe
// path when a candidate PID disappears between snapshots (process
// exited cleanly with no recycler taking its slot yet).
func TestCleanupLogWatchers_ApplySkipsExitedPID(t *testing.T) {
	originalSnap := snapshotProcessesFn
	defer func() { snapshotProcessesFn = originalSnap }()

	calls := 0
	snapshotProcessesFn = func() ([]processRow, error) {
		calls++
		if calls == 1 {
			return []processRow{
				{PID: 1234, ParentPID: 99999, Name: "tail.exe",
					Cmdline: `tail -F /d/dev/.scratch/x.log`,
					StartTime: 1000},
			}, nil
		}
		// Second call: PID 1234 missing — process exited cleanly.
		return []processRow{}, nil
	}

	a := NewAPI()
	got, err := a.CleanupLogWatchers(LogWatcherCleanupOpts{
		IncludeLive: true,
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("CleanupLogWatchers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	if got[0].KillErr != "skipped: process exited between snapshot and kill" {
		t.Errorf("expected exited-PID skip, got KillErr=%q", got[0].KillErr)
	}
}

// TestFilterWatcherCandidates_DefaultCaseInsensitive verifies that
// uppercase paths and lowercase tokens still match the default regex
// patterns, mirroring PowerShell -match operator's case-insensitive
// default. Codex Cloud bot P2 on PR #131 caught this — Go regexp is
// case-sensitive by default, but the source PS1 detection isn't.
func TestFilterWatcherCandidates_DefaultCaseInsensitive(t *testing.T) {
	// Path-matched watcher with UPPERCASE .SCRATCH/.LOG path.
	// Orphan-grep with lowercase `traceback` token (PS1 default
	// includes `Traceback` capitalized).
	rows := []processRow{
		{PID: 100, ParentPID: 50, Name: "tail.exe", Cmdline: `tail -F /D/Dev/.SCRATCH/run.LOG`},
		{PID: 200, ParentPID: 99999, Name: "grep.exe", Cmdline: `grep -E --line-buffered "traceback|fail"`},
	}
	pidSet := map[int]bool{100: true, 50: true, 200: true} // 99999 absent → row 200 is orphan

	pathRe, err := compileWatcherRegex("", defaultLogWatcherPathRegex)
	if err != nil {
		t.Fatalf("compile path regex: %v", err)
	}
	orphanRe, err := compileWatcherRegex("", defaultOrphanGrepRegexTokens)
	if err != nil {
		t.Fatalf("compile orphan regex: %v", err)
	}

	matches := filterWatcherCandidates(rows, pidSet, pathRe, orphanRe)
	if len(matches) != 2 {
		t.Errorf("matches = %d, want 2 (uppercase path + lowercase orphan-grep token); got %+v", len(matches), matches)
	}
}
