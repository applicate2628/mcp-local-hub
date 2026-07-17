package cli

import (
	"context"
	"errors"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/daemonrecovery"
	"mcp-local-hub/internal/process"
)

// Port-squatter reap (P2a, decision D-A). When the liveness sweep observes
// port_owner_mismatch — a live process OTHER than the tracked child owns a
// daemon's intended port — the supervisor may reap that owner IFF a strict
// identity gate proves it is a disowned mcphub daemon child FOR THIS TASK.
// Otherwise it must NOT kill: a genuinely foreign process yields an honest
// observe-only warn, and an unverifiable owner fails CLOSED (no kill). The
// authorization classifier lives in internal/daemonrecovery and is shared
// by this automatic sweep adapter and the operator recovery operation.
//
// Security contract: work-items/decisions/2026-07-02-da-supervisor-reap-verified-own-port-squatter.md.

type squatterVerdict = daemonrecovery.Verdict

const (
	squatterOwnTask    = daemonrecovery.VerdictOwnTask
	squatterForeign    = daemonrecovery.VerdictForeign
	squatterUnverified = daemonrecovery.VerdictUnverified
)

// squatterLookupIdentityFn resolves a PID into a full ProcessIdentity
// (image path + verbatim command line + creation time). It is nil on
// non-Windows targets — process.LookupProcessIdentity only compiles on
// Windows — so classifyPortSquatter fails closed to squatterUnverified there
// (MUST-FIX #6: Windows-only reap). The Windows build sets it in
// supervise_squatter_windows.go's init. Tests swap it to drive gates without
// shelling PowerShell; nil-ing it exercises the non-Windows observe-only path.
var squatterLookupIdentityFn func(context.Context, int) (process.ProcessIdentity, error)

// squatterExeMatchesFn is the handle-truth executable gate (gate 3):
// QueryFullProcessImageName on a live handle, which argv spoofing cannot beat.
// It exists on every platform (false on POSIX-other). Injectable for tests.
var squatterExeMatchesFn = process.PIDExecutableMatches

// squatterTerminatePIDFn is the identity-re-verifying kill primitive used by
// the automatic sweep reap paths (MUST-FIX #2: kill EXCLUSIVELY via
// TerminatePIDWithIdentity — held-handle re-verify of
// exe+basename+start-time, ACCESS_DENIED fails closed, no raw TerminateProcess).
// Operator recovery instead owns one held generation through
// recoverHoldProcessFn. This automatic-path seam remains injectable for tests
// so no real process is killed.
var squatterTerminatePIDFn = process.TerminatePIDWithIdentity

// Audit `source` discriminators (MUST-FIX #8): every verdict/kill is tagged
// with the trigger that produced it so an operator can tell the automatic
// pre-spawn gate (F1), the automatic quarantine self-heal sweep (F3), the
// existing liveness port_owner_mismatch sweep (P2a), and the operator-driven
// recover verb apart in supervisor-events.log. "sweep"/"recover" are the
// pre-existing literals; these name the two NEW automatic triggers.
const (
	squatterSourcePreSpawn        = "prespawn"         // F1: pre-spawn port-owner gate on the controller loop
	squatterSourceQuarantineSweep = "quarantine-sweep" // F3: quarantine self-heal second pass on the liveness sweep
)

// classifyPortSquatter decides whether ownerPID (the live foreign owner of
// d.Port, per a port_owner_mismatch verdict) may be reaped as a disowned child
// of THIS task. It is pure — no rate limiting, no kill, no event emission — so
// automatic sweep adapters can reuse it. Operator recovery calls the shared
// daemonrecovery classifier directly while holding the target generation. On
// squatterOwnTask (and on a post-lookup Foreign) it returns
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
	sharedTracked := make(map[string]daemonrecovery.RuntimeEntry, len(tracked))
	for task, entry := range tracked {
		sharedTracked[task] = daemonrecovery.RuntimeEntry{CurrentPID: entry.CurrentPID, OrphanPID: entry.OrphanPID}
	}
	var lookupIdentity func(int) (process.ProcessIdentity, error)
	if squatterLookupIdentityFn != nil {
		lookupIdentity = func(pid int) (process.ProcessIdentity, error) {
			return squatterLookupIdentityFn(context.Background(), pid)
		}
	}
	return daemonrecovery.ClassifyPortOwner(d, ownerPID, selfPID, sharedTracked, daemonrecovery.ClassifierDependencies{
		LookupIdentity:    lookupIdentity,
		ExecutableMatches: squatterExeMatchesFn,
	})
}

// squatterKillProof builds the identity proof for the reap from a fresh
// LookupProcessIdentity result. Sourcing StartedAt/ExecutablePath here — from
// THIS pass's read — makes it structurally impossible to reap without a fresh
// identity (MUST-FIX #3). A zero-value id yields an empty ExecutablePath and a
// zero PID, so TerminatePIDWithIdentity fails closed.
func squatterKillProof(id process.ProcessIdentity) process.PIDIdentityProof {
	return daemonrecovery.KillProof(id)
}

// squatterStartedAt renders the second-precision CreationDateUnix as the
// RFC3339Nano proof timestamp. Second precision is within
// pidIdentityStartTolerance (1s), so TerminatePIDWithIdentity's start-time
// re-verify on the held handle matches deterministically.
func squatterStartedAt(id process.ProcessIdentity) string {
	return daemonrecovery.StartedAt(id)
}

// commandLineMatchesTaskArgv is gate 5 — the SOLE task-discriminator (MUST-FIX
// #4): the observed argv must (a) begin with THIS descriptor's subcommand token
// sequence in command position, AND (b) carry every discriminating flag/value
// pair for the task, each matched exact-token. The subcommand anchor (F2)
// rejects sibling subcommands that ALSO register --server/--daemon flags
// (relay/restart/stop/install → `mcphub relay --server X --daemon Y` is NOT a
// daemon). The three daemon-row shapes:
//   - serena-proxy: `daemon serena-proxy` + `--task-name <canonical>` (unique per workspace).
//   - LSP workspace-proxy: `daemon workspace-proxy` + `--workspace <path>` + `--language <lang>`
//     (it carries no --task-name/--server/--daemon; the workspace path is unique per task).
//   - global/legacy daemon: `daemon --…` + `--server <server>` + `--daemon <daemon>`.
//
// An unknown descriptor shape yields false (→ Foreign, fail-closed).
func commandLineMatchesTaskArgv(tokens []string, d api.SupervisorDaemon) bool {
	return daemonrecovery.CommandLineMatchesTaskArgv(tokens, d)
}

// commandLineHasAdjacentTokenPair reports whether tokens contains flag
// immediately followed by value, each an EXACT whole-token match (MUST-FIX #4:
// no substring/prefix matching — `serena-b1` must not match `serena-b133f336`).
func commandLineHasAdjacentTokenPair(tokens []string, flag, value string) bool {
	return daemonrecovery.CommandLineHasAdjacentTokenPair(tokens, flag, value)
}

// tokenizeWindowsCommandLine splits a Windows command line into argv tokens.
// It is a direct port of the Go standard library's os package
// commandLineToArgv/readNextArg parser (the "Prior to 2008" CommandLineToArgvW
// rule set — see http://daviddeley.com/autohotkey/parameters/parameters.htm),
// which is validated byte-for-byte against golang.org/x/sys/windows.CommandLineToArgv
// for the security-relevant ARGUMENT tokens by the differential test in
// supervise_squatter_tokenizer_windows_test.go.
//
// It does NOT replicate CommandLineToArgvW's special program-name (argv[0])
// parsing; the classifier only ever inspects tokens[1:] (the subcommand +
// flags), so argv[0] fidelity is irrelevant and intentionally not claimed.
// Divergence bias is fail-closed: any parse that does not reproduce the
// descriptor's exact argument tokens yields a token that fails exact equality →
// Foreign (no kill), never a false OwnTask.
//
// Key quoting rules: 2n backslashes before a quote → n backslashes + toggle
// quote mode; 2n+1 → n backslashes + a literal quote; a `""` inside a quoted
// span emits one literal `"` (the double-double-quote rule); backslashes not
// before a quote are literal; unquoted whitespace delimits.
func tokenizeWindowsCommandLine(s string) []string {
	return daemonrecovery.TokenizeWindowsCommandLine(s)
}

// boundSquatterField caps an attacker-influenceable observed string to
// daemonrecovery.BoundEventField's shared 2048-byte limit, truncating on a
// UTF-8 rune boundary so the
// emitted body stays valid and — critically — well under the 16 KB whole-body
// cap that would otherwise evict the forensic scalars (MUST-FIX #1 / H1).
func boundSquatterField(s string) string {
	return daemonrecovery.BoundEventField(s)
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
					"source":          "sweep",
					"actor":           api.CurrentOSUser(),
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

// pruneAbsent drops per-task rate-limit state (lastLookup + reapAttempts) for
// tasks not in the current sweep's running set, so the maps cannot grow
// unbounded across the supervisor's lifetime on task churn (F5 — parity with
// the P1b bind-latch key-sweep). Called at the end of each sweep.
func (l *squatterReapLimiter) pruneAbsent(seen map[string]struct{}) {
	if l == nil {
		return
	}
	for task := range l.lastLookup {
		if _, ok := seen[task]; !ok {
			delete(l.lastLookup, task)
		}
	}
	for task := range l.reapAttempts {
		if _, ok := seen[task]; !ok {
			delete(l.reapAttempts, task)
		}
	}
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
	// stoppedFn reports whether the operator has STOPPED a task (desired != running
	// OR an active stop window). F3's quarantine self-heal (supervise_liveness.go)
	// must NOT reap + auto-restart a daemon the operator deliberately stopped — the
	// stop owns cleanup (Codex PR-3 P2-3). It mirrors the F2 quarantine-parole stop
	// gate (supervisor_controller.go's runQuarantineParoleTick). Wired in production
	// over the controller's daemonIntent cache; nil in direct-sweep tests → treated
	// as not-stopped (today's behavior).
	stoppedFn func(task string) bool
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
		emitSquatterEvent(events, "daemon-port-squatter-unverified", "sweep", squatterUnverified, d, ownerPID, process.ProcessIdentity{}, map[string]any{
			"note": "port owner mismatch observed; no reap capability wired — observe-only, no restart",
		})
		return squatterSweepObserveOnly
	}

	// Lookup rate limit: at most one identity lookup per task per 30s.
	if !reap.limiter.allowLookup(task, now) {
		emitSquatterEvent(events, "daemon-port-squatter-unverified", "sweep", squatterUnverified, d, ownerPID, process.ProcessIdentity{}, map[string]any{
			"rate_limited": true,
			"note":         "identity lookup rate-limited (<=1/30s per task); run 'mcphub daemon recover " + task + "' to force recovery now",
		})
		return squatterSweepObserveOnly
	}
	reap.limiter.recordLookup(task, now)

	verdict, id := classifyPortSquatter(d, ownerPID, selfPID, tracked)
	switch verdict {
	case squatterForeign:
		emitSquatterEvent(events, "daemon-port-squatter-foreign", "sweep", verdict, d, ownerPID, id, map[string]any{
			"note": "port owner is NOT a disowned child of this task (tracked sibling, different binary, or argv does not name this task); NOT killed — observe-only",
		})
		return squatterSweepObserveOnly
	case squatterUnverified:
		emitSquatterEvent(events, "daemon-port-squatter-unverified", "sweep", verdict, d, ownerPID, id, map[string]any{
			"note": "port owner identity could not be verified (lookup failed / non-Windows); fail-closed, no kill",
		})
		return squatterSweepObserveOnly
	case squatterOwnTask:
		if !reap.limiter.allowReap(task, now) {
			emitSquatterEvent(events, "daemon-port-squatter-unverified", "sweep", squatterUnverified, d, ownerPID, id, map[string]any{
				"rate_limited": true,
				"note":         "reap attempts exhausted for this failure window (per-task or global cap); run 'mcphub daemon recover " + task + "' to force recovery",
			})
			return squatterSweepObserveOnly
		}
		globalCount := reap.limiter.recordReap(task, now)
		proof := squatterKillProof(id)
		err := reap.reapFn(d, proof)
		if errors.Is(err, process.ErrProcessAlreadyExited) {
			emitSquatterEvent(events, "daemon-port-squatter-already-exited", "sweep", verdict, d, ownerPID, id, map[string]any{
				"reap_count_window": globalCount,
				"note":              "verified-own port squatter had already exited; no reap was performed; restarting this task to rebind its port",
			})
			return squatterSweepReapedFallThrough
		}
		if err != nil {
			emitSquatterEvent(events, "daemon-port-squatter-reap-failed", "sweep", verdict, d, ownerPID, id, map[string]any{
				"err":               err.Error(),
				"reap_count_window": globalCount,
				"note":              "identity-gated reap failed; NOT restarting while the port is still held",
			})
			return squatterSweepObserveOnly
		}
		emitSquatterEvent(events, "daemon-port-squatter-reaped", "sweep", verdict, d, ownerPID, id, map[string]any{
			"reap_count_window": globalCount,
			"note":              "verified-own port squatter exit confirmed; restarting this task to rebind its port",
		})
		return squatterSweepReapedFallThrough
	}
	return squatterSweepObserveOnly
}

// emitSquatterEvent writes a warn audit event with pre-bounded identity fields.
// The forensic scalars (squatter_pid, verdict, port, source, actor) plus the
// bounded command_line/executable_path/started_at keep the whole body well
// under the 16 KB whole-body cap so the identity can never be evicted
// (MUST-FIX #1). source distinguishes the sweep from the operator recover verb
// ("sweep" vs "recover"); actor names the OS user so an operator-driven kill is
// attributable (F1 / D-A Security-argument clause 6).
func emitSquatterEvent(events *api.SupervisorEventLog, event, source string, verdict squatterVerdict, d api.SupervisorDaemon, ownerPID int, id process.ProcessIdentity, extra map[string]any) {
	daemonrecovery.EmitAuditEvent(events, event, source, verdict, d, ownerPID, id, extra)
}

// squatterAutoOutcome is the result of the shared automatic-trigger port-owner
// gate (F1 pre-spawn + F3 quarantine self-heal). It reports WHAT the classifier
// found and whether a reap happened; the caller maps it to its own consequence
// (F1: spawn / hold-in-backoff; F3: post EvManualRestart / observe-only). The
// helper itself drives NO SM transition and takes NO port-free wait — those are
// caller-owned so the single classify+reap+audit core stays reusable.
type squatterAutoOutcome int

const (
	// squatterAutoForeign: the port owner exists and was identity-read but is
	// NOT a disowned child of this task — NEVER killed (observe-only).
	squatterAutoForeign squatterAutoOutcome = iota
	// squatterAutoUnverified: the owner's identity could not be established
	// (lookup failure / non-Windows / owner is this task's own tracked child) —
	// fail closed, no kill.
	squatterAutoUnverified
	// squatterAutoReaped: a verified-own disowned child was confirmed reaped or
	// was already gone. The audit event distinguishes those outcomes; the caller
	// may proceed to (re)spawn into the freed port in either case.
	squatterAutoReaped
	// squatterAutoReapFailed: the owner was verified-own but the identity-gated
	// kill failed — the port is still held; the caller must NOT spawn over it.
	squatterAutoReapFailed
	// squatterAutoRateLimited: the per-task lookup or reap budget was exhausted
	// this failure window — no classify/kill this pass; the caller must NOT
	// spawn (it cannot know the owner is reapable) and should retry later.
	squatterAutoRateLimited
)

// reapSquatterForAutomaticTrigger is the single owner of the rate-limited
// classify → identity-gated reap → audit flow for the two NEW automatic
// triggers (F1 pre-spawn gate, F3 quarantine self-heal). The caller has already
// resolved the effective port and probed a live foreign owner (ownerPID > 0);
// this decides whether that owner is a reapable disowned child of THIS task and,
// if so, kills it EXCLUSIVELY via squatterTerminatePIDFn (MUST-FIX #2 — the
// automatic adapter over the same held-generation process primitive used by
// operator recovery; no raw TerminateProcess, no ACCESS_DENIED retry). Every verdict/kill is audited with
// the caller's `source` (MUST-FIX #8) and H1-bounded identity fields.
//
// It deliberately mirrors handleSquatterMismatchOnSweep's rate-limit + classify
// structure but (a) reaps via squatterTerminatePIDFn directly rather than the
// sweep's reapFn closure (so its audit `source` is not the closure-baked
// "sweep"), and (b) returns a richer outcome the caller maps to its own action.
// The shipped P2a mismatch path (handleSquatterMismatchOnSweep) is left
// untouched. limiter may be nil (tests / no wiring): its methods are nil-safe,
// so a nil limiter simply disables rate-limiting.
func reapSquatterForAutomaticTrigger(
	d api.SupervisorDaemon,
	ownerPID, selfPID int,
	tracked map[string]DaemonRuntimeEntry,
	limiter *squatterReapLimiter,
	events *api.SupervisorEventLog,
	source string,
	now time.Time,
) squatterAutoOutcome {
	task := canonicalSupervisorTaskName(d.TaskName)

	// Lookup rate limit (D-A "Rate limit"): at most one identity lookup per task
	// per 30s. The identity read (LookupProcessIdentity) is the expensive gate;
	// the caller's cheap port-owner probe already ran, so throttling here bounds
	// the WMI/CIM cost without hiding a freed port (a freed port has ownerPID<=0
	// and never reaches this helper).
	if !limiter.allowLookup(task, now) {
		emitSquatterEvent(events, "daemon-port-squatter-unverified", source, squatterUnverified, d, ownerPID, process.ProcessIdentity{}, map[string]any{
			"rate_limited": true,
			"note":         "identity lookup rate-limited (<=1/30s per task); run 'mcphub daemon recover " + task + "' to force recovery now",
		})
		return squatterAutoRateLimited
	}
	limiter.recordLookup(task, now)

	verdict, id := classifyPortSquatter(d, ownerPID, selfPID, tracked)
	switch verdict {
	case squatterForeign:
		emitSquatterEvent(events, "daemon-port-squatter-foreign", source, verdict, d, ownerPID, id, map[string]any{
			"note": "port owner is NOT a disowned child of this task (tracked sibling, different binary, or argv does not name this task); NOT killed",
		})
		return squatterAutoForeign
	case squatterUnverified:
		emitSquatterEvent(events, "daemon-port-squatter-unverified", source, verdict, d, ownerPID, id, map[string]any{
			"note": "port owner identity could not be verified (lookup failed / non-Windows / this task's own child); fail-closed, no kill",
		})
		return squatterAutoUnverified
	case squatterOwnTask:
		if !limiter.allowReap(task, now) {
			emitSquatterEvent(events, "daemon-port-squatter-unverified", source, squatterUnverified, d, ownerPID, id, map[string]any{
				"rate_limited": true,
				"note":         "reap attempts exhausted for this failure window (per-task or global cap); run 'mcphub daemon recover " + task + "' to force recovery",
			})
			return squatterAutoRateLimited
		}
		globalCount := limiter.recordReap(task, now)
		proof := squatterKillProof(id)
		err := squatterTerminatePIDFn(proof)
		if errors.Is(err, process.ErrProcessAlreadyExited) {
			emitSquatterEvent(events, "daemon-port-squatter-already-exited", source, verdict, d, ownerPID, id, map[string]any{
				"reap_count_window": globalCount,
				"note":              "verified-own port squatter had already exited; no reap was performed",
			})
			return squatterAutoReaped
		}
		if err != nil {
			emitSquatterEvent(events, "daemon-port-squatter-reap-failed", source, verdict, d, ownerPID, id, map[string]any{
				"err":               err.Error(),
				"reap_count_window": globalCount,
				"note":              "identity-gated reap failed; NOT spawning/restarting while the port is still held",
			})
			return squatterAutoReapFailed
		}
		emitSquatterEvent(events, "daemon-port-squatter-reaped", source, verdict, d, ownerPID, id, map[string]any{
			"reap_count_window": globalCount,
			"note":              "verified-own port squatter exit confirmed",
		})
		return squatterAutoReaped
	}
	return squatterAutoUnverified
}
