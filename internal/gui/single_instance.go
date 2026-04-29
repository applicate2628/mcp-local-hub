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
}

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
// covers the AcquireSingleInstanceAt(--port 0) → RewritePidportPort
// startup window (Codex PR #23 P2 #1). Total retry budget is bounded
// at 5 × 100ms = 500ms (plus per-attempt pingTimeout on the final
// successful read, which short-circuits via Healthy).
const (
	probeStartupRetries = 5
	probeStartupBackoff = 100 * time.Millisecond
)

// probe is the internal implementation shared by Probe and
// KillRecordedHolder. pingTimeout controls how long pingIncumbent
// waits before declaring the incumbent unreachable.
//
// Startup-window retry (Codex PR #23 P2 #1): when classification
// would otherwise be VerdictLiveUnreachable AND the recorded port is
// 0, the function retries up to probeStartupRetries times spaced by
// probeStartupBackoff, re-reading the pidport on each iteration. This
// closes the kill-vulnerable window between AcquireSingleInstanceAt
// (which writes pidport with port=0 when --port=0) and
// RewritePidportPort (which writes the bound port after listener-
// ready): a holder finishing its bind during the retry loop flips the
// verdict from LiveUnreachable to Healthy. The retry only runs in the
// (LiveUnreachable AND port==0) case so genuinely stuck instances
// with a real-but-wrong port keep their immediate LiveUnreachable
// classification.
func probe(ctx context.Context, pidportPath string, pingTimeout time.Duration) Verdict {
	v := probeOnce(ctx, pidportPath, pingTimeout)
	if !(v.Class == VerdictLiveUnreachable && v.Port == 0) {
		return v
	}
	// Retry loop: re-read pidport on each iteration in case the
	// holder finishes its bind and writes a non-zero port. We deal
	// with port=0 specifically — a real stuck-incumbent verdict
	// keeps its LiveUnreachable classification with port==N and
	// does not get retried.
	for i := 0; i < probeStartupRetries; i++ {
		select {
		case <-ctx.Done():
			return v
		case <-time.After(probeStartupBackoff):
		}
		retry := probeOnce(ctx, pidportPath, pingTimeout)
		// Any verdict OTHER than (LiveUnreachable AND port==0)
		// is final — return it. This includes:
		//   - Healthy (holder finished bind + ping matches)
		//   - LiveUnreachable with port==N (real stuck instance)
		//   - DeadPID (holder exited mid-startup)
		//   - Malformed (pidport corrupted under us)
		if !(retry.Class == VerdictLiveUnreachable && retry.Port == 0) {
			return retry
		}
		// Still port==0 — keep the latest verdict (its mtime/
		// PIDStart are the freshest) and try again.
		v = retry
	}
	return v
}

// probeOnce runs a single classification pass without retry. Split
// out from probe so the retry loop above can call it cleanly.
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

	id, _ := processID(pid)
	v.PIDAlive = id.Alive
	v.PIDImage = id.ImagePath
	v.PIDCmdline = truncateCmdline(id.Cmdline, 1024)
	v.PIDStart = id.StartTime

	if !id.Alive {
		v.Class = VerdictDeadPID
		v.Diagnose = fmt.Sprintf("recorded PID %d is not alive", pid)
		v.Hint = "The previous incumbent process has exited. The lock should release on its own; if not, reboot."
		return v
	}

	matched, perr := pingIncumbent(ctx, port, pingTimeout)
	if perr == nil && matched == pid {
		v.Class = VerdictHealthy
		v.PingMatch = true
		v.Diagnose = fmt.Sprintf("incumbent PID %d is healthy on port %d", pid, port)
		v.Hint = ""
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
		if !cmdlineIsGui(v.PIDCmdline) {
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
// safe logging/JSON. The image gate runs before the argv gate
// (matchBasename then cmdlineIsGui), so a pathologically large
// argv[0] from a non-mcphub process is rejected before the
// (truncated) argv[1] is read by cmdlineIsGui. Truncation is
// display/JSON-bounding only, not a security mitigation.
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
