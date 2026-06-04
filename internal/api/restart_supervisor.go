package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type supervisorRestartRespawnFunc func(ctx context.Context, taskName string, force bool, timeoutMs int) (RespawnResult, error)

var supervisorRestartRespawnFn supervisorRestartRespawnFunc = DialSupervisorIPCRespawn

func setSupervisorRestartHooksForTest(fn supervisorRestartRespawnFunc) func() {
	prev := supervisorRestartRespawnFn
	supervisorRestartRespawnFn = fn
	return func() { supervisorRestartRespawnFn = prev }
}

func restartSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, false, fmt.Errorf("resolve supervisor intent path: %w", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read supervisor-intent.json: %w", err)
	}
	if intent == nil || len(intent.Daemons) == 0 {
		return nil, false, nil
	}
	var targets []SupervisorDaemon
	for _, d := range intent.Daemons {
		if isSupervisorRestartMaintenanceTask(d.TaskName) {
			continue
		}
		rowServer := strings.TrimSpace(d.Server)
		rowDaemon := strings.TrimSpace(d.Daemon)
		if rowServer == "" || rowDaemon == "" {
			parsedServer, parsedDaemon := ParseManagedTaskName(d.TaskName)
			if rowServer == "" {
				rowServer = parsedServer
			}
			if rowDaemon == "" {
				rowDaemon = parsedDaemon
			}
		}
		if rowDaemon == "" {
			rowDaemon = "default"
		}
		if server != "" && rowServer != server {
			continue
		}
		if daemonFilter != "" && rowDaemon != daemonFilter {
			continue
		}
		d.TaskName = normalizeSupervisorRestartTaskName(d.TaskName)
		targets = append(targets, d)
	}
	if len(targets) == 0 {
		return nil, false, nil
	}
	results := make([]RestartResult, 0, len(targets))
	for _, d := range targets {
		result, err := supervisorRestartRespawnFn(ctx, d.TaskName, false, 5000)
		if err != nil {
			results = append(results, RestartResult{TaskName: d.TaskName, Err: err.Error()})
			continue
		}
		if !result.Success {
			msg := result.Message
			if msg == "" {
				msg = result.Code
			}
			results = append(results, RestartResult{TaskName: d.TaskName, Err: msg})
			continue
		}
		NewAPI().recordRestartIntentForTask(strings.TrimPrefix(d.TaskName, `\`), nil)
		results = append(results, RestartResult{TaskName: d.TaskName})
	}
	return results, true, nil
}

func normalizeSupervisorRestartTaskName(taskName string) string {
	if taskName == "" || strings.HasPrefix(taskName, `\`) {
		return taskName
	}
	return `\` + taskName
}

func isSupervisorRestartMaintenanceTask(taskName string) bool {
	name := strings.TrimPrefix(taskName, `\`)
	return strings.HasSuffix(name, "-weekly-refresh")
}
