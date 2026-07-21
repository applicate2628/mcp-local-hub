package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mcp-local-hub/internal/api"
)

// supervisorStderrSink is the supervisor's captured-stderr destination.
//
// PROBLEM IT SOLVES. The Go runtime writes an unrecovered panic (`panic:
// ...` + goroutine traceback) to file descriptor 2 and then exits 2. It
// does NOT run deferred functions on other goroutines, does not reach any
// Emit call, and on Windows produces no Windows Error Reporting record. In
// the 2026-07-20 forensic window the supervisor died 8 times out of 9
// starts with no `supervisor-exit` row and no `supervisor-handler-panic`
// row — and its stderr was bound to nothing, so the runtime's own message
// went nowhere. Pointing the OS-level stderr handle at a file makes that
// message durable.
//
// VERIFIED, not assumed: on go1.26.5/windows-amd64 the Go runtime resolves
// the stderr handle per write via GetStdHandle, so a SetStdHandle performed
// after process start IS honored by the runtime's panic writer. Probed
// 2026-07-20 — a panic after the redirect landed in full (message + stack)
// in the sink file with exit status 2.
//
// INTERACTIVE VS DETACHED. The redirect fires ONLY when the supervisor's
// stderr is not a console. An operator running `mcphub supervise` in a
// terminal keeps their stderr exactly as it was — output they are watching
// is never swallowed, and an interactive crash is already visible to them
// (there is no forensic gap in that case). The gap is specific to the
// detached shapes — the autostart Task Scheduler shim, the liveness task,
// and GUI-spawned supervisors — where stderr is a pipe, a file, or an
// invalid handle, and that is exactly where the redirect applies.
//
// Teeing to both console and file was considered and REJECTED: teeing
// requires a pipe plus a pump goroutine, and on a runtime panic the process
// exits immediately after the write, so the pump may never be scheduled and
// the panic message would be lost in the pipe buffer — losing precisely the
// event the sink exists to capture.
type supervisorStderrSink struct {
	// path is the absolute sink path, valid even when no redirect happened
	// (so the heartbeat's oversize probe and the audit row can name it).
	path string
	// file is the open sink handle, nil when no redirect happened.
	file *os.File
	// redirected reports whether this process's stderr now points at path.
	redirected bool
	// reason explains the disposition for the audit row: "redirected",
	// "interactive-console", or "redirect-failed".
	reason string
	// rotated reports whether an oversize sink was rotated to .1 at open.
	rotated bool
	// err carries a non-fatal redirect failure for the audit row.
	err error
	// saved is the process stderr binding this sink displaced. Retained so
	// release() can restore it — see release() for why that matters even
	// though the supervisor normally exits immediately afterwards.
	saved savedStderrBinding
}

// supervisorStderrSinkHint renders the sink's absolute path for an operator
// -facing diagnostic, degrading to the bare leaf name when the state dir
// cannot be resolved (a diagnostic must never itself fail).
//
// It exists because the GUI's supervisor owner captures the spawned child's
// stderr into a bounded buffer to surface startup crashes (PR #212 r5), and
// the child rebinds its own stderr to the sink once it passes the singleton
// lock. Post-lock crash text therefore never reaches that buffer, so an
// empty tail is NOT evidence of silence — the diagnostic has to say where
// the text actually went.
//
// TEEING WAS RECONSIDERED FOR THIS CASE AND STILL REJECTED, for a reason
// that survives the different context: the GUI's buffer is filled across a
// pipe by the parent, so the child's traceback would have to be written,
// flushed through the pipe, and read by the parent before the runtime
// terminates the child. That is the same unscheduled-pump race that ruled
// teeing out for the panic path — and it fails in exactly the case that
// matters most (a panic). A pointer to a file that is guaranteed to hold
// the text is strictly more reliable than a tee that is guaranteed only
// most of the time.
func supervisorStderrSinkHint() string {
	stateDir, err := stateDirFunc()
	if err != nil || stateDir == "" {
		return api.SupervisorStderrSinkFileLeaf
	}
	return filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf)
}

// openSupervisorStderrSink points this process's stderr at
// <stateDir>/supervisor-stderr.log unless stderr is an interactive console.
//
// It NEVER returns an error that should abort supervisor startup: losing
// stderr capture degrades forensics, it does not degrade supervision, and a
// supervisor that refuses to start because a log file could not be opened
// would be a strictly worse outcome than the gap it is closing. The
// disposition is reported through the returned value for the caller to
// audit.
//
// MUST be called during single-goroutine startup, before any `go` statement:
// it assigns the os.Stderr package variable, which is only race-free while
// no other goroutine can be reading it.
func openSupervisorStderrSink(stateDir string) *supervisorStderrSink {
	sink := &supervisorStderrSink{
		path: filepath.Join(stateDir, api.SupervisorStderrSinkFileLeaf),
	}

	// Interactive check FIRST — never touch a console the operator is
	// watching, and never rotate a sink we are not going to write.
	if stderrIsInteractiveConsole() {
		sink.reason = "interactive-console"
		return sink
	}

	rotated, rotErr := api.RotateSupervisorStderrSinkIfOversize(sink.path)
	sink.rotated = rotated
	if rotErr != nil {
		// A failed rotation is not a reason to skip capture — append to the
		// oversize file rather than losing the next panic. The heartbeat's
		// oversize probe keeps the condition visible.
		sink.err = rotErr
	}

	f, err := os.OpenFile(sink.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		sink.reason = "redirect-failed"
		sink.err = fmt.Errorf("open supervisor stderr sink %s: %w", sink.path, err)
		return sink
	}

	// Capture the binding we are about to displace BEFORE displacing it, so
	// release() can restore it. Failure to capture is not fatal — capture is
	// only needed for orderly release, and losing panic capture would be the
	// worse outcome — but it is recorded for the audit row.
	saved, captureErr := captureStderrBinding()
	if captureErr != nil {
		sink.err = captureErr
	}
	sink.saved = saved

	if err := redirectProcessStderr(f); err != nil {
		_ = f.Close()
		sink.reason = "redirect-failed"
		sink.err = fmt.Errorf("redirect stderr to %s: %w", sink.path, err)
		return sink
	}

	// Point the Go-level variable at the same file so code writing through
	// os.Stderr and the runtime writing through raw fd 2 agree on one
	// destination and one file offset (both are O_APPEND).
	os.Stderr = f
	sink.file = f
	sink.redirected = true
	sink.reason = "redirected"

	// Session banner. This is load-bearing forensics, not decoration: the
	// sink is otherwise empty on a healthy host, so consecutive banners with
	// no exit marker between them are themselves the evidence that a session
	// died without a graceful path. It also gives every captured panic an
	// unambiguous owning pid and start time.
	fmt.Fprintf(f, "=== mcphub supervisor stderr sink | pid=%d | session-start=%s ===\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))

	return sink
}

// release restores the displaced process stderr binding and closes the
// sink file. It is idempotent and safe on a sink that never redirected.
//
// WHY THIS EXISTS even though the supervisor normally exits right after.
// The sink holds a process-wide OS resource (a rebound std handle / fd 2)
// plus an open file handle. "The process is about to exit anyway" is true
// of the production path but NOT of the resource's contract: runSupervise
// returns in tests, and on Windows an unreleased handle makes the sink file
// undeletable, which broke every TestRunSupervise_* temp-dir cleanup with
// "The process cannot access the file because it is being used by another
// process". A resource with no owner and no release path is a defect even
// when one caller happens not to notice.
//
// ORDER IS LOAD-BEARING: restore the binding BEFORE closing the file.
// Closing first would leave stderr pointing at a closed handle for the
// window in between, so a panic in that window would be lost — and lost
// silently, which is the failure mode this whole change exists to remove.
// THE fd-2 EXCEPTION (POSIX). When the parent handed us a process with fd 2
// closed, os.OpenFile hands the sink the lowest free descriptor — which is 2
// itself, the degenerate case redirectProcessStderr accepts as already-bound.
// In that case s.file IS fd 2, so the Close() below would close the very
// descriptor restore() just rebound, leaving the process with no stderr at
// all and — worse — with fd 2 free for the next open() to claim, so a later
// stray write to os.Stderr would land in an unrelated file. We therefore skip
// the close and leave fd 2 bound to the sink: strictly safer, since the only
// cost is one descriptor held for the process lifetime while any stray stderr
// write goes somewhere forensically useful. Always false on Windows, where
// stderr is bound by HANDLE rather than by descriptor number.
func (s *supervisorStderrSink) release() {
	if s == nil || !s.redirected {
		return
	}
	_ = s.saved.restore()
	if s.file != nil {
		if !sinkOwnsProcessStderrFD(s.file) {
			_ = s.file.Close()
		}
		s.file = nil
	}
	s.redirected = false
}

// releaseOnExit is the deferred owner of the sink's exit behavior for
// runSupervise. It MUST be deferred directly by runSupervise so its
// recover() sees a main-goroutine panic.
//
// THE BUG THIS FIXES (adversarial review, 2026-07-20). A plain
// `defer sink.release()` looks correct and is not: Go runs the panicking
// goroutine's deferred functions BEFORE the runtime prints the traceback.
// So on a MAIN-goroutine panic the release fired first, restored the
// original stderr — which under detached autostart is bound to nothing —
// and the traceback then went to the void. The sink was blind to panics on
// the one goroutine that runs the whole of startup, the final select loop,
// and every signal / IPC-exit handler.
//
// Independently reproduced on windows-amd64 before fixing:
//
//	main-defer       panic-in-sink=NO   panic-in-original-stderr=YES
//	main-nodefer     panic-in-sink=YES  panic-in-original-stderr=NO
//	goroutine-defer  panic-in-sink=YES  panic-in-original-stderr=NO
//
// The existing subprocess test panicked on a BACKGROUND goroutine (the
// third row), which is exactly why it could not see the hole.
//
// THE CONTRACT, in three parts:
//
//  1. When unwinding, do NOT restore and do NOT close. Leaving the sink
//     bound is the whole point — the runtime prints into it moments later.
//     Losing the handle here is not a leak worth fixing: the process is
//     about to be terminated by the runtime.
//  2. NEVER swallow. The recovered value is re-raised, so the panic value,
//     the traceback, exit status 2, and the recovery layer's respawn all
//     behave exactly as before this sink existed. Same contract as
//     guardSupervisorGoroutine.
//  3. On a non-panic ERROR return, write the error into the sink BEFORE
//     releasing. cobra prints the returned error only after runSupervise
//     returns, by which point the restore has already redirected it to the
//     detached void — so without this the sink would show a session banner
//     with no marker at all, indistinguishable from a hard death.
func (s *supervisorStderrSink) releaseOnExit(errp *error) {
	if r := recover(); r != nil {
		s.noteMainGoroutinePanic(r)
		// Re-raise with the sink still bound so the runtime's traceback
		// lands in it. Deliberately no release: see part 1 above.
		panic(r)
	}
	if errp != nil && *errp != nil {
		s.noteErrorExit(*errp)
	}
	s.release()
}

// noteMainGoroutinePanic marks the sink immediately before the Go runtime
// prints the traceback, so an operator reading the file can tell a
// main-goroutine panic from a background-goroutine one (the latter also
// carries a `supervisor-goroutine-panic` event row; this one cannot,
// because the event log's own defer chain is already unwinding).
func (s *supervisorStderrSink) noteMainGoroutinePanic(r any) {
	if s == nil || s.file == nil {
		return
	}
	fmt.Fprintf(s.file, "=== mcphub supervisor stderr sink | pid=%d | MAIN-GOROUTINE PANIC=%v | at=%s | runtime traceback follows ===\n",
		os.Getpid(), r, time.Now().UTC().Format(time.RFC3339Nano))
}

// noteErrorExit records a non-panic error return. Without it an early
// startup failure (event-log open, supervise-startup-failed, overlay/IPC
// wiring) leaves the sink holding a bare session banner — visually
// identical to a hard death, which would send an operator hunting a crash
// that never happened.
func (s *supervisorStderrSink) noteErrorExit(err error) {
	if s == nil || s.file == nil || err == nil {
		return
	}
	fmt.Fprintf(s.file, "=== mcphub supervisor stderr sink | pid=%d | error-exit=%v | at=%s ===\n",
		os.Getpid(), err, time.Now().UTC().Format(time.RFC3339Nano))
}

// noteGracefulExit writes a closing banner so an operator reading the sink
// can tell a clean shutdown from a death. Absence of this marker before the
// next session banner is the positive signal that the previous session died
// without reaching any graceful exit path.
func (s *supervisorStderrSink) noteGracefulExit(cause string) {
	if s == nil || s.file == nil {
		return
	}
	fmt.Fprintf(s.file, "=== mcphub supervisor stderr sink | pid=%d | graceful-exit=%s | at=%s ===\n",
		os.Getpid(), cause, time.Now().UTC().Format(time.RFC3339Nano))
}

// auditBody renders the sink disposition for the startup audit row.
func (s *supervisorStderrSink) auditBody() map[string]any {
	body := map[string]any{
		"path":       s.path,
		"redirected": s.redirected,
		"reason":     s.reason,
		"rotated":    s.rotated,
	}
	if s.err != nil {
		body["err"] = s.err.Error()
	}
	return body
}

// severity is "warn" when stderr capture is NOT in place on a detached
// supervisor (the forensic gap is open) and "info" otherwise. An
// interactive console is info: the operator can see the output directly.
func (s *supervisorStderrSink) severity() string {
	if s.reason == "redirect-failed" {
		return "warn"
	}
	return "info"
}
