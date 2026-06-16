// Package gdb implements a NATIVE Go MCP server that drives GDB through its
// machine-interface (GDB/MI) protocol. It is the gdb counterpart of mcphub's
// native lldb-bridge (internal/lldb) and a drop-in replacement for the external
// GDB-MCP python server (the uv-run server.py the gdb manifest used to launch).
//
// WHY this replaces GDB-MCP: GDB-MCP decides gdb is "available" with a python
// `subprocess.run(['gdb','--version'])` probe. That probe fails inside the
// console-less mcphub daemon even though gdb IS installed and on the daemon PATH
// — so GDB-MCP reports "gdb not available" and every gdb tool is dead. mcphub's
// native lldb bridge does not have this problem because it spawns the debugger by
// absolute path via Go's exec.Command (no python subprocess in a console-less
// host). This package does the same for gdb: it spawns gdb directly via
// exec.Command at the toolchain-resolved absolute path and speaks GDB/MI on its
// stdin/stdout.
//
// This file (session.go) owns the GDB/MI session: spawning gdb, draining its
// startup banner, sending commands, parsing MI output records back into
// human-readable console text, and terminating the process. The MCP server and
// tool handlers live in server.go; the cobra subcommand lives in cmd.go.
package gdb

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/process"
	"mcp-local-hub/internal/toolchain"
)

// runResultDeadline bounds how long Command waits for a single MI command's
// result record (and, for a `^running` command, for the subsequent `*stopped`
// async record). Without it a `-exec-continue` against a program that never
// halts would block the reader forever. 30s matches the task brief.
const runResultDeadline = 30 * time.Second

// terminateGrace bounds how long Terminate waits for gdb to exit after the
// `-gdb-exit` request before killing the process hard.
const terminateGrace = 2 * time.Second

// session is a single live GDB/MI session: one gdb child process driven over its
// stdin/stdout. One in-flight Command at a time is enforced by mu so MI records
// from concurrent commands can never interleave on the shared stdout reader.
//
// The reader/writer seam is what lets tests exercise the MI parser without
// spawning real gdb: startSession wires the real os pipes, but a test constructs
// a session directly with a canned io.Reader of MI lines and a discard writer.
type session struct {
	mu sync.Mutex

	cmd    *exec.Cmd      // nil in injected-reader tests
	stdin  io.WriteCloser // command channel into gdb
	reader *bufio.Reader  // MI record stream out of gdb (stderr merged in)

	gdbPath string // resolved gdb path used for the spawn (diagnostics)
	version string // `gdb --version` first line captured at start (diagnostics)
}

// startSessionFunc is the injectable seam server.go uses to create a session.
// The production implementation is startSession; tests substitute a fake that
// returns a session backed by a canned reader so no real gdb is spawned.
type startSessionFunc func(gdbPath, program string) (*session, error)

// startVersionProbe resolves a started session's version by running
// `<gdbPath> --version` via Go exec. It is a package-level seam so tests can
// exercise startSession's version-population path without spawning real gdb. The
// production implementation (defaultStartVersion) reuses gdbVersionLine — the
// SAME `--version` exec that backs debugger_status — because the session is
// spawned with `-q`, which suppresses the startup banner the version was
// previously (and unreliably) scraped from. defaultStartVersion returns "" when
// the probe fails so a version-probe failure never aborts an otherwise-healthy
// session start.
var startVersionProbe = defaultStartVersion

// defaultStartVersion is the production startVersionProbe: it runs
// `<gdbPath> --version` and returns the first non-empty line, or "" on failure.
func defaultStartVersion(gdbPath string) string {
	version, _ := gdbVersionLine(gdbPath)
	return version
}

// startSession spawns `gdb --interpreter=mi3 --nx -q [--args program]`, wires its
// stdin/stdout (stderr merged into stdout so MI log/error records are not lost),
// drains the MI startup banner up to the first `(gdb) ` prompt, and disables the
// interactive confirm + pagination prompts that would otherwise block a
// non-interactive MI driver. gdbPath defaults to toolchain.DefaultGdbPath() when
// the caller passes "".
//
//   - --interpreter=mi3 selects the GDB/MI machine interface (line-oriented,
//     parseable) instead of the human REPL.
//   - --nx skips ~/.gdbinit so a user's interactive config cannot inject prompts
//     or commands that would desync the MI stream.
//   - -q silences the introductory banner noise.
func startSession(gdbPath, program string) (*session, error) {
	if gdbPath == "" {
		gdbPath = toolchain.DefaultGdbPath()
	}

	args := gdbStartArgs(program)

	cmd := exec.Command(gdbPath, args...)
	process.NoConsole(cmd) // suppress console flash on a windowsgui parent

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("gdb stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("gdb stdout pipe: %w", err)
	}
	// Merge stderr into the same pipe as stdout: GDB/MI log-stream records and
	// some startup diagnostics land on stderr, and the MI reader must see them in
	// order rather than dropping them.
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start gdb (%s): %w", gdbPath, err)
	}

	s := &session{
		cmd:     cmd,
		stdin:   stdin,
		reader:  bufio.NewReader(stdout),
		gdbPath: gdbPath,
	}

	// Drain the MI startup banner up to the first prompt. A spawn that dies
	// immediately (bad binary, missing DLL) surfaces here as a read error rather
	// than a later confusing command failure. The banner text itself is NOT used
	// for the version: gdb is spawned with `-q`, which suppresses the version
	// banner, so the banner is empty in production and scraping it yielded "".
	if _, err := s.readUntilPrompt(runResultDeadline); err != nil {
		s.Terminate()
		return nil, fmt.Errorf("gdb startup banner: %w", err)
	}
	// Populate the version from a dedicated `<gdb> --version` probe (the same exec
	// debugger_status uses), which is reliable regardless of the `-q` banner.
	s.version = startVersionProbe(gdbPath)

	// Disable interactive confirmation + pagination so non-interactive MI driving
	// never blocks on a `---Type <return> to continue---` or a `[y/n]` prompt.
	if _, err := s.command("-gdb-set confirm off"); err != nil {
		s.Terminate()
		return nil, fmt.Errorf("gdb-set confirm off: %w", err)
	}
	if _, err := s.command("-gdb-set pagination off"); err != nil {
		s.Terminate()
		return nil, fmt.Errorf("gdb-set pagination off: %w", err)
	}
	return s, nil
}

func gdbStartArgs(program string) []string {
	args := []string{"--interpreter=mi3", "--nx", "-q"}
	if program != "" {
		// Use --args before the caller-provided program so GDB treats it as the
		// inferior executable instead of continuing option parsing. Without this,
		// a program name beginning with e.g. "-ex=shell ..." would be executed as a
		// GDB startup command.
		args = append(args, "--args", program)
	}
	return args
}

// Command sends cmd to gdb and returns the human-readable console output (the
// C-unescaped `~`/`@` stream records) collected until the command's result
// record. A `^error,msg="..."` result is surfaced as an error. It serializes
// callers so only one command is ever in flight on the shared reader.
func (s *session) Command(cmd string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.command(cmd)
}

// command is the lock-free body of Command; startSession calls it directly
// during setup (already single-threaded) to avoid re-entrant locking.
func (s *session) command(cmd string) (string, error) {
	cmd = strings.TrimRight(cmd, "\r\n")
	if _, err := io.WriteString(s.stdin, cmd+"\n"); err != nil {
		return "", fmt.Errorf("write gdb command %q: %w", cmd, err)
	}
	return s.readResult(runResultDeadline)
}

// Terminate requests a graceful `-gdb-exit`, waits up to terminateGrace for the
// process to exit, then kills it hard. Closing stdin and the process are both
// best-effort; Terminate is safe to call on a session whose start failed midway.
func (s *session) Terminate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Best-effort graceful exit. Ignore the error: a dead/dying gdb makes this
	// write fail, and the kill path below still runs.
	if s.stdin != nil {
		_, _ = io.WriteString(s.stdin, "-gdb-exit\n")
	}

	if s.cmd == nil || s.cmd.Process == nil {
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		return
	}

	done := make(chan struct{})
	go func() {
		_, _ = s.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(terminateGrace):
		_ = s.cmd.Process.Kill()
		<-done
	}
	if s.stdin != nil {
		_ = s.stdin.Close()
	}
}

// readResult reads MI records until the command's RESULT RECORD (`^`-prefixed)
// and returns the accumulated console/target stream text. For a `^running`
// result it keeps reading until the program halts (`*stopped`) or the session
// ends (`^exit`/`^done`), bounded by deadline, so a run/continue/step returns the
// stop info rather than the bare "running" acknowledgement.
//
// MI record shapes handled (one per line):
//
//	~"..."   console-stream  → C-unescaped into the returned text
//	@"..."   target-stream   → C-unescaped into the returned text
//	&"..."   log-stream      → diagnostic; not surfaced as output
//	^done / ^running / ^connected / ^exit / ^error,msg="..."  result records
//	*stopped,... / *running,... / =...  async records
//	(gdb)    the prompt (terminator/no-op)
func (s *session) readResult(deadline time.Duration) (string, error) {
	end := time.Now().Add(deadline)
	var out strings.Builder

	for {
		line, err := s.readLine(end)
		if err != nil {
			return out.String(), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || line == "(gdb) " || line == "(gdb)" {
			continue
		}

		switch line[0] {
		case '~', '@':
			// Console / target output stream: payload is a C-quoted string.
			out.WriteString(unescapeMIString(stripStreamPrefix(line)))
		case '&':
			// Log stream (gdb's own echo / diagnostics): not user output.
		case '^':
			rec := line[1:]
			if strings.HasPrefix(rec, "error") {
				return out.String(), fmt.Errorf("%s", miErrorMessage(rec))
			}
			if strings.HasPrefix(rec, "running") {
				// The command launched the inferior; keep reading until it halts
				// or the session ends, appending any stop/stream info.
				stop, serr := s.readUntilStop(end)
				out.WriteString(stop)
				return out.String(), serr
			}
			// ^done / ^connected / ^exit and any other result record: the command
			// is complete.
			return out.String(), nil
		case '*', '=':
			// Async record arriving before a result record (rare for a plain
			// command, but possible). Surface a human-readable stop summary.
			if summary := asyncSummary(line); summary != "" {
				out.WriteString(summary)
			}
		default:
			// Unrecognized / partial line: pass it through so nothing is silently
			// dropped.
			out.WriteString(line)
			out.WriteByte('\n')
		}
	}
}

// readUntilStop continues reading after a `^running` result until the inferior
// stops (`*stopped`) or the session ends (`^exit`/`^done`), accumulating console
// stream output and a human-readable stop summary. Bounded by end.
func (s *session) readUntilStop(end time.Time) (string, error) {
	var out strings.Builder
	for {
		line, err := s.readLine(end)
		if err != nil {
			return out.String(), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || line == "(gdb) " || line == "(gdb)" {
			continue
		}

		switch line[0] {
		case '~', '@':
			out.WriteString(unescapeMIString(stripStreamPrefix(line)))
		case '&':
			// log stream: ignore
		case '*':
			if strings.HasPrefix(line, "*stopped") {
				if summary := asyncSummary(line); summary != "" {
					out.WriteString(summary)
				}
				return out.String(), nil
			}
			// *running or other async: keep waiting for the stop.
		case '=':
			// notify async (=thread-created, =library-loaded, …): ignore.
		case '^':
			rec := line[1:]
			if strings.HasPrefix(rec, "error") {
				return out.String(), fmt.Errorf("%s", miErrorMessage(rec))
			}
			// ^exit (gdb is exiting) or ^done: the run-class command is finished
			// without a separate *stopped (e.g. the inferior ran to completion).
			return out.String(), nil
		}
	}
}

// readUntilPrompt drains records up to and including the next `(gdb) ` prompt,
// returning the console/target stream text seen along the way. Used to consume
// the MI startup banner.
func (s *session) readUntilPrompt(deadline time.Duration) (string, error) {
	end := time.Now().Add(deadline)
	var out strings.Builder
	for {
		line, err := s.readLine(end)
		if err != nil {
			return out.String(), err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "(gdb) " || trimmed == "(gdb)" {
			return out.String(), nil
		}
		if len(trimmed) > 0 && (trimmed[0] == '~' || trimmed[0] == '@') {
			out.WriteString(unescapeMIString(stripStreamPrefix(trimmed)))
		}
	}
}

// readLine reads one line from the MI stream, failing once the deadline passes.
// bufio.Reader has no per-read deadline, so the deadline is enforced between
// lines: a read that returns after end still surfaces a timeout. This bounds a
// never-stopping `*stopped` wait while staying simple (no reader goroutine).
func (s *session) readLine(end time.Time) (string, error) {
	if time.Now().After(end) {
		return "", fmt.Errorf("timed out waiting for gdb output after %s", runResultDeadline)
	}
	line, err := s.reader.ReadString('\n')
	if err != nil {
		if line != "" {
			// Return the partial final line (e.g. a prompt with no trailing
			// newline at EOF) so the caller can still classify it.
			return line, nil
		}
		if err == io.EOF {
			return "", fmt.Errorf("gdb closed its output stream (process exited)")
		}
		return "", fmt.Errorf("read gdb output: %w", err)
	}
	return line, nil
}

// stripStreamPrefix removes the leading stream marker (`~`/`@`/`&`) from an MI
// stream record, returning the still-quoted C string payload (including its
// surrounding double quotes). Callers pass the result to unescapeMIString.
func stripStreamPrefix(line string) string {
	if len(line) == 0 {
		return line
	}
	switch line[0] {
	case '~', '@', '&':
		return line[1:]
	}
	return line
}

// miErrorMessage extracts the human-readable message from an MI error result
// record body of the form `error,msg="..."`. Falls back to the raw record body
// when no msg field is present.
func miErrorMessage(rec string) string {
	const key = "msg="
	i := strings.Index(rec, key)
	if i < 0 {
		return "gdb error: " + rec
	}
	return "gdb error: " + unescapeMIString(rec[i+len(key):])
}

// asyncSummary turns an MI async record (`*stopped,...`) into a short
// human-readable line so a run/step/continue returns why and where it halted.
func asyncSummary(line string) string {
	// line looks like: *stopped,reason="breakpoint-hit",...,frame={func="main",...}
	body := line
	if i := strings.IndexByte(body, ','); i >= 0 {
		body = body[i+1:]
	} else {
		// No fields (e.g. bare "*stopped"): still report the class.
		class := strings.TrimPrefix(line, "*")
		return "[" + class + "]\n"
	}

	parts := []string{}
	if v := miFieldValue(body, "reason"); v != "" {
		parts = append(parts, "reason="+v)
	}
	if v := miFieldValue(body, "func"); v != "" {
		parts = append(parts, "func="+v)
	}
	if v := miFieldValue(body, "file"); v != "" {
		parts = append(parts, "file="+v)
	}
	if v := miFieldValue(body, "line"); v != "" {
		parts = append(parts, "line="+v)
	}
	if len(parts) == 0 {
		return "[stopped]\n"
	}
	return "[stopped: " + strings.Join(parts, " ") + "]\n"
}

// miFieldValue pulls the value of the first `key="..."` occurrence out of an MI
// record body, C-unescaping the quoted value. Returns "" when the key is absent.
// It is a deliberately small extractor — not a full MI tuple parser — sufficient
// for surfacing the handful of stop-frame fields asyncSummary reports.
func miFieldValue(body, key string) string {
	needle := key + "=\""
	i := strings.Index(body, needle)
	if i < 0 {
		return ""
	}
	rest := body[i+len(needle)-1:] // keep the opening quote for unescapeMIString
	return unescapeMIString(rest)
}

// firstNonEmptyLine returns the first non-blank line of s (the gdb version line
// captured from the startup banner), trimmed of trailing whitespace.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			return t
		}
	}
	return ""
}

// unescapeMIString C-unescapes a GDB/MI quoted string payload. The input may
// include its surrounding double quotes (the usual MI stream-record shape); they
// are stripped. It decodes the escapes MI emits: \n \t \r \" \\ \a \b \f \v, the
// octal escape gdb uses for ESC (\033) and other control bytes, and leaves an
// unrecognized escape's following byte verbatim. Robust to a missing closing
// quote (partial line).
func unescapeMIString(s string) string {
	// Strip the leading quote if present. The value ends at the FIRST UNESCAPED
	// closing quote, which the loop below detects — NOT the last quote in the
	// input. This matters because miFieldValue passes the whole rest of an MI
	// record (e.g. `add",file="test.cpp",line="2"`); a LastIndexByte scan would
	// over-read past this field's closing quote and splice in the trailing
	// fields, producing garbled stop summaries like `func=add",file=...`. For a
	// well-formed single quoted stream record the first unescaped closing quote
	// IS the last quote, so that caller is unaffected.
	if len(s) >= 1 && s[0] == '"' {
		s = s[1:]
	}

	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' {
			// First unescaped closing quote terminates the value. (An escaped
			// quote `\"` is consumed by the backslash branch below before it can
			// reach this check, so only a real closing quote breaks here.)
			break
		}
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch e := s[i]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			// Octal escape (gdb emits \033 for ESC and similar control bytes).
			// Consume up to three octal digits.
			val := int(e - '0')
			n := 1
			for n < 3 && i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '7' {
				i++
				val = val*8 + int(s[i]-'0')
				n++
			}
			b.WriteByte(byte(val))
		default:
			// Unknown escape: emit the escaped byte verbatim (drop the backslash).
			b.WriteByte(e)
		}
	}
	return b.String()
}
