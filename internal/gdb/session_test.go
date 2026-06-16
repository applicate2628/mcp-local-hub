package gdb

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

// nopWriteCloser is a discard io.WriteCloser for the session's stdin: the MI
// parser tests don't care what gets written to gdb, only what is read back.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// newFakeSession builds a session whose MI output is the canned miOutput string
// and whose stdin is discarded. No real gdb is spawned (cmd is nil), so Command
// can be exercised purely against the parser. The canned output must terminate
// every command (a `^`-result line) or readResult would block until the 30s
// deadline.
func newFakeSession(miOutput string) *session {
	return &session{
		stdin:  nopWriteCloser{io.Discard},
		reader: bufio.NewReader(strings.NewReader(miOutput)),
	}
}

func TestUnescapeMIString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain quoted", `"hello"`, "hello"},
		{"newline", `"hello\n"`, "hello\n"},
		{"tab", `"a\tb"`, "a\tb"},
		{"escaped quote", `"say \"hi\""`, `say "hi"`},
		{"backslash", `"a\\b"`, `a\b`},
		{"esc octal", `"\033[0m"`, "\x1b[0m"},
		{"carriage return", `"x\r\n"`, "x\r\n"},
		{"unknown escape passes byte", `"\q"`, "q"},
		{"no surrounding quotes", `bare`, "bare"},
		{"missing closing quote", `"partial`, "partial"},
		{"empty quotes", `""`, ""},
		{"bell+backspace", `"a\ab\b"`, "a\ab\b"},
		// Regression: miFieldValue passes the whole rest of an MI record, so the
		// value must terminate at THIS field's closing quote, not the record's
		// last quote (the old LastIndexByte scan spliced trailing fields in).
		{"stops at field boundary in record", `"add",file="x.cpp",line="2"`, "add"},
		{"escaped quote then field boundary", `"a\"b",next="y"`, `a"b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unescapeMIString(tc.in); got != tc.want {
				t.Errorf("unescapeMIString(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestCommand_ConsoleThenDone: console-stream payloads are C-unescaped and
// accumulated, and reading stops at the `^done` result record.
func TestCommand_ConsoleThenDone(t *testing.T) {
	mi := "~\"hello\\n\"\n" +
		"~\"world\\n\"\n" +
		"^done\n" +
		"(gdb) \n"
	s := newFakeSession(mi)

	out, err := s.Command("print x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "hello\nworld\n" {
		t.Errorf("Command output = %q, want %q", out, "hello\nworld\n")
	}
}

// TestCommand_TargetStreamIncluded: @-target-stream records are surfaced too;
// &-log-stream records are NOT.
func TestCommand_StreamKinds(t *testing.T) {
	mi := "&\"break main\\n\"\n" + // log stream: must be dropped
		"@\"target says hi\\n\"\n" + // target stream: surfaced
		"~\"console line\\n\"\n" + // console stream: surfaced
		"^done\n" +
		"(gdb) \n"
	s := newFakeSession(mi)

	out, err := s.Command("anything")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "target says hi\nconsole line\n"
	if out != want {
		t.Errorf("Command output = %q, want %q", out, want)
	}
}

// TestCommand_Error: a `^error,msg="..."` result record is returned as an error
// with the unescaped message.
func TestCommand_Error(t *testing.T) {
	mi := "^error,msg=\"No symbol \\\"foo\\\" in current context.\"\n" +
		"(gdb) \n"
	s := newFakeSession(mi)

	out, err := s.Command("print foo")
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}
	if !strings.Contains(err.Error(), "No symbol") {
		t.Errorf("error = %q, want it to contain the gdb message", err.Error())
	}
}

// TestCommand_RunningThenStopped: a `^running` result keeps the reader going
// until the next `*stopped` async record, and the stop summary is included.
func TestCommand_RunningThenStopped(t *testing.T) {
	mi := "^running\n" +
		"*running,thread-id=\"all\"\n" +
		"~\"some program output\\n\"\n" +
		"*stopped,reason=\"breakpoint-hit\",disp=\"keep\",bkptno=\"1\",frame={func=\"main\",file=\"a.c\",line=\"10\"}\n" +
		"(gdb) \n"
	s := newFakeSession(mi)

	out, err := s.Command("-exec-continue")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "some program output") {
		t.Errorf("output missing pre-stop console text: %q", out)
	}
	if !strings.Contains(out, "breakpoint-hit") {
		t.Errorf("output missing stop reason: %q", out)
	}
	if !strings.Contains(out, "func=main") {
		t.Errorf("output missing stop frame func: %q", out)
	}
	if !strings.Contains(out, "line=10") {
		t.Errorf("output missing stop line: %q", out)
	}
}

// TestCommand_RunningThenExit: a `^running` followed by `^exit` (the inferior
// ran to completion and gdb is exiting) returns without error rather than
// waiting forever for a *stopped that never comes.
func TestCommand_RunningThenExit(t *testing.T) {
	mi := "^running\n" +
		"~\"[Inferior 1 exited normally]\\n\"\n" +
		"^exit\n"
	s := newFakeSession(mi)

	out, err := s.Command("-exec-run")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "exited normally") {
		t.Errorf("output missing inferior-exit console text: %q", out)
	}
}

// TestReadUntilPrompt drains the startup banner up to the first prompt, keeping
// only the console/target stream text.
func TestReadUntilPrompt(t *testing.T) {
	mi := "=thread-group-added,id=\"i1\"\n" +
		"~\"GNU gdb (GDB) 14.2\\n\"\n" +
		"(gdb) \n"
	s := newFakeSession(mi)

	banner, err := s.readUntilPrompt(runResultDeadline)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(banner, "GNU gdb") {
		t.Errorf("banner missing version text: %q", banner)
	}
	if v := firstNonEmptyLine(banner); v != "GNU gdb (GDB) 14.2" {
		t.Errorf("firstNonEmptyLine = %q, want the version line", v)
	}
}

// TestCommand_EOFBeforeResult: if gdb closes its output before producing a
// result record, Command surfaces an explicit "process exited" error rather than
// hanging.
func TestCommand_EOFBeforeResult(t *testing.T) {
	mi := "~\"partial output\\n\"\n" // no ^result, then EOF
	s := newFakeSession(mi)

	_, err := s.Command("info registers")
	if err == nil {
		t.Fatalf("expected an EOF/process-exited error")
	}
	if !strings.Contains(err.Error(), "process exited") {
		t.Errorf("error = %q, want it to mention the process exiting", err.Error())
	}
}

// TestAsyncSummary covers the stop-record summarizer directly, including the
// bare *stopped (no fields) and *running paths.
func TestAsyncSummary(t *testing.T) {
	// Exact-match (not Contains): each field value must be clean, with no
	// trailing junk spliced in from later fields. Under the pre-fix LastIndexByte
	// termination, reason would have been "end-stepping-range\",frame={func=...42"
	// and this exact assertion would fail — that is the regression guard.
	got := asyncSummary(`*stopped,reason="end-stepping-range",frame={func="foo",file="x.c",line="42"}`)
	if want := "[stopped: reason=end-stepping-range func=foo file=x.c line=42]\n"; got != want {
		t.Errorf("asyncSummary = %q, want %q", got, want)
	}
	if bare := asyncSummary("*stopped"); !strings.Contains(bare, "stopped") {
		t.Errorf("bare *stopped summary = %q, want it to mention stopped", bare)
	}
}

// TestDefaultStartVersion_ProbeFailureReturnsEmpty: when the `<gdb> --version`
// exec fails (here a guaranteed-nonexistent binary, so no real gdb is spawned),
// defaultStartVersion returns "" without erroring — a version-probe failure must
// never abort an otherwise-healthy session start. The non-error path (a real
// gdb) is covered end-to-end by the injected-probe startTool tests in
// server_test.go.
func TestDefaultStartVersion_ProbeFailureReturnsEmpty(t *testing.T) {
	if v := defaultStartVersion(`Z:\does\not\exist\nope-gdb.exe`); v != "" {
		t.Errorf("defaultStartVersion on a missing binary = %q, want \"\"", v)
	}
}
