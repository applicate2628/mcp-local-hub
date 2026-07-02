package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	supervisorPortProbeTimeout = 300 * time.Millisecond
	// supervisorPortBindGrace is the POST-FIRST-BIND grace: once a daemon's
	// port has been observed bound by its current PID at least once, a
	// subsequently-unbound port is only tolerated for this long before the
	// sweep treats it as a restart trigger. This is the byte-identical 5s rule
	// that predates P1b; P1b only changes the PRE-first-bind phase (see
	// supervisorStartupBindDeadline).
	supervisorPortBindGrace    = 5 * time.Second
	supervisorLivenessInterval = 5 * time.Second

	// supervisorDefaultStartupBindDeadline / supervisorSerenaStartupBindDeadline
	// are the P1b first-bind deadlines applied BEFORE a daemon's first observed
	// bind of the current generation (a fresh spawn that has not yet bound its
	// port). They replace the flat 5s grace for that startup phase so a
	// slow-starting daemon is not terminate-first-then-respawned mid-startup.
	supervisorDefaultStartupBindDeadline = 60 * time.Second
	supervisorSerenaStartupBindDeadline  = 120 * time.Second

	supervisorLivenessRuntimeClearedBodyKey = "runtime_pid_cleared"

	supervisorLivenessReasonMissingPID          = "missing_pid"
	supervisorLivenessReasonPIDDead             = "pid_dead"
	supervisorLivenessReasonPIDIdentityMissing  = "pid_identity_missing"
	supervisorLivenessReasonPIDIdentityMismatch = "pid_identity_mismatch"
	supervisorLivenessReasonPortOwnerUnverified = "port_owner_unverified"
	supervisorLivenessReasonPortUnbound         = "port_unbound"
	supervisorLivenessReasonPortOwnerSelf       = "port_owner_self"
	supervisorLivenessReasonPortOwnerMismatch   = "port_owner_mismatch"
)

type supervisorLivenessProbe struct {
	PIDAlive     func(pid int) bool
	PIDIdentity  func(proof process.PIDIdentityProof) error
	PortLive     func(port int) bool
	PortOwnerPID func(port int) (pid int, ok bool, err error)
}

var supervisorLivenessProbeFns = defaultSupervisorLivenessProbe()

func defaultSupervisorLivenessProbe() supervisorLivenessProbe {
	probe := supervisorLivenessProbe{
		PIDAlive:    process.IsPidAlive,
		PIDIdentity: process.VerifyPIDIdentity,
		PortLive:    supervisorPortLive,
	}
	// Install the OS-level port-owner verification ONLY on platforms with a real
	// implementation (Windows netstat, Linux /proc). On macOS and other POSIX
	// targets api.LoopbackPortOwnerPID fails closed (errPortOwnerUnsupported);
	// installing it there would short-circuit the PortLive TCP fallback below and
	// classify every live daemon port_owner_unverified forever — a regression from
	// the prior TCP liveness on the documented macOS-preview/POSIX paths (Codex
	// bot #271 P2). A nil PortOwnerPID falls through to PortLive.
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		probe.PortOwnerPID = supervisorPortOwnerPID
	}
	return probe
}

var supervisorSelfPIDFn = os.Getpid

func setSupervisorLivenessProbeForTest(p supervisorLivenessProbe) func() {
	prev := supervisorLivenessProbeFns
	supervisorLivenessProbeFns = p
	return func() { supervisorLivenessProbeFns = prev }
}

func supervisorPortLive(port int) bool {
	if port <= 0 {
		return true
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), supervisorPortProbeTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func supervisorPortOwnerPID(port int) (int, bool, error) {
	return api.LoopbackPortOwnerPID(port)
}

// supervisorLivenessUsesProductionPortOwnerProbe reports whether the given
// PortOwnerPID closure is the production per-port netstat probe
// (supervisorPortOwnerPID). The sweep only swaps in the single-snapshot probe
// for that exact production probe:
//   - a nil probe (macOS / other POSIX) keeps the PortLive TCP fallback — no
//     snapshot, no behavior change;
//   - a TEST-injected per-port closure (a different func pointer) is left
//     intact so existing per-port-probe sweep tests drive the SAME verdicts
//     (the snapshot seam would otherwise replace their injected closure with a
//     snapshot resolved from the real netstat default and change the verdict).
//
// Function values are not == comparable in Go, so identity is checked via the
// underlying code pointer (reflect.Value.Pointer); both sides are package-level
// funcs, so the comparison is stable.
func supervisorLivenessUsesProductionPortOwnerProbe(fn func(port int) (int, bool, error)) bool {
	if fn == nil {
		return false
	}
	return reflect.ValueOf(fn).Pointer() == reflect.ValueOf(supervisorPortOwnerPID).Pointer()
}

func hydrateControllerRunningStates(ctrl *supervisorController, currentRunning map[string]bool) {
	if ctrl == nil {
		return
	}
	for taskName, running := range currentRunning {
		if !running {
			continue
		}
		ctrl.smStates.Store(canonicalSupervisorTaskName(taskName), api.StRunning)
	}
}

func startSupervisorLivenessMonitor(
	ctxDone <-chan struct{},
	stateDir string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	ticker := time.NewTicker(supervisorLivenessInterval)
	defer ticker.Stop()
	// bindLatch (P1b) records the generation at which each task's port was
	// FIRST observed bound by its current PID. It is owned solely by this
	// monitor goroutine and threaded into every sweep — no tracker/persist
	// mutation, so it introduces no Conc-F2 single-writer exposure. Keyed by
	// canonical task name; the value is the generation that latched, so a
	// respawn (new generation) invalidates the latch and the startup deadline
	// re-applies. Absent tasks are pruned at the end of each sweep.
	bindLatch := map[string]int{}
	// Immediate first sweep at supervisor start, BEFORE the first ticker
	// tick. Warm-restart leaves alive-but-port-stale daemons recorded as
	// running in supervisor-state.json (loadSupervisorCurrentRunning keeps
	// their live PID for exactly this handoff — Codex bot #268 r10 P1). The
	// startup reconcile treats them as running and no-ops, so without this
	// immediate sweep the wedged wrapper would survive the full 5s liveness
	// interval before the terminate-first-then-respawn fires. Sweeping once
	// up front terminates the stale PID immediately, then the ticker drives
	// the steady-state cadence. Healthy daemons and dead-PID rows are
	// no-ops here (dead rows were already cleared to CurrentPID=0).
	//
	// Warm-restart stale rows carry an OLD StartedAt (well past any startup
	// deadline), so even the longer P1b deadline is already expired at this
	// first sweep and they terminate immediately — the handoff behavior is
	// preserved.
	sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events, bindLatch)
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events, bindLatch)
		}
	}
}

// livenessSweepIntent returns the freshest supervisor intent for a sweep:
// the on-disk supervisor-intent.json when stateDir is set (so a mid-run
// install/migrate that rewrote ports is honored), falling back to the
// startup snapshot on any read error or when stateDir is empty (tests).
//
// This deliberately re-reads + re-parses the file EVERY sweep with no
// stat/mtime/size cache. An mtime/size stat-gate is NOT a safe
// change-detector for this file: a same-byte-length rewrite within the
// filesystem's mtime resolution (e.g. a migration flipping a daemon port
// 9123→9124 in a same-second write) produces an identical (mtime, size)
// tuple, so a stat-gated cache would serve the stale parse and the
// liveness sweep — which DRIVES RESTART DECISIONS — would act on stale
// intent. The read also goes through the hardened inode-anchored pipeline
// (handle-relative open + DACL/identity verification), which dominates the
// per-sweep cost; a content-hash gate that still pays that read every tick
// would save only the trivial json.Unmarshal, so it is not worth the added
// cache surface on a correctness-critical path. The unconditional re-read
// at a 5s cadence is correct and cheap enough; correctness wins over the
// micro-optimization here.
func livenessSweepIntent(stateDir string, fallback *api.SupervisorIntentFile) *api.SupervisorIntentFile {
	if stateDir == "" {
		return fallback
	}
	if refreshed, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json")); err == nil {
		return refreshed
	}
	return fallback
}

func sweepSupervisorLivenessOnce(
	_ string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
	bindLatch map[string]int,
) {
	if tracker == nil || loop == nil || intent == nil {
		return
	}
	// bindLatch is nil-safe for the existing direct-call tests that predate the
	// P1b latch: a nil map reads as "no task latched", so every daemon gets its
	// startup deadline as grace (never the 5s post-bind rule) and no latch is
	// recorded — which for a slow-startup daemon is the correct, more-lenient
	// side. Production always passes the monitor-owned map.
	byTask := map[string]api.SupervisorDaemon{}
	for _, d := range intent.Daemons {
		byTask[canonicalSupervisorTaskName(d.TaskName)] = d
	}
	now := time.Now().UTC()

	// Perf: take ONE OS port-owner snapshot for the whole sweep and resolve
	// every running daemon against it, instead of letting each daemon's
	// liveness check spawn its own `netstat -ano` (15 running daemons → 15
	// spawns EVERY 5s, continuously — the same anti-pattern /api/status fixed
	// in a699713, which deliberately left the sweep on the per-port path).
	// The seam is identical to supervisorStatusDaemons. Critically, the sweep
	// DRIVES RESTARTS, so the snapshot-backed probe must yield BYTE-IDENTICAL
	// liveness verdicts vs the per-port netstat:
	//   - snapshot error → every port returns that err → port_owner_unverified
	//     (observe-only, NEVER a restart — same fail-closed outcome as a
	//     per-port netstat failure);
	//   - port in map → its owner PID (== tracked PID → live; != → mismatch;
	//     == supervisor self → port_owner_self);
	//   - port NOT in map → (0, false, nil) = port_unbound, exactly what a
	//     per-port netstat finding nothing returns.
	// The snapshot replaces ONLY the production per-port probe; when a test
	// injects its own PortOwnerPID closure (a different func pointer than the
	// production supervisorPortOwnerPID), that injected per-port probe is kept
	// so existing per-port-probe sweep tests drive the exact same verdicts.
	// When PortOwnerPID is nil (macOS / other POSIX) no snapshot is taken and
	// the per-daemon PortLive TCP fallback is preserved unchanged.
	livenessProbe := supervisorLivenessProbeFns
	if supervisorLivenessUsesProductionPortOwnerProbe(livenessProbe.PortOwnerPID) {
		snapshot, snapErr := loopbackPortOwnersSnapshotFn()
		livenessProbe.PortOwnerPID = func(port int) (int, bool, error) {
			if snapErr != nil {
				return 0, false, snapErr
			}
			if pid, ok := snapshot[port]; ok {
				return pid, true, nil
			}
			return 0, false, nil
		}
	}

	// seenTasks tracks which latch keys survived this sweep so absent tasks can
	// be pruned at the end (a removed / never-running daemon must not keep a
	// stale latch alive forever).
	seenTasks := map[string]struct{}{}
	for taskName, entry := range tracker.Snapshot() {
		taskName = canonicalSupervisorTaskName(taskName)
		if entry.State != daemonRuntimeStateRunning || entry.CurrentPID <= 0 {
			continue
		}
		d, ok := byTask[taskName]
		if !ok {
			continue
		}
		seenTasks[taskName] = struct{}{}
		// P1b first-bind deadline: BEFORE the current generation's first
		// observed bind, grant the (longer) per-descriptor startup deadline so a
		// slow-starting daemon is not killed mid-startup; AFTER first bind fall
		// back to the byte-identical 5s post-bind grace. The latch is keyed by
		// generation, so a respawn (new generation) invalidates it and the
		// startup deadline re-applies.
		startupDeadline := supervisorStartupBindDeadline(d)
		grace := supervisorPortBindGrace
		latchedGen, latched := bindLatch[taskName]
		if !latched || latchedGen != entry.PIDGeneration {
			grace = startupDeadline
		}
		live, reason, bound := supervisorDaemonEntryLiveWithProbe(d, entry, now, livenessProbe, grace)
		if bound && bindLatch != nil {
			bindLatch[taskName] = entry.PIDGeneration
		}
		if live {
			continue
		}
		// P1b: a port_unbound verdict with NO latched bind for the current
		// generation means the STARTUP DEADLINE expired before the daemon ever
		// bound — the wedged-startup case. Emit a distinct daemon-bind-timeout
		// (warn) before the existing stale + EvManualRestart so the expiry is
		// observable and distinguishable from a post-bind port loss. A post-bind
		// unbound (already latched at the current generation) keeps only the
		// existing daemon-running-state-stale event.
		if reason == supervisorLivenessReasonPortUnbound {
			curLatchedGen, curLatched := bindLatch[taskName]
			neverBoundThisGen := !curLatched || curLatchedGen != entry.PIDGeneration
			if neverBoundThisGen && events != nil {
				waited := time.Duration(0)
				if !entry.StartedAt.IsZero() {
					waited = now.Sub(entry.StartedAt)
				}
				_ = events.Emit(api.SupervisorEvent{
					Severity: "warn",
					Source:   "liveness",
					Event:    "daemon-bind-timeout",
					TaskName: taskName,
					Body: map[string]any{
						"pid":              entry.CurrentPID,
						"port":             d.Port,
						"deadline_seconds": int(startupDeadline / time.Second),
						"waited_seconds":   int(waited / time.Second),
						"note":             "no port bind observed this supervisor session before the first-bind deadline; restarting (the bind latch is per-session, so a daemon that bound before a supervisor restart also reads here — the restart is still correct)",
					},
				})
			}
		}
		// Port-owner PROBE ERROR (could not determine the socket owner — e.g.
		// netstat policy-blocked or /proc owner mapping unavailable). This is
		// observed but is NOT proof
		// the daemon is dead, so it must drive NEITHER a restart
		// (EvManualRestart → fleet restart loop) NOR a teardown (the default
		// EvChildExit → StRunning→StBackoffWaiting crash path). Emit the warn
		// for observability and leave the daemon running (post no event); the
		// next sweep re-probes. A genuine owner-mismatch / unbound-port / dead
		// PID still routes to its own restart/exit reason (Codex bot #268 P2).
		if reason == supervisorLivenessReasonPortOwnerUnverified {
			if events != nil {
				_ = events.Emit(api.SupervisorEvent{
					Severity: "warn",
					Source:   "liveness",
					Event:    "daemon-port-owner-unverified",
					TaskName: taskName,
					Body: map[string]any{
						"pid":  entry.CurrentPID,
						"port": d.Port,
						"note": "OS-level port-owner probe failed; leaving the daemon running (a probe error is not proof of a dead or foreign-owned daemon, so no restart is issued)",
					},
				})
			}
			continue
		}
		if events != nil {
			_ = events.Emit(api.SupervisorEvent{
				Severity: "warn",
				Source:   "liveness",
				Event:    "daemon-running-state-stale",
				TaskName: taskName,
				Body: map[string]any{
					"pid":    entry.CurrentPID,
					"port":   d.Port,
					"reason": reason,
				},
			})
		}
		eventKind := api.EvChildExit
		if supervisorLivenessReasonNeedsRestart(reason) {
			eventKind = api.EvManualRestart
		}
		body := map[string]any{
			"pid":    entry.CurrentPID,
			"port":   d.Port,
			"reason": reason,
		}
		// Single-writer discipline (Codex deep-sec PR #268 Conc-F2): the
		// sweep runs on its own goroutine, but tracker mutations + the
		// supervisor-state.json persist for a task must happen ONLY on the
		// event-loop goroutine so a sweep clear can never race a handler
		// MarkSpawned/MarkExited+persist (PersistTo snapshots the tracker
		// BEFORE taking supervisorStateFileMu, so two concurrent persists
		// can last-writer-win with a stale snapshot). Instead of clearing
		// the runtime here, mark the event so handleLoopEvent performs the
		// MarkExited+persist on the loop before the SM transition. The
		// reason routing is unchanged: a dead-PID restart reason
		// (NeedsRestart && !HasLivePID) carries the clear instruction; a
		// live-PID restart reason keeps its PID for the terminate-first
		// handoff (its old reaper / terminate owns the exit).
		if eventKind == api.EvManualRestart && !supervisorLivenessReasonHasLivePID(reason) {
			body[supervisorLivenessRuntimeClearedBodyKey] = true
		}
		loop.Post(api.LoopEvent{
			Kind:     eventKind,
			TaskName: taskName,
			Body:     body,
		})
	}
	// Prune latch entries whose task is no longer running/tracked so the map
	// cannot leak entries for removed daemons across the supervisor's lifetime.
	if bindLatch != nil {
		for taskName := range bindLatch {
			if _, ok := seenTasks[taskName]; !ok {
				delete(bindLatch, taskName)
			}
		}
	}
}

func supervisorLivenessRestartClearedRuntime(ev api.LoopEvent) bool {
	if ev.Kind != api.EvManualRestart || ev.Body == nil {
		return false
	}
	cleared, _ := ev.Body[supervisorLivenessRuntimeClearedBodyKey].(bool)
	if !cleared {
		return false
	}
	reason, _ := ev.Body["reason"].(string)
	return supervisorLivenessReasonNeedsRestart(reason)
}

// daemonExpectedIdentityExe returns the executable path a DAEMON process is
// expected to run as: its configured Command (the exact exe the supervisor
// exec'd via exec.Command(d.Command, d.Args...)), normalized to match the
// internal comparison done by process.VerifyPIDIdentity
// (normalizeWindowsExecutablePath / normalizeExpectedExecutablePath both apply
// filepath.Abs + EvalSymlinks + Clean). Falls back to canonicalMcphubPath()
// (the SUPERVISOR's own binary) only when command is empty, for
// defense-in-depth on legacy intent rows that predate a populated Command.
//
// This MUST NOT be confused with canonicalMcphubPath(): a daemon's expected
// exe is its install path (e.g. ~/.local/bin/mcphub.exe), which differs from
// the supervisor's own os.Executable() whenever the supervisor runs from a
// different binary than the daemons it spawned (e.g. a dev build supervising
// release-path daemons). Comparing a daemon's exe to the supervisor's own path
// is the false-mismatch bug fixed here — it drove every live daemon to
// pid_identity_mismatch → fleet-wide restart churn (bug
// 2026-06-09-supervisor-loses-current-pid-false-quarantine.md, Layer A).
func daemonExpectedIdentityExe(command string) string {
	if command == "" {
		return canonicalMcphubPath()
	}
	exe := command
	// A bare command name (no directory part) is resolved by exec.Command via
	// PATH (LookPath), NOT relative to the supervisor's CWD. filepath.Abs would
	// wrongly prepend the CWD, so the identity proof would compare the live
	// daemon against <cwd>/<name> and report a false pid_identity_mismatch for
	// legacy/fallback rows whose Command is bare (e.g. "mcphub.exe" from the
	// supervisorDaemonsFromPlan mcphubShortName fallback). Resolve it the same
	// way spawn does; if it is not on PATH it is unspawnable anyway, so fall
	// back to the supervisor's own binary (the prior behavior for these rows).
	// (Codex bot #270 P2.)
	if filepath.Base(exe) == exe {
		looked, err := exec.LookPath(exe)
		if err != nil {
			return canonicalMcphubPath()
		}
		exe = looked
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Clean(exe)
}

// supervisorStartupBindDeadline resolves the P1b first-bind deadline for a
// descriptor: an explicit StartupBindDeadlineSeconds>0 wins; otherwise a
// serena-proxy descriptor (whose language-server subprocess is slow to come
// up) gets 120s; everything else gets the 60s default. The isSerenaProxyDescriptor
// arm is defense for pre-field serena rows written before StartupBindDeadlineSeconds
// existed — a fresh install stamps 120 explicitly.
func supervisorStartupBindDeadline(d api.SupervisorDaemon) time.Duration {
	if d.StartupBindDeadlineSeconds > 0 {
		return time.Duration(d.StartupBindDeadlineSeconds) * time.Second
	}
	if isSerenaProxyDescriptor(d) {
		return supervisorSerenaStartupBindDeadline
	}
	return supervisorDefaultStartupBindDeadline
}

// supervisorDaemonEntryLive evaluates one daemon's liveness against the GLOBAL
// liveness probe set (supervisorLivenessProbeFns). It is a thin wrapper over
// supervisorDaemonEntryLiveWithProbe so callers that resolve every daemon
// against a SHARED port-owner snapshot (supervisorStatusDaemons) can pass a
// snapshot-backed probe instead, taking ONE OS port-owner query per refresh
// rather than one per daemon — with zero behavior change for the wrapper's
// existing callers (the liveness sweep).
//
// The wrapper has NO bind latch (it is called from the status path + the
// startup runtime scan, which are stateless per-call), so it passes the
// descriptor's first-bind deadline as grace and discards the portBoundByCurrentPID
// return. Tradeoff: a status refresh reports a bound-then-lost port as Stale
// only after the (longer) deadline instead of 5s — display-only; restart
// decisions run exclusively through the sweep, which DOES latch.
func supervisorDaemonEntryLive(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, string) {
	live, reason, _ := supervisorDaemonEntryLiveWithProbe(d, entry, now, supervisorLivenessProbeFns, supervisorStartupBindDeadline(d))
	return live, reason
}

// supervisorDaemonEntryLiveWithProbe evaluates one daemon's liveness. bindGrace
// is the tolerance applied to an unbound / owner-unverifiable port before it
// becomes a not-live reason — the sweep passes the P1b startup deadline before
// the current generation's first observed bind, and the 5s post-bind grace
// afterwards. portBoundByCurrentPID is true only when the port was positively
// confirmed bound by the tracked current PID (the verified-owner success return
// and the PortLive-fallback success); the sweep latches on it so subsequent
// unbound windows fall under the 5s post-bind rule.
func supervisorDaemonEntryLiveWithProbe(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time, probe supervisorLivenessProbe, bindGrace time.Duration) (bool, string, bool) {
	if probe.PIDAlive == nil {
		probe.PIDAlive = process.IsPidAlive
	}
	if entry.CurrentPID <= 0 {
		return false, supervisorLivenessReasonMissingPID, false
	}
	if !probe.PIDAlive(entry.CurrentPID) {
		return false, supervisorLivenessReasonPIDDead, false
	}
	if probe.PIDIdentity != nil {
		if entry.StartedAt.IsZero() {
			return false, supervisorLivenessReasonPIDIdentityMissing, false
		}
		// The daemon runs from its CONFIGURED command (d.Command — the exact
		// exe the supervisor exec'd), which may differ from the supervisor's
		// own binary. Compare the live PID against the daemon's exe, NOT the
		// supervisor's canonicalMcphubPath(), or a dev-build supervisor would
		// flag every release-path daemon pid_identity_mismatch (bug
		// 2026-06-09-supervisor-loses-current-pid-false-quarantine.md).
		expectedExe := daemonExpectedIdentityExe(d.Command)
		if expectedExe == "" {
			return false, supervisorLivenessReasonPIDIdentityMissing, false
		}
		err := probe.PIDIdentity(process.PIDIdentityProof{
			PID:            entry.CurrentPID,
			ExecutablePath: expectedExe,
			StartedAt:      entry.StartedAt.UTC().Format(time.RFC3339Nano),
		})
		if err != nil {
			if errors.Is(err, process.ErrProcessIdentityUnsupported) {
				// Keep the PIDAlive result on platforms without start-time proof.
			} else if errors.Is(err, process.ErrProcessAlreadyExited) {
				return false, supervisorLivenessReasonPIDDead, false
			} else {
				return false, supervisorLivenessReasonPIDIdentityMismatch, false
			}
		}
	}
	if d.Port <= 0 {
		// No port to bind — nothing to latch (portBoundByCurrentPID stays false
		// so the sweep never treats a port-less daemon as "bound").
		return true, "", false
	}
	if probe.PortOwnerPID != nil {
		ownerPID, ok, err := probe.PortOwnerPID(d.Port)
		if err != nil {
			// PROBE ERROR: the OS-level owner lookup could not run (e.g.
			// netstat is policy-blocked by AppLocker/WDAC, or the port is out
			// of range). This is NOT proof the daemon is dead — it only means
			// we could not VERIFY who owns the socket. Treating it as a kill
			// signal restart-loops the whole fleet indefinitely because every
			// 5s sweep repeats the same failing probe (Codex bot #268 P2,
			// supervise_liveness.go:261). But it is ALSO NOT proof the daemon
			// is healthy: a bound+answering port could be a DIFFERENT process
			// while the tracked PID is merely still alive, so we must NOT report
			// a clean live and suppress the ambiguity just because some listener
			// answers (Codex bot #268 P2, supervise_liveness.go:303). Within the
			// bind grace a freshly-spawned daemon may not have bound yet — give
			// it grace; otherwise fall through to the UNVERIFIED reason below.
			// The owner is unverifiable, so we CANNOT confirm the current PID
			// bound the port — portBoundByCurrentPID stays false.
			if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < bindGrace {
				return true, "", false
			}
			// Past grace, port not answering, owner unverifiable. This is
			// genuinely ambiguous: it is still NOT proof a FOREIGN process owns
			// the port (the definition of port_owner_mismatch), so a probe
			// error must not drive a restart. Return the UNVERIFIED reason; the
			// sweep observes it with a warn and leaves the daemon running
			// (port_owner_unverified is deliberately excluded from
			// supervisorLivenessReasonNeedsRestart). The live PID is retained
			// for handoff because the reason stays in
			// supervisorLivenessReasonHasLivePID.
			return false, supervisorLivenessReasonPortOwnerUnverified, false
		}
		if !ok {
			if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < bindGrace {
				return true, "", false
			}
			return false, supervisorLivenessReasonPortUnbound, false
		}
		if supervisorSelfPIDFn != nil && ownerPID == supervisorSelfPIDFn() {
			return false, supervisorLivenessReasonPortOwnerSelf, false
		}
		if ownerPID != entry.CurrentPID {
			return false, supervisorLivenessReasonPortOwnerMismatch, false
		}
		// Verified: the tracked current PID owns the port. This is the positive
		// first-bind proof the sweep latches on so subsequent unbound windows
		// fall under the 5s post-bind grace.
		return true, "", true
	}
	if probe.PortLive == nil {
		probe.PortLive = supervisorPortLive
	}
	if !probe.PortLive(d.Port) {
		if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < bindGrace {
			return true, "", false
		}
		return false, supervisorLivenessReasonPortUnbound, false
	}
	// PortLive-fallback (macOS / POSIX with no OS owner probe): TCP confirms a
	// listener answers on the port. We cannot prove WHICH pid owns it, but the
	// PID-identity gate above already confirmed the tracked PID is our binary,
	// so treat the answering port as bound-by-current for latch purposes.
	return true, "", true
}

// supervisorLivenessReasonNeedsRestart reports whether a not-live reason
// represents a CONFIRMED problem the supervisor should resolve by restarting
// the daemon (terminate-first → respawn). Only reasons that prove the daemon
// is no longer correctly serving its port qualify:
//   - port_unbound: the port is not bound and the bind grace has elapsed.
//   - port_owner_mismatch: a DIFFERENT live process owns the port.
//   - port_owner_self: the supervisor itself owns the port (stale handoff).
//
// port_owner_unverified is deliberately EXCLUDED: it is a probe ERROR (the
// OS-level owner lookup could not run), NOT proof of a dead or foreign-owned
// daemon. Restarting on a probe failure would loop the fleet indefinitely
// (Codex bot #268 P2). The sweep short-circuits that reason to an
// observe-only warn with no event posted, so it never reaches this predicate
// in practice; excluding it here keeps the "what restarts" invariant honest
// at its source. (It DOES remain in supervisorLivenessReasonHasLivePID so the
// startup retain-for-handoff path keeps the live PID rather than clearing it.)
func supervisorLivenessReasonNeedsRestart(reason string) bool {
	switch reason {
	case supervisorLivenessReasonPortUnbound,
		supervisorLivenessReasonPortOwnerMismatch,
		supervisorLivenessReasonPortOwnerSelf:
		return true
	default:
		return false
	}
}

func supervisorLivenessReasonHasLivePID(reason string) bool {
	switch reason {
	case supervisorLivenessReasonPortUnbound,
		supervisorLivenessReasonPortOwnerMismatch,
		supervisorLivenessReasonPortOwnerSelf,
		supervisorLivenessReasonPortOwnerUnverified:
		return true
	default:
		return false
	}
}

func supervisorIntentPortMapForStateDir(stateDir string) map[string]int {
	out := map[string]int{}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil || intent == nil {
		return out
	}
	for _, d := range intent.Daemons {
		out[canonicalSupervisorTaskName(d.TaskName)] = d.Port
	}
	return out
}

// supervisorIntentCommandMapForStateDir returns canonical task name ->
// configured Command from supervisor-intent.json. The startup runtime scan
// (loadSupervisorCurrentRunning) needs this because supervisor-state.json
// (api.SupervisorDaemonState) carries only runtime PID/state, NOT the
// daemon's exe path, yet the PID-identity proof must compare against the
// DAEMON's configured exe (its install path), not the supervisor's own
// canonicalMcphubPath(). A task absent from intent (or whose Command is
// empty) yields no entry; the caller falls back to canonicalMcphubPath()
// via daemonExpectedIdentityExe(""). Mirrors supervisorIntentPortMapForStateDir.
func supervisorIntentCommandMapForStateDir(stateDir string) map[string]string {
	out := map[string]string{}
	intent, err := api.ReadSupervisorIntent(filepath.Join(stateDir, "supervisor-intent.json"))
	if err != nil || intent == nil {
		return out
	}
	for _, d := range intent.Daemons {
		out[canonicalSupervisorTaskName(d.TaskName)] = d.Command
	}
	return out
}
