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

// TestEffectiveParentAlive verifies the parent-alive contract used by
// log-watcher cleanup gating: missing/zero parent is treated as not
// alive, real parent PIDs in the snapshot are alive, and POSIX
// ppid==1 (init/systemd reparenting) is treated as orphaned.
//
// Background: Codex Cloud bot P1 on PR #131 caught the original
// regression — without the POSIX init special case, every reparented
// orphan watcher (`tail`, `grep` adopted by PID 1) appeared as
// "parent alive" and the default IncludeLive=false apply path skipped
// it. Codex Cloud bot P1 on PR #135 round 2 caught the over-correction
// in #139 (which removed the special case wholesale). The current
// helper restores the special case but documents WHY it's safe in this
// caller's narrow context (watcherProcessNames gate filters to
// bash/sh/tail/grep — none of which are legitimate direct init
// children).
func TestEffectiveParentAlive(t *testing.T) {
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

	// PID 1 special-case on POSIX: when ppid is init we classify as
	// orphan because watcher-class processes aren't legitimately
	// spawned directly by init. On Windows, PID 1 has no special
	// reparent semantics (the snapshot is authoritative).
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

// TestEffectiveParentAlive_PosixReparentedToInit_OrphanDetected is the
// regression test for Codex Cloud bot P1 on PR #135 round 2. When a
// watcher's original parent (e.g. an agent shell at PID 42) exits on
// POSIX, the kernel reparents the watcher to PID 1. Because we have
// only a single live snapshot — no separate "original parent" field
// in processRow — the helper must use ppid==1 itself as the
// reparented-to-init signal. Without this branch the default Apply
// path (IncludeLive=false) silently skips orphan termination.
func TestEffectiveParentAlive_PosixReparentedToInit_OrphanDetected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not auto-reparent on parent exit; this contract is POSIX-only")
	}
	// Snapshot AFTER the original parent exited and the watcher was
	// reparented to PID 1. The original parent (42) is no longer in
	// the pidSet — only init (1) and the watcher itself (200).
	pidSet := map[int]bool{1: true, 200: true}
	if got := effectiveParentAlive(1, pidSet); got {
		t.Errorf("POSIX reparented to init: ppid=1 → got %v, want false (orphan)", got)
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

// TestCleanupLogWatchers_FailClosedOnZeroStartTime is the regression
// test for the xhigh+subagents reliability-engineer finding on PR #131
// commit 4e46eba (kosyak
// 2026-05-07-startime-zero-fail-open-bypasses-pid-reuse-guard.md):
// when wmic CIM_DATETIME parse or ps etime parse fails, StartTime
// falls back to 0. If both the original snapshot row AND the fresh
// revalidation row have StartTime==0, the identity check
// `cur.StartTime != original.StartTime` evaluates to `0 != 0` = false
// and the guard degenerates to pure name equality, letting a
// recycled same-named PID through — defeating the very kosyak-#11
// PID-reuse protection. Apply must refuse the kill and record an
// explicit "snapshot start-time unknown" skip.
func TestCleanupLogWatchers_FailClosedOnZeroStartTime(t *testing.T) {
	originalSnap := snapshotProcessesFn
	defer func() { snapshotProcessesFn = originalSnap }()

	// Both snapshots return PID 1234 with StartTime=0 — the degenerate
	// case the revalidation guard cannot anchor against.
	snapshotProcessesFn = func() ([]processRow, error) {
		return []processRow{
			{PID: 1234, ParentPID: 99999, Name: "tail.exe",
				Cmdline:   `tail -F /d/dev/.scratch/x.log`,
				StartTime: 0},
		}, nil
	}

	a := NewAPI()
	got, err := a.CleanupLogWatchers(LogWatcherCleanupOpts{
		IncludeLive: true, // skip parent-alive gate so kill loop runs
		DryRun:      false,
	})
	if err != nil {
		t.Fatalf("CleanupLogWatchers: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %+v", len(got), got)
	}
	want := "skipped: snapshot start-time unknown (cannot revalidate identity)"
	if got[0].KillErr != want {
		t.Errorf("expected start-time-unknown skip, got KillErr=%q want %q", got[0].KillErr, want)
	}
}

// TestParseWmicProcessRows_LeadingBlankLine is the regression test
// for Codex Cloud bot P1 on PR #131 commit 1f59a65 / kosyak
// 2026-05-07-wmic-csv-parse-not-tolerant-shipped-parallel-mechanism.md:
// wmic /format:csv on some Windows versions emits a blank line before
// the header. The prior csv.Reader-based parser treated that blank
// record AS the header, leaving colIdx empty so every subsequent
// per-row column lookup returned "" and processRow.Name/Cmdline were
// always empty. The line-based rewrite skips blanks before the
// "first non-blank line == header" gate.
func TestParseWmicProcessRows_LeadingBlankLine(t *testing.T) {
	// Three blank lines, then header, then two rows. Paths kept
	// space-free because basenameFromCmdline splits on first space
	// when the path isn't surrounded by inner quotes (existing helper
	// semantics, unchanged here — see processes.go:184 and the
	// basenameFromCmdline contract).
	sample := "\n\n\nNode,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize\n" +
		`HOST,"C:\Apps\app.exe --flag",20260507030000.000000+000,500,1234,1048576` + "\n" +
		`HOST,"powershell.exe -NoProfile",20260507031500.000000+000,500,5678,2097152` + "\n"
	rows, err := parseWmicProcessRows(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseWmicProcessRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (blank lines must be skipped before header); got %+v", len(rows), rows)
	}
	if rows[0].PID != 1234 {
		t.Errorf("row 0 PID = %d, want 1234 (header parse must consume the column-name line, not a blank)", rows[0].PID)
	}
	if rows[0].Name != "app.exe" {
		t.Errorf("row 0 Name = %q, want app.exe (basenameFromCmdline of C:\\Apps\\app.exe)", rows[0].Name)
	}
	if rows[1].PID != 5678 {
		t.Errorf("row 1 PID = %d, want 5678", rows[1].PID)
	}
}

// TestParseWmicProcessRows_UnescapedQuotesInCommandLine verifies that
// rows with quotes inside quoted fields (which wmic does NOT escape per
// RFC 4180) parse without aborting the snapshot. Codex Cloud bot P1 on
// PR #131 commit 1f59a65: the prior csv.Reader rejected such rows with
// ErrBareQuote and `return nil, err` killed the entire snapshot.
func TestParseWmicProcessRows_UnescapedQuotesInCommandLine(t *testing.T) {
	// The middle row carries a CommandLine with an unbalanced literal
	// quote that strict RFC 4180 csv.Reader rejects but the tolerant
	// splitCSVLine handles (simple state machine that toggles inQuote
	// on every quote regardless of escaping).
	sample := "Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize\n" +
		`HOST,"normal.exe",20260507030000.000000+000,500,1000,1024` + "\n" +
		`HOST,"weird.exe --json={"key":"value"}",20260507030500.000000+000,500,2000,2048` + "\n" +
		`HOST,"trailing.exe",20260507031000.000000+000,500,3000,4096` + "\n"
	rows, err := parseWmicProcessRows(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseWmicProcessRows must NOT error on unescaped quotes; got: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("rows = %d, want >= 2 (at minimum the well-formed first and last rows must survive); got %+v", len(rows), rows)
	}
	pids := make(map[int]bool)
	for _, r := range rows {
		pids[r.PID] = true
	}
	if !pids[1000] || !pids[3000] {
		t.Errorf("expected well-formed rows (PID 1000 and 3000) to survive; got pids=%v", pids)
	}
}

// TestParseWmicProcessRows_MalformedPIDRowSkipped verifies that one
// row with a non-numeric ProcessId field is `continue`'d, not the
// whole-snapshot `return nil, err` the prior code did. Codex Cloud
// bot P1 on PR #131 commit 1f59a65.
func TestParseWmicProcessRows_MalformedPIDRowSkipped(t *testing.T) {
	sample := "Node,CommandLine,CreationDate,ParentProcessId,ProcessId,WorkingSetSize\n" +
		`HOST,"good.exe",20260507030000.000000+000,500,1000,1024` + "\n" +
		`HOST,"bad.exe",20260507030500.000000+000,500,NOTANUMBER,2048` + "\n" +
		`HOST,"alsogood.exe",20260507031000.000000+000,500,2000,4096` + "\n"
	rows, err := parseWmicProcessRows(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("parseWmicProcessRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (good rows survive, bad row skipped, snapshot not aborted); got %+v", len(rows), rows)
	}
	if rows[0].PID != 1000 || rows[1].PID != 2000 {
		t.Errorf("PIDs = %d/%d, want 1000/2000 (the malformed-PID row must be skipped between them)",
			rows[0].PID, rows[1].PID)
	}
}
