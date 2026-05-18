package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mcp-local-hub/internal/api"
)

func supervisorStatusDaemons(stateDir string) ([]map[string]any, error) {
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

	statePath := filepath.Join(stateDir, "supervisor-state.json")
	state, stateErr := api.ReadSupervisorState(statePath)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read supervisor-state.json: %w", stateErr)
	}
	var daemonStates map[string]api.SupervisorDaemonState
	if stateErr == nil && state != nil {
		daemonStates = state.Daemons
	}

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
		stateText := "Unknown"
		if ok {
			stateText = supervisorStatusGUIState(runtimeState.State)
		}
		args := d.Args
		if args == nil {
			args = []string{}
		}
		rows = append(rows, map[string]any{
			"task_name":        taskName,
			"server":           server,
			"daemon":           daemon,
			"command":          d.Command,
			"args":             args,
			"workspace":        d.Workspace,
			"port":             d.Port,
			"state":            stateText,
			"current_pid":      runtimeState.CurrentPID,
			"started_at":       runtimeState.StartedAt,
			"restart_history":  runtimeState.RestartHistory,
			"backoff_until":    runtimeState.BackoffUntil,
			"quarantine_since": runtimeState.QuarantineSince,
			"is_maintenance":   isSupervisorMaintenanceTask(taskName),
		})
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
	case "backoff", "backoff-waiting":
		return "Restarting"
	default:
		return raw
	}
}

func isSupervisorMaintenanceTask(taskName string) bool {
	name := strings.TrimPrefix(taskName, `\`)
	return strings.HasSuffix(name, "-weekly-refresh")
}
