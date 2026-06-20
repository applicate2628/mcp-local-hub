package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// resolveManifestDaemonPortFn is the per-(server, daemon) manifest port
// lookup the status producer calls for daemons whose intent Port is 0 (the
// PR #211-and-earlier installs that wrote Port=0 for every daemon).
// Indirected through a package var so status tests can inject a deterministic
// resolver AND assert it is loaded AT MOST ONCE PER SERVER per refresh — the
// per-server-manifest memo (newManifestPortResolver) wraps this fn so a
// server with N Port=0 daemons reads + parses that server's manifest.yaml
// ONCE, not N times. Defaults to the production embed-first lookup.
var resolveManifestDaemonPortFn = api.ResolveManifestDaemonPort

// newManifestPortResolver returns a closure that resolves a daemon's port
// from its server manifest, MEMOIZED PER SERVER for the lifetime of the
// returned closure (one supervisorStatusDaemons refresh).
//
// Perf: ResolveManifestDaemonPort reads + parses the WHOLE server manifest
// YAML on every call (loadManifestForServer → loadManifestYAMLEmbedFirst →
// config.ParseManifest). A server with multiple daemons whose intent Port is
// 0 (e.g. serena's dynamic workspace-proxy pool, or any multi-daemon server
// from a pre-port-seeding install) used to re-parse the SAME manifest once
// per daemon row. The memo collapses that to ONE parse per server per status
// refresh — the same per-iteration-redundant-parse class the port-owner
// snapshot already fixed for netstat. The lookup result is unchanged: the
// underlying fn is keyed on (server, daemon) and the memo stores the exact
// (port, ok) pair it returned, so the resolved value is byte-identical.
func newManifestPortResolver() func(server, daemon string) (int, bool) {
	type portResult struct {
		port int
		ok   bool
	}
	// memo[server][daemon] → resolved (port, ok). The server-level map is
	// allocated lazily on the first daemon seen for that server; its mere
	// presence records that the server's manifest was already consulted, so
	// a second daemon of the same server reuses the cached entry instead of
	// re-reading the manifest.
	memo := map[string]map[string]portResult{}
	return func(server, daemon string) (int, bool) {
		byDaemon, seenServer := memo[server]
		if seenServer {
			if r, ok := byDaemon[daemon]; ok {
				return r.port, r.ok
			}
		} else {
			byDaemon = map[string]portResult{}
			memo[server] = byDaemon
		}
		port, ok := resolveManifestDaemonPortFn(server, daemon)
		byDaemon[daemon] = portResult{port: port, ok: ok}
		return port, ok
	}
}

func supervisorStatusDaemons(stateDir string, tracker *DaemonRuntimeTracker) ([]map[string]any, error) {
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
		snapshot, snapErr := loopbackPortOwnersSnapshotFn()
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

	// Per-refresh memo so a server with multiple Port=0 daemons reads +
	// parses that server's manifest.yaml ONCE, not once per daemon row.
	resolveManifestPort := newManifestPortResolver()

	rows := make([]map[string]any, 0, len(intent.Daemons))
	for _, d := range intent.Daemons {
		taskName := canonicalSupervisorTaskName(d.TaskName)
		server := strings.TrimSpace(d.Server)
		daemon := strings.TrimSpace(d.Daemon)
		if server == "" || daemon == "" {
			parsedServer, parsedDaemon := api.ParseManagedTaskName(taskName)
			if server == "" {
				server = parsedServer
			}
			if daemon == "" {
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
		// Port enrichment fallback: supervisor-intent.json from PR
		// #211 and earlier wrote Port=0 for every daemon (migration
		// did not seed the field from the manifest). The GUI matrix
		// then renders "—" even though the daemon is listening on the
		// manifest-declared port. Look up the canonical port from the
		// manifest when the intent value is 0 so existing installs
		// see the right port without needing an intent-file
		// migration. Future migrate code should populate Port at
		// write time — when it does, this lookup becomes a no-op.
		port := d.Port
		if port == 0 && server != "" {
			if p, ok := resolveManifestPort(server, daemon); ok {
				port = p
			}
		}
		stalePID := 0
		if ok && runtimeState.State == daemonRuntimeStateRunning {
			live, reason := supervisorDaemonEntryLiveWithProbe(api.SupervisorDaemon{
				TaskName: d.TaskName,
				Server:   server,
				Daemon:   daemon,
				Port:     port,
			}, runtimeState, time.Now().UTC(), livenessProbe)
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
