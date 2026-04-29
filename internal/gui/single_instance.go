package gui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

// ErrSingleInstanceBusy is returned by AcquireSingleInstance when another
// mcphub gui process already holds the lock. Callers should read the
// pidport file, probe the incumbent's /api/ping, and POST
// /api/activate-window before giving up.
var ErrSingleInstanceBusy = errors.New("another mcphub gui is already running")

// SingleInstanceLock represents the acquired single-instance ownership.
// Release must be called on shutdown (or by an errdefer immediately after
// acquisition) to free the flock. The pidport file is intentionally NOT
// removed on Release — see Release() for the rationale.
type SingleInstanceLock struct {
	pidport string
	fl      *flock.Flock
}

// AcquireSingleInstance tries to become the sole mcphub gui process for
// this user. On success it writes a pidport record at PidportPath() and
// returns a lock the caller must Release on shutdown.
//
// The lock is a flock-managed adjacent .lock file — the same pattern
// workspace-registry uses elsewhere in the codebase. It is NOT a Windows
// named kernel mutex; portability across Linux/macOS was favored over
// the tiny-but-theoretical advantage of kernel-level serialization on
// Windows alone.
func AcquireSingleInstance(port int) (*SingleInstanceLock, error) {
	p, err := PidportPath()
	if err != nil {
		return nil, err
	}
	return acquireSingleInstanceAt(p, port)
}

// acquireSingleInstanceAt is the injectable form used by tests.
func acquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	fl := flock.New(pidportPath + ".lock")
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("flock %s: %w", pidportPath+".lock", err)
	}
	if !ok {
		return nil, ErrSingleInstanceBusy
	}
	if err := os.WriteFile(pidportPath, []byte(formatPidport(os.Getpid(), port)), 0o600); err != nil {
		_ = fl.Unlock()
		return nil, fmt.Errorf("write pidport: %w", err)
	}
	return &SingleInstanceLock{pidport: pidportPath, fl: fl}, nil
}

// Release releases ONLY the flock — it does NOT remove the pidport file.
// Idempotent.
//
// Removing the pidport on Release is unsafe: a racing successor that
// acquires the flock (between our Unlock and Remove) and writes its own
// pidport would have its file deleted. Round 7 (unlock-first) and round
// 8 (ownership PID check before Remove) both left a TOCTOU window
// between the read and the remove. The flock is the source of truth for
// ownership; the pidport file is metadata that the next acquirer
// overwrites atomically via os.WriteFile in acquireSingleInstanceAt.
//
// Stale-file harmless because:
//   - No flock holder + listener gone → TryActivateIncumbent probes the
//     port → connection-refused → "incumbent unreachable" error surfaces
//     correctly to the caller.
//   - Next acquirer overwrites the file before any second-instance
//     handshake can read it.
func (l *SingleInstanceLock) Release() {
	if l == nil || l.fl == nil {
		return
	}
	_ = l.fl.Unlock()
	l.fl = nil
}

// ReadPidport reads "<PID> <PORT>\n" format. Returns (0,0,err) on parse
// failure or missing file. Second-instance callers use it to probe the
// incumbent.
func ReadPidport(path string) (pid, port int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Fields(string(b))
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("malformed pidport %q", string(b))
	}
	pid, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse pid: %w", err)
	}
	port, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse port: %w", err)
	}
	return pid, port, nil
}

func formatPidport(pid, port int) string {
	return fmt.Sprintf("%d %d\n", pid, port)
}

// AcquireSingleInstanceAt is the exported form of acquireSingleInstanceAt
// so callers outside the gui package (cli) can share the same path.
func AcquireSingleInstanceAt(pidportPath string, port int) (*SingleInstanceLock, error) {
	return acquireSingleInstanceAt(pidportPath, port)
}

// RewritePidportPort overwrites the pidport file with the current PID and
// the supplied port. Used by the CLI after Server.Start resolves an
// OS-assigned port (--port 0): the lock was acquired before bind with
// the originally requested port, but second-instance handshake probes
// need the actual bound port. The caller must hold the single-instance
// lock — the flock on *.lock gates ownership, the pidport file is
// ownership metadata the lock holder freely updates.
func RewritePidportPort(pidportPath string, port int) error {
	return os.WriteFile(pidportPath, []byte(formatPidport(os.Getpid(), port)), 0o600)
}

// WritePidport overwrites the pidport file with the supplied PID and
// port. Used by the CLI after Server.Start signals ready, called
// unconditionally so the takeover path (--force --kill) replaces the
// killed incumbent's PID + port with the current process's PID + bound
// port. Idempotent for the normal-acquire path (PID + port are already
// what AcquireSingleInstanceAt wrote, modulo --port 0 ephemeral
// resolution). The caller must hold the single-instance lock.
//
// Codex PR #23 P2 #2: replaces the previous conditional
// RewritePidportPort(actualPort) call which only fired when actualPort
// != requestedPort and only updated the port field — leaving the
// killed incumbent's PID stale in the pidport after a successful kill.
func WritePidport(pidportPath string, pid, port int) error {
	return os.WriteFile(pidportPath, []byte(formatPidport(pid, port)), 0o600)
}

// VerdictClass enumerates the result of Probe / KillRecordedHolder.
type VerdictClass int

const (
	VerdictHealthy         VerdictClass = iota // incumbent ping matches recorded PID
	VerdictLiveUnreachable                     // recorded PID is alive but not serving HTTP
	VerdictDeadPID                             // recorded PID does not exist
	VerdictMalformed                           // pidport is missing/garbage/incomplete
	VerdictKilledRecovered                     // KillRecordedHolder succeeded; new flock acquired
	VerdictKillRefused                         // three-part identity gate failed
	VerdictKillFailed                          // SIGKILL/TerminateProcess returned error
	VerdictRaceLost                            // post-kill, a competitor won the new acquire
)

func (c VerdictClass) String() string {
	switch c {
	case VerdictHealthy:
		return "Healthy"
	case VerdictLiveUnreachable:
		return "LiveUnreachable"
	case VerdictDeadPID:
		return "DeadPID"
	case VerdictMalformed:
		return "Malformed"
	case VerdictKilledRecovered:
		return "KilledRecovered"
	case VerdictKillRefused:
		return "KillRefused"
	case VerdictKillFailed:
		return "KillFailed"
	case VerdictRaceLost:
		return "RaceLost"
	}
	return fmt.Sprintf("VerdictClass(%d)", int(c))
}

// Verdict bundles the Probe result. JSON marshaling skips Diagnose
// and Hint (Codex r6 #4): A4-b's POST /api/force-kill returns the
// raw structured fields and the UI formats locally.
//
// pidCmdlineRaw and macOSUnsupported are unexported and therefore
// invisible to encoding/json. They carry signals that must NOT
// reach JSON or the diagnostic block: pidCmdlineRaw is the full,
// untruncated argv the identity-gate (cmdlineIsGui) reads; the
// public PIDCmdline is the truncated display/JSON copy. Truncating
// argv before the gate would drop argv[1] when argv[0] (the
// binary path) exceeds 1KB and let a non-GUI mcphub subcommand
// pass the gate's len(argv)==1 branch (Codex iter-3 P2 #1).
//
// macOSUnsupported, when true, marks a Verdict produced on a
// platform where processIDImpl returned errMacOSProbeUnsupported
// (currently darwin). KillRecordedHolder reads it to short-circuit
// to a macOS-specific KillRefused message instead of cascading
// through the image/argv/start-time gates with empty fields and
// emitting "image '' is not an mcphub binary" (Codex iter-3 P2 #2).
type Verdict struct {
	Class      VerdictClass `json:"class"`
	PID        int          `json:"pid"`
	Port       int          `json:"port"`
	Mtime      time.Time    `json:"mtime"`
	PIDAlive   bool         `json:"pid_alive"`
	PIDImage   string       `json:"pid_image"`
	PIDCmdline []string     `json:"pid_cmdline"` // truncated to 1KB display
	PIDStart   time.Time    `json:"pid_start"`
	PingMatch  bool         `json:"ping_match"`
	Diagnose   string       `json:"-"`
	Hint       string       `json:"-"`

	// pidCmdlineRaw is the untruncated argv used by the identity
	// gate. Unexported so encoding/json never serializes it; the
	// truncated PIDCmdline above is the only argv that reaches
	// display, JSON, or the diagnostic block.
	pidCmdlineRaw []string

	// macOSUnsupported flags Verdicts produced when processIDImpl
	// returned errMacOSProbeUnsupported. KillRecordedHolder uses
	// this to refuse the kill with a macOS-specific message.
	macOSUnsupported bool
}

// KillOpts controls KillRecordedHolder behavior.
type KillOpts struct {
	// PingTimeout is how long pingIncumbent waits before declaring
	// "unreachable". Default 500ms when zero.
	PingTimeout time.Duration
	// AcquireDeadline is the maximum total time KillRecordedHolder
	// waits for TryLock to succeed after sending the kill signal.
	// Default 2s when zero.
	AcquireDeadline time.Duration
	// AcquireBackoff is the inter-attempt delay during the
	// post-kill TryLock poll. Default 50ms when zero.
	AcquireBackoff time.Duration
	// Expected, when populated (non-zero PID), is the identity the
	// caller already showed to the user (e.g. via runForceKill's
	// confirmation prompt). KillRecordedHolder's internal re-probe
	// must observe an identical (PID, Port, Mtime) tuple before any
	// kill happens; otherwise classification flips to
	// VerdictRaceLost and no signal is sent. Closes the TOCTOU
	// window between the cli's first Probe and the internal probe
	// where a competitor could rewrite the pidport with a different
	// PID and trick the gate into killing the wrong process.
	// Codex iter-5 P1.
	Expected ExpectedIdentity
}

// ExpectedIdentity carries the (PID, Port, Mtime) tuple the caller
// already validated against before invoking KillRecordedHolder.
// A zero PID disables the check (back-compat for callers that do not
// pre-Probe). Codex iter-5 P1.
type ExpectedIdentity struct {
	PID   int
	Port  int
	Mtime time.Time
}

// IsZero reports whether the ExpectedIdentity carries no expectation.
// PID == 0 is the canonical "unset" sentinel — any pidport with a
// recorded PID of 0 is malformed and would already be rejected by
// probe/ReadPidport before reaching here.
func (e ExpectedIdentity) IsZero() bool { return e.PID == 0 }

// Probe inspects the pidport file and classifies the incumbent's
// state without taking any destructive action. Used by bare
// `mcphub gui --force` to build the diagnostic block.
//
// Class progression:
//   - Pidport unreadable / unparseable → VerdictMalformed.
//   - processID(pid).Alive == false   → VerdictDeadPID.
//   - PID alive AND ping matches      → VerdictHealthy.
//   - PID alive AND ping fails/wrong  → VerdictLiveUnreachable.
//
// Three-part identity gate is NOT run here — it's specific to
// KillRecordedHolder. Probe is read-only and provides display data.
func Probe(ctx context.Context, pidportPath string) Verdict {
	return probe(ctx, pidportPath, 500*time.Millisecond)
}

// probeStartupRetries / probeStartupBackoff bound the retry loop that
// covers the AcquireSingleInstanceAt → ReadyHook startup window
// (Codex PR #23 P2 #1, widened in iter-2). Total retry budget is
// bounded at 5 × 100ms = 500ms (plus per-attempt pingTimeout on the
// final successful read, which short-circuits via Healthy).
//
// probeStartupWindow is the mtime threshold separating "incumbent
// just wrote pidport, listener may still be binding" from "real stuck
// incumbent". 5s is intentionally generous: it is far better to add
// ~500ms latency to a 5-seconds-old "real stuck" case than to kill a
// healthy-but-slow startup. Real stuck incumbents will always have
// pidport mtime well past 5s old, so they skip the retry and are
// classified LiveUnreachable on the first probe.
const (
	probeStartupRetries = 5
	probeStartupBackoff = 100 * time.Millisecond
	probeStartupWindow  = 5 * time.Second
)

// probe is the internal implementation shared by Probe and
// KillRecordedHolder. pingTimeout controls how long pingIncumbent
// waits before declaring the incumbent unreachable.
//
// Startup-window retry (Codex PR #23 P2 #1, widened in iter-2):
// when classification would otherwise be VerdictLiveUnreachable AND
// the recorded PID is alive AND the pidport mtime is recent
// (within probeStartupWindow), the function retries up to
// probeStartupRetries times spaced by probeStartupBackoff, re-reading
// the pidport on each iteration. This closes the kill-vulnerable
// window between AcquireSingleInstanceAt (which writes pidport with
// {pid, requestedPort} immediately) and Server.Start binding
// 127.0.0.1:requestedPort (which signals ready and triggers the
// final pidport rewrite): a holder finishing its bind during the
// retry loop flips the verdict from LiveUnreachable to Healthy.
//
// The mtime gate (instead of the iter-1 `port==0` gate) is the right
// signal because the same race exists for explicit `--port=N`:
// AcquireSingleInstanceAt writes pidport with `{pid, N}` before
// Server.Start binds. The iter-1 gate missed that case entirely.
// Real stuck incumbents have pidport mtimes far older than the
// startup window, so they still skip the retry and classify
// LiveUnreachable immediately.
func probe(ctx context.Context, pidportPath string, pingTimeout time.Duration) Verdict {
	v := probeOnce(ctx, pidportPath, pingTimeout)
	if !shouldRetryProbe(v) {
		return v
	}
	// Retry loop: re-read pidport on each iteration in case the
	// holder finishes its bind. The mtime gate and PIDAlive gate
	// keep this loop bounded — once mtime ages past the startup
	// window, or the PID dies, retries stop.
	for i := 0; i < probeStartupRetries; i++ {
		select {
		case <-ctx.Done():
			return v
		case <-time.After(probeStartupBackoff):
		}
		retry := probeOnce(ctx, pidportPath, pingTimeout)
		// Any verdict that no longer meets the retry conditions is
		// final — return it. This includes:
		//   - Healthy (holder finished bind + ping matches)
		//   - LiveUnreachable with old mtime (real stuck instance)
		//   - DeadPID (holder exited mid-startup)
		//   - Malformed (pidport corrupted under us)
		if !shouldRetryProbe(retry) {
			return retry
		}
		// Still in the startup window — keep the latest verdict
		// (its mtime/PIDStart are the freshest) and try again.
		v = retry
	}
	return v
}

// shouldRetryProbe reports whether a Verdict represents a transient
// startup-race state worth retrying. Returns true iff:
//
//  1. Class == VerdictLiveUnreachable (alive PID, ping fails);
//  2. PIDAlive == true (defensive — Class implies it, but pin it);
//  3. Pidport mtime is non-zero and within probeStartupWindow.
//
// The mtime gate replaces the iter-1 (port==0) gate, which only
// covered the --port=0 startup race and missed the analogous
// --port=N startup race entirely. (Codex PR #23 P2 #1 iter-2.)
func shouldRetryProbe(v Verdict) bool {
	if v.Class != VerdictLiveUnreachable {
		return false
	}
	if !v.PIDAlive {
		return false
	}
	if v.Mtime.IsZero() {
		return false
	}
	return time.Since(v.Mtime) < probeStartupWindow
}

// probeOnce runs a single classification pass without retry. Split
// out from probe so the retry loop above can call it cleanly.
//
// Ordering invariant (Codex iter-3 P2 #2): ping runs FIRST, before
// processID. Healthy classification depends on ping match alone —
// processID telemetry is needed for the LiveUnreachable vs DeadPID
// distinction and for the destructive identity gate, but Healthy
// is a ping-only verdict. This lets bare `mcphub gui --force` on
// macOS detect a healthy incumbent and route to handshake, even
// though processIDImpl returns errMacOSProbeUnsupported. Pre-fix
// code returned VerdictMalformed early on macOS and never reached
// the ping branch, breaking activate-window for healthy incumbents.
//
// Truncation invariant (Codex iter-3 P2 #1): id.Cmdline is the
// raw, untruncated argv. We store it on Verdict.pidCmdlineRaw
// (unexported) for the identity gate, and truncate only when
// populating the display field Verdict.PIDCmdline. Truncating
// before the gate would let a non-GUI mcphub subcommand whose
// argv[0] exceeds 1KB pass cmdlineIsGui's len(argv)==1 branch.
func probeOnce(ctx context.Context, pidportPath string, pingTimeout time.Duration) Verdict {
	v := Verdict{}
	pid, port, err := ReadPidport(pidportPath)
	if err != nil || pid <= 0 {
		v.Class = VerdictMalformed
		v.Diagnose = fmt.Sprintf("pidport unreadable or empty: %v", err)
		v.Hint = "Reboot the OS or remove the pidport directory contents manually."
		return v
	}
	v.PID = pid
	v.Port = port
	if st, statErr := os.Stat(pidportPath); statErr == nil {
		v.Mtime = st.ModTime()
	}

	// Ping first. A successful ping that matches the recorded PID
	// is a complete Healthy verdict regardless of whether processID
	// is supported on this platform.
	matched, perr := pingIncumbent(ctx, port, pingTimeout)
	pingMatched := perr == nil && matched == pid

	id, idErr := processID(pid)
	v.PIDAlive = id.Alive
	v.PIDImage = id.ImagePath
	v.pidCmdlineRaw = id.Cmdline
	v.PIDCmdline = truncateCmdline(id.Cmdline, 1024)
	v.PIDStart = id.StartTime
	v.macOSUnsupported = errors.Is(idErr, errMacOSProbeUnsupported)

	if pingMatched {
		v.Class = VerdictHealthy
		v.PingMatch = true
		v.Diagnose = fmt.Sprintf("incumbent PID %d is healthy on port %d", pid, port)
		v.Hint = ""
		return v
	}

	// Codex PR #23 P2 #3 (iter-2, refined iter-3): on platforms
	// where the identity probe is unimplemented (currently macOS —
	// see probe_darwin.go), processIDImpl returns ProcessIdentity{}
	// + a sentinel error. Without a healthy ping we have no useful
	// liveness signal, so classify as VerdictLiveUnreachable with
	// macOS-specific diagnose/hint. KillRecordedHolder reads
	// macOSUnsupported and refuses with a clear message instead of
	// cascading through identity gates that read empty fields.
	if v.macOSUnsupported {
		v.Class = VerdictLiveUnreachable
		v.PIDAlive = false
		if perr != nil {
			v.Diagnose = fmt.Sprintf("recorded PID %d: macOS identity probe not supported and /api/ping on %d failed: %v", pid, port, perr)
		} else {
			v.Diagnose = fmt.Sprintf("recorded PID %d: macOS identity probe not supported and /api/ping on %d returned PID %d", pid, port, matched)
		}
		v.Hint = "macOS: identity probe not supported; --force --kill is blocked. Reboot is the recovery path."
		return v
	}

	if !id.Alive {
		v.Class = VerdictDeadPID
		v.Diagnose = fmt.Sprintf("recorded PID %d is not alive", pid)
		v.Hint = "The previous incumbent process has exited. The lock should release on its own; if not, reboot."
		return v
	}

	v.Class = VerdictLiveUnreachable
	if perr != nil {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d failed: %v", pid, port, perr)
	} else {
		v.Diagnose = fmt.Sprintf("recorded PID %d alive but /api/ping on %d returned PID %d", pid, port, matched)
	}
	v.Hint = "Kill the recorded PID manually OR rerun with --force --kill (subject to identity gate)."
	return v
}

// KillRecordedHolder is the destructive opt-in path for
// `mcphub gui --force --kill`. Runs the healthy-incumbent early-exit,
// then the three-part identity gate, then SIGKILL/TerminateProcess
// on the recorded PID, then a TryLock poll loop until acquired or
// AcquireDeadline expires.
//
// Returns (lock, verdict, err). On VerdictKilledRecovered, lock is
// the freshly-acquired SingleInstanceLock the caller must Release.
// On all other Verdicts lock is nil.
//
// Three-part identity gate (memo §"Why automation is opt-in"):
//
//  1. matchBasename(image) — "mcphub.exe" Windows / "mcphub" POSIX.
//  2. argv subcommand: argv[1] == "gui" OR len(argv) == 1.
//     The len(argv)==1 branch covers cmd/mcphub/main.go:32 which
//     internally appends "gui" to os.Args when invoked with no
//     arguments (Explorer/Start-menu double-click); externally the
//     command line is just the executable path.
//  3. process start time ≤ pidport mtime + 1s tolerance.
//
// Codex r4 #7: never os.Remove the lock file. The OS releases the
// flock as a side effect of process termination.
func KillRecordedHolder(ctx context.Context, pidportPath string, opts KillOpts) (*SingleInstanceLock, Verdict, error) {
	if opts.PingTimeout == 0 {
		opts.PingTimeout = 500 * time.Millisecond
	}
	if opts.AcquireDeadline == 0 {
		opts.AcquireDeadline = 2 * time.Second
	}
	if opts.AcquireBackoff == 0 {
		opts.AcquireBackoff = 50 * time.Millisecond
	}

	v := probe(ctx, pidportPath, opts.PingTimeout)
	switch v.Class {
	case VerdictMalformed, VerdictDeadPID:
		return nil, v, fmt.Errorf("kill skipped: %s", v.Class)
	case VerdictHealthy:
		// Codex r5 #7b: incumbent is healthy — do NOT kill. Caller
		// routes to handshake instead. Verdict is returned as-is so
		// the cli layer can print "incumbent is healthy; activating
		// instead of killing" before TryActivateIncumbent.
		return nil, v, nil
	}

	// Codex iter-5 P1: TOCTOU guard between the caller's confirmed
	// identity (the PID it showed to the user, or the cli's first
	// Probe in --yes mode) and the identity our internal re-probe
	// just observed. A competitor that rewrote the pidport between
	// the two probes would flip PID/Port/Mtime; if we proceed here
	// we may signal a different process than the user confirmed.
	// Mismatch → VerdictRaceLost; no kill attempted. The check is
	// gated on Expected.PID != 0 so callers that don't pre-Probe
	// (no production callers today, but the seam is preserved for
	// older tests) keep their original behavior.
	if !opts.Expected.IsZero() {
		if v.PID != opts.Expected.PID || v.Port != opts.Expected.Port || !v.Mtime.Equal(opts.Expected.Mtime) {
			confirmed := opts.Expected
			v.Class = VerdictRaceLost
			v.Diagnose = fmt.Sprintf(
				"pidport changed between user confirmation and kill: confirmed PID %d port %d mtime %s, found PID %d port %d mtime %s",
				confirmed.PID, confirmed.Port, confirmed.Mtime.UTC().Format(time.RFC3339Nano),
				v.PID, v.Port, v.Mtime.UTC().Format(time.RFC3339Nano),
			)
			v.Hint = "Rerun mcphub gui without --force to handshake with the new incumbent."
			return nil, v, fmt.Errorf("pidport changed mid-prompt")
		}
	}

	// Codex iter-3 P2 #2: macOS shortcut — when probeOnce flagged
	// the verdict as macOSUnsupported, processIDImpl returned no
	// useful identity signals. Refuse the kill explicitly with a
	// macOS-specific diagnose instead of letting the gate emit
	// "image '' is not an mcphub binary". Production gate semantics
	// are unchanged on linux/windows where macOSUnsupported is
	// always false. The override seam is honored first so existing
	// scenario-4/4b tests can still mock the gate.
	if v.macOSUnsupported && identityGateOverride == nil {
		v.Class = VerdictKillRefused
		v.Diagnose = "kill refused: macOS identity probe not supported; reboot is the recovery path"
		v.Hint = "Tracked as backlog: macOS libproc/sysctl-based identity (see probe_darwin.go)."
		return nil, v, fmt.Errorf("kill refused: macOS identity probe not supported")
	}

	// LiveUnreachable: run the three-part identity gate.
	//
	// identityGateOverride, when set by tests, replaces the full
	// three-part (image/argv/start-time) check. Production builds
	// always use the real gate below (identityGateOverride is nil
	// by default).
	if identityGateOverride != nil {
		if refused, reason := identityGateOverride(v); refused {
			v.Class = VerdictKillRefused
			v.Diagnose = "identity gate (test override): " + reason
			v.Hint = ""
			return nil, v, fmt.Errorf("kill refused (override): %s", reason)
		}
	} else {
		if !matchBasename(v.PIDImage) {
			v.Class = VerdictKillRefused
			v.Diagnose = fmt.Sprintf("recorded PID %d image %q is not an mcphub binary", v.PID, v.PIDImage)
			v.Hint = "Identity-gate (image basename) failed; identify and kill the actual flock holder via OS tools."
			return nil, v, fmt.Errorf("kill refused: image gate")
		}
		// Codex iter-3 P2 #1: read v.pidCmdlineRaw (the unmodified
		// argv populated by probeOnce), not v.PIDCmdline (truncated
		// for display). Truncating before this gate would drop
		// argv[1] when argv[0] (the binary path) exceeds 1KB and
		// allow a non-GUI mcphub subcommand whose long path triggers
		// the truncation to pass the len(argv)==1 branch.
		if !cmdlineIsGui(v.pidCmdlineRaw) {
			v.Class = VerdictKillRefused
			v.Diagnose = fmt.Sprintf("recorded PID %d argv subcommand is not 'gui' (argv=%v)", v.PID, v.PIDCmdline)
			v.Hint = "Identity-gate (argv subcommand) failed; the recorded PID is a different mcphub subcommand."
			return nil, v, fmt.Errorf("kill refused: argv gate")
		}
		if !startTimeBeforeMtime(v.PIDStart, v.Mtime, time.Second) {
			v.Class = VerdictKillRefused
			v.Diagnose = fmt.Sprintf("recorded PID %d start-time %s postdates pidport mtime %s — PID-recycled", v.PID, v.PIDStart.Format(time.RFC3339), v.Mtime.Format(time.RFC3339))
			v.Hint = "Identity-gate (start-time) failed; the PID has been recycled to a different process."
			return nil, v, fmt.Errorf("kill refused: start-time gate")
		}
	}

	// All three gates passed. Kill.
	if err := killProcess(v.PID); err != nil {
		v.Class = VerdictKillFailed
		v.Diagnose = fmt.Sprintf("kill PID %d failed: %v", v.PID, err)
		v.Hint = "Permission denied or process already gone; rerun mcphub gui without --force to handshake."
		return nil, v, err
	}

	// postKillHook fires between the kill and the acquire-poll loop.
	// Tests use it to simulate a race-winner competing for the flock.
	if postKillHook != nil {
		postKillHook()
	}

	// Acquire-poll loop (memo §"Take-over protocol" step 5g).
	deadline := time.Now().Add(opts.AcquireDeadline)
	for time.Now().Before(deadline) {
		lock, err := AcquireSingleInstanceAt(pidportPath, v.Port)
		if err == nil {
			v.Class = VerdictKilledRecovered
			v.Diagnose = fmt.Sprintf("force-killed previous incumbent PID %d and acquired lock", v.PID)
			v.Hint = ""
			return lock, v, nil
		}
		if !errors.Is(err, ErrSingleInstanceBusy) {
			v.Class = VerdictKillFailed
			v.Diagnose = fmt.Sprintf("post-kill acquire failed: %v", err)
			v.Hint = ""
			return nil, v, err
		}
		time.Sleep(opts.AcquireBackoff)
	}
	v.Class = VerdictRaceLost
	v.Diagnose = fmt.Sprintf("kill succeeded but a competitor acquired the lock during %s deadline", opts.AcquireDeadline)
	v.Hint = "Rerun mcphub gui without --force to handshake with the new incumbent."
	return nil, v, fmt.Errorf("race lost")
}

// cmdlineIsGui implements the rev 9 argv-subcommand gate:
// argv[1] == "gui" OR len(argv) == 1 (Explorer no-arg auto-gui).
func cmdlineIsGui(argv []string) bool {
	if len(argv) == 1 {
		return true
	}
	if len(argv) >= 2 && argv[1] == "gui" {
		return true
	}
	return false
}

// startTimeBeforeMtime returns true iff start ≤ mtime + tolerance.
// A start time strictly later than the pidport mtime indicates the
// PID was recycled to a process that began AFTER our pidport was
// written.
func startTimeBeforeMtime(start, mtime time.Time, tolerance time.Duration) bool {
	if start.IsZero() || mtime.IsZero() {
		// Defensive: missing telemetry → fail closed.
		return false
	}
	return !start.After(mtime.Add(tolerance))
}

// truncateCmdline caps the total argv string length at maxBytes for
// safe logging/JSON. The identity gate (cmdlineIsGui) reads the raw
// argv from Verdict.pidCmdlineRaw, NOT the truncated PIDCmdline this
// function produces, so truncation cannot influence the gate's
// decision. (Codex iter-3 P2 #1: pre-fix code truncated before the
// gate, which dropped argv[1] when argv[0] exceeded maxBytes and
// let the len(argv)==1 branch of cmdlineIsGui pass for a non-GUI
// mcphub subcommand whose binary path was long enough.)
//
// Truncation is display/JSON-bounding only, not a security mitigation.
func truncateCmdline(argv []string, maxBytes int) []string {
	if len(argv) == 0 {
		return argv
	}
	out := make([]string, 0, len(argv))
	used := 0
	for _, a := range argv {
		if used+len(a) > maxBytes {
			remaining := maxBytes - used
			if remaining > 0 {
				out = append(out, a[:remaining])
			}
			break
		}
		out = append(out, a)
		used += len(a)
	}
	return out
}
