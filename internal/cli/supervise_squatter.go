package cli

import (
	"errors"
	"time"
	"unicode/utf8"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// Port-squatter reap (P2a, decision D-A). When the liveness sweep observes
// port_owner_mismatch — a live process OTHER than the tracked child owns a
// daemon's intended port — the supervisor may reap that owner IFF a strict
// identity gate proves it is a disowned mcphub daemon child FOR THIS TASK.
// Otherwise it must NOT kill: a genuinely foreign process yields an honest
// observe-only warn, and an unverifiable owner fails CLOSED (no kill). This is
// the single owner of that classification, shared by the sweep and the
// `mcphub daemon recover` verb (P2b).
//
// Security contract: work-items/decisions/2026-07-02-da-supervisor-reap-verified-own-port-squatter.md.

type squatterVerdict int

const (
	// squatterOwnTask: every identity gate passed — the owner IS this task's
	// disowned mcphub child (our binary at the configured path, our argv naming
	// THIS task). Only this verdict authorizes a reap.
	squatterOwnTask squatterVerdict = iota
	// squatterForeign: the owner exists and was identity-read, but it is NOT a
	// child of this task (a tracked sibling, a different binary, or argv that
	// does not name this task). Observe-only, NEVER killed.
	squatterForeign
	// squatterUnverified: the owner's identity could not be established
	// (OpenProcess/lookup failure, PID gone, or non-Windows where no
	// start-time-proof handle exists). Fail closed — observe-only, no kill.
	squatterUnverified
)

func (v squatterVerdict) String() string {
	switch v {
	case squatterOwnTask:
		return "own_task"
	case squatterForeign:
		return "foreign"
	default:
		return "unverified"
	}
}

// squatterLookupIdentityFn resolves a PID into a full ProcessIdentity
// (image path + verbatim command line + creation time). It is nil on
// non-Windows targets — process.LookupProcessIdentity only compiles on
// Windows — so classifyPortSquatter fails closed to squatterUnverified there
// (MUST-FIX #6: Windows-only reap). The Windows build sets it in
// supervise_squatter_windows.go's init. Tests swap it to drive gates without
// shelling PowerShell; nil-ing it exercises the non-Windows observe-only path.
var squatterLookupIdentityFn func(pid int) (process.ProcessIdentity, error)

// squatterExeMatchesFn is the handle-truth executable gate (gate 3):
// QueryFullProcessImageName on a live handle, which argv spoofing cannot beat.
// It exists on every platform (false on POSIX-other). Injectable for tests.
var squatterExeMatchesFn = process.PIDExecutableMatches

// squatterTerminatePIDFn is the identity-re-verifying kill primitive used by
// BOTH the sweep reap closure and the `daemon recover` verb (MUST-FIX #2: kill
// EXCLUSIVELY via TerminatePIDWithIdentity — held-handle re-verify of
// exe+basename+start-time, ACCESS_DENIED fails closed, no raw TerminateProcess).
// Injectable for tests so no real process is killed.
var squatterTerminatePIDFn = process.TerminatePIDWithIdentity

// squatterEventFieldCap bounds each attacker-influenceable observed string
// (command_line, executable_path) BEFORE it enters a supervisor event body
// (MUST-FIX #1 / H1). supervisor_events.go truncates the ENTIRE Body to a
// sentinel at 16 KB — NOT per-field — so an unbounded hostile CommandLine or
// exe-path would evict the forensic scalars (squatter_pid / verdict /
// executable_path / started_at) from the audit. Field-level pre-bounding keeps
// the whole body well under the 16 KB cap so the identity survives.
const squatterEventFieldCap = 2048

// classifyPortSquatter decides whether ownerPID (the live foreign owner of
// d.Port, per a port_owner_mismatch verdict) may be reaped as a disowned child
// of THIS task. It is pure — no rate limiting, no kill, no event emission — so
// both the sweep (rate-limited, killing) and `daemon recover` (operator-driven)
// can reuse it. On squatterOwnTask (and on a post-lookup Foreign) it returns
// the fresh ProcessIdentity so the caller builds the kill proof and the audit
// body from THIS pass's read (MUST-FIX #3). It fails closed to
// squatterUnverified on any ambiguity.
//
// Gate order (ALL must pass for squatterOwnTask):
//  1. ownerPID is a real live foreign PID (>0, != self, != this task's child).
//  2. NOT a tracked sibling — no OTHER task's CurrentPID or OrphanPID equals
//     ownerPID (MUST-FIX #5). A port collision between two of our daemons must
//     never resolve by killing the sibling.
//  4. Identity read (Windows-only). A read failure → Unverified. Done BEFORE
//     the exe gate so an OpenProcess/lookup failure (Unverified) is
//     distinguishable from a genuine exe mismatch (Foreign) — SHOULD #8.
//  3. Exe gate, handle truth — the owner's image IS this daemon's configured
//     binary. The lookup above proved the process exists, so a miss here is a
//     genuine foreign binary → Foreign.
//  5. Argv gate — the owner's command line names THIS task exactly, matched on
//     whitespace tokens with exact per-token equality (MUST-FIX #4). This is
//     the SOLE task-discriminator: every mcphub process (daemons, gui,
//     supervise, status, daemon recover) shares mcphub.exe, so the exe gate
//     cannot separate tasks. A prefix/substring bug here is P0 friendly-fire.
func classifyPortSquatter(d api.SupervisorDaemon, ownerPID, selfPID int, tracked map[string]DaemonRuntimeEntry) (squatterVerdict, process.ProcessIdentity) {
	canon := canonicalSupervisorTaskName(d.TaskName)

	// Gate 1 (defensive — MUST-FIX #7): a reapable squatter is a live foreign
	// PID distinct from the supervisor itself and from this task's own tracked
	// child. port_owner_self is handled by its own reason before the mismatch
	// arm; asserting it here too keeps the classifier honest if reused elsewhere.
	if ownerPID <= 0 || (selfPID != 0 && ownerPID == selfPID) {
		return squatterUnverified, process.ProcessIdentity{}
	}
	if entry, ok := tracked[canon]; ok && ownerPID == entry.CurrentPID {
		// The owner IS this task's current child — not a squatter at all.
		return squatterUnverified, process.ProcessIdentity{}
	}

	// Gate 2 (MUST-FIX #5): refuse to kill another tracked daemon. Iterate BOTH
	// CurrentPID and OrphanPID of every OTHER task.
	for task, entry := range tracked {
		if canonicalSupervisorTaskName(task) == canon {
			continue
		}
		if ownerPID == entry.CurrentPID || (entry.OrphanPID != 0 && ownerPID == entry.OrphanPID) {
			return squatterForeign, process.ProcessIdentity{}
		}
	}

	// Platform gate (MUST-FIX #6): no start-time-proof identity lookup on
	// non-Windows → fail closed to observe-only.
	if squatterLookupIdentityFn == nil {
		return squatterUnverified, process.ProcessIdentity{}
	}

	// Gate 4 FIRST (SHOULD #8): a fresh identity read. Any error — OpenProcess
	// access-denied, CIM failure, PID gone — is Unverified (fail closed). This
	// is also the SOLE source of the kill proof (MUST-FIX #3).
	id, err := squatterLookupIdentityFn(ownerPID)
	if err != nil {
		return squatterUnverified, process.ProcessIdentity{}
	}

	// Gate 3 (handle truth): the owner's image must BE this daemon's configured
	// binary. The lookup above proved the process exists, so a miss here is a
	// genuine different-binary owner → Foreign (distinguished from Unverified).
	expectedExe := daemonExpectedIdentityExe(d.Command)
	if expectedExe == "" || !squatterExeMatchesFn(ownerPID, expectedExe) {
		return squatterForeign, id
	}

	// Gate 5 (argv, SOLE task-discriminator, MUST-FIX #4): the owner's command
	// line must carry every discriminating flag/value pair for THIS task as
	// adjacent whitespace tokens with exact per-token equality.
	pairs, ok := squatterRequiredArgvPairs(d)
	if !ok {
		// Unknown descriptor shape (should not occur among supervisor-intent.json
		// daemon rows). Cannot prove this-task ownership → refuse.
		return squatterForeign, id
	}
	tokens := tokenizeWindowsCommandLine(id.CommandLine)
	for _, p := range pairs {
		if !commandLineHasAdjacentTokenPair(tokens, p[0], p[1]) {
			return squatterForeign, id
		}
	}
	return squatterOwnTask, id
}

// squatterKillProof builds the identity proof for the reap from a fresh
// LookupProcessIdentity result. Sourcing StartedAt/ExecutablePath here — from
// THIS pass's read — makes it structurally impossible to reap without a fresh
// identity (MUST-FIX #3). A zero-value id yields an empty ExecutablePath and a
// zero PID, so TerminatePIDWithIdentity fails closed.
func squatterKillProof(id process.ProcessIdentity) process.PIDIdentityProof {
	return process.PIDIdentityProof{
		PID:            id.PID,
		ExecutablePath: id.ExecutablePath,
		StartedAt:      squatterStartedAt(id),
	}
}

// squatterStartedAt renders the second-precision CreationDateUnix as the
// RFC3339Nano proof timestamp. Second precision is within
// pidIdentityStartTolerance (2s), so TerminatePIDWithIdentity's start-time
// re-verify on the held handle matches deterministically.
func squatterStartedAt(id process.ProcessIdentity) string {
	if id.CreationDateUnix <= 0 {
		return ""
	}
	return time.Unix(id.CreationDateUnix, 0).UTC().Format(time.RFC3339Nano)
}

// squatterRequiredArgvPairs returns the flag/value token pairs that must ALL
// appear (adjacent, exact) in a squatter's command line for it to be THIS
// task's child, plus ok=false when the descriptor shape yields no unique
// discriminator (→ the caller refuses / Foreign). The three daemon-row shapes:
//   - serena-proxy: `--task-name <canonical>` — unique per workspace.
//   - LSP workspace-proxy: `--workspace <path>` AND `--language <lang>` (it
//     carries no --task-name/--server/--daemon), the workspace path being
//     unique per task.
//   - global/legacy daemon: `--server <server>` AND `--daemon <daemon>`.
func squatterRequiredArgvPairs(d api.SupervisorDaemon) ([][2]string, bool) {
	if isSerenaProxyDescriptor(d) {
		tn := canonicalSupervisorTaskName(d.TaskName)
		if tn == "" {
			return nil, false
		}
		return [][2]string{{"--task-name", tn}}, true
	}
	if isLSPWorkspaceProxyDescriptor(d) {
		ws := lspWorkspaceProxyArgValue(d, "--workspace")
		lang := lspWorkspaceProxyArgValue(d, "--language")
		if ws == "" || lang == "" {
			return nil, false
		}
		return [][2]string{{"--workspace", ws}, {"--language", lang}}, true
	}
	if isGlobalDaemonDescriptor(d) {
		if d.Server == "" || d.Daemon == "" {
			return nil, false
		}
		return [][2]string{{"--server", d.Server}, {"--daemon", d.Daemon}}, true
	}
	return nil, false
}

// isGlobalDaemonDescriptor reports whether a descriptor is a global/legacy
// server daemon (`daemon --server … --daemon …`). It EXCLUDES the proxy shapes
// and anything that is not a bare `daemon` subcommand, so sibling subcommands
// (gui / supervise / status / restart / relay / daemon recover) never resolve a
// discriminator (MUST-FIX #4 — those must classify Foreign).
func isGlobalDaemonDescriptor(d api.SupervisorDaemon) bool {
	return len(d.Args) >= 1 && d.Args[0] == "daemon" &&
		!isSerenaProxyDescriptor(d) && !isLSPWorkspaceProxyDescriptor(d)
}

// commandLineHasAdjacentTokenPair reports whether tokens contains flag
// immediately followed by value, each an EXACT whole-token match (MUST-FIX #4:
// no substring/prefix matching — `serena-b1` must not match `serena-b133f336`).
func commandLineHasAdjacentTokenPair(tokens []string, flag, value string) bool {
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == flag && tokens[i+1] == value {
			return true
		}
	}
	return false
}

// tokenizeWindowsCommandLine splits a Windows command line into argv tokens
// following the CommandLineToArgvW quoting/backslash rules so a value with
// embedded (quoted) spaces — e.g. a workspace path `--workspace "C:\My Proj"` —
// tokenizes to the single unquoted value `C:\My Proj`, and quotes cannot be
// used to smuggle a cross-task match (MUST-FIX #4). The parser is deliberately
// conservative: any ambiguity produces a token that fails EXACT equality, so
// the failure direction is Foreign (no kill), never a false OwnTask.
//
// Rules: 2n backslashes before a quote → n backslashes + toggle quote mode;
// 2n+1 backslashes before a quote → n backslashes + a literal quote;
// backslashes not before a quote are literal; unquoted whitespace delimits.
func tokenizeWindowsCommandLine(s string) []string {
	var tokens []string
	var cur []rune
	inArg := false
	inQuotes := false
	backslashes := 0

	emitBackslashes := func(n int) {
		for i := 0; i < n; i++ {
			cur = append(cur, '\\')
		}
	}
	endArg := func() {
		tokens = append(tokens, string(cur))
		cur = cur[:0]
		inArg = false
	}

	for _, r := range s {
		switch {
		case r == '\\':
			backslashes++
			inArg = true
		case r == '"':
			emitBackslashes(backslashes / 2)
			if backslashes%2 == 1 {
				cur = append(cur, '"')
			} else {
				inQuotes = !inQuotes
			}
			backslashes = 0
			inArg = true
		case (r == ' ' || r == '\t') && !inQuotes:
			emitBackslashes(backslashes)
			backslashes = 0
			if inArg {
				endArg()
			}
		default:
			emitBackslashes(backslashes)
			backslashes = 0
			cur = append(cur, r)
			inArg = true
		}
	}
	emitBackslashes(backslashes)
	if inArg {
		endArg()
	}
	return tokens
}

// boundSquatterField caps an attacker-influenceable observed string to
// squatterEventFieldCap bytes, truncating on a UTF-8 rune boundary so the
// emitted body stays valid and — critically — well under the 16 KB whole-body
// cap that would otherwise evict the forensic scalars (MUST-FIX #1 / H1).
func boundSquatterField(s string) string {
	if len(s) <= squatterEventFieldCap {
		return s
	}
	cut := squatterEventFieldCap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[truncated]"
}

// squatterReapFunc is the identity-gated kill capability. The production
// implementation (makeProductionSquatterReapFn) wraps TerminatePIDWithIdentity;
// tests inject a recorder.
type squatterReapFunc func(d api.SupervisorDaemon, proof process.PIDIdentityProof) error

// makeProductionSquatterReapFn returns the reap capability wired in
// runSupervise beside the spawn/terminate closures (single owner). It emits a
// pre-bounded reap-requested audit event then kills EXCLUSIVELY via
// squatterTerminatePIDFn (TerminatePIDWithIdentity), which re-verifies
// exe+basename+start-time on the held handle and fails closed on ACCESS_DENIED
// with no retry (MUST-FIX #2).
func makeProductionSquatterReapFn(events *api.SupervisorEventLog) squatterReapFunc {
	return func(d api.SupervisorDaemon, proof process.PIDIdentityProof) error {
		if events != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "liveness",
				Event:    "daemon-port-squatter-reap-requested",
				TaskName: canonicalSupervisorTaskName(d.TaskName),
				Body: map[string]any{
					"squatter_pid":    proof.PID,
					"executable_path": boundSquatterField(proof.ExecutablePath),
					"started_at":      proof.StartedAt,
					"port":            d.Port,
				},
			})
		}
		return squatterTerminatePIDFn(proof)
	}
}

// Rate-limit budget (sweep-local, D-A "Rate limit"): at most one identity
// lookup per squatterLookupMinInterval per task and at most
// squatterMaxReapsPerTaskWindow reaps per failure window per task; a global cap
// (SHOULD #9) bounds fleet-wide reaps per window. Beyond any cap the sweep
// downgrades to observe-only pointing at the recover verb.
const (
	squatterLookupMinInterval       = 30 * time.Second
	squatterMaxReapsPerTaskWindow   = 3
	squatterMaxGlobalReapsPerWindow = 10
)

// squatterReapLimiter is owned solely by the liveness-monitor goroutine (like
// the P1b bind latch) — no cross-goroutine access, so it needs no lock.
type squatterReapLimiter struct {
	window       time.Duration
	lastLookup   map[string]time.Time
	reapAttempts map[string][]time.Time
	globalReaps  []time.Time
}

func newSquatterReapLimiter() *squatterReapLimiter {
	return &squatterReapLimiter{
		window:       respawnFailureWindow,
		lastLookup:   map[string]time.Time{},
		reapAttempts: map[string][]time.Time{},
	}
}

func (l *squatterReapLimiter) allowLookup(task string, now time.Time) bool {
	if l == nil {
		return true
	}
	last, ok := l.lastLookup[task]
	return !ok || now.Sub(last) >= squatterLookupMinInterval
}

func (l *squatterReapLimiter) recordLookup(task string, now time.Time) {
	if l == nil {
		return
	}
	l.lastLookup[task] = now
}

func (l *squatterReapLimiter) pruneWindow(task string, now time.Time) {
	cutoff := now.Add(-l.window)
	kept := make([]time.Time, 0, len(l.reapAttempts[task]))
	for _, ts := range l.reapAttempts[task] {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	if len(kept) == 0 {
		delete(l.reapAttempts, task)
	} else {
		l.reapAttempts[task] = kept
	}
	global := make([]time.Time, 0, len(l.globalReaps))
	for _, ts := range l.globalReaps {
		if ts.After(cutoff) {
			global = append(global, ts)
		}
	}
	l.globalReaps = global
}

func (l *squatterReapLimiter) allowReap(task string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.pruneWindow(task, now)
	if len(l.globalReaps) >= squatterMaxGlobalReapsPerWindow {
		return false
	}
	return len(l.reapAttempts[task]) < squatterMaxReapsPerTaskWindow
}

// recordReap records a reap attempt (per-task + global) and returns the global
// reap count in the current window (SHOULD #9 — surfaced in the audit body).
func (l *squatterReapLimiter) recordReap(task string, now time.Time) int {
	if l == nil {
		return 0
	}
	l.reapAttempts[task] = append(l.reapAttempts[task], now)
	l.globalReaps = append(l.globalReaps, now)
	return len(l.globalReaps)
}

// squatterSweepReaper bundles the production reap capability with the sweep's
// rate-limit state, threaded into the sweep by the liveness monitor. nil in
// direct-sweep unit tests (mismatch then handled observe-only, no reap).
type squatterSweepReaper struct {
	reapFn  squatterReapFunc
	limiter *squatterReapLimiter
}

type squatterSweepOutcome int

const (
	// squatterSweepObserveOnly: post NO loop event — a foreign/unverified/
	// rate-limited squatter cannot be displaced by restarting our own child,
	// and a reap failure leaves the port held.
	squatterSweepObserveOnly squatterSweepOutcome = iota
	// squatterSweepReapedFallThrough: the verified-own squatter was reaped;
	// fall through to the normal stale + EvManualRestart post so the SM
	// respawns this task and rebinds the freed port.
	squatterSweepReapedFallThrough
)

// handleSquatterMismatchOnSweep runs the rate-limited classify+reap on the
// sweep goroutine for a port_owner_mismatch. It emits the audit event and
// returns whether the sweep should fall through to EvManualRestart. All
// identity/kill work is on the sweep goroutine; SM consequences travel only via
// the posted EvManualRestart, preserving single-writer discipline.
func handleSquatterMismatchOnSweep(
	d api.SupervisorDaemon,
	ownerPID, selfPID int,
	tracked map[string]DaemonRuntimeEntry,
	events *api.SupervisorEventLog,
	reap *squatterSweepReaper,
	now time.Time,
) squatterSweepOutcome {
	task := canonicalSupervisorTaskName(d.TaskName)

	// No reap capability wired (direct-sweep unit tests / a host without the
	// closure): pure observe-only. Restarting our own child while a foreign
	// process holds the port is the exact futile loop that manufactures the
	// quarantine (defect C), so post NOTHING.
	if reap == nil || reap.reapFn == nil {
		emitSquatterEvent(events, "daemon-port-squatter-unverified", squatterUnverified, d, ownerPID, process.ProcessIdentity{}, map[string]any{
			"note": "port owner mismatch observed; no reap capability wired — observe-only, no restart",
		})
		return squatterSweepObserveOnly
	}

	// Lookup rate limit: at most one identity lookup per task per 30s.
	if !reap.limiter.allowLookup(task, now) {
		emitSquatterEvent(events, "daemon-port-squatter-unverified", squatterUnverified, d, ownerPID, process.ProcessIdentity{}, map[string]any{
			"rate_limited": true,
			"note":         "identity lookup rate-limited (<=1/30s per task); run 'mcphub daemon recover " + task + "' to force recovery now",
		})
		return squatterSweepObserveOnly
	}
	reap.limiter.recordLookup(task, now)

	verdict, id := classifyPortSquatter(d, ownerPID, selfPID, tracked)
	switch verdict {
	case squatterForeign:
		emitSquatterEvent(events, "daemon-port-squatter-foreign", verdict, d, ownerPID, id, map[string]any{
			"note": "port owner is NOT a disowned child of this task (tracked sibling, different binary, or argv does not name this task); NOT killed — observe-only",
		})
		return squatterSweepObserveOnly
	case squatterUnverified:
		emitSquatterEvent(events, "daemon-port-squatter-unverified", verdict, d, ownerPID, id, map[string]any{
			"note": "port owner identity could not be verified (lookup failed / non-Windows); fail-closed, no kill",
		})
		return squatterSweepObserveOnly
	case squatterOwnTask:
		if !reap.limiter.allowReap(task, now) {
			emitSquatterEvent(events, "daemon-port-squatter-unverified", squatterUnverified, d, ownerPID, id, map[string]any{
				"rate_limited": true,
				"note":         "reap attempts exhausted for this failure window (per-task or global cap); run 'mcphub daemon recover " + task + "' to force recovery",
			})
			return squatterSweepObserveOnly
		}
		globalCount := reap.limiter.recordReap(task, now)
		proof := squatterKillProof(id)
		err := reap.reapFn(d, proof)
		if err != nil && !errors.Is(err, process.ErrProcessAlreadyExited) {
			emitSquatterEvent(events, "daemon-port-squatter-reap-failed", verdict, d, ownerPID, id, map[string]any{
				"err":               err.Error(),
				"reap_count_window": globalCount,
				"note":              "identity-gated reap failed; NOT restarting while the port is still held",
			})
			return squatterSweepObserveOnly
		}
		emitSquatterEvent(events, "daemon-port-squatter-reaped", verdict, d, ownerPID, id, map[string]any{
			"reap_count_window": globalCount,
			"already_exited":    errors.Is(err, process.ErrProcessAlreadyExited),
			"note":              "verified-own port squatter reaped; restarting this task to rebind its port",
		})
		return squatterSweepReapedFallThrough
	}
	return squatterSweepObserveOnly
}

// emitSquatterEvent writes a warn audit event with pre-bounded identity fields.
// The forensic scalars (squatter_pid, verdict, port) plus the bounded
// command_line/executable_path/started_at keep the whole body well under the
// 16 KB whole-body cap so the identity can never be evicted (MUST-FIX #1).
func emitSquatterEvent(events *api.SupervisorEventLog, event string, verdict squatterVerdict, d api.SupervisorDaemon, ownerPID int, id process.ProcessIdentity, extra map[string]any) {
	if events == nil {
		return
	}
	body := map[string]any{
		"squatter_pid": ownerPID,
		"verdict":      verdict.String(),
		"port":         d.Port,
	}
	if id.ExecutablePath != "" {
		body["executable_path"] = boundSquatterField(id.ExecutablePath)
	}
	if id.CommandLine != "" {
		body["command_line"] = boundSquatterField(id.CommandLine)
	}
	if started := squatterStartedAt(id); started != "" {
		body["started_at"] = started
	}
	for k, v := range extra {
		body[k] = v
	}
	_ = events.Emit(api.SupervisorEvent{
		Severity: "warn",
		Source:   "liveness",
		Event:    event,
		TaskName: canonicalSupervisorTaskName(d.TaskName),
		Body:     body,
	})
}
