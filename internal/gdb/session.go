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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
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

// terminateGrace bounds observation of GDB shutdown before returning pending
// or applying the explicit termination fallback.
const terminateGrace = 2 * time.Second

type parentPipe interface {
	readPipe
	writePipe
}

type readPipe interface {
	io.ReadCloser
	SetReadDeadline(time.Time) error
}

type writePipe interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
}

// session owns a single GDB child and the parent's two pipe endpoints.
type session struct {
	readGate chan struct{} // serializes commands and owns the MI reader
	stdin    writePipe
	stdout   readPipe
	reader   *bufio.Reader

	writeGate          chan struct{} // one context-bound write at a time
	stateMu            sync.Mutex
	closing            chan struct{}
	processDone        chan struct{} // the sole Wait has returned
	done               chan struct{} // the Wait and pipe close outcomes are available
	result             sessionSettlement
	removeFromRegistry func()
	kill               func() error
	forceTried         bool
	forceErr           error
	exitWriteErr       error

	gdbPath string
	version string
}

type sessionSettlement struct {
	state   *os.ProcessState
	waitErr error
	pipeErr error
}

func (r sessionSettlement) settled() bool {
	return r.state != nil && r.pipeErr == nil
}

func (r sessionSettlement) err() error {
	if r.state == nil {
		return errors.Join(errors.New("gdb process exit is unconfirmed"), r.waitErr, r.pipeErr)
	}
	return errors.Join(r.waitErr, r.pipeErr)
}

func newSession(stdin writePipe, stdout readPipe, kill func() error) *session {
	return &session{
		stdin: stdin, stdout: stdout, reader: bufio.NewReader(stdout), kill: kill,
		readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1), closing: make(chan struct{}), processDone: make(chan struct{}), done: make(chan struct{}),
	}
}

// startSessionFunc is the injectable seam server.go uses to create a session.
// The production implementation is startSession; tests substitute a fake that
// returns a session backed by a canned reader so no real gdb is spawned.
type startSessionFunc func(ctx context.Context, gdbPath, program string) (*session, error)

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
func startSession(ctx context.Context, gdbPath, program string) (*session, error) {
	if err := ctx.Err(); err != nil {
		return nil, context.Cause(ctx)
	}
	if gdbPath == "" {
		gdbPath = toolchain.DefaultGdbPath()
	}

	args := gdbStartArgs(program)

	cmd := exec.Command(gdbPath, args...)
	process.NoConsole(cmd) // suppress console flash on a windowsgui parent

	stdin, childInput, err := debuggerPipe(false)
	if err != nil {
		return nil, fmt.Errorf("gdb stdin pipe: %w", err)
	}
	stdout, childOutput, err := debuggerPipe(true)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("gdb stdout pipe: %w", err), childInput.Close(), stdin.Close())
	}
	// Both parent files must support interruption before we acquire a child.
	if err := errors.Join(stdin.SetWriteDeadline(time.Time{}), stdout.SetReadDeadline(time.Time{}), context.Cause(ctx)); err != nil {
		return nil, errors.Join(fmt.Errorf("arm gdb pipes: %w", err),
			childInput.Close(), stdin.Close(), childOutput.Close(), stdout.Close())
	}
	// Direct files leave the parent endpoints with this session. Cmd.Wait
	// cannot close the reader while it still has MI records to consume.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = childInput, childOutput, childOutput
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start gdb (%s): %w", gdbPath, err),
			childInput.Close(), stdin.Close(), childOutput.Close(), stdout.Close())
	}

	s := newSession(stdin, stdout, cmd.Process.Kill)
	s.gdbPath = gdbPath
	childCloseErr := errors.Join(childInput.Close(), childOutput.Close())
	s.result.pipeErr = childCloseErr
	go s.reap(func() (*os.ProcessState, error) {
		err := cmd.Wait()
		return cmd.ProcessState, err
	})

	return s, s.completeStartup(ctx, childCloseErr)
}

// completeStartup keeps the acquired owner available to the registry even
// when initialization or its cleanup fails.
func (s *session) completeStartup(ctx context.Context, startErr error) error {
	if startErr == nil {
		startErr = s.initialize(ctx)
	}
	if startErr != nil {
		return errors.Join(startErr, s.terminate(context.Background(), terminateGrace))
	}
	return nil
}

func (s *session) initialize(ctx context.Context) error {
	if banner, err := s.startupBanner(ctx); err != nil {
		if banner != "" {
			return fmt.Errorf("gdb startup banner: %s: %w", banner, err)
		}
		return fmt.Errorf("gdb startup banner: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for _, command := range []string{"-gdb-set confirm off", "-gdb-set pagination off"} {
		if _, err := s.Command(ctx, command); err != nil {
			return fmt.Errorf("%s: %w", command, err)
		}
	}
	// -q suppresses the splash. Ask the already-started MI session so the
	// version read uses the same deadlines and owner as the setup commands.
	var console strings.Builder
	reply, err := s.command(ctx, "-gdb-version", &console)
	if err != nil {
		return fmt.Errorf("-gdb-version: %w", err)
	}
	if err := context.Cause(ctx); err != nil {
		return fmt.Errorf("-gdb-version: %w", err)
	}
	if s.isClosing() {
		return errors.New("gdb exited during startup")
	}
	switch reply.class {
	case "done":
		s.version = firstNonEmptyLine(console.String())
	case "error":
		// A completed MI refusal leaves the established session usable.
	default:
		return fmt.Errorf("-gdb-version: unexpected MI result %q", reply.class)
	}
	return nil
}

func (s *session) startupBanner(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, runResultDeadline)
	defer cancel()
	if err := s.acquireReader(ctx); err != nil {
		return "", err
	}
	defer s.releaseReader()
	finish, err := armPipeDeadline(ctx, s.stdout.SetReadDeadline)
	if err != nil {
		return "", err
	}
	out, err := s.readUntilPrompt(ctx)
	return out, errors.Join(err, finish())
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

var errSessionClosing = errors.New("gdb session is closing")

// miReply keeps the parsed result separate from transport and settlement errors.
type miReply struct {
	output string
	class  string
	record string
}

// Command reads one result with exclusive ownership of the MI reader. Once
// terminal evidence is observed, the caller also observes session settlement.
func (s *session) Command(ctx context.Context, command string) (string, error) {
	reply, err := s.command(ctx, command, nil)
	if reply.class == "error" {
		err = errors.Join(errors.New(miErrorMessage(reply.record)), err)
	}
	return reply.output, err
}

// commandTiming holds elapsed milestones for one diagnostic request. Writes
// and reads overlap; neither is assumed to complete first.
type commandTiming struct {
	entered, acquired, written, replied time.Time
}

type commandTimingKey struct{}

func (t *commandTiming) report(w io.Writer) {
	elapsed := func(at time.Time) int64 {
		if at.IsZero() {
			return -1
		}
		return at.Sub(t.entered).Microseconds()
	}
	// Fixed keys and durations only. Never include requests, session identity,
	// commands, output, paths, error strings, or wall-clock values.
	_, _ = fmt.Fprintf(w, "gdb-command-timing acquired_us=%d write_done_us=%d reply_done_us=%d total_us=%d\n",
		elapsed(t.acquired), elapsed(t.written), elapsed(t.replied), elapsed(time.Now()))
}

func (s *session) command(ctx context.Context, command string, console *strings.Builder) (miReply, error) {
	if s.isClosing() {
		return miReply{}, errSessionClosing
	}
	if err := s.acquireReader(ctx); err != nil {
		return miReply{}, err
	}
	timing, _ := ctx.Value(commandTimingKey{}).(*commandTiming)
	if timing != nil {
		timing.acquired = time.Now()
	}
	reply, err := s.runCommand(ctx, command, console, timing)
	s.releaseReader()
	if !s.isClosing() {
		return reply, err
	}

	ctx, cancel := context.WithTimeout(ctx, terminateGrace)
	defer cancel()
	settlement, awaitErr := s.awaitSettlement(ctx)
	if awaitErr != nil {
		return reply, errors.Join(err, awaitErr)
	}
	return reply, errors.Join(err, settlement.err())
}

// runCommand owns the serialized MI reader for the entire write/read exchange.
func (s *session) runCommand(ctx context.Context, command string, console *strings.Builder, timing *commandTiming) (miReply, error) {
	ctx, cancel := context.WithTimeout(ctx, runResultDeadline)
	defer cancel()
	if s.isClosing() {
		return miReply{}, errSessionClosing
	}
	finish, err := armPipeDeadline(ctx, s.stdout.SetReadDeadline)
	if err != nil {
		return miReply{}, err
	}
	// Admit one write before reading. The peer may be blocked writing the
	// preceding prompt, so complete the read and write concurrently.
	write, err := s.prepareWrite(ctx, command, false)
	if err != nil {
		return miReply{}, errors.Join(err, finish())
	}
	writeDone := make(chan error, 1)
	go func() {
		err := write()
		if timing != nil {
			timing.written = time.Now() // published through writeDone before reporting
		}
		if err != nil {
			cancel()
		}
		writeDone <- err
	}()
	awaitWrite := sync.OnceValue(func() error { return <-writeDone })
	// A terminal read must not retire the session before the admitted
	// write has settled. On a read error, interrupt that write first.
	closeAfterWrite := func(readFailed bool) {
		if readFailed {
			cancel()
		}
		_ = awaitWrite()
		s.beginClosing()
	}
	reply, readErr := s.readResult(ctx, console, closeAfterWrite)
	if timing != nil {
		timing.replied = time.Now()
	}
	return reply, errors.Join(readErr, awaitWrite(), finish())
}

// armPipeDeadline sets the absolute I/O bound before entering the kernel. On
// cancellation, advance the same deadline without taking the reader or writer
// lock. Join the callback before another operation can rearm this file.
func armPipeDeadline(ctx context.Context, setDeadline func(time.Time) error) (func() error, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("missing gdb I/O deadline")
	}
	if err := setDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set gdb I/O deadline: %w", err)
	}
	interrupted := make(chan struct{})
	var interruptErr error
	stop := context.AfterFunc(ctx, func() {
		if err := setDeadline(time.Now()); err != nil {
			interruptErr = fmt.Errorf("interrupt gdb I/O: %w", err)
		}
		close(interrupted)
	})
	return func() error {
		if stop() {
			return nil
		}
		<-interrupted
		return interruptErr
	}, nil
}

func (s *session) writeCommand(ctx context.Context, command string, graceful bool) error {
	write, err := s.prepareWrite(ctx, command, graceful)
	if err != nil {
		return err
	}
	return write()
}

// prepareWrite reserves the endpoint until its returned one-shot write exits.
func (s *session) prepareWrite(ctx context.Context, command string, graceful bool) (func() error, error) {
	select {
	case s.writeGate <- struct{}{}:
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
	if (!graceful && s.isClosing()) || (graceful && s.hasWaitResult()) {
		<-s.writeGate
		return nil, errSessionClosing
	}
	finish, err := armPipeDeadline(ctx, s.stdin.SetWriteDeadline)
	if err != nil {
		<-s.writeGate
		return nil, err
	}
	command = strings.TrimRight(command, "\r\n")
	return func() error {
		defer func() { <-s.writeGate }()
		_, err := io.WriteString(s.stdin, command+"\n")
		err = errors.Join(err, finish())
		if err != nil {
			s.beginClosing()
			return fmt.Errorf("write gdb command %q: %w", command, errors.Join(err, context.Cause(ctx)))
		}
		return nil
	}, nil
}

func (s *session) hasWaitResult() bool {
	select {
	case <-s.processDone:
		return true
	default:
		return false
	}
}

func (s *session) isClosing() bool {
	select {
	case <-s.closing:
		return true
	default:
		return false
	}
}

func (s *session) beginClosing() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.markClosing()
}

// markClosing is called with stateMu held.
func (s *session) markClosing() {
	if !s.isClosing() {
		close(s.closing)
	}
}

func (s *session) reap(wait func() (*os.ProcessState, error)) {
	state, waitErr := wait()
	s.stateMu.Lock()
	s.markClosing()
	s.result.state = state
	s.result.waitErr = waitErr
	close(s.processDone)
	s.stateMu.Unlock()

	// Any reader that started before closing may finish with the original
	// stream. Wait itself never touches these parent-owned endpoints.
	s.readGate <- struct{}{}
	s.writeGate <- struct{}{}
	pipeErr := errors.Join(s.stdin.Close(), s.stdout.Close())
	<-s.writeGate
	s.releaseReader()

	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.result.pipeErr = errors.Join(s.result.pipeErr, pipeErr)
	if s.result.settled() && s.removeFromRegistry != nil {
		s.removeFromRegistry()
		s.removeFromRegistry = nil
	}
	close(s.done)
}

func (s *session) onSettled(forget func()) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	select {
	case <-s.done:
		if s.result.settled() {
			forget()
		}
	default:
		s.removeFromRegistry = forget
	}
}

func (s *session) awaitSettlement(ctx context.Context) (sessionSettlement, error) {
	select {
	case <-s.done:
		return s.result, nil
	default:
	}
	select {
	case <-s.done:
		return s.result, nil
	case <-ctx.Done():
		return sessionSettlement{}, fmt.Errorf("gdb session settlement pending: %w", context.Cause(ctx))
	}
}

func (s *session) acquireReader(ctx context.Context) error {
	select {
	case s.readGate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func (s *session) releaseReader() { <-s.readGate }

// drainExitOutput lets GDB finish emitting MI output (including its exit
// response). Only the session's current reader owns the pipe; this receiver
// takes over within the same grace period if an earlier command is finishing.
func (s *session) drainExitOutput(ctx context.Context) {
	if s.acquireReader(ctx) != nil {
		return
	}
	defer s.releaseReader()
	finish, err := armPipeDeadline(ctx, s.stdout.SetReadDeadline)
	if err != nil {
		return
	}
	// The settlement result comes from the original Wait, not from the
	// drain. EOF and deadline expiration are normal ends of this receiver.
	_, _ = io.Copy(io.Discard, s.reader)
	_ = finish()
}

// terminate is the explicit owner cleanup path. It sends at most one graceful
// exit and escalates only against the original child, at most once.
func (s *session) terminate(ctx context.Context, grace time.Duration) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	graceCtx, cancel := context.WithTimeout(ctx, grace)

	s.stateMu.Lock()
	requestExit := !s.isClosing()
	s.markClosing()
	s.stateMu.Unlock()
	var drained chan struct{}
	if requestExit {
		drained = make(chan struct{})
		go func() {
			defer close(drained)
			s.drainExitOutput(graceCtx)
		}()
		if err := s.writeCommand(graceCtx, "-gdb-exit", true); err != nil && !errors.Is(err, errSessionClosing) {
			s.stateMu.Lock()
			s.exitWriteErr = fmt.Errorf("request gdb exit: %w", err)
			s.stateMu.Unlock()
		}
	}

	result, err := s.awaitSettlement(graceCtx)
	cancel()
	if drained != nil {
		<-drained
	}
	if err == nil {
		return s.terminationResult(result)
	}
	if ctx.Err() != nil {
		return err
	}

	s.stateMu.Lock()
	select {
	case <-s.processDone:
		// A returned Wait is never replaced by a second process operation.
	default:
		if ctx.Err() == nil && !s.forceTried {
			s.forceTried = true
			if err := s.kill(); err != nil {
				s.forceErr = fmt.Errorf("force gdb exit: %w", err)
			}
		}
	}
	forceErr, exitWriteErr := s.forceErr, s.exitWriteErr
	s.stateMu.Unlock()
	if forceErr != nil && !errors.Is(forceErr, os.ErrProcessDone) {
		return errors.Join(exitWriteErr, forceErr)
	}

	forceCtx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	result, err = s.awaitSettlement(forceCtx)
	if err != nil {
		return errors.Join(exitWriteErr, forceErr, err)
	}
	return s.terminationResult(result)
}

func (s *session) terminationResult(r sessionSettlement) error {
	if r.settled() {
		// The request's postcondition is a released owner, including a
		// non-zero exit status from the supported forced shutdown.
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return errors.Join(s.exitWriteErr, s.forceErr, r.err())
}

// resultRecord recognizes a result class after the optional numeric MI token.
func (s *session) resultRecord(line string, onClosing func(bool)) (class, record string, ok bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == len(line) || line[i] != '^' {
		return "", "", false
	}
	record = line[i+1:]
	class, _, _ = strings.Cut(record, ",")
	if class == "exit" && !s.isClosing() {
		onClosing(false)
	}
	return class, record, true
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
func (s *session) readResult(ctx context.Context, console *strings.Builder, onClosing func(bool)) (miReply, error) {
	var out strings.Builder

	for {
		line, err := s.readLine(ctx, onClosing)
		if err != nil {
			out.WriteString(line)
			return miReply{output: out.String()}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || line == "(gdb) " || line == "(gdb)" {
			continue
		}

		if class, record, ok := s.resultRecord(line, onClosing); ok {
			if class == "running" {
				stop, err := s.readUntilStop(ctx, onClosing)
				out.WriteString(stop)
				return miReply{output: out.String(), class: class, record: record}, err
			}
			return miReply{output: out.String(), class: class, record: record}, nil
		}

		switch line[0] {
		case '~', '@':
			// Console / target output stream: payload is a C-quoted string.
			text := unescapeMIString(stripStreamPrefix(line))
			out.WriteString(text)
			if console != nil && line[0] == '~' {
				console.WriteString(text)
			}
		case '&':
			// Log stream (gdb's own echo / diagnostics): not user output.
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
// stream output and a human-readable stop summary.
func (s *session) readUntilStop(ctx context.Context, onClosing func(bool)) (string, error) {
	var out strings.Builder
	for {
		line, err := s.readLine(ctx, onClosing)
		if err != nil {
			out.WriteString(line)
			return out.String(), err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || line == "(gdb) " || line == "(gdb)" {
			continue
		}

		if class, record, ok := s.resultRecord(line, onClosing); ok {
			if class == "error" {
				return out.String(), errors.New(miErrorMessage(record))
			}
			return out.String(), nil
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
		}
	}
}

// readUntilPrompt drains records up to and including the next `(gdb) ` prompt,
// returning the console/target stream text seen along the way. Used to consume
// the MI startup banner.
func (s *session) readUntilPrompt(ctx context.Context) (string, error) {
	onClosing := func(bool) { s.beginClosing() }
	var out strings.Builder
	for {
		line, err := s.readLine(ctx, onClosing)
		if err != nil {
			out.WriteString(line)
			return out.String(), err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "(gdb) " || trimmed == "(gdb)" {
			return out.String(), nil
		}
		if class, _, ok := s.resultRecord(trimmed, onClosing); ok && class == "exit" {
			return out.String(), errors.New("gdb exited before its startup prompt")
		}
		if len(trimmed) > 0 && (trimmed[0] == '~' || trimmed[0] == '@') {
			out.WriteString(unescapeMIString(stripStreamPrefix(trimmed)))
		}
	}
}

// readLine returns any incomplete bytes with their error. Callers may report
// those bytes, but only a complete line can affect MI parsing.
func (s *session) readLine(ctx context.Context, onClosing func(bool)) (string, error) {
	if err := context.Cause(ctx); err != nil {
		onClosing(true)
		return "", err
	}
	line, err := s.reader.ReadString('\n')
	if err == nil {
		return line, nil
	}
	onClosing(true)
	if errors.Is(err, io.EOF) {
		return line, fmt.Errorf("gdb closed its output stream before the result: %w", errors.Join(err, context.Cause(ctx)))
	}
	return line, fmt.Errorf("read gdb output: %w", errors.Join(err, context.Cause(ctx)))
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

// firstNonEmptyLine returns the first non-blank line with surrounding whitespace removed.
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
