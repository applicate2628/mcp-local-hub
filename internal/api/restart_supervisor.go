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

// selectSupervisorOwnedTargets filters intent.Daemons down to the rows a
// supervisor-side maintenance pass (restart respawn / stop reconcile)
// should act on for the given (server, daemonFilter) scope. Maintenance
// rows are skipped, server/daemon identity falls back to
// ParseManagedTaskName when the descriptor fields are blank, and every
// returned TaskName is normalized to canonical leading-backslash form.
// Shared by restartSupervisorOwnedDaemons and stopSupervisorOwnedDaemons
// (spec §4 Phase A.1).
func selectSupervisorOwnedTargets(intent *SupervisorIntentFile, server, daemonFilter string) []SupervisorDaemon {
	if intent == nil || len(intent.Daemons) == 0 {
		return nil
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
	return targets
}

// loadSupervisorOwnedTargets reads supervisor-intent.json from the
// default state dir and selects the supervisor-owned targets in scope.
// A missing intent file (no supervisor install) yields (nil, nil) so
// callers fall through to the legacy scheduler path.
func loadSupervisorOwnedTargets(server, daemonFilter string) ([]SupervisorDaemon, error) {
	intentPath, err := DefaultSupervisorIntentPath()
	if err != nil {
		return nil, fmt.Errorf("resolve supervisor intent path: %w", err)
	}
	intent, err := ReadSupervisorIntent(intentPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read supervisor-intent.json: %w", err)
	}
	return selectSupervisorOwnedTargets(intent, server, daemonFilter), nil
}

func restartSupervisorOwnedDaemons(ctx context.Context, server, daemonFilter string) ([]RestartResult, bool, error) {
	targets, err := loadSupervisorOwnedTargets(server, daemonFilter)
	if err != nil {
		return nil, false, err
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
			results = append(results, RestartResult{TaskName: d.TaskName, Err: msg, Code: result.Code})
			continue
		}
		NewAPI().recordRestartIntentForTask(strings.TrimPrefix(d.TaskName, `\`), nil)
		results = append(results, RestartResult{TaskName: d.TaskName, Code: result.Code})
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
	// Use the shared maintenance predicate so *-watchdog rows are skipped on
	// restart too, not just *-weekly-refresh: a watchdog row left in a legacy
	// or hand-edited supervisor-intent.json must not go through supervisor
	// respawn as if it were a daemon (deep-sec #268).
	return isMaintenanceTaskName(taskName)
}
