package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mcp-local-hub/internal/api"
	"mcp-local-hub/internal/process"
)

// rssByPID resolves a process's resident set size (working-set bytes) by
// PID. Defaults to the platform implementation (Windows reads it via
// GetProcessMemoryInfo; other OSes return ok=false). Indirected through a
// package var so status tests can inject a deterministic value without a
// live process. ok=false means "RAM unknown" — the producer omits the
// ram_bytes field rather than emitting a misleading 0.
var rssByPID = process.ResidentSetSizeByPID

// loopbackPortOwnersSnapshotFn takes a SINGLE OS port-owner snapshot
// (one `netstat -ano` on Windows / one /proc/net/tcp read on Linux) mapping
// every IPv4-loopback LISTENING port to its owning PID. supervisorStatusDaemons
// takes ONE snapshot per refresh and resolves all daemons against it, instead
// of one per-port netstat spawn per daemon (the cold /api/status hot path:
// 15 running daemons used to fire 15 netstat spawns). Indirected through a
// package var so status tests can inject a deterministic snapshot and assert
// it is invoked EXACTLY ONCE regardless of daemon count. nil PortOwnerPID on
// the global liveness probe (macOS / other POSIX) skips the snapshot entirely
// and keeps the per-daemon PortLive TCP fallback.
var loopbackPortOwnersSnapshotFn = api.LoopbackPortOwnersSnapshot

// statusPortOwnersTTL bounds how often the supervisor status handler takes a
// fresh netstat snapshot. Concurrent AND rapid status IPC calls within the TTL
// share ONE snapshot. Stale warm-cache callers kick at most one background
// refresh and keep serving the cached result instead of stacking behind netstat.
const statusPortOwnersTTL = 1 * time.Second

// statusPortOwnersProbeTimeout caps a single netstat snapshot. On a host whose
// system network stack is wedged (netstat -ano observed >20s), the probe is
// killed at this deadline and returns a snapshot error -> per-daemon
// port_owner_unverified -> a FAST status IPC reply, never a >5s hang that trips
// the restart-watcher. 3s < the 5s status-IPC client timeout by design.
const statusPortOwnersProbeTimeout = 3 * time.Second

// statusPortOwnersCoalescer owns "how often to netstat for status" -- the single
// owner of netstat-frequency on the status path (the client-side DaemonStatusSnapshot
// cache is re-scoped to IPC/HTTP fan-in amortization only; see decision doc). It
// caches BOTH the snapshot AND the error for the TTL so a persistently-failing
// netstat is rate-limited to one probe/TTL instead of storming.
type statusPortOwnersCoalescer struct {
	mu           sync.Mutex
	takenAt      time.Time
	genAtProbe   uint64
	snapshot     map[int]int
	err          error
	inflight     bool
	inflightDone chan struct{}
	snapshotFn   func(context.Context) (map[int]int, error) // seam; default api.LoopbackPortOwnersSnapshotContext
	genFn        func() uint64                              // seam; default DaemonRuntimeTracker.Generation
	nowFn        func() time.Time                           // seam; default time.Now
	timeout      time.Duration                              // default statusPortOwnersProbeTimeout
}

func newStatusPortOwnersCoalescer(tracker *DaemonRuntimeTracker) *statusPortOwnersCoalescer {
	genFn := func() uint64 { return 0 }
	if tracker != nil {
		genFn = tracker.Generation
	}
	return &statusPortOwnersCoalescer{
		snapshotFn: api.LoopbackPortOwnersSnapshotContext,
		genFn:      genFn,
		nowFn:      time.Now,
		timeout:    statusPortOwnersProbeTimeout,
	}
}

// Get returns the coalesced port-owner snapshot. Within statusPortOwnersTTL
// (and an unchanged fleet generation) it returns the cached (snapshot, err).
// A warm cache that is only TTL-stale serves the stale result immediately
// while ONE background probe refreshes it for later callers — freshness there
// is a perf concern only, the fleet has not changed. A cold cache OR a fleet
// GENERATION change (a daemon respawned/exited since the cached probe) JOINS
// the single in-flight probe instead: a just-respawned daemon must never be
// classified against the pre-change owner map (read-your-writes — Codex #514
// r2; serving stale there reported a healthy fresh PID as Restarting/stale_pid
// until the async refresh landed). The join is bounded: each probe is
// ctx-limited to statusPortOwnersProbeTimeout (3s) < the 5s status-IPC budget,
// only fleet-change-adjacent calls pay it, and at most two rounds are waited
// (a respawn landing mid-probe triggers one fresh probe; after that the
// freshest cache is served — the documented self-correcting degraded mode —
// rather than risking unbounded waiting).
func (c *statusPortOwnersCoalescer) Get() (map[int]int, error) {
	now := c.nowFn()
	gen := c.currentGeneration()

	c.mu.Lock()
	if !c.takenAt.IsZero() && now.Sub(c.takenAt) < statusPortOwnersTTL && gen == c.genAtProbe {
		snapshot, err := c.snapshot, c.err
		c.mu.Unlock()
		return snapshot, err
	}

	// Warm cache, unchanged fleet, TTL-stale only → serve stale now, refresh
	// in the background.
	if !c.takenAt.IsZero() && gen == c.genAtProbe {
		if !c.inflight {
			c.startProbeLocked(gen)
		}
		snapshot, err := c.snapshot, c.err
		c.mu.Unlock()
		return snapshot, err
	}

	// Cold cache or generation mismatch → join/start the single probe and wait
	// for it ONCE. The total wait is thereby capped at one probe
	// (statusPortOwnersProbeTimeout, 3s) < the 5s status-IPC deadline — waiting
	// a second round after a mid-probe fleet change could stack 3s+3s > 5s and
	// re-open the exact timeout this path eliminates (Codex #514 r3). If the
	// generation moved again WHILE the joined probe ran (rare: a respawn racing
	// an already-slow probe), the freshest COMPLETED result is returned as-is
	// (≤ one probe stale, self-correcting) and a background probe is kicked for
	// the next caller.
	done := c.inflightDone
	if !c.inflight {
		done = c.startProbeLocked(c.currentGeneration())
	}
	c.mu.Unlock()
	<-done
	c.mu.Lock()
	if c.genAtProbe != c.currentGeneration() && !c.inflight {
		c.startProbeLocked(c.currentGeneration())
	}
	snapshot, err := c.snapshot, c.err
	c.mu.Unlock()
	return snapshot, err
}

func (c *statusPortOwnersCoalescer) startProbeLocked(gen uint64) chan struct{} {
	done := make(chan struct{})
	c.inflight = true
	c.inflightDone = done
	go c.runProbe(gen, done)
	return done
}

func (c *statusPortOwnersCoalescer) runProbe(gen uint64, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	snapshot, err := c.snapshotFn(ctx)
	takenAt := c.nowFn()

	c.mu.Lock()
	c.snapshot = snapshot
	c.err = err
	c.takenAt = takenAt
	c.genAtProbe = gen
	c.inflight = false
	if c.inflightDone == done {
		c.inflightDone = nil
	}
	c.mu.Unlock()
	close(done)
}

func (c *statusPortOwnersCoalescer) currentGeneration() uint64 {
	if c.genFn == nil {
		return 0
	}
	return c.genFn()
}

func supervisorStatusDaemons(stateDir string, tracker *DaemonRuntimeTracker, coalescer *statusPortOwnersCoalescer) ([]map[string]any, error) {
	intentPath := filepath.Join(stateDir, "supervisor-intent.json")
	intent, err := api.ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []map[string]any{}, nil
		}
		return nil, fmt.Errorf("read supervisor-intent.json: %w", err)
	}
	if intent == nil || len(intent.Daemons) == 0 {
		return []map[string]any{}, nil
	}

	daemonStates := tracker.Snapshot()

	// Perf: take ONE OS port-owner snapshot for the whole refresh and resolve
	// every daemon against it, instead of letting each daemon's liveness check
	// spawn its own `netstat -ano` (15 running daemons → 15 spawns → ~730ms
	// cold /api/status; same anti-pattern scan.go already fixed). Only when the
	// global liveness probe HAS an OS-level port-owner lookup (Windows/Linux);
	// when it is nil (macOS / other POSIX) we keep today's PortLive TCP fallback
	// unchanged — no snapshot, no behavior change.
	livenessProbe := supervisorLivenessProbeFns
	if livenessProbe.PortOwnerPID != nil {
		var snapshot map[int]int
		var snapErr error
		if coalescer != nil {
			snapshot, snapErr = coalescer.Get()
		} else {
			// nil coalescer -> direct non-ctx probe, byte-identical to pre-fix
			// (keeps the existing direct-call status tests valid).
			snapshot, snapErr = loopbackPortOwnersSnapshotFn()
		}
		// Replace the per-port netstat lookup with a closure that resolves from
		// the single shared snapshot. Semantics match the per-port path EXACTLY:
		//   - snapshot error → every port returns that err → the daemon is
		//     classified port_owner_unverified (within bind grace → live), the
		//     same fail-closed outcome as a per-port netstat failure;
		//   - port in map → its owner PID (== tracked PID → live; != → mismatch;
		//     == supervisor self → port_owner_self);
		//   - port NOT in map → (0, false, nil) = port_unbound, exactly what a
		//     per-port netstat finding nothing returns.
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

	// The port-resolution owner, memoized per refresh (one manifest parse per
	// server). It resolves each daemon's EFFECTIVE port + first-bind deadline —
	// the single authority the liveness sweep, squatter, and recover paths also
	// use, so a Port=0 descriptor shows its manifest port for display exactly as
	// the sweep protects it.
	portResolver := api.NewDaemonPortResolver()

	rows := make([]map[string]any, 0, len(intent.Daemons))
	for _, d := range intent.Daemons {
		taskName := canonicalSupervisorTaskName(d.TaskName)
		// SERVER: field-first, but fail closed on a real `--server`-flag MISMATCH.
		// DescriptorServerName returns the Server field when it agrees with (or the row
		// has no) `--server` FLAG, the flag value when the field is blank, and "" ONLY
		// on a field/argv `--server` MISMATCH — so it doubles as the mismatch detector.
		// Critically, this status server feeds the secret-rotation restart bucketing
		// (`runningByServer` in internal/api/secrets.go): a lying-cache row
		// ({Server:memory, args --server time}) must NOT display/route as "memory".
		//   - populated field → keep DescriptorServerName's result: the field if it
		//     agrees / has no --server flag, "" if it contradicts the flag (fail closed).
		//   - blank field + --server flag → the flag value.
		//   - blank field + NO --server flag → task-name recovery. This covers the LEGACY
		//     POSITIONAL argv shape (`daemon <server>` / `daemon <server> --daemon <d>`,
		//     e.g. Args ["daemon","memory"]) whose server lives only in the task name —
		//     the earlier !DescriptorHasGlobalDaemonArgv gate wrongly blocked it because
		//     that predicate is true for any `daemon …` argv incl. positional (bot #506).
		resolvedServer := api.DescriptorServerName(d)
		server := strings.TrimSpace(d.Server)
		if server != "" {
			server = resolvedServer // keep-if-agrees / "" on --server-flag mismatch
		} else if resolvedServer != "" {
			server = resolvedServer // blank field + --server flag
		} else {
			parsedServer, _ := api.ParseManagedTaskName(taskName)
			server = parsedServer // positional / true task-name-only row
		}
		// DAEMON: field-first, recovering a BLANK field via the owner args then the
		// task name. A populated Daemon field is PRESERVED (bot #506 P2) — the daemon
		// column within a resolved server is secondary, and a corrupt row cannot be
		// restarted regardless (the pair restart selector fails closed on the
		// field/argv mismatch), so a populated-daemon-that-contradicts-its-argv is a
		// fail-safe display residual, not an outcome bug. The positional shape recovers
		// its daemon from the task name (DescriptorServerDaemon fails without a --server
		// flag, then ParseManagedTaskName splits \mcp-local-hub-<server>-<daemon>).
		daemon := strings.TrimSpace(d.Daemon)
		if daemon == "" {
			if _, rd, ok := api.DescriptorServerDaemon(d); ok {
				daemon = rd
			} else {
				_, parsedDaemon := api.ParseManagedTaskName(taskName)
				daemon = parsedDaemon
			}
		}
		if daemon == "" {
			daemon = "default"
		}
		runtimeState, ok := daemonStates[taskName]
		if !ok {
			runtimeState, ok = daemonStates[strings.TrimPrefix(taskName, `\`)]
		}
		stateText := "Idle"
		if ok {
			stateText = supervisorStatusGUIState(runtimeState.State)
		}
		args := d.Args
		if args == nil {
			args = []string{}
		}
		// Resolve the EFFECTIVE port + first-bind deadline through the owner. A
		// Port=0 descriptor (PR #211-and-earlier installs wrote Port=0 for every
		// daemon) shows its manifest port instead of "—"; a Port>0 row short-
		// circuits.
		//
		// Copy the WHOLE row (mirrors gui/daemon_env.go) so a populated field that a
		// partial argv COMPLETES (`{Server:memory,Daemon:default,Args:[daemon --server
		// memory]}`) keeps its Daemon field and resolves the manifest port — rebuilding
		// effDesc without the fields dropped that completion and regressed the port to
		// 0 (commission fable r6b P1). Then OVERWRITE Server/Daemon with the
		// task-name-recovered identity ONLY when it is SAFE (not a corrupt/partial
		// GLOBAL argv): for a corrupt global the kept fields still disagree with the
		// args, so the owner fails closed and reports an HONEST port 0 rather than
		// fabricating a manifest port for a malformed row.
		effDesc := d
		if !api.DescriptorHasGlobalDaemonArgv(d) {
			effDesc.Server = server
			effDesc.Daemon = daemon
		}
		port, deadlineSecs, _ := portResolver.Resolve(effDesc)
		stalePID := 0
		if ok && runtimeState.State == daemonRuntimeStateRunning {
			// Status has no bind latch (it is a stateless per-refresh view), so
			// pass the descriptor's P1b startup deadline as grace and discard the
			// portBoundByCurrentPID return. Tradeoff: a bound-then-lost port shows
			// Stale in status only after the deadline instead of 5s — display-only;
			// restart decisions run exclusively through the latch-owning sweep.
			live, reason, _, _ := supervisorDaemonEntryLiveWithProbe(api.SupervisorDaemon{
				TaskName:                   d.TaskName,
				Server:                     server,
				Daemon:                     daemon,
				Port:                       port,
				Args:                       d.Args,
				StartupBindDeadlineSeconds: deadlineSecs,
			}, runtimeState, time.Now().UTC(), livenessProbe, time.Duration(deadlineSecs)*time.Second)
			// port_owner_unverified is a probe ERROR (e.g. netstat blocked),
			// not a restart: the liveness sweep deliberately only observes it
			// (no EvManualRestart), so the status must not report "Restarting"
			// for it — keep the running view (deep-sec #268).
			if !live && reason != supervisorLivenessReasonPortOwnerUnverified {
				stalePID = runtimeState.CurrentPID
				stateText = "Restarting"
				runtimeState.CurrentPID = 0
			}
		}
		// NOTE: restart-policy runtime state (restart_history / backoff_until
		// / quarantine_since / queued_action) is deliberately NOT in this IPC
		// status row. It is in-memory-only in the supervisor and resets on
		// cold restart (see api.SupervisorDaemonState's doc comment); the
		// pre-2026-06-20 row emitted those keys as always-empty placeholders
		// that no consumer (GUI, mcphub status CLI, health.go) ever read, so
		// they were dropped with the persisted fields (audit P3).
		row := map[string]any{
			"task_name":      taskName,
			"server":         server,
			"daemon":         daemon,
			"command":        d.Command,
			"args":           args,
			"workspace":      d.Workspace,
			"port":           port,
			"state":          stateText,
			"current_pid":    runtimeState.CurrentPID,
			"started_at":     daemonRuntimeStartedAt(runtimeState.StartedAt),
			"is_maintenance": isSupervisorMaintenanceTask(taskName),
		}
		if stalePID != 0 {
			row["stale_pid"] = stalePID
		}
		// Surface orphan PID when present (Windows post-create orphan
		// path where best-effort kill failed). Operator visibility for
		// manual cleanup; SEPARATE from current_pid because terminate
		// path reads current_pid as the live daemon PID. Closes bot
		// finding on PR #238 fd51536 (P2 persist-the-preserved-
		// orphan-PID).
		if runtimeState.OrphanPID != 0 {
			row["orphan_pid"] = runtimeState.OrphanPID
		}
		// Surface per-spawn Job Object allocation state when it has
		// been explicitly probed (not nil). Tri-state preserved across
		// IPC: only &true/&false rows emit the field; nil rows omit
		// it so legacy state files and pre-spawn daemons surface as
		// "unknown" (no badge) rather than "unprotected". Closes
		// consultant strategic concern #1 on PR #241.
		if runtimeState.JobProtection != nil {
			row["job_protection"] = *runtimeState.JobProtection
		}
		// Surface a pre-spawn existence-gate HOLD (P1.1) so the operator sees
		// WHY a daemon is not running and WHAT to do, rather than an
		// unexplained non-running row. Omitted entirely on the happy path.
		if runtimeState.SpawnHoldReason != "" {
			row["spawn_hold_reason"] = runtimeState.SpawnHoldReason
			row["spawn_hold_path"] = runtimeState.SpawnHoldPath
		}
		// Per-daemon resident-set-size (RAM). Looked up by the live
		// current_pid only when the daemon is actually Running — a
		// port-stale daemon already had CurrentPID zeroed above (stateText
		// flipped to "Restarting"), and an Idle/Stopped row has no live
		// process to measure. rssByPID returns ok=false when RAM cannot be
		// determined (non-Windows, PID recycled, OpenProcess denied); we
		// omit ram_bytes in that case so the GUI renders no RAM row rather
		// than a misleading "0 MB". Best-effort diagnostic — no error path.
		if stateText == "Running" && runtimeState.CurrentPID > 0 {
			if ram, okRAM := rssByPID(runtimeState.CurrentPID); okRAM && ram > 0 {
				row["ram_bytes"] = ram
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func canonicalSupervisorTaskName(taskName string) string {
	if taskName == "" || strings.HasPrefix(taskName, `\`) {
		return taskName
	}
	return `\` + taskName
}

// supervisorStatusGUIState is a thin delegate over the canonical daemon-state
// classifier (internal/api/daemon_state.go::ProjectGUIState). The producer-side
// vocabulary (tracker raw-runtime state -> Title-case GUI/IPC-wire state) lives
// in one place shared with the IPC-consumer and /api/health projections; this
// function name is kept so its caller (supervisorStatusDaemons) is unchanged.
func supervisorStatusGUIState(raw string) string {
	return api.ProjectGUIState(raw)
}

func daemonRuntimeStartedAt(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return startedAt.UTC().Format(time.RFC3339Nano)
}

func isSupervisorMaintenanceTask(taskName string) bool {
	// Use the shared predicate so *-watchdog rows are treated as maintenance
	// too (not just *-weekly-refresh) — keeps the CLI env selectors and the
	// status is_maintenance flag consistent with the rest of the codebase
	// (deep-sec #268: a watchdog row must be excluded from env selectors).
	return api.IsMaintenanceTaskName(taskName)
}
