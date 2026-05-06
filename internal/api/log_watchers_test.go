package api

import (
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
