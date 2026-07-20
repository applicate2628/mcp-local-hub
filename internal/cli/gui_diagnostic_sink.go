package cli

import (
	"io"
	"os"
	"strings"
	"sync"

	"mcp-local-hub/internal/api"
)

// guiConsoleReleasedDiagnosticEvent is the hub-mcp event id carrying a GUI
// runtime diagnostic that was written AFTER the console was released and
// would otherwise have gone to a closed console handle.
const guiConsoleReleasedDiagnosticEvent = "gui-console-released-diagnostic"

// consoleReleaseSink is the ONE writer every GUI runtime diagnostic goes
// through, and the reason there is a single one is the failure it exists
// to prevent.
//
// process.ReleaseParentConsole closes this process's console handles. Every
// later os.Stdout / os.Stderr write on a terminal-launched GUI then goes to
// a dead handle, and because every call site is a fmt.Fprintf whose error
// is discarded, the message vanishes with no trace and no error. That took
// out, among others, the supervisor exit-attribution line — the only record
// of a supervisor crash that happens before the supervisor's own audit log
// exists.
//
// Fixing that per-call-site would mean editing every writer and trusting
// every future one to remember. Instead the sink is installed as the
// command's out/err writer, so existing AND future `cmd.OutOrStderr()`
// sites are covered without knowing this exists.
//
// Post-release it DUAL-WRITES: durable hub-mcp event first, then the
// original stream. The second write matters — FreeConsole closes console
// handles but does NOT touch pipe or file handles, so a redirected run
// (`mcphub gui > log 2>&1`, `mcphub gui | tee`) keeps receiving exactly
// what it received before. Ordering matters too: the durable write happens
// first so a dead-handle failure on the fallback can never cost us the
// record. This mirrors the respawn-cap / respawn-failed paths, which
// already dual-write to LogHubMcpEvent and stderr.
//
// NOT covered: Go runtime panic output. The runtime writes it to file
// descriptor 2 directly rather than through any io.Writer, so no writer
// swap can intercept it; after a console release a GUI panic still leaves
// no trace on a terminal launch. Capturing it needs the process's stderr
// FD re-pointed at a file, which is a different mechanism and a separate
// change.
type consoleReleaseSink struct {
	mu       sync.Mutex
	fallback io.Writer
	durable  bool
}

func newConsoleReleaseSink(fallback io.Writer) *consoleReleaseSink {
	return &consoleReleaseSink{fallback: fallback}
}

// setFallback points the sink at the stream it should keep forwarding to.
//
// The identity guard is load-bearing, not defensive noise: the install
// sequence in startGuiServerWithStartup captures cmd.OutOrStderr() and
// then calls cmd.SetErr(sink), so a SECOND GUI start in the same process
// (an in-process test) would otherwise hand the sink itself as its own
// fallback and every write would recurse until the stack died.
func (s *consoleReleaseSink) setFallback(w io.Writer) {
	if w == io.Writer(s) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fallback = w
}

// engageDurableSink switches the sink into post-console-release mode. It is
// called at, and only at, the console release — one-way and idempotent,
// matching ReleaseParentConsole itself.
func (s *consoleReleaseSink) engageDurableSink() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.durable = true
}

// Write always reports success for the full buffer. Callers are
// fmt.Fprintf diagnostics that discard the error anyway, and reporting a
// short write or an error would be worse than useless: it cannot be acted
// on, and on a released console the fallback failing is the EXPECTED state
// rather than a fault. The durable write is where delivery is actually
// guaranteed.
func (s *consoleReleaseSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	durable, fallback := s.durable, s.fallback
	s.mu.Unlock()

	if durable {
		// One fmt.Fprintf produces exactly one Write of the whole
		// formatted message, so one event per operator-facing line.
		if msg := strings.TrimRight(string(p), "\r\n"); strings.TrimSpace(msg) != "" {
			_ = api.LogHubMcpEvent("warn", guiConsoleReleasedDiagnosticEvent, map[string]any{
				"message": msg,
			})
		}
	}
	if fallback != nil {
		_, _ = fallback.Write(p)
	}
	return len(p), nil
}

// guiRuntimeStdout / guiRuntimeStderr are the process-wide GUI diagnostic
// sinks.
//
// Process-global mutable state in a library package is normally a defect,
// so the justification is explicit: the console is itself a process-global
// resource, exactly one GUI runs per process (the single-instance flock
// guarantees it), and the alternative — threading a sink through the
// supervisor owner, the respawn manager and the spawnSupervisorFn seam —
// would change a test-facing seam signature to carry a value that is
// process-scoped by nature. They are installed by
// startGuiServerWithStartup and are pass-through no-ops until the console
// is actually released.
var (
	guiRuntimeStdout = newConsoleReleaseSink(os.Stdout)
	guiRuntimeStderr = newConsoleReleaseSink(os.Stderr)
)

// installGUIRuntimeSinks routes the GUI command's output through the
// switchable sinks. Capturing the current writers BEFORE SetOut/SetErr
// preserves whatever a caller (or a test) injected as the real target.
func installGUIRuntimeSinks(cmd interface {
	OutOrStdout() io.Writer
	OutOrStderr() io.Writer
	SetOut(io.Writer)
	SetErr(io.Writer)
}) {
	guiRuntimeStdout.setFallback(cmd.OutOrStdout())
	guiRuntimeStderr.setFallback(cmd.OutOrStderr())
	cmd.SetOut(guiRuntimeStdout)
	cmd.SetErr(guiRuntimeStderr)
}

// releaseConsoleForBackgroundGUI performs the console release and the sink
// switch as ONE step, because they are one decision: after this returns,
// console-backed stdio is dead and the durable sink is the reason the next
// diagnostic still lands somewhere.
func releaseConsoleForBackgroundGUI(release func()) {
	release()
	guiRuntimeStdout.engageDurableSink()
	guiRuntimeStderr.engageDurableSink()
}
