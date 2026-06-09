package cli

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

const (
	supervisorPortProbeTimeout = 300 * time.Millisecond
	supervisorPortBindGrace    = 5 * time.Second
	supervisorLivenessInterval = 5 * time.Second

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
	if runtime.GOOS == "windows" {
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
	sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events)
	for {
		select {
		case <-ctxDone:
			return
		case <-ticker.C:
			sweepSupervisorLivenessOnce(stateDir, livenessSweepIntent(stateDir, intent), tracker, loop, events)
		}
	}
}

// livenessSweepIntent returns the freshest supervisor intent for a sweep:
// the on-disk supervisor-intent.json when stateDir is set (so a mid-run
// install/migrate that rewrote ports is honored), falling back to the
// startup snapshot on any read error or when stateDir is empty (tests).
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
	stateDir string,
	intent *api.SupervisorIntentFile,
	tracker *DaemonRuntimeTracker,
	loop *api.EventLoop,
	events *api.SupervisorEventLog,
) {
	if tracker == nil || loop == nil || intent == nil {
		return
	}
	byTask := map[string]api.SupervisorDaemon{}
	for _, d := range intent.Daemons {
		byTask[canonicalSupervisorTaskName(d.TaskName)] = d
	}
	now := time.Now().UTC()
	for taskName, entry := range tracker.Snapshot() {
		taskName = canonicalSupervisorTaskName(taskName)
		if entry.State != daemonRuntimeStateRunning || entry.CurrentPID <= 0 {
			continue
		}
		d, ok := byTask[taskName]
		if !ok {
			continue
		}
		live, reason := supervisorDaemonEntryLive(d, entry, now)
		if live {
			continue
		}
		// Port-owner PROBE ERROR (could not determine the socket owner — e.g.
		// netstat policy-blocked) that even the TCP fallback could not turn
		// into a positive liveness result. This is observed but is NOT proof
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
						"note": "OS-level port-owner probe failed and the TCP fallback did not confirm liveness; leaving the daemon running (a probe error is not proof of a dead or foreign-owned daemon, so no restart is issued)",
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

func supervisorDaemonEntryLive(d api.SupervisorDaemon, entry DaemonRuntimeEntry, now time.Time) (bool, string) {
	probe := supervisorLivenessProbeFns
	if probe.PIDAlive == nil {
		probe.PIDAlive = process.IsPidAlive
	}
	if entry.CurrentPID <= 0 {
		return false, supervisorLivenessReasonMissingPID
	}
	if !probe.PIDAlive(entry.CurrentPID) {
		return false, supervisorLivenessReasonPIDDead
	}
	if probe.PIDIdentity != nil {
		if entry.StartedAt.IsZero() {
			return false, supervisorLivenessReasonPIDIdentityMissing
		}
		expectedExe := canonicalMcphubPath()
		if expectedExe == "" {
			return false, supervisorLivenessReasonPIDIdentityMissing
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
				return false, supervisorLivenessReasonPIDDead
			} else {
				return false, supervisorLivenessReasonPIDIdentityMismatch
			}
		}
	}
	if d.Port <= 0 {
		return true, ""
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
			// supervise_liveness.go:261). Degrade to the TCP loopback liveness
			// probe: the PID is already alive + identity-verified at this
			// point, so a bound+answering port is positive proof the daemon is
			// serving — return live. Distinguish this from a confirmed owner
			// MISMATCH below (a DIFFERENT live PID owns the port), which is a
			// real wedged-handoff that legitimately needs a restart.
			portLive := probe.PortLive
			if portLive == nil {
				portLive = supervisorPortLive
			}
			if portLive(d.Port) {
				return true, ""
			}
			// Port not answering yet, but within the bind grace a
			// freshly-spawned daemon may not have bound — treat as live (same
			// grace rule as the port-unbound and TCP-probe paths below).
			if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
				return true, ""
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
			return false, supervisorLivenessReasonPortOwnerUnverified
		}
		if !ok {
			if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
				return true, ""
			}
			return false, supervisorLivenessReasonPortUnbound
		}
		if supervisorSelfPIDFn != nil && ownerPID == supervisorSelfPIDFn() {
			return false, supervisorLivenessReasonPortOwnerSelf
		}
		if ownerPID != entry.CurrentPID {
			return false, supervisorLivenessReasonPortOwnerMismatch
		}
		return true, ""
	}
	if probe.PortLive == nil {
		probe.PortLive = supervisorPortLive
	}
	if !probe.PortLive(d.Port) {
		if !entry.StartedAt.IsZero() && now.Sub(entry.StartedAt) < supervisorPortBindGrace {
			return true, ""
		}
		return false, supervisorLivenessReasonPortUnbound
	}
	return true, ""
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
