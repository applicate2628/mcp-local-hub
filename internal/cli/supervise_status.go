package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mcp-local-hub/internal/api"
)

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
	rows := make([]map[string]any, 0, len(intent.Daemons))
	for _, d := range intent.Daemons {
		taskName := canonicalSupervisorTaskName(d.TaskName)
		server, daemon := api.ParseManagedTaskName(taskName)
		if server == "" && d.Server != "" {
			server = d.Server
		}
		if daemon == "" {
			if d.Daemon != "" {
				daemon = d.Daemon
			} else {
				daemon = "default"
			}
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
			if p, ok := api.ResolveManifestDaemonPort(server, daemon); ok {
				port = p
			}
		}
		stalePID := 0
		if ok && runtimeState.State == daemonRuntimeStateRunning {
			live, _ := supervisorDaemonEntryLive(api.SupervisorDaemon{
				TaskName: d.TaskName,
				Server:   server,
				Daemon:   daemon,
				Port:     port,
			}, runtimeState, time.Now().UTC())
			if !live {
				stalePID = runtimeState.CurrentPID
				stateText = "Restarting"
				runtimeState.CurrentPID = 0
			}
		}
		row := map[string]any{
			"task_name":        taskName,
			"server":           server,
			"daemon":           daemon,
			"command":          d.Command,
			"args":             args,
			"workspace":        d.Workspace,
			"port":             port,
			"state":            stateText,
			"current_pid":      runtimeState.CurrentPID,
			"started_at":       daemonRuntimeStartedAt(runtimeState.StartedAt),
			"restart_history":  []api.RestartEvent{},
			"backoff_until":    "",
			"quarantine_since": "",
			"is_maintenance":   isSupervisorMaintenanceTask(taskName),
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

func supervisorStatusGUIState(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "running":
		return "Running"
	case "idle":
		return "Stopped"
	case "backoff", "backoff-waiting", "spawning":
		return "Restarting"
	case "quarantine", "quarantined":
		return "Quarantined"
	case "":
		return "Idle"
	default:
		return raw
	}
}

func daemonRuntimeStartedAt(startedAt time.Time) string {
	if startedAt.IsZero() {
		return ""
	}
	return startedAt.UTC().Format(time.RFC3339Nano)
}

func isSupervisorMaintenanceTask(taskName string) bool {
	name := strings.TrimPrefix(taskName, `\`)
	return strings.HasSuffix(name, "-weekly-refresh")
}
